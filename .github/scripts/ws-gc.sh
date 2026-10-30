#!/usr/bin/env bash
set -euo pipefail

TTL_DAYS="${WS_TTL_DAYS:-14}"
CUTOFF=$(( $(date +%s) - TTL_DAYS * 86400 ))

git fetch --filter=blob:none --no-tags origin "+refs/ws/*:refs/remotes/origin/ws/*"

while IFS=$'\t' read -r sha ref ct; do
  if [ "$ct" -lt "$CUTOFF" ]; then
    ws_ref="refs/ws/${ref#refs/remotes/origin/ws/}"
    echo "Pruning expired workspace ${ws_ref} (sha=${sha}, committer_ts=${ct})"
    git push --force-with-lease="${ws_ref}:${sha}" origin ":${ws_ref}"
  fi
done < <(git for-each-ref --format='%(objectname)	%(refname)	%(committerdate:unix)' refs/remotes/origin/ws/)
