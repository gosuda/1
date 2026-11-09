#!/usr/bin/env bash
# Poll refs/heads/lander of gosuda/0 and roll the lander container forward.
#
#   LANDER_DEPLOY_URL    read-only clone URL of gosuda/0 (deploy key or token in the URL/credential helper)
#   LANDER_ENV_FILE      env file with LANDER_* settings (App id, key path, ...)
#   LANDER_KEY_DIR       host directory with the App key, signing key and allowed signers (mounted read-only)
#   LANDER_STATE_DIR     host directory for the persistent object store
#   DOCKER_HOST          optional: sandbox daemon used for presubmit (never mount the host docker socket)
#
# The new image must pass `lander check` and keep running for 20 s, otherwise the previous image
# keeps (or regains) the container name. The three newest images are kept for rollback.
set -euo pipefail

: "${LANDER_DEPLOY_URL:?}" "${LANDER_ENV_FILE:?}" "${LANDER_KEY_DIR:?}" "${LANDER_STATE_DIR:?}"
NAME="${LANDER_CONTAINER:-lander}"
INTERVAL="${LANDER_DEPLOY_INTERVAL:-60}"
WORK="${LANDER_DEPLOY_WORK:-/var/lib/lander-deploy}"
mkdir -p "$WORK"

run_container() {
  docker run -d --name "$NAME" --restart unless-stopped \
    --env-file "$LANDER_ENV_FILE" \
    ${DOCKER_HOST:+-e DOCKER_HOST} \
    -v "$LANDER_STATE_DIR:/var/lib/lander" \
    -v "$LANDER_KEY_DIR:/etc/lander:ro" \
    -p 127.0.0.1:8080:8080 \
    "$1" >/dev/null
}

deploy() {
  local sha="$1" tag="lander:$1" prev
  prev=$(docker inspect --format '{{.Config.Image}}' "$NAME" 2>/dev/null || true)
  rm -rf "$WORK/src" && mkdir -p "$WORK/src"
  git -C "$WORK/src" init -q
  git -C "$WORK/src" fetch -q --depth=1 "$LANDER_DEPLOY_URL" "$sha"
  git -C "$WORK/src" checkout -q FETCH_HEAD
  docker build -q -t "$tag" "$WORK/src" >/dev/null
  docker run --rm --env-file "$LANDER_ENV_FILE" -v "$LANDER_KEY_DIR:/etc/lander:ro" "$tag" check
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  run_container "$tag"
  sleep 20
  if [ "$(docker inspect --format '{{.State.Running}}' "$NAME")" != "true" ]; then
    echo "deploy: $tag failed to stay up; rolling back to ${prev:-<none>}" >&2
    docker logs --tail 50 "$NAME" >&2 || true
    docker rm -f "$NAME" >/dev/null 2>&1 || true
    [ -n "$prev" ] && run_container "$prev"
    return 1
  fi
  echo "$sha" >"$WORK/current"
  docker images --format '{{.Repository}}:{{.Tag}} {{.CreatedAt}}' lander | sort -k2 -r | awk 'NR>3 {print $1}' | xargs -r docker rmi >/dev/null 2>&1 || true
}

while true; do
  sha=$(git ls-remote "$LANDER_DEPLOY_URL" refs/heads/lander | cut -f1 || true)
  if [ -n "$sha" ] && [ "$sha" != "$(cat "$WORK/current" 2>/dev/null || true)" ] && [ "$sha" != "$(cat "$WORK/failed" 2>/dev/null || true)" ]; then
    echo "deploy: rolling out $sha"
    deploy "$sha" || echo "$sha" >"$WORK/failed"
  fi
  sleep "$INTERVAL"
done
