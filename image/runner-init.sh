#!/bin/bash
# runner-init — PID-1-adjacent entrypoint for the weft-runner-github microVM.
#
# The host daemon (weft-runner-github) writes the JIT config to
# /run/weft/cfg/github-jit.txt via the cfg share. weft-init mounts that share
# very early, but on slower hypervisors the mount can lag a few hundred ms
# behind our exec, so we busy-wait briefly before declaring it missing.
#
# The JIT blob is a single-use credential (it embeds a runner registration
# token plus an RSA private key). We must never echo it back through the VM
# console — `weft microvm logs --follow` is operator-readable. `log()` strips
# any occurrence of the blob defensively, and we keep the variable out of any
# trace or error path that could surface it.

set -euo pipefail

JIT_FILE=/run/weft/cfg/github-jit.txt
SHUTDOWN_FIFO=/run/weft-shutdown
JIT_BLOB=""

log() {
    local msg="$*"
    # Defensive: scrub the blob in case a caller interpolated it. Only mask
    # once the blob is loaded; before that ${JIT_BLOB} is empty and the
    # substitution is a no-op.
    if [ -n "${JIT_BLOB}" ]; then
        msg=${msg//${JIT_BLOB}/<JIT-REDACTED>}
    fi
    printf 'runner-init: %s\n' "${msg}" >&2
}

on_err() {
    local rc=$?
    log "fatal: line $1 exited rc=${rc}"
    exit "${rc}"
}
trap 'on_err ${LINENO}' ERR

log "waiting for ${JIT_FILE}"
deadline=$(( $(date +%s) + 30 ))
while [ ! -s "${JIT_FILE}" ]; do
    if [ "$(date +%s)" -ge "${deadline}" ]; then
        log "timeout: ${JIT_FILE} never appeared after 30s; cfg share not mounted?"
        exit 1
    fi
    sleep 0.2
done
log "found JIT config (blob length $(wc -c <"${JIT_FILE}") bytes)"

# Load the blob into a variable now so log() can mask it from this point on.
# `read -r` strips a trailing newline if present; the daemon writes the blob
# as a single line, but we tolerate both forms.
JIT_BLOB=$(tr -d '\n' <"${JIT_FILE}")
if [ -z "${JIT_BLOB}" ]; then
    log "JIT file is empty after newline trim"
    exit 1
fi

log "exec actions/runner --jitconfig <redacted>"
# runuser drops to the unprivileged 'runner' uid created in the Dockerfile.
# --jitconfig implies --ephemeral; the runner self-deregisters and exits 0
# once its assigned job finishes. stdout/stderr are inherited from this
# process, which weft-init plumbs onto the VM console, so `weft microvm
# logs --follow` sees every byte the runner prints.
set +e
runuser -u runner -- /opt/actions-runner/run.sh --jitconfig "${JIT_BLOB}"
rc=$?
set -e
log "actions/runner exited rc=${rc}"

if [ -e "${SHUTDOWN_FIFO}" ]; then
    log "signalling weft-init via ${SHUTDOWN_FIFO}"
    printf 'runner-exit %d\n' "${rc}" >"${SHUTDOWN_FIFO}" || true
fi

# weft-init treats ENTRYPOINT exit as VM stop, so a bare exit is enough.
exit "${rc}"
