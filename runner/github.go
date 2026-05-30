// Package runner — GitHub REST shim.
//
// Only the endpoints we actively need are wrapped:
//
//   - mintRegistrationToken — `POST /orgs/{owner}/actions/runners/registration-token`
//     (or `/repos/{owner}/{repo}/…` for the repo-scoped variant).
//     Returns the short-lived (~1h) token that lets a runner identify itself
//     during the deprecated `./config.sh` flow.
//
//   - mintJITConfig — `POST /orgs/{owner}/actions/runners/generate-jitconfig`
//     (or `…/repos/…`). Returns a base64-encoded runner config blob that
//     `actions/runner --jitconfig <BLOB>` will consume as a one-shot
//     ephemeral runner. This is the modern path: GitHub allocates the runner
//     ID + runtime URL + creds in one call, the binary inside the VM never
//     needs a registration token, and `--ephemeral` is implicit.
//
//   - listRunners / removeRunner — janitorial endpoints used by the daemon
//     to garbage-collect runners whose backing microVM was lost mid-flight
//     (host crash, agent restart). GitHub keeps "Offline" runners in the UI
//     forever otherwise, and ephemeral semantics rely on de-registration.
//
// Auth: we accept either a Personal Access Token (`Authorization: token …`)
// or a GitHub App installation token (`Authorization: Bearer …` — same
// header content, GitHub accepts both). The caller hands us the literal
// header value as `token` and we send it unchanged with `token <…>` framing,
// which works for both. If we ever need GitHub App-specific endpoints
// (per-installation queries), we add a separate code path.

package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// githubAPIBase is the GitHub REST entry point. Made a var so tests can
// redirect it to an httptest server; production callers should not touch it.
var githubAPIBase = "https://api.github.com"

// gh is a tiny HTTP shim around the few endpoints we need. It exists so
// Register / Run can be unit-tested by swapping `client` for an httptest
// server's client without dragging in a heavyweight SDK.
type gh struct {
	client *http.Client
	token  string // PAT or installation token, sent as "token <token>"
}

func newGH(token string) *gh {
	return &gh{
		// 30s is enough for any of the endpoints we hit — they're all
		// CPU-bound on GitHub's side, not user-driven. A shorter timeout
		// would risk false positives when the daemon's network is slow.
		client: &http.Client{Timeout: 30 * time.Second},
		token:  token,
	}
}

// scopePath returns the URL fragment GitHub uses for the registration /
// JIT-config / list-runners endpoints, mirroring the org-vs-repo branching
// pattern documented in the REST API reference.
func scopePath(owner, repo string) string {
	if repo == "" {
		return "/orgs/" + url.PathEscape(owner)
	}
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
}

func (g *gh) do(ctx context.Context, method, path string, body any, out any) error {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, githubAPIBase+path, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	// GitHub recommends pinning the API version. Picking an explicit
	// version lets the runner survive future default-version flips at
	// GitHub's end (a runner deployment that breaks because the API moved
	// out from under it is the kind of incident we want to make
	// impossible).
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.token != "" {
		req.Header.Set("Authorization", "token "+g.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Try to surface GitHub's error envelope ({"message":"...", ...})
		// so the operator sees the real reason instead of just a status
		// code. Fall back to the raw body if it's not JSON.
		var ghErr struct {
			Message          string `json:"message"`
			DocumentationURL string `json:"documentation_url"`
		}
		if jsonErr := json.Unmarshal(respBody, &ghErr); jsonErr == nil && ghErr.Message != "" {
			return fmt.Errorf("github %s %s: %s (HTTP %d; see %s)",
				method, path, ghErr.Message, resp.StatusCode, ghErr.DocumentationURL)
		}
		return fmt.Errorf("github %s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// regTokenResponse mirrors the GitHub `POST registration-token` payload.
type regTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// mintRegistrationToken returns a registration token valid for ~1h.
// Used by the classic `./config.sh --token …` flow. Modern ephemeral runners
// use mintJITConfig instead; we keep this around because some operators want
// to bootstrap a long-lived runner once for diagnostics, and the endpoint is
// the canonical "your PAT actually has the right scope" sanity-check.
func (g *gh) mintRegistrationToken(ctx context.Context, owner, repo string) (regTokenResponse, error) {
	var out regTokenResponse
	err := g.do(ctx, http.MethodPost, scopePath(owner, repo)+"/actions/runners/registration-token", nil, &out)
	return out, err
}

// jitConfigResponse mirrors the `POST generate-jitconfig` payload.
type jitConfigResponse struct {
	Runner struct {
		ID     int      `json:"id"`
		Name   string   `json:"name"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	} `json:"runner"`
	EncodedJITConfig string `json:"encoded_jit_config"`
}

// jitConfigRequest is the payload `generate-jitconfig` expects.
type jitConfigRequest struct {
	Name          string   `json:"name"`
	RunnerGroupID int      `json:"runner_group_id"`
	Labels        []string `json:"labels"`
	WorkFolder    string   `json:"work_folder,omitempty"`
}

// mintJITConfig produces a one-shot runner credential bundle. The opaque
// `EncodedJITConfig` blob is passed verbatim to `actions/runner --jitconfig`
// inside the microVM — it carries the runtime URL, runner ID, signing key
// and ephemeral-runner flag. Each call mints a fresh runner, so the daemon
// calls this once per microVM it spawns.
func (g *gh) mintJITConfig(ctx context.Context, owner, repo, name string, runnerGroupID int, labels []string) (jitConfigResponse, error) {
	if runnerGroupID == 0 {
		// Default runner group ("Default", ID 1) — GitHub rejects the
		// call without it, even on free plans where there's only one
		// group. The org-scoped endpoint *requires* it; the repo-scoped
		// one accepts it but ignores its meaning. Sending it
		// unconditionally is simpler than branching.
		runnerGroupID = 1
	}
	req := jitConfigRequest{Name: name, RunnerGroupID: runnerGroupID, Labels: labels}
	var out jitConfigResponse
	err := g.do(ctx, http.MethodPost, scopePath(owner, repo)+"/actions/runners/generate-jitconfig", req, &out)
	return out, err
}

// runner identifies an existing self-hosted runner in the list-runners /
// remove-runner endpoints. Only the fields we use are kept.
type runner struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"` // "online" | "offline"
}

// listRunners enumerates the self-hosted runners attached to the org/repo
// scope. Used at startup to garbage-collect "offline" leftovers from prior
// daemon invocations whose VMs died unexpectedly.
func (g *gh) listRunners(ctx context.Context, owner, repo string) ([]runner, error) {
	var out struct {
		TotalCount int      `json:"total_count"`
		Runners    []runner `json:"runners"`
	}
	if err := g.do(ctx, http.MethodGet, scopePath(owner, repo)+"/actions/runners?per_page=100", nil, &out); err != nil {
		return nil, err
	}
	return out.Runners, nil
}

// removeRunner deletes a runner by ID. Idempotent on the GitHub side — a
// 404 means already gone, which is the expected outcome when racing the
// runner's own deregistration. We don't surface 404 as an error.
func (g *gh) removeRunner(ctx context.Context, owner, repo string, id int) error {
	path := fmt.Sprintf("%s/actions/runners/%d", scopePath(owner, repo), id)
	err := g.do(ctx, http.MethodDelete, path, nil, nil)
	if err != nil && strings.Contains(err.Error(), "HTTP 404") {
		return nil
	}
	return err
}
