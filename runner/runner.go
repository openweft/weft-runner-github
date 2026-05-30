// Package runner is the daemon side of weft-runner-github.
//
// Lifecycle in one paragraph: Register hits GitHub once to sanity-check the
// PAT + scope, then writes a JSON config. Run loads that config, dials the
// weft cluster, and maintains a pool of N ephemeral microVMs. Each pool slot
// mints a fresh JIT runner config from GitHub, boots a microVM with that
// config injected, and waits for the VM to exit (which, for an
// `actions/runner --jitconfig … --ephemeral` invocation, happens exactly
// when the assigned job finishes — that's the whole point of the ephemeral
// flag). On exit we delete the VM, garbage-collect the offline runner on
// GitHub's side, and spin up a replacement.
//
// What is NOT here (and won't be in this commit):
//
//   - The in-VM agent that runs `actions/runner --jitconfig …`. That's an
//     image-side concern: build a rootfs that ships actions/runner +
//     systemd unit + the runner's "read JIT config from /run/weft/cfg/
//     github-jit.txt" wiring. The daemon only needs to make that file
//     present, which today happens via the microVM's `cfg` share.
//
//   - Log streaming back to the daemon. The agent inside the VM does its
//     own log shipping to GitHub via the runtime API; the daemon doesn't
//     need to proxy it. We do however watch the VM lifecycle so we can
//     replace a stuck VM after IdleTimeout.

package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// RegisterOptions are the inputs to `weft-runner-github register`. Owner +
// Token are required. Repo selects a repo-scoped runner (empty = org-wide).
type RegisterOptions struct {
	Owner      string
	Repo       string
	Token      string
	Labels     []string
	ConfigFile string
}

