# gosuda/1: contribute to the gosuda monorepo

This is the **user repository** of the `gosuda` monorepo. Contributors and coding agents push workspaces,
landing requests, and review branches **here**. The monorepo itself (`gosuda/0`) is internal and read-only for
members: it is written only by the lander, the service that lands your changes.

> [!IMPORTANT]
> **Start with the [onboarding guide](./docs/onboarding.md).** It takes about five minutes.
> Never push to `gosuda/0`, and never run code from this repository's branches that you did not write
> yourself: anyone in the organization can write here. This page and the guide are generated from the
> control plane of `gosuda/0` and restored automatically if overwritten.

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

- Source tree: <https://magpie.gosuda.org/tree> · History: <https://magpie.gosuda.org/history> · Diff links: <https://magpie.gosuda.org/diff>
- Design and rules (read access to `gosuda/0` required): [`docs/README.md`](https://github.com/gosuda/0/blob/main/docs/README.md), [`AGENTS.md`](https://github.com/gosuda/0/blob/main/AGENTS.md)
