# Agent Governance and Pure-Git Operations

This specification defines how autonomous coding agents operate inside `gosuda/0` and its user repository `gosuda/1`. Agents execute standard Git commands guided by `AGENTS.md`, `.gitconfig` aliases, and `.agents/` skill modules. A pre-execution hook blocks direct mutations of trunk references and pushes to `origin`.

## 1. Layered Instruction Architecture

The repository splits agent instructions across four layers to bound context consumption. Figure 1 shows the discovery and loading order across these layers.

```mermaid
flowchart TD
  R["Root AGENTS.md (always loaded)"] --> P["Domain AGENTS.md (loaded when editing subtree)"]
  R --> S[".agents/skills/*/SKILL.md (loaded on demand)"]
  R --> HK[".agents/hooks.json (PreToolUse command enforcement)"]
```

Table 1 specifies the file path, activation trigger, and owned scope for each customization layer.

| Layer | Path | Activation Trigger | Owned Scope |
| :--- | :--- | :--- | :--- |
| Root Rules | `/AGENTS.md` | Always loaded at session start | Reference boundaries, `OWNERS` and visibility rules, and the workspace, review, experimental, and mirror procedures |
| Subtree Rules | `/<domain>/<project>/AGENTS.md` | Loaded when reading or editing files in `<project>` | Project build commands, test targets, and package-specific invariants |
| Workspace Skills | `/.agents/skills/<name>/SKILL.md` | Loaded when the agent activates `<name>` | Runbooks for workspace submission, conflict resolution, repository creation and import, and mirror synchronization |
| Lifecycle Hooks | `/.agents/hooks.json` | Executed before every `run_command` tool call | Deterministic rejection of prohibited `git push` and `git pull` invocations, implemented by `.agents/scripts/guard-git.sh` |

## 2. Invariants and Safety Guardrails

Every coding agent operating in `gosuda/0` obeys these operational invariants. The pre-execution hook enforces the Git-level boundaries before any shell command runs. The lander enforces the rest on every landing.

1. **One Task per Workspace Reference**: Every task creates a dedicated branch named `ws/<actor>/<id>` rooted at `origin/suda/main`. Agents never reuse a workspace reference across unrelated tasks.
2. **No Direct Pushes to the Trunk or to `origin`**: After `git suda-init`, `origin` (`gosuda/0`) has an invalid push URL, and the hook denies any `git push` that names `origin`, `refs/suda/main`, or `refs/suda/history`. All trunk updates pass through `refs/heads/land/<id>` on `gosuda/1`.
3. **Import Boundary Enforcement**: Agents editing public domains (`//web/*`, `//net/*`, `//ipc/*`, `//storage/*`, `//crypto/*`, `//ai/*`, `//devtools/*`) never import packages from `//corp/*` or `//experimental/*`. The lander rejects any landing that violates this.
4. **Scoped Sparse Checkout**: Agents check out only the domain directories required for the active task. The hook rewrites `git sparse-checkout set` so that `.agents` and `.github` stay checked out.
5. **Fetch Control Plane and Trunk Only From `origin`**: Agents never fetch `refs/heads/main`, `refs/suda/*`, or any other trunk reference from `gosuda/1`.

> [!CAUTION]
> Never execute `git push --force` without `--force-with-lease` on any reference. Unconditional force pushes overwrite commits written by concurrent agents and cause permanent work loss.

## 3. Standard Agent Command Mapping

Agents execute native `git` subcommands or the `.gitconfig` aliases tracked on `refs/heads/main`. Table 2 maps each lifecycle operation to its native Git command and its alias. `ws-*` aliases push to the `ws` remote (`gosuda/1`) and fetch from `origin` (`gosuda/0`).

| Operation | Native Git Command | `.gitconfig` Alias |
| :--- | :--- | :--- |
| Attach trunk and `ws` remote | `git remote add ws <origin URL with /1>`, then `git fetch --filter=blob:none origin` | `git suda-init` |
| Set sparse directories | `git sparse-checkout set ipc/hqq web/gosuda` | `git sparse-checkout set ...` or `git ws-sparse <dirs>` |
| Create workspace | `git fetch --filter=blob:none origin && git switch -c ws/<actor>/<id> origin/suda/main` | `git ws-new <actor>/<id>` |
| Create experimental sandbox | `git sparse-checkout add experimental/<actor>/<name> && git switch -c ws/<actor>/exp-<name> origin/suda/main` | `git ws-exp <actor> <name>` |
| Graduate experimental package | `git mv experimental/<actor>/<name> <domain>/<name>` | `git ws-graduate <src> <dst>` |
| Save WIP snapshot | `git push --force-with-lease ws HEAD:refs/ws/<actor>/<id>` | `git ws-save` |
| Open ephemeral GitHub PR | `git push --force-with-lease ws ${base}:refs/heads/base/<id> HEAD:refs/heads/review/<id>` | `git ws-pr` |
| Generate diff link | `echo "https://magpie.gosuda.org/diff/$(git symbolic-ref --short HEAD)"` | `git ws-diff` |
| Rebase onto latest trunk | `git fetch --filter=blob:none origin && git rebase --onto origin/suda/main $(git rev-list --max-parents=0 HEAD) HEAD` | `git ws-sync` |
| Submit to landing queue | `git push --force-with-lease ws HEAD:refs/heads/land/<id>` | `git ws-land` |
| Query commit history | `git fetch --filter=blob:none origin refs/suda/history && git log FETCH_HEAD -- <path>` | `git suda-log <path>` |
| Fetch staged external changes | `git fetch ws` (the refspec maps `refs/ws/*` to `refs/remotes/ws/ws/*`) | none |

