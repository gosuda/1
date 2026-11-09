# Technical Feasibility and Verification Report

This report records the verification results behind the two-repository monorepo design on Git 2.54.0 and on GitHub. It documents the plumbing corrections required for in-memory merges, atomic reference updates, and the platform constraints that determine where each reference lives.

## 1. Architectural Assessment Summary

Table 1 summarizes the technical evaluation for each subsystem in the design.

| Subsystem | Assessment | Technical Rationale |
| :--- | :--- | :--- |
| Merkle-tree deduplication across orphan snapshots | Verified | Unchanged subtrees and blobs keep identical SHA-1 hashes across parentless commits. |
| Custom reference isolation (`refs/ws/*`, `refs/suda/*`) | Verified | Standard `git clone` and the GitHub branch selector ignore references outside `refs/heads/*` and `refs/tags/*`. |
| In-memory 3-way merge (`git merge-tree --write-tree`) | Verified with corrections | Requires `--merge-base=<tree>` and exit-code parsing rather than positional base arguments. |
| Separation of `refs/heads/main` and `refs/suda/main` | Verified | The control plane stays a small, separate branch while all project trees live in `refs/suda/main`. |
| Two-tier CI via subdirectory workflows | Verified | GitHub Actions ignores `/.github/workflows/` inside subdirectories of `gosuda/0` until `capy` exports the subtree to a standalone repository root. |
| Trunk copy on the user repository | Verified | `receive-pack` on `gosuda/0` advertises `refs/suda/*` (checked with a `GIT_TRACE_PACKET` dry-run push). A `--filter=blob:none` client therefore sends only workspace deltas when the copy is present. Without an advertised base, `pack-objects` needs blobs the client never fetched. |

## 2. Empirical Verification on Git 2.54.0

A local verification script tested each claim against a bare repository on Git 2.54.0. The script exercised two-commit workspaces, non-overlapping edits, conflicting edits, and concurrent atomic pushes.

Table 2 lists the tested command patterns, observed exit codes, and required corrections.

| Test ID | Tested Mechanism | Observed Result | Required Plumbing Correction |
| :--- | :--- | :--- | :--- |
| V1 | Object deduplication across orphan commits | 27 objects before merge; 33 objects after publishing a new orphan snapshot | None; Git added only 1 commit, 3 path trees, and 2 modified blobs. |
| V2 | Default `git clone` reference filtering | Clone fetched only `refs/heads/main` | None for clone; note that `git ls-remote` still lists `refs/ws/*` and `refs/suda/*` to any reader. |
| V3 | Merge of unrelated histories without explicit base | Exit code `128` (`fatal: refusing to merge unrelated histories`) | Supply the explicit base tree via `--merge-base`. |
| V4 | Positional 3-argument `git merge-tree --write-tree BASE MAIN WORK` | Exit code `129` (usage error) | Pass the base tree with `--merge-base="$BASE_TREE" "$MAIN_TREE" "$WORK_TREE"`. |
| V5 | Stdout capture on `git merge-tree --write-tree` conflict | Exit code `1`; stdout contains the tree hash followed by stage lines and conflict text | Check the exit code first, then extract only line 1 (`head -n1`) when the exit code is `0`. The lander uses `--name-only` and reads the conflicting paths on exit code `1`. |
| V6 | Base resolution via `refs/ws/<id>~1^{tree}` | Returned the first workspace commit instead of the trunk base on a 2-commit branch | Resolve the root commit of the workspace chain using `git rev-list --max-parents=0 "$WORK_SHA"`. |
| V7 | Concurrent landing with unconditional `git push --force` | Second push silently overwrote the first landing | Use `git push --atomic` with `--force-with-lease=<ref>:<expected-sha>` on every updated reference. |
| V8 | Atomic push rejection when one lease is stale | Exit code `1` (`atomic push failed for ref ... status: 7`); zero references moved | None; confirmed that one stale lease aborts the entire multi-reference transaction. |

## 3. GitHub Platform Constraints and Mitigations

GitHub enforces specific rules on custom reference namespaces, repository rulesets, workflow triggers, and plan features. Table 3 maps each platform constraint to the mitigation built into the design. The first row was measured on 2026-10-30 against the `gosuda` organization.

| Platform Constraint | Operational Impact | Mitigation in the Design |
| :--- | :--- | :--- |
| The organization is on GitHub Free, so rulesets and branch protection are unavailable on private `gosuda/0` (`GET /repos/gosuda/0/rulesets` and `.../branches/main/protection` return HTTP 403). | Branch protection cannot guard the trunk. | Repository permissions are the enforcement. Members have read access to `gosuda/0`, and only the `suda-lander` App and the org owners have write access. |
| Ruleset targets are `branch`, `tag`, `push`, and `repository`. They cannot target `refs/suda/*`. | A ruleset cannot restrict writes to the trunk references. | Same as above. The lander is the only writer of `refs/suda/*`, and `origin` has an invalid push URL for members. |
| Custom references are readable by all repository readers | Any user with read permission can fetch `refs/ws/*` and `refs/suda/*` via `git ls-remote`. | Both repositories are private. No credential is ever stored in a reference (architecture §2). |
| GitHub Actions `push` triggers fire only for `refs/heads/*` and `refs/tags/*` | Pushing `refs/ws/*` or `refs/heads/land/*` on `gosuda/1` triggers no workflow. | The lander polls `refs/heads/land/*` on every cycle, so landing needs no trigger. An optional, signed `/wake` request only shortens the wait. |
| GitHub Actions meters runner minutes on private repositories | Running CI for private repositories consumes monthly quota. | `gosuda/0` has no workflows. Presubmit runs on the lander server, and project CI runs on the public mirrors after export. |
| The repository activity API reports the actor and the `after` SHA of each ref update (response shape verified on `gosuda/0`). | Identity depends on the activity entry for a tip, which may not be visible at the moment of the push. | The lander defers a request whose tip has no visible entry and retries on a later cycle. |

## 4. Not Verified

The following platform behaviors are outside the verification scope of this report:

- Server-side behavior of `git push --atomic` on `github.com` for more than three references in one push. The lander pushes at most three references per landing (`refs/suda/main`, `refs/suda/history`, and `refs/heads/main` when the control plane changes).
- GitHub secondary rate limits when the lander exports to more than 10 mirror repositories in a single cycle.
