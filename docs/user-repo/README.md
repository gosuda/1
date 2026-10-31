# gosuda/1: contribute to the gosuda monorepo

**Browse the current monorepo tree: [magpie.gosuda.org/head](https://magpie.gosuda.org/head)**

This is the **user repository** of the `gosuda` monorepo. Contributors and coding agents push workspaces,
landing requests, and review branches **here**. The monorepo itself (`gosuda/0`) is internal and read-only for
members: it is written only by the lander, the service that lands your changes.

> [!IMPORTANT]
> **Start with the [onboarding guide](./docs/onboarding.md).** It takes about five minutes.
> Never push to `gosuda/0`, and never run code from this repository's branches that you did not write
> yourself: anyone in the organization can write here. This page and the guide are generated from the
> control plane of `gosuda/0` and restored automatically if overwritten.

## Set up in one line

With the [GitHub CLI](https://cli.github.com) logged in (`gh auth login`):

```bash
gh api -H 'Accept: application/vnd.github.raw' repos/gosuda/0/contents/.github/scripts/onboard.sh | bash -s -- -d gosuda-0 ipc/hqq web/gosuda
```

This clones `gosuda/0` read-only, registers the aliases, adds the `ws` remote (this repository), disables
pushes to `origin`, and selects the directories you list. Run it again later to refresh the aliases.
The script is served from `gosuda/0`, where only the lander writes; never run a copy taken from a branch of
this repository. Prefer to read it first? <https://github.com/gosuda/0/blob/main/.github/scripts/onboard.sh>

## Contribution flow at a glance

```text
git ws-new <you>/<task>     create a workspace from the trunk
  ... edit, commit ...
git ws-save                 push work in progress     -> refs/ws/<you>/<task>
git ws-sync                 rebase onto the latest trunk
git ws-land                 submit for landing        -> refs/heads/land/<task>
```

The lander picks the request up within about a minute, checks identity, ownership, import visibility and
tests, merges it into the trunk, and exports changed projects to their public mirrors. The result appears as
the `suda/land` status on your commit.

## How development works here

### The model

- There is **one source tree**, the trunk (`refs/suda/main`). Every project lives in a directory of it
  (`net/ivnp`, `web/gosuda`, `ipc/hqq/HQQ`, ...), and shared code is just another directory. You can change a
  library and the projects that use it in **one commit**, and it lands atomically.
- You never push to the trunk, and there is no long-lived `main` to branch from. You work in a **workspace**:
  a branch `ws/<you>/<task>` in this repository whose root is a snapshot of the trunk. `git ws-new` creates it
  and `git ws-sync` moves its root to the latest trunk. Use your GitHub login as `<you>`.
- You ask for a change to land by pushing it to `refs/heads/land/<task>` (`git ws-land`). The **lander**, a
  single service, is the only writer of the trunk. It merges your change, runs the checks, and publishes.
- Public repositories such as `gosuda/HQQ` are **mirrors**: the lander exports each project's directory there
  after every landing, and imports changes made there by project owners.

### A change, step by step

1. **Pick the directories** your task touches and make them visible. The onboarding one-liner takes them as
   arguments; add more later with `git sparse-checkout add net/ivnp net/portalite`.
2. **Create a workspace**: `git ws-new alice/ivnp-fix-header`. One task per workspace; do not reuse a branch.
3. **Edit and test** inside the project directories. Commit as usual, any number of commits.
4. **Save** (`git ws-save`) whenever you want a backup or want somebody to look at it. Anyone in the
   organization can read `refs/ws/alice/ivnp-fix-header`; never commit credentials.
5. **Sync** (`git ws-sync`) so your root is the newest trunk. Resolve rebase conflicts here, not in the lander.
   Never use `git pull`.
6. **Ask for review** if you changed files you do not own (see below): `git ws-pr`.
7. **Land** (`git ws-land`) and watch the status:

   ```bash
   gh api repos/gosuda/1/commits/$(git rev-parse HEAD)/statuses --jq '.[] | "\(.context) \(.state) \(.description)"'
   ```

   `landed as <sha>` means the trunk contains it and the mirrors are being updated. Anything else names what
   to fix; the [onboarding guide](./docs/onboarding.md#watch-the-result) lists every message.

### Changing shared code

- Edit the library and its callers in the same workspace. The root `go.work` makes Go modules in the trunk
  build against each other's **local** code, so the whole change compiles and tests before you land.
- A committed `go.mod` must never contain `replace x => ../y`; the lander rejects it. A `require` of another
  monorepo module names a **published** version, because that is what the module's public mirror builds
  against. If your change needs a new library API in a dependent module's mirror, publish a library version
  first and raise the `require` in a second landing.
- Public projects (`web/`, `net/`, `ipc/`, `storage/`, `crypto/`, `ai/`, `devtools/`) may import each other and
  `third_party/`, never `corp/` or `experimental/`. `corp/` may import public code; nothing imports
  `experimental/`. The lander checks this on every landing.
- Do not commit large generated or binary files into a project that is mirrored; every mirror receives them.

### Who may change what

- `OWNERS` files (read from the current trunk, so a change cannot grant itself access) decide who may change a
  path, walking from the file's directory up to the root. Your identity is the GitHub account that pushed the
  landing branch; commit authors and `Reviewed-by:` lines count for nothing.
- If you own every path you changed, `git ws-land` is enough.
- Otherwise run **`git ws-pr`**. It opens a pull request here (`base/<task>` <- `review/<task>`) that shows only
  your change. An owner approves it **on the exact commit you will land**; push another commit and you need a
  new approval. Then run `git ws-land`.
- Changes under `.github/` always need an owner other than you.

### Experiments, new projects, imports

- **Sandbox**: `git ws-exp <you> <name>` gives you `experimental/<you>/<name>`, which you can land without any
  `OWNERS` approval. It is never mirrored, and nothing outside `experimental/` may import it.
- **Graduate** a sandbox into a real project: `git ws-graduate experimental/<you>/<name> <domain>/<name>`, then
  get an owner's approval for the new location.
- **New project**: `.github/scripts/create-repo.sh <prefix> <owner/repo>`; **import a repository**:
  `.github/scripts/import-repo.sh <prefix> <owner/repo>`. Both build one local commit (plus one for `go.work`
  when it applies) and print the next steps. A public mirror repository must exist and be reachable by the
  lander first; ask an org owner.

### After it lands

- The trunk advances atomically (`refs/suda/main` and `refs/suda/history`), and each project whose directory
  changed is exported to its mirror within a couple of minutes.
- A pull request opened on a public mirror is **imported** as a staged change, and the lander opens a review
  pull request here titled `[<project>#<N>] <original title>`. Review it like any other change; a member lands
  it with the two commands in its description, and the lander then closes the original. See
  [section 6 of the guide](./docs/onboarding.md#6-changes-that-arrive-from-public-mirrors).
- Security fixes that must stay private until released use the embargo flow in the guide (section 7).

### Rules of thumb

- Push only to `ws` (this repository). `origin` is read-only; `git pull` is never needed.
- One task per workspace; sync before you land; keep commits reviewable.
- Run the project's own tests first. The lander runs tests for the projects your change affects, plus their
  dependents, when presubmit is enabled.
- When in doubt, read the status message: it says what is wrong and what to do.

## What lives where

| Reference | Meaning | Written by |
| :--- | :--- | :--- |
| `refs/ws/<actor>/<id>` | Your work in progress | You |
| `refs/heads/land/<id>` | A landing request; deleted once processed | You |
| `refs/heads/base/<id>`, `refs/heads/review/<id>` | A review pull request pair (`git ws-pr`) | You |
| `refs/ws/capy/<project>-pr-<N>`, `refs/ws/capy/<project>-main` | External changes staged from public mirrors | The lander |
| `refs/suda/main`, `refs/suda/history` | **Untrusted copies** of the trunk, kept only so pushes stay small | The lander |
| `refs/heads/main` | This page and the guide | The lander |

## Links

- Current tree: <https://magpie.gosuda.org/head> · Source tree (snapshot): <https://magpie.gosuda.org/tree> · History: <https://magpie.gosuda.org/history> · Diff links: <https://magpie.gosuda.org/diff>
- Design and rules (read access to `gosuda/0` required): [`docs/README.md`](https://github.com/gosuda/0/blob/main/docs/README.md), [`AGENTS.md`](https://github.com/gosuda/0/blob/main/AGENTS.md)
