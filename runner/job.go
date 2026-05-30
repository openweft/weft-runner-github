// runner/job.go — per-job microVM lifecycle.
//
// Contract with the in-VM image:
//   - The microVM's `cfg` share carries github-jit.txt (one line, the
//     base64-encoded JIT config from `generate-jitconfig`).
//   - A systemd unit (or equivalent init script) inside the VM reads that
//     file and execs `actions/runner --jitconfig <BLOB>`. The `--ephemeral`
//     flag is implicit in the JIT config; the runner self-deregisters and
//     exits 0 once its assigned job finishes.
//   - When the runner exits, our init (PID 1 / weft-init) wires it to a
//     shutdown — that's what the rest of weft-microvm expects from a
//     long-running workload.
//
// This file does NOT speak gRPC to the weft control plane directly. It
// shells out to the `weft` CLI for the lifecycle moves (microvm register /
// start / wait / delete). Reasons:
//
//  1. Go module-graph hygiene — weft is the "umbrella" module and importing
//     it from a sibling runner would create a downstream→upstream edge that
//     `go mod` will sort out unhappily.
//  2. The CLI surface is the same one operators use to debug a wedged
//     runner ("just run `weft microvm logs <name>`"), so a shell-out keeps
//     us aligned with the operator path.
//  3. The handful of commands we need is small (4) and stable.
//
// If/when point (1) is addressed (e.g. by splitting a `weft/control` Go SDK
// out of the umbrella into its own module), this file becomes the obvious
// place to swap in the typed API.

package runner

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// dispatchJob is the per-job loop body. It:
//
//  1. Materialises a temp `cfg` directory carrying github-jit.txt.
//  2. Asks `weft` to register a microVM with that cfg directory mounted at
//     /run/weft/cfg/ (the path weft-init already exposes).
//  3. Starts the VM and blocks until it exits.
//  4. Deletes the VM (cleanup is idempotent on the weft side).
//
// We let the caller (runOneJob) own GC of the GitHub-side runner row; this
// function's job is the VM lifecycle only.
func dispatchJob(ctx context.Context, weftEndpoint, image, vmName, encodedJIT string) error {
	cfgDir, err := os.MkdirTemp("", "weft-runner-github-"+vmName+"-cfg-")
	if err != nil {
		return fmt.Errorf("mktemp cfg: %w", err)
	}
	defer os.RemoveAll(cfgDir)

	jitPath := filepath.Join(cfgDir, "github-jit.txt")
	// 0600: the JIT blob is a single-use credential, but it's still a
	// credential — same posture as the persisted PAT.
	if err := os.WriteFile(jitPath, []byte(encodedJIT), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", jitPath, err)
	}

	// We invoke `weft microvm …` subcommands rather than `weft vm …`
	// because, per the CLI naming convention, microVMs and "VM classic"
	// (full disk image) take different paths inside weft. The runner is
	// always microVM-shaped (OCI rootfs, ephemeral).
	endpointFlag := "--endpoint=" + weftEndpoint
	register := exec.CommandContext(ctx, "weft", "microvm", "register",
		endpointFlag,
		"--name="+vmName,
		"--image="+image,
		"--cfg="+cfgDir,
	)
	register.Stderr = os.Stderr
	register.Stdout = os.Stderr
	if err := register.Run(); err != nil {
		return fmt.Errorf("weft microvm register: %w", err)
	}
	// From here on we must `weft microvm delete` no matter how we
	// exit, otherwise weft-side state leaks across daemon restarts.
	// Use a fresh context for delete so a parent cancellation still
	// reaches the teardown.
	defer func() {
		delCtx, delCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer delCancel()
		del := exec.CommandContext(delCtx, "weft", "microvm", "delete", endpointFlag, "--name="+vmName)
		del.Stderr = os.Stderr
		if err := del.Run(); err != nil {
			log.Printf("weft-runner-github: delete %s failed: %v (leaked weft-side VM)", vmName, err)
		}
	}()

	start := exec.CommandContext(ctx, "weft", "microvm", "start", endpointFlag, "--name="+vmName)
	start.Stderr = os.Stderr
	start.Stdout = os.Stderr
	if err := start.Run(); err != nil {
		return fmt.Errorf("weft microvm start: %w", err)
	}

	// Block on the VM. `weft microvm wait` returns when the VM
	// transitions to Stopped (the runner exited) or fails for any
	// other reason. Pipe its output through us so operators can `tail
	// -f` the runner log and see what's happening inside.
	wait := exec.CommandContext(ctx, "weft", "microvm", "wait", endpointFlag, "--name="+vmName)
	wait.Stderr = os.Stderr
	wait.Stdout = os.Stderr
	if err := wait.Run(); err != nil {
		return fmt.Errorf("weft microvm wait: %w", err)
	}
	log.Printf("weft-runner-github: vm %s exited cleanly", vmName)
	return nil
}
