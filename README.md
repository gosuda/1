# gosuda/1

User repository for [`gosuda/0`](https://github.com/gosuda/0). Members push workspaces (`refs/ws/<actor>/<id>`), landing requests (`refs/heads/land/<id>`), and review branches (`refs/heads/base|review/<id>`) here. The trunk (`refs/suda/main`, `refs/suda/history`) is written only by the lander; the copies in this repository exist so pushes from `--filter=blob:none` clones negotiate against an advertised base and carry no trust.

Setup and workflow: see `docs/README.md` in `gosuda/0`. Do not fetch the control plane or the trunk from this repository.
