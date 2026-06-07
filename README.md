# weft-runner-github

Self-hosted GitHub Actions runner backed by **weft** ephemeral microVMs.

## What it does

`weft-runner-github` registers as a GitHub Actions self-hosted runner
against an org / repo / enterprise, then for each job assigned to it:

1. Asks a weft cluster to spawn a fresh **ephemeral microVM** from an OCI rootfs
   (e.g. `ghcr.io/actions/runner-images:ubuntu-24.04-arm64`) — clean state per
   job, isolated, throwaway.
2. Runs `actions/runner --jitconfig … --ephemeral` inside that VM via a thin
   agent (boot-time bootstrap drops the runner binary + JIT config +
   workdir mount).
3. Streams logs back to GitHub directly via the Actions Runtime API, marks
   the job done, and tears the VM down.

Sibling of `weft-runner-gitlab` and `weft-runner-forgejo` — the three share
the microVM-spawn primitive but plug into their respective CI control planes.
Each implements the platform's own protocol (GitHub Actions's Runtime API
here, GitLab's `/api/v4/jobs/request` in the GitLab sibling, Forgejo's
Connect-over-JSON in the Forgejo one).

## Status

**Operational** — the seven implementation steps in `doc.go` ship :
Registration token mint, JIT-config-per-job, `runOneJob` long-poll, microVM
dispatch via weft-client, log streaming by the in-VM `actions/runner`
binary directly to GitHub, cleanup-on-cancel + idle timeout, and an
`gcOfflineRunners` pass that keeps the per-org runner registry tidy.
See `doc.go` for the per-step status.

## Quick start

```sh
# 1. Mint a PAT (or GitHub App installation token) with admin:org scope
#    for an org-wide runner, or repo:admin for a repo-scoped one. The PAT
#    is used to MINT a short-lived registration token at register time ;
#    it stays out of every per-job code path.

# 2. Register the runner.
weft-runner-github register \
  --owner my-org \
  --token $GITHUB_PAT \
  --labels "weft,microvm,arm64"

# 3. Start polling for jobs. Each job spawns a fresh microVM on the
#    target weft cluster.
weft-runner-github run \
  --weft-endpoint tcp:weft.example.com:7330 \
  --image ghcr.io/actions/runner-images:ubuntu-24.04-arm64
```

## Architecture

See `doc.go` for the design intent and component boundaries ;
`runner/runner.go` for the lifecycle layer, `runner/github.go` for the
GitHub REST client.
