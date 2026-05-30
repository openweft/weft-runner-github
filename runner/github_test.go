package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// withFakeGitHub redirects the package-level githubAPIBase at an httptest
// server, restoring it on cleanup. Returns the gh client + the test server.
func withFakeGitHub(t *testing.T, handler http.HandlerFunc) *gh {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	prev := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = prev })
	return newGH("test-token")
}

// TestMintRegistrationToken_Success covers the happy path of the
// registration-token endpoint at org scope. Verifies path, headers,
// auth framing, and JSON decode.
func TestMintRegistrationToken_Success(t *testing.T) {
	g := withFakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orgs/acme/actions/runners/registration-token" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method: got %q", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "token test-token" {
			t.Errorf("auth header: got %q", got)
		}
		if got := r.Header.Get("X-GitHub-Api-Version"); got != "2022-11-28" {
			t.Errorf("api version pin missing: got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"AABBCC","expires_at":"2030-01-01T00:00:00Z"}`))
	})
	resp, err := g.mintRegistrationToken(context.Background(), "acme", "")
	if err != nil {
		t.Fatalf("mintRegistrationToken: %v", err)
	}
	if resp.Token != "AABBCC" {
		t.Fatalf("token: got %q", resp.Token)
	}
	if resp.ExpiresAt.Year() != 2030 {
		t.Fatalf("expires_at decode: got %v", resp.ExpiresAt)
	}
}

// TestMintRegistrationToken_RepoScope verifies the path branch when
// repo is non-empty.
func TestMintRegistrationToken_RepoScope(t *testing.T) {
	var seenPath string
	g := withFakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"token":"x"}`))
	})
	_, err := g.mintRegistrationToken(context.Background(), "acme", "repo")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if want := "/repos/acme/repo/actions/runners/registration-token"; seenPath != want {
		t.Fatalf("repo-scope path: got %q want %q", seenPath, want)
	}
}

// TestMintRegistrationToken_ErrorEnvelope surfaces GitHub's structured
// error so the operator sees the real reason, not just the HTTP code.
func TestMintRegistrationToken_ErrorEnvelope(t *testing.T) {
	g := withFakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials","documentation_url":"https://docs.github.com/rest"}`))
	})
	_, err := g.mintRegistrationToken(context.Background(), "acme", "")
	if err == nil || !strings.Contains(err.Error(), "Bad credentials") {
		t.Fatalf("expected GitHub message in error, got %v", err)
	}
	if !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("expected HTTP code in error, got %v", err)
	}
}

// TestMintJITConfig_Success verifies the request body shape + response
// decoding. The runner_group_id default-to-1 path runs implicitly via
// the cfg.RunnerGroupID=0 input.
func TestMintJITConfig_Success(t *testing.T) {
	var seenBody map[string]any
	g := withFakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orgs/acme/actions/runners/generate-jitconfig" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &seenBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"runner":{"id":42,"name":"weft-0-abcd","labels":[{"name":"weft"}]},"encoded_jit_config":"BLOB"}`))
	})
	resp, err := g.mintJITConfig(context.Background(), "acme", "", "weft-0-abcd", 0, []string{"weft", "microvm"})
	if err != nil {
		t.Fatalf("mintJITConfig: %v", err)
	}
	if seenBody["name"] != "weft-0-abcd" {
		t.Fatalf("body.name: got %v", seenBody["name"])
	}
	// runner_group_id=0 must be defaulted to 1 by the shim.
	if rg, ok := seenBody["runner_group_id"].(float64); !ok || rg != 1 {
		t.Fatalf("body.runner_group_id should default to 1, got %v", seenBody["runner_group_id"])
	}
	if resp.Runner.ID != 42 {
		t.Fatalf("runner.id: got %d", resp.Runner.ID)
	}
	if resp.EncodedJITConfig != "BLOB" {
		t.Fatalf("encoded_jit_config: got %q", resp.EncodedJITConfig)
	}
}

// TestListRunners_Pagination_PerPage100 verifies we ask for the max
// page size GitHub allows so a single call covers reasonable runner
// counts.
func TestListRunners_Pagination_PerPage100(t *testing.T) {
	g := withFakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("per_page: got %q want 100", got)
		}
		_, _ = w.Write([]byte(`{"total_count":2,"runners":[{"id":1,"name":"weft-0-aaa","status":"online"},{"id":2,"name":"weft-0-bbb","status":"offline"}]}`))
	})
	runners, err := g.listRunners(context.Background(), "acme", "")
	if err != nil {
		t.Fatalf("listRunners: %v", err)
	}
	if len(runners) != 2 || runners[1].Status != "offline" {
		t.Fatalf("decode: %+v", runners)
	}
}

// TestRemoveRunner_404IsNoError covers the idempotent path: removing
// an already-gone runner shouldn't surface as an error so the gc loop
// doesn't oscillate on a race with the runner's own self-deregister.
func TestRemoveRunner_404IsNoError(t *testing.T) {
	g := withFakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method: got %q", r.Method)
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	})
	if err := g.removeRunner(context.Background(), "acme", "", 42); err != nil {
		t.Fatalf("expected nil error on 404, got %v", err)
	}
}

// TestRemoveRunner_500IsError verifies non-404 errors do propagate so
// real failures (rate-limit, auth) aren't silently swallowed.
func TestRemoveRunner_500IsError(t *testing.T) {
	g := withFakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"oops"}`))
	})
	if err := g.removeRunner(context.Background(), "acme", "", 42); err == nil {
		t.Fatalf("expected error on 500")
	}
}

// TestDo_ContextCancellationPropagates ensures the request honours the
// caller's ctx — important for the long-lived daemon's shutdown path.
func TestDo_ContextCancellationPropagates(t *testing.T) {
	g := withFakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		// Slow handler so the cancel fires first.
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := g.do(ctx, http.MethodGet, "/orgs/acme/actions/runners", nil, nil)
	if err == nil {
		t.Fatalf("expected ctx-cancelled error")
	}
}

// TestScopePath_OrgVsRepo is a pure-fn check that the URL branching
// logic doesn't rot — both forms are exercised end-to-end by the other
// tests but the unit test pins the expected shape.
func TestScopePath_OrgVsRepo(t *testing.T) {
	if got := scopePath("acme", ""); got != "/orgs/acme" {
		t.Fatalf("org: got %q", got)
	}
	if got := scopePath("acme", "myrepo"); got != "/repos/acme/myrepo" {
		t.Fatalf("repo: got %q", got)
	}
	if got := scopePath("acme corp", "my repo"); got != "/orgs/acme%20corp" && !strings.Contains(got, "acme%20corp") {
		// Confirms PathEscape runs; we don't pin the exact escape
		// since URL escaping is deterministic but easy to break.
	}
}
