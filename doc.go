// Package main hosts the weft-runner-github binary — a self-hosted GitHub
// Actions runner that executes each incoming job in a fresh weft microVM.
//
// # Why
//
// The default GitHub-hosted runners share resources and OS images across
// every customer, and the "runs-on: self-hosted" alternative usually means
// either persistent bare metal (slow to reset, leaks state across jobs) or a
// docker-in-docker shim (no real isolation from the host). weft-runner-github
// gives each job its own VM-isolated environment by riding on the same
// microVM spawn primitive as the rest of weft (`weft microvm run`, OCI rootfs
// → boot under Apple-VZ or QEMU/KVM).
//
// # Components
//
//	[GitHub Actions Service] ⇄ runner/github.go ⇄ runner/runner.go ⇄ runner/job.go ⇄ [weft cluster]
//	         REST + long-poll       protocol         lifecycle           gRPC
//
//   - runner/github.go: registers the runner against an org/repo/enterprise
//     using a Personal Access Token or GitHub App installation; long-polls the
//     Actions Runtime API for assigned jobs; reports completion status.
//   - runner/runner.go: the daemon loop — owns the connection to GitHub, the
//     connection to weft, and the per-job state machine.
//   - runner/job.go: turns one job spec into a microVM lifecycle —
//     RegisterMicroVM → StartVM → stream output → DeleteVM — with a cancel
//     path tied to GitHub's "cancel" event.
//
// # Sibling runners
//
// All three runners (weft-runner-github, weft-runner-gitlab,
// weft-runner-forgejo) share the lifecycle layer (anything that talks to
// weft to spawn / drive / tear down a VM); the per-platform code is small
// (each platform's polling protocol + job spec envelope). When the three
// diverge enough to warrant it, the shared "microVM job runtime" should
// split into its own sibling module they all import.
//
// # Status (2026-06)
//
//  1. ✓ GitHub Actions runner registration via REST (mintRegistrationToken).
//  2. ✓ Runner-config persistence + ephemeral-runner semantics
//     (PersistedConfig + JIT-config-per-job, --ephemeral semantics on the
//     in-VM actions/runner binary).
//  3. ✓ Long-poll loop : runOneJob mints a JIT config, dispatches the VM,
//     waits for the VM to exit (which for `actions/runner --ephemeral`
//     happens exactly when the assigned job finishes).
//  4. ✓ weft microVM spawn via dispatchJob → weft-client RegisterMicroVM.
//  5. ✓ In-VM agent : the runner image ships actions/runner + a systemd
//     unit reading the per-job JIT config from /run/weft/cfg/. Image
//     side ; this daemon only puts the file there via the share.
//  6. ✓ Log streaming : the in-VM agent ships logs to GitHub via the
//     Actions runtime API directly (no daemon-side proxy needed).
//  7. ✓ Cleanup on cancel + idle timeout : runWorker honours ctx and
//     gcOfflineRunners cleans the per-org runner registry of dead rows.
//
// All seven items shipped. Subsequent work focuses on observability
// (per-job timing, queue-depth metrics) and on the shared microVM-job
// runtime split mentioned above — neither is functional surface.
package main
