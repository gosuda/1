#!/usr/bin/env bash
set -euo pipefail

PAYLOAD=$(cat)
CMD=$(python3 -c '
import json, sys
try:
    data = json.load(sys.stdin)
    print(data.get("toolCall", {}).get("args", {}).get("CommandLine", ""))
except Exception:
    print("")
' <<<"$PAYLOAD")

deny() {
  python3 -c '
import json, sys
print(json.dumps({"decision": "deny", "reason": sys.argv[1]}))
' "$1"
  exit 0
}

allow() {
  printf '{"decision":"allow"}\n'
  exit 0
}

if [[ "$CMD" =~ (^|[[:space:]\;\&\|])git[[:space:]]+pull([[:space:]]|$) ]]; then
  deny "Do not run git pull in gosuda/0. Run git ws-sync (git rebase --onto origin/suda/main) instead."
fi

if [[ "$CMD" =~ (^|[[:space:]\;\&\|])git[[:space:]]+push([[:space:]]|$) ]]; then
  if [[ "$CMD" =~ git[[:space:]]+push[^\;\&\|]*(--force([[:space:]]|$)|[[:space:]]-f([[:space:]]|$)) ]] && [[ ! "$CMD" =~ --force-with-lease ]]; then
    deny "Unconditional git push --force is prohibited. Pass --force-with-lease instead."
  fi
  if [[ "$CMD" =~ :refs/suda/(main|history) ]] && [[ ! "$CMD" =~ (import-pilot\.sh|import-repo\.sh|create-repo\.sh|land\.sh|sync-inbound\.sh|sync-control-plane\.sh) ]]; then
    deny "Direct pushes to refs/suda/main or refs/suda/history are prohibited. Push to refs/heads/land/<id> instead."
  fi
fi

if [[ "$CMD" =~ (^|[[:space:]\;\&\|])git[[:space:]]+sparse-checkout[[:space:]]+set([[:space:]]|$) ]]; then
  if [[ ! "$CMD" =~ \.agents ]] || [[ ! "$CMD" =~ \.github ]]; then
    python3 -c '
import json, re, sys
cmd = sys.argv[1]
rewritten = re.sub(r"\b(git\s+sparse-checkout\s+set)\s+", r"\1 .agents .github ", cmd)
print(json.dumps({"decision": "allow", "overwrite": {"CommandLine": rewritten}}))
' "$CMD"
    exit 0
  fi
fi

allow
