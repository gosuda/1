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

if [[ "$CMD" =~ (^|[[:space:]\;\&\|])git[[:space:]]+pull([[:space:]]|$) ]]; then
  deny "Do not run git pull in gosuda/0. Run git ws-sync (git rebase --onto origin/suda/main) instead."
fi

if [[ "$CMD" =~ (^|[[:space:]\;\&\|])git[[:space:]]+push([[:space:]]|$) ]]; then
  if [[ "$CMD" =~ git[[:space:]]+push[^\;\&\|]*(--force([[:space:]]|$)|[[:space:]]-f([[:space:]]|$)) ]] && [[ ! "$CMD" =~ --force-with-lease ]]; then
    deny "Unconditional git push --force is prohibited. Pass --force-with-lease instead."
  fi
  if [[ "$CMD" =~ git[[:space:]]+push([[:space:]]+-[^[:space:]]+)*[[:space:]]+origin([[:space:]]|$) ]]; then
    deny "origin (gosuda/0) is written only by the lander. Push workspaces and landing requests to the ws remote (git ws-save, git ws-land)."
  fi
  if [[ "$CMD" =~ :refs/suda/(main|history) ]]; then
    deny "Direct pushes to refs/suda/main or refs/suda/history are prohibited. Push to refs/heads/land/<id> on the ws remote instead (git ws-land)."
  fi
fi

# The stash is one ref (refs/stash) shared by every worktree of a clone: with several agents in worktrees, one
# agent pops another agent's work. Listing and showing stays allowed.
if [[ "$CMD" =~ (^|[[:space:]\;\&\|])git[[:space:]]+stash([[:space:]]|$) ]] && [[ ! "$CMD" =~ git[[:space:]]+stash[[:space:]]+(list|show)([[:space:]]|$) ]]; then
  deny "Do not use git stash: the stash is shared by every worktree of this clone. Commit the work in progress (git ws-save) or back it up with git ws-autosave once."
fi

# Rewrites, applied together:
# - Worktrees share their remote-tracking refs, and two concurrent fetches fail with "cannot lock ref": every plain
#   fetch goes through the clone-wide fetch lock of the workspace tools (git suda-ws fetch).
# - git sparse-checkout set keeps .agents and .github.
python3 -c '
import json, re, sys
cmd = sys.argv[1]
out = re.sub(r"(^|[\s;&|])git\s+fetch(?=\s|$)", r"\1git suda-ws fetch", cmd)
if re.search(r"(^|[\s;&|])git\s+sparse-checkout\s+set(\s|$)", out) and not (".agents" in out and ".github" in out):
    out = re.sub(r"\b(git\s+sparse-checkout\s+set)\s+", r"\1 .agents .github ", out)
if out == cmd:
    print(json.dumps({"decision": "allow"}))
else:
    print(json.dumps({"decision": "allow", "overwrite": {"CommandLine": out}}))
' "$CMD"
