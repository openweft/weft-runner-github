#!/bin/bash
# runner-init — PID-1-adjacent entrypoint for the weft-runner-github microVM.
#
# The host daemon (weft-runner-github) writes the JIT config to
# /run/weft/cfg/github-jit.txt via the cfg share. weft-init mounts that share
# very early, but on slower hypervisors the mount can lag a few hundred ms
# behind our exec, so we busy-wait briefly before declaring it missing.

set -euo pipefail

log() { printf 'runner-init: %s\n' "$*" >&2; }

JIT_FILE=/run/weft/cfg/github-jit.txt
SHUTDOWN_FIFO=/run/weft-shutdown

log "waiting for ${JIT_FILE}"
deadline=$(( $(date +%s) + 30 ))
while [ ! -s "${JIT_FILE}" ]; do
    if [ "$(date +%s)" -ge "${deadline}" ]; then
        log "timeout: ${JIT_FILE} never appeared after 30s; cfg share not mounted?"
        exit 1
    fi
    sleep 0.2
done
log "found JIT config (${#JIT_FILE} bytes path, blob length $(wc -c <"${JIT_FILE}") bytes)"

JIT_BLOB=$(cat "${JIT_FILE}")

log "exec actions/runner"
# runuser drops to the unprivileged 'runner' uid created in the Dockerfile.
# --jitconfig implies --ephemeral; the runner self-deregisters and exits 0
# once its assigned job finishes. Trap the exit so we can signal weft-init.
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
