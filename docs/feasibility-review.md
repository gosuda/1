# Technical Feasibility and Verification Report

This report records the verification results for the Git CAS monorepo design on Git 2.54.0. It documents the plumbing corrections required for in-memory merges, atomic reference updates, and GitHub platform compatibility.

## 1. Architectural Assessment Summary

Table 1 summarizes the technical evaluation for each subsystem in the monorepo design.

| Subsystem | Assessment | Technical Rationale |
| :--- | :--- | :--- |
| Merkle-tree deduplication across orphan snapshots | Verified | Unchanged subtrees and blobs keep identical SHA-1 hashes across parentless commits. |
| Custom reference isolation (`refs/ws/*`, `refs/suda/*`) | Verified | Standard `git clone` and the GitHub branch selector ignore references outside `refs/heads/*` and `refs/tags/*`. |
| In-memory 3-way merge (`git merge-tree --write-tree`) | Verified with corrections | Requires `--merge-base=<tree>` and exit-code parsing rather than positional base arguments. |
| Separation of `refs/heads/main` and `refs/suda/main` | Verified | Keeps `refs/heads/main` at 20 KiB while storing all project trees in `refs/suda/main`. |
| Two-tier CI via subdirectory workflows | Verified | GitHub Actions ignores `/.github/workflows/` inside subdirectories of `gosuda/0` until `capy` exports the subtree to a standalone repository root. |

## 2. Empirical Verification on Git 2.54.0

A local verification script tested each claim against a bare repository on Git 2.54.0. The script exercised two-commit workspaces, non-overlapping edits, conflicting edits, and concurrent atomic pushes.

Table 2 lists the tested command patterns, observed exit codes, and required corrections.

| Test ID | Tested Mechanism | Observed Result | Required Plumbing Correction |
| :--- | :--- | :--- | :--- |
| V1 | Object deduplication across orphan commits | 27 objects before merge; 33 objects after publishing a new orphan snapshot | None; Git added only 1 commit, 3 path trees, and 2 modified blobs. |
| V2 | Default `git clone` reference filtering | Clone fetched only `refs/heads/main` | None for clone; note that `git ls-remote` still lists `refs/ws/*` and `refs/suda/*` to any reader. |
| V3 | Merge of unrelated histories without explicit base | Exit code `128` (`fatal: refusing to merge unrelated histories`) | Supply the explicit base tree via `--merge-base`. |
| V4 | Positional 3-argument `git merge-tree --write-tree BASE MAIN WORK` | Exit code `129` (usage error) | Pass the base tree with `--merge-base="$BASE_TREE" "$MAIN_TREE" "$WORK_TREE"`. |
| V5 | Stdout capture on `git merge-tree --write-tree` conflict | Exit code `1`; stdout contains the tree hash followed by stage lines and conflict text | Check the exit code first and extract only line 1 (`head -n1`) when exit code is `0`. |
| V6 | Base resolution via `refs/ws/<id>~1^{tree}` | Returned the first workspace commit instead of the trunk base on a 2-commit branch | Resolve the root commit of the workspace chain using `git rev-list --max-parents=0 "$WORK_SHA"`. |
| V7 | Concurrent landing with unconditional `git push --force` | Second push silently overwrote the first landing | Use `git push --atomic` with `--force-with-lease=<ref>:<expected-sha>` on every updated reference. |
| V8 | Atomic push rejection when one lease is stale | Exit code `1` (`atomic push failed for ref ... status: 7`); zero references moved | None; confirmed that one stale lease aborts the entire multi-reference transaction. |

## 3. GitHub Platform Constraints and Mitigations

GitHub enforces specific rules on custom reference namespaces, repository rulesets, and workflow triggers. Table 3 maps each platform constraint to the mitigation built into `gosuda/0`.

| Platform Constraint | Operational Impact | Mitigation in `gosuda/0` |
| :--- | :--- | :--- |
| Repository rulesets apply only to `refs/heads/*` and `refs/tags/*` | GitHub rulesets cannot restrict write access to `refs/suda/*` or `refs/ws/*` directly | Restrict direct human write access on `gosuda/0` or mirror `refs/suda/history` to a protected branch (`refs/heads/audit/history`). |
| Custom references are readable by all repository readers | Any user with read permission on `gosuda/0` can fetch `refs/ws/*` via `git ls-remote` | Keep `gosuda/0` private and enforce pre-push credential checks. |
| GitHub Actions `push` triggers only fire on `refs/heads/*` and `refs/tags/*` | Pushing to `refs/ws/*` does not trigger workflows | Use `refs/ws/*` for saving work-in-progress and push to `refs/heads/land/<id>` to trigger `land.yml`. |
| GitHub Actions meters runner minutes on private repositories | Running multi-OS test matrices inside private `gosuda/0` consumes monthly quota | Keep `land.yml` under 15 seconds and run full project CI on the 105 public mirror repositories after `capy` export. |

## 4. Not Verified

The following platform behaviors require verification against the live `gosuda` GitHub organization during Phase 1 deployment:

- Server-side behavior of `git push --atomic` across more than three references simultaneously on `github.com`.
- GitHub Actions secondary rate limits when `capy` pushes updates to more than 10 standalone public repositories in a single `land.yml` run.
