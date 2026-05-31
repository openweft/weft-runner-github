# weft-runner-github / in-VM runtime image

This directory builds the OCI image booted as a microVM by `weft-runner-github`.
Each job runs in a fresh VM with this rootfs. The runtime is live: once
`/run/weft/cfg/github-jit.txt` lands on the cfg share, `runner-init` execs
the canonical `actions/runner` binary in JIT mode and the VM picks up its
assigned job.

## Boot contract

`weft-runner-github` (the host daemon) mints a single-use JIT runner config
from GitHub and exposes it to the VM through the cfg share:

| Path inside VM                    | Producer                | Consumer                        |
| --------------------------------- | ----------------------- | ------------------------------- |
| `/run/weft/cfg/github-jit.txt`    | host daemon, before boot| `runner-init` (this image)      |
| `/run/weft-shutdown` (if present) | weft-init               | `runner-init`, post-exit signal |

`runner-init` busy-waits up to 30 s for `github-jit.txt`, reads it, then execs:

```
runuser -u runner -- /opt/actions-runner/run.sh --jitconfig "$(cat /run/weft/cfg/github-jit.txt)"
```

The `--jitconfig` mode implies `--ephemeral`: the runner self-deregisters on
GitHub and exits 0 as soon as its assigned job finishes. weft-init treats the
ENTRYPOINT exit as a VM stop.

The blob is a credential. `runner-init` never echoes it to the console — its
internal `log()` rewrites any occurrence of the blob to `<JIT-REDACTED>`
before printing.

## Build + push (CI)

```
docker buildx build \
    --platform linux/amd64,linux/arm64 \
    -t ghcr.io/openweft/weft-runner-github:v0.1.0 \
    --push \
    image/
```

CI (`.github/workflows/image.yml`) builds and pushes on every push to `main`,
tagging both `latest` and the short commit SHA.

## Build + run locally

Useful for verifying `runner-init` end-to-end without going through the host
daemon. The JIT validation will fail (the blob is bogus) but you'll see the
runner accept `--jitconfig`, attempt the unpack, and bail — that's the signal
the wiring is right.

```
docker build -t weft-runner-github:dev image/

# Fake cfg share with a dummy blob:
mkdir -p /tmp/weft-cfg
printf 'dummy-jit-blob\n' >/tmp/weft-cfg/github-jit.txt

docker run --rm \
    -v /tmp/weft-cfg:/run/weft/cfg:ro \
    weft-runner-github:dev
# expected: runner-init logs "exec actions/runner --jitconfig <redacted>"
#           then actions/runner aborts on the malformed blob (rc != 0).
```

`task image-smoke` automates this and asserts the exit-code + log shape.

## Use with `weft-runner-github`

```
weft-runner-github register --owner=<org> --token=<pat> --config=/etc/weft-runner-github.json
weft-runner-github run \
    --config=/etc/weft-runner-github.json \
    --weft-endpoint=unix:///var/run/weft/agent.sock \
    --image=ghcr.io/openweft/weft-runner-github:v0.1.0 \
    --pool-size=4
```

The daemon never reaches into this image; the only coupling is the cfg-share
filename above. Anything else — extra tooling, additional runner labels, a
non-Debian base — is a private decision of this Dockerfile.

## actions/runner version

Pinned via the `RUNNER_VERSION` build arg (defaults to 2.319.1). Bumping is a
one-line change here; the CI build will republish.

## Troubleshooting

- **`runner-init: timeout: /run/weft/cfg/github-jit.txt never appeared after 30s`**
  The cfg share never mounted. Check that the host daemon registered the VM
  with `--cfg <dir>` and that `weft-init` is mounting `/run/weft/cfg`
  (`weft microvm logs --follow <vm>` shows the mount sequence).

- **`actions/runner exited rc=1`, log mentions `Crypto`, `RSA`, or `decode`**
  The blob was truncated or re-encoded. The daemon writes the JIT response
  verbatim, one line, no whitespace. `tr -d '\n'` in `runner-init` strips an
  accidental trailing newline; anything else (CR, BOM) will break the
  validator.

- **`actions/runner exited rc=2`, log mentions `EAFNOSUPPORT` / "address family not supported"**
  The .NET runtime tried to bind a dual-stack socket on a host without IPv6.
  This is rare on weft microVMs (we wire IPv6 by default) but surfaces under
  some containerd networks. Workarounds, in order of preference:
  1. Enable IPv6 on the microVM (preferred — it's a one-line CNI tweak).
  2. Set `DOTNET_SYSTEM_NET_DISABLEIPV6=1` in the image env, e.g. via
     `docker run -e ...` or by adding an `ENV` line to the Dockerfile.
  Note: `actions/runner` has no `--runneronly-mode` flag despite the name
  appearing in old forum threads; it does not exist in 2.319.x.

- **`actions/runner exited rc=0` but no job ran**
  JIT configs are single-use. If GitHub already consumed the registration
  (e.g. a previous VM picked it up) the runner exits clean immediately. The
  host daemon shouldn't reuse a blob; if it does, that's a daemon-side bug.
