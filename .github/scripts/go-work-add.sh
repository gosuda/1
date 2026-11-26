#!/usr/bin/env bash
# Adds every Go module found under the given prefixes to the root go.work of the current workspace
# branch and commits it once (separate from the import commit, which must stay an unmodified copy of
# the mirror). The root go.work is not exported, so this never changes mirror-visible content.
#
# Usage: go-work-add.sh <prefix> [<prefix> ...]
#   Committed go.mod files must not contain local `replace` directives: go.work resolves sibling
#   modules locally. The `go` line of go.work is raised when a module needs a newer one.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=workspace-lib.sh
. "$SCRIPT_DIR/workspace-lib.sh"

[ $# -ge 1 ] || ws_usage "usage: go-work-add.sh <prefix> [<prefix> ...]"
for p in "$@"; do ws_validate_prefix "$p"; done

ws_check_workspace
for p in "$@"; do ws_prepare_sparse "$p"; done
[ -f go.work ] || ws_die "go.work not found at the workspace root"

python3 - "$REPO_ROOT" "$@" <<'PY'
import os
import re
import subprocess
import sys

root, prefixes = sys.argv[1], sys.argv[2:]
os.chdir(root)


def tracked_gomods(prefix):
    out = subprocess.run(["git", "ls-files", "-z", "--", prefix], capture_output=True, text=True, check=True).stdout
    mods = (p for p in out.split("\0") if p == "go.mod" or p.endswith("/go.mod"))
    # The go tool ignores testdata and vendor directories, so their go.mod files are fixtures, not modules.
    return sorted(p for p in mods if not {"testdata", "vendor"} & set(p.split("/")[:-1]))


def version(s):
    return tuple(int(x) for x in (s.split(".") + ["0", "0"])[:3])


dirs, need = [], (0, 0, 0)
for prefix in prefixes:
    mods = tracked_gomods(prefix)
    if not mods:
        sys.exit(f"error: no tracked go.mod under {prefix}")
    for m in mods:
        d = os.path.dirname(m) or "."
        text = open(m, encoding="utf-8").read()
        for rep in re.findall(r"(?m)^\s*(?:replace\s+)?\S+(?:\s+v\S+)?\s+=>\s+(\.\.?/\S*)", text):
            sys.exit(f"error: {m} has a local replace directive ({rep}); remove it and rely on go.work")
        g = re.search(r"(?m)^go\s+(\S+)", text)
        if g:
            need = max(need, version(g.group(1)))
        dirs.append("./" + d)

work = open("go.work", encoding="utf-8").read()
m = re.search(r"(?ms)^use\s*\(\n(.*?)^\)\n", work)
if m:
    existing = [l.strip() for l in m.group(1).splitlines() if l.strip()]
else:
    existing = re.findall(r"(?m)^use\s+(\S+)\s*$", work)
merged = sorted(set(existing) | set(dirs))
added = [d for d in merged if d not in existing]
gm = re.search(r"(?m)^go\s+(\S+)\s*$", work)
cur = version(gm.group(1)) if gm else (0, 0, 0)
go_line = gm.group(1) if gm else "1.21"
if need > cur:
    go_line = ".".join(str(x) for x in need)
    print(f"go.work: raising go directive to {go_line}")
if not added and gm and go_line == gm.group(1):
    print("go.work: nothing to add")
    sys.exit(0)
use_block = "use (\n" + "".join(f"\t{d}\n" for d in merged) + ")\n"
if m:
    new = work[: m.start()] + use_block + work[m.end():]
else:
    new = re.sub(r"(?m)^use\s+\S+\s*\n", "", work).rstrip("\n") + "\n\n" + use_block
if gm:
    new = re.sub(r"(?m)^go\s+\S+\s*$", f"go {go_line}", new, count=1)
else:
    new = f"go {go_line}\n\n" + new
open("go.work", "w", encoding="utf-8").write(new)
for d in added:
    print(f"go.work: use {d}")
PY

git add -- go.work
if git diff --cached --quiet; then
  echo "no changes: go.work already covers the modules"
  exit 0
fi
git commit -q -m "go.work: add the Go modules of $(printf '//%s ' "$@" | sed 's/ $//')"
echo "committed $(git rev-parse --short HEAD) on $(git symbolic-ref --short HEAD): $(git log -1 --format=%s)"
