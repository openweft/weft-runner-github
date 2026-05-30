# weft-runner-github / in-VM runtime image

This directory builds the OCI image booted as a microVM by `weft-runner-github`.
Each job runs in a fresh VM with this rootfs.

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

## Build + push

```
docker buildx build \
    --platform linux/amd64,linux/arm64 \
    -t ghcr.io/openweft/weft-runner-github:v0.1.0 \
    --push \
    image/
```

CI (`.github/workflows/image.yml`) builds and pushes on every push to `main`,
tagging both `latest` and the short commit SHA.

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
