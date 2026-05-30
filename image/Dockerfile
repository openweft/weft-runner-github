# syntax=docker/dockerfile:1.7

# In-VM runtime image for weft-runner-github.
#
# Boot contract (see ../runner/job.go):
#   - weft-init mounts the cfg share at /run/weft/cfg/
#   - github-jit.txt is written there by the host daemon, single-use JIT blob
#   - ENTRYPOINT busy-waits for that file, then execs actions/runner

ARG RUNNER_VERSION=2.319.1

FROM debian:bookworm-slim AS fetch
ARG TARGETARCH
ARG RUNNER_VERSION
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates=\* \
        curl=\* \
    && rm -rf /var/lib/apt/lists/*

# TARGETARCH is "amd64" or "arm64"; actions/runner publishes "x64" + "arm64".
RUN set -eux; \
    case "${TARGETARCH}" in \
        amd64) RUNNER_ARCH=x64 ;; \
        arm64) RUNNER_ARCH=arm64 ;; \
        *) echo "unsupported arch: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    mkdir -p /opt/actions-runner; \
    curl -fsSL -o /tmp/runner.tar.gz \
        "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-${RUNNER_ARCH}-${RUNNER_VERSION}.tar.gz"; \
    tar -xzf /tmp/runner.tar.gz -C /opt/actions-runner; \
    rm /tmp/runner.tar.gz

FROM debian:bookworm-slim AS runtime
ARG RUNNER_VERSION
LABEL org.opencontainers.image.source="https://github.com/openweft/weft-runner-github"
LABEL org.opencontainers.image.description="In-VM rootfs for weft-runner-github (actions/runner ${RUNNER_VERSION})"
LABEL org.opencontainers.image.licenses="Apache-2.0"

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# actions/runner is a .NET binary and segfaults on Debian without libicu.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates=\* \
        curl=\* \
        tar=\* \
        git=\* \
        jq=\* \
        libicu-dev=\* \
    && rm -rf /var/lib/apt/lists/*

# uid 1000: matches the conventional non-root user inside the microVM and
# avoids actions/runner's "Must not run interactively with sudo" guard.
RUN useradd --create-home --uid 1000 --shell /bin/bash runner

COPY --from=fetch /opt/actions-runner /opt/actions-runner
RUN chown -R runner:runner /opt/actions-runner

COPY runner-init.sh /usr/local/bin/runner-init
RUN chmod +x /usr/local/bin/runner-init

ENTRYPOINT ["/usr/local/bin/runner-init"]