## 4. Control-Plane File Specifications

The control plane on `refs/heads/main` ships the files that configure Git aliases, agent instructions, and the command hook.

### 4.1 Tracked `.gitconfig` Specification

The `.gitconfig` file defines the aliases that wrap the `refs/suda/*`, `refs/ws/*`, and `refs/heads/land/*` reference conventions. Its behavior:

- `suda-init` copies the tracked `.gitconfig` into the Git directory and includes it. It adds the `ws` remote derived from the origin URL, sets `remote.origin.pushurl` to an invalid `no-push://` URL, configures the `refs/suda/*` fetch refspec on `origin`, and fetches from `origin`.
- `ws-new`, `ws-sync`, `ws-exp`, and `suda-log` fetch from `origin`.
- `ws-save`, `ws-land`, and `ws-pr` push to `ws`. `ws-land` pushes `HEAD` to `refs/heads/land/<id>` and does not call any API. `ws-pr` opens its pull request with `gh pr create --repo gosuda/1`.
- `ws-diff` prints the diff link for the current branch.

### 4.2 Root `AGENTS.md`

The root [AGENTS.md](../AGENTS.md) is the normative set of rules loaded into every agent session. This document does not repeat it.

### 4.3 Pre-Execution Hook Specification (`.agents/hooks.json`)

The `git-guard` hook inspects every shell command before execution and rejects prohibited Git invocations.

```json
{
  "git-guard": {
    "PreToolUse": [
      {
        "matcher": "run_command",
        "hooks": [
          {
            "type": "command",
            "command": "./scripts/guard-git.sh",
            "timeout": 5
          }
        ]
      }
    ]
  }
}
```

`.agents/scripts/guard-git.sh` applies these rules, in order:

1. Deny `git pull`. The reason names `git ws-sync` as the replacement.
2. Deny `git push` with an unconditional `--force` or `-f` that does not also carry `--force-with-lease`.
3. Deny `git push` that names `origin`. The reason names `git ws-save` and `git ws-land`.
4. Deny `git push` that names `refs/suda/main` or `refs/suda/history`.
5. Rewrite `git sparse-checkout set` so that `.agents` and `.github` are included.
6. Allow every other command.

## 5. Verification Matrix for Agent Workflows

Evaluate agent compliance against the scenarios in Table 3 before deploying changes to `AGENTS.md` or `.agents/`.

| Scenario | Input Condition | Expected Observable Outcome |
| :--- | :--- | :--- |
| Standard task landing | Agent modifies one file in `//net/portalite` and runs `git ws-land` | `suda/land` status `success` on the tip in `gosuda/1`; `refs/suda/main` advances; the land ref is deleted; the mirror `gosuda/portalite` receives the export in the same or a following cycle |
| Trunk advanced without conflict | `refs/suda/main` advances in `//web/gosuda` while the agent edits `//net/ivnp` | `git ws-sync` rebases cleanly; the lander merges in memory with `merge-tree` exit code `0` |
| Overlapping line conflict | Both `refs/suda/main` and the workspace modify the same lines | Status `failure` with the reason `merge conflict`; the agent runs `git ws-sync`, resolves the markers, re-tests, and re-submits |
| Prohibited push to `main` | Agent invokes `git push origin HEAD:main` | `guard-git.sh` denies execution and points to `git ws-land` |
| Cross-boundary import violation | Public package in `//web/securl` imports a `//corp/*` package by its module path | Status `failure` with a visibility reason; `refs/suda/main` unchanged |
| Unknown base | Workspace root is not the tree of any of the last 500 trunk commits | Status `failure` with `unknown base tree; run git ws-sync`; the agent runs `git ws-sync` and re-submits |
| Forged approval | Commit trailers claim `Reviewed-by: @owner` and the author name matches an owner, but the GitHub requester is not an owner and no approval is bound to the tip | Status `failure` with an `OWNERS` reason; trunk unchanged |
| Self-approving `OWNERS` | A new directory adds its own `OWNERS` and files that the same change lands | Rejected unless an ancestor `OWNERS` approves the requester: the chain is read from the base trunk tree, where the new directory has no `OWNERS` yet |
| Experimental path by another login | Requester `alice` lands under `experimental/bob/...` | Rejected unless the `OWNERS` chain approves; `bob` may land freely |
| Concurrent landing race | Two agents push `refs/heads/land/a` and `refs/heads/land/b` in the same interval | Both are processed in push-time order within the cycle; neither update is lost |
| Mirror push by a non-owner | A maintainer without ownership of the prefix pushes to the mirror branch | Not landed. The import is staged as `refs/ws/capy/<wf>-main` for a member to land |
| Break-glass push to `main` | An org owner pushes `refs/heads/main` directly | Next cycle lands an overlay-sync commit with `Suda-Break-Glass:` in the trunk history |
