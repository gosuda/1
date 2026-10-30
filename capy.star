load("//.github/scripts/capy/defs.star", "capy", "core", "gomod", "metadata")

PILOT_REPOS = [
    ("GRT", "net/grt", "gosuda/GRT", False),
    ("website", "web/gosuda", "gosuda/website", False),
    ("IVNP", "net/ivnp", "gosuda/IVNP", False),
    ("HQQ", "ipc/hqq/HQQ", "gosuda/HQQ", False),
    ("hqq-sqlite3", "ipc/hqq/hqq-sqlite3", "gosuda/hqq-sqlite3", False),
    ("hqq-zstd", "ipc/hqq/hqq-zstd", "gosuda/hqq-zstd", False),
    ("securl", "web/securl", "gosuda/securl", False),
    ("portalite", "net/portalite", "gosuda/portalite", False),
    ("azure-magpie", "devtools/azure-magpie", "gosuda/azure-magpie", False),
]

for name, prefix, repo, strip_replaces in PILOT_REPOS:
    transforms = [
        core.exclude(["internal_notes/**", "**.local.toml"]),
        metadata.expose_label("GitOrigin-RevId"),
    ]
    if strip_replaces:
        transforms.append(gomod.strip_local_replaces())
    capy.workflow(
        name = name,
        origin = capy.monorepo_path(prefix),
        destination = capy.github_repo(repo, branch = "main"),
        mode = "SQUASH",
        transformations = transforms,
    )