// Register sanity-checks the token+scope by minting (and discarding) a
// registration token, then writes a persisted config Run can load. We mint
// the *registration* token rather than a JIT config because the latter is
// destructive (it creates a runner row in GitHub's database), whereas the
// former is a cheap "does this PAT have admin:org on the scope" probe.
func Register(opts RegisterOptions) error {
	if opts.Owner == "" || opts.Token == "" {
		return errors.New("register: --owner and --token are required")
	}
	if opts.ConfigFile == "" {
		return errors.New("register: --config is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	g := newGH(opts.Token)
	tok, err := g.mintRegistrationToken(ctx, opts.Owner, opts.Repo)
	if err != nil {
		return fmt.Errorf("mint registration token (scope check): %w", err)
	}
	log.Printf("weft-runner-github register: token scope ok (expires %s)", tok.ExpiresAt.Format(time.RFC3339))

	cfg := PersistedConfig{
		Owner:      opts.Owner,
		Repo:       opts.Repo,
		Token:      opts.Token,
		Labels:     opts.Labels,
		NamePrefix: "weft",
	}
	if err := writeConfig(opts.ConfigFile, cfg); err != nil {
		return err
	}
	log.Printf("weft-runner-github register: config written to %s", opts.ConfigFile)
	return nil
}

// RunOptions configures the long-lived daemon loop.
type RunOptions struct {
	ConfigFile   string
	WeftEndpoint string
	Image        string
	IdleTimeout  int

	// PoolSize is the maximum number of in-flight ephemeral runners
	// (microVMs). Defaults to 1 if zero; an operator running many
	// short workflows in parallel will want to raise this.
	PoolSize int
}

// Run boots the runner daemon. Loads config, dials weft, garbage-collects
// pre-existing offline runners, then runs the pool until ctx is cancelled.
func Run(ctx context.Context, opts RunOptions) error {
	if opts.ConfigFile == "" || opts.WeftEndpoint == "" || opts.Image == "" {
		return errors.New("run: --config, --weft-endpoint, --image are required")
	}
	cfg, err := readConfig(opts.ConfigFile)
	if err != nil {
		return err
	}
	pool := opts.PoolSize
	if pool <= 0 {
		pool = 1
	}

	g := newGH(cfg.Token)

	// GC pass: ephemeral semantics mean every VM that died left an
	// "offline" runner row behind. Clean those up so the UI stays
	// readable and GitHub doesn't refuse to mint new JIT configs once
	// the per-org runner cap (1000 default) is hit. We only purge rows
	// whose Name starts with our NamePrefix to avoid stomping on runners
	// owned by other daemons.
	if err := gcOfflineRunners(ctx, g, cfg); err != nil {
		log.Printf("weft-runner-github run: gc warning (continuing): %v", err)
	}

	// Spawn one worker per pool slot. Each worker loops: mint JIT,
	// spawn VM, wait, cleanup. Workers are independent — they share no
	// per-job state — so a wedged VM in one slot doesn't block others.
	var wg sync.WaitGroup
	wg.Add(pool)
	for slot := 0; slot < pool; slot++ {
		slot := slot
		go func() {
			defer wg.Done()
			runWorker(ctx, slot, g, cfg, opts)
		}()
	}
	log.Printf("weft-runner-github run: %d worker(s) up against scope %s/%s, image %s",
		pool, cfg.Owner, cfg.Repo, opts.Image)

	<-ctx.Done()
	log.Printf("weft-runner-github run: ctx cancelled, draining workers")
	wg.Wait()
	return ctx.Err()
}

// runWorker is one pool slot's loop. It exits cleanly when ctx is cancelled
// — never panics out, always tears down its current VM first so we don't
// leak runners on shutdown.
func runWorker(ctx context.Context, slot int, g *gh, cfg PersistedConfig, opts RunOptions) {
	// Exponential backoff cap when GitHub or weft is unhappy. Linear
	// retry would either flood (too fast) or stall (too slow); 5–60s
	// jittered is the band the actions/runner main binary uses too.
	backoff := 5 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if err := runOneJob(ctx, slot, g, cfg, opts); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("weft-runner-github slot %d: %v — retrying in %s", slot, err, backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			if backoff < 60*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 5 * time.Second
	}
}

// runOneJob is the unit of work: mint JIT, register VM with weft, wait,
// teardown.
func runOneJob(ctx context.Context, slot int, g *gh, cfg PersistedConfig, opts RunOptions) error {
	name, err := ephemeralName(cfg.NamePrefix, slot)
	if err != nil {
		return fmt.Errorf("name: %w", err)
	}
	jit, err := g.mintJITConfig(ctx, cfg.Owner, cfg.Repo, name, cfg.RunnerGroupID, cfg.Labels)
	if err != nil {
		return fmt.Errorf("mint jitconfig: %w", err)
	}
	log.Printf("weft-runner-github slot %d: jit %s (runner id=%d)", slot, name, jit.Runner.ID)

	err = dispatchJob(ctx, opts.WeftEndpoint, opts.Image, name, jit.EncodedJITConfig)
	gcErr := g.removeRunner(ctx, cfg.Owner, cfg.Repo, jit.Runner.ID)
	if err != nil {
		return fmt.Errorf("dispatch: %w", err)
	}
	if gcErr != nil {
		log.Printf("weft-runner-github slot %d: gc warning (runner %d): %v", slot, jit.Runner.ID, gcErr)
	}
	return nil
}

// ephemeralName picks a unique runner name. GitHub deduplicates by name
// within a scope, so a collision would silently steal an existing runner's
// slot. 8 hex chars of entropy = 32 bits, more than enough for the in-flight
// pool sizes we expect (10s of runners per daemon).
func ephemeralName(prefix string, slot int) (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%d-%s", prefix, slot, hex.EncodeToString(b[:])), nil
}

// gcOfflineRunners deletes any "offline" runners under our NamePrefix.
// Bounded: we list once at startup, paginating up to 100 runners (GitHub's
// per_page max), which covers the realistic blast radius of a daemon that
// crashed with a few dozen VMs in flight. Operators with very large pools
// can run `gh api … /actions/runners` to clean up manually if needed.
func gcOfflineRunners(ctx context.Context, g *gh, cfg PersistedConfig) error {
	runners, err := g.listRunners(ctx, cfg.Owner, cfg.Repo)
	if err != nil {
		return err
	}
	purged := 0
	for _, r := range runners {
		if r.Status != "offline" {
			continue
		}
		if !strings.HasPrefix(r.Name, cfg.NamePrefix+"-") {
			continue
		}
		if err := g.removeRunner(ctx, cfg.Owner, cfg.Repo, r.ID); err != nil {
			log.Printf("weft-runner-github gc: remove %s (id=%d) failed: %v", r.Name, r.ID, err)
			continue
		}
		purged++
	}
	if purged > 0 {
		log.Printf("weft-runner-github gc: purged %d offline runner(s)", purged)
	}
	return nil
}
