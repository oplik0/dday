#!/usr/bin/env bash
# Stand up a throwaway Synapse homeserver for the E2EE end-to-end test
# (test/e2e). It is NOT part of CI: the normal `go test -race -tags goolm ./...`
# suite runs against an in-memory fake and needs no homeserver. This is the live
# server the `e2e`-tagged test talks to, driven by `make e2e`.
#
# Usage:
#   scripts/e2e-synapse.sh up      generate config, start Synapse, register users
#   scripts/e2e-synapse.sh down     stop + remove the container AND its data volume
#   scripts/e2e-synapse.sh wait     block until /_matrix/client/versions answers
#
# It is idempotent: `up` twice reuses the running container and re-registers the
# users (already-exists is tolerated). `down` leaves the matrixdotorg/synapse
# image in place (only the container + volume are removed), so the next `up` is
# fast. No secrets or state are written into the repo.
set -euo pipefail

IMAGE="${SYNAPSE_IMAGE:-matrixdotorg/synapse:latest}"
CONTAINER="${SYNAPSE_CONTAINER:-dday-e2e-synapse}"
VOLUME="${SYNAPSE_VOLUME:-dday-e2e-synapse-data}"
PORT="${SYNAPSE_PORT:-8008}"
SERVER_NAME="${SYNAPSE_SERVER_NAME:-localhost}"
BASE_URL="http://localhost:${PORT}"

# Test accounts the e2e test logs in as. Passwords are throwaway and local-only.
BOT_USER="bot";   BOT_PASS="botpass123"
ALICE_USER="alice"; ALICE_PASS="alicepass123"

log() { printf '[e2e-synapse] %s\n' "$*" >&2; }

wait_versions() {
  log "waiting for ${BASE_URL}/_matrix/client/versions ..."
  for _ in $(seq 1 120); do
    if curl -fsS "${BASE_URL}/_matrix/client/versions" >/dev/null 2>&1; then
      log "homeserver is up"
      return 0
    fi
    sleep 1
  done
  log "ERROR: homeserver did not come up within 120s"
  docker logs "${CONTAINER}" 2>&1 | tail -n 40 >&2 || true
  return 1
}

register_user() {
  local user="$1" pass="$2"
  # register_new_matrix_user reads registration_shared_secret from the config.
  # Tolerate "User ID already taken" so `up` stays idempotent.
  if docker exec "${CONTAINER}" register_new_matrix_user \
      -u "${user}" -p "${pass}" --no-admin \
      -c /data/homeserver.yaml "${BASE_URL}" 2>&1 | tee /dev/stderr | grep -qi "already taken"; then
    log "user ${user} already exists (ok)"
  else
    log "registered user ${user}"
  fi
}

up() {
  if [ -z "$(docker images -q "${IMAGE}")" ]; then
    log "pulling ${IMAGE} ..."
    docker pull "${IMAGE}"
  fi

  # (Re)generate config into the volume on first use.
  if [ -z "$(docker volume ls -q -f "name=^${VOLUME}\$")" ]; then
    docker volume create "${VOLUME}" >/dev/null
    log "generating Synapse config (server_name=${SERVER_NAME}, report_stats=no)"
    docker run --rm \
      -v "${VOLUME}:/data" \
      -e SYNAPSE_SERVER_NAME="${SERVER_NAME}" \
      -e SYNAPSE_REPORT_STATS=no \
      "${IMAGE}" generate >/dev/null

    # Enable open registration and loosen every rate limit so the test can
    # register + burst logins/messages freely. `generate` already wrote a random
    # registration_shared_secret (used by register_new_matrix_user) and does NOT
    # emit any of the keys below, so appending them is duplicate-free.
    #
    # The append runs INSIDE a container (the volume is owned by the synapse uid):
    # the heredoc feeds cat's stdin from within the container's own shell, so no
    # `docker run -i` is needed. A previous python-over-stdin version silently
    # no-op'd without -i, leaving the default rc_login.address burst of 3 to 429
    # the test's logins.
    log "patching homeserver.yaml (open registration + relaxed rate limits)"
    docker run --rm -v "${VOLUME}:/data" --entrypoint sh "${IMAGE}" -c 'cat >> /data/homeserver.yaml <<YAML

# --- appended by scripts/e2e-synapse.sh (test-only, insecure) ---
enable_registration: true
enable_registration_without_verification: true
registration_requires_token: false
rc_message: {per_second: 1000, burst_count: 1000}
rc_registration: {per_second: 1000, burst_count: 1000}
rc_login:
  address: {per_second: 1000, burst_count: 1000}
  account: {per_second: 1000, burst_count: 1000}
  failed_attempts: {per_second: 1000, burst_count: 1000}
rc_joins:
  local: {per_second: 1000, burst_count: 1000}
  remote: {per_second: 1000, burst_count: 1000}
rc_invites:
  per_room: {per_second: 1000, burst_count: 1000}
  per_user: {per_second: 1000, burst_count: 1000}
rc_key_requests: {per_second: 1000, burst_count: 1000}
rc_admin_redaction: {per_second: 1000, burst_count: 1000}
YAML'
  else
    log "reusing existing volume ${VOLUME}"
  fi

  # Start (or reuse) the container.
  if [ -n "$(docker ps -q -f "name=^${CONTAINER}\$")" ]; then
    log "container ${CONTAINER} already running"
  else
    docker rm -f "${CONTAINER}" >/dev/null 2>&1 || true
    log "starting ${CONTAINER} on :${PORT}"
    docker run -d --name "${CONTAINER}" \
      -v "${VOLUME}:/data" \
      -p "${PORT}:8008" \
      "${IMAGE}" >/dev/null
  fi

  wait_versions
  register_user "${BOT_USER}" "${BOT_PASS}"
  register_user "${ALICE_USER}" "${ALICE_PASS}"
  log "ready: ${BASE_URL} (users: ${BOT_USER}, ${ALICE_USER})"
}

down() {
  log "removing container ${CONTAINER} and volume ${VOLUME}"
  docker rm -f "${CONTAINER}" >/dev/null 2>&1 || true
  docker volume rm "${VOLUME}" >/dev/null 2>&1 || true
  log "cleaned up (image ${IMAGE} kept)"
}

case "${1:-}" in
  up)   up ;;
  down) down ;;
  wait) wait_versions ;;
  *)    echo "usage: $0 {up|down|wait}" >&2; exit 2 ;;
esac
