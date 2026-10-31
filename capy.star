load("//.github/scripts/capy/defs.star", "capy", "core", "gomod", "metadata")

PILOT_REPOS = [
    ("GRT", "net/grt", "gosuda/GRT", False),
    ("website", "web/gosuda", "gosuda/website", False),
    ("IVNP", "net/ivnp", "gosuda/IVNP", False),
    ("HQQ", "ipc/hqq/HQQ", "gosuda/HQQ", False),
    ("hqq-sqlite3", "ipc/hqq/hqq-sqlite3", "gosuda/hqq-sqlite3", False),
    ("hqq-zstd", "ipc/hqq/hqq-zstd", "gosuda/hqq-zstd", False),
    ("securl", "web/securl", "gosuda/securl", False),
    ("portalite", "net/portalite", "gosuda/portalite", False, "master"),
    ("azure-magpie", "devtools/azure-magpie", "gosuda/azure-magpie", False),
    ("keyless_tls", "crypto/keyless_tls", "gosuda/keyless_tls", False),
    ("x402-facilitator", "crypto/x402-facilitator", "gosuda/x402-facilitator", False),
    ("portal-tunnel", "net/portal-tunnel", "gosuda/portal-tunnel", False),
    ("potly", "devtools/potly", "gosuda/potly", False),
    ("AutoIRC2P", "net/autoirc2p", "gosuda/AutoIRC2P", False),
    ("portal-multiplatform-sdk", "net/portal-multiplatform-sdk", "gosuda/portal-multiplatform-sdk", False),
    ("randflake", "crypto/randflake", "gosuda/randflake", False),
    ("jimin", "ai/jimin", "gosuda/jimin", False),
    ("telemetry.gosuda.org", "web/telemetry", "gosuda/telemetry.gosuda.org", False),
    ("supersql", "storage/supersql", "gosuda/supersql-alpha", False),
    ("zeroproxy", "net/zeroproxy", "gosuda/zeroproxy", False),
]

# Entries are (name, monorepo prefix, mirror repo, strip_replaces[, branch]); branch defaults to "main".
for entry in PILOT_REPOS:
    name, prefix, repo, strip_replaces = entry[0], entry[1], entry[2], entry[3]
    branch = entry[4] if len(entry) > 4 else "main"
    transforms = [
        core.exclude(["internal_notes/**", "**.local.toml"]),
        metadata.expose_label("GitOrigin-RevId"),
    ]
    if strip_replaces:
        transforms.append(gomod.strip_local_replaces())
    capy.workflow(
        name = name,
        origin = capy.monorepo_path(prefix),
        destination = capy.github_repo(repo, branch = branch),
        mode = "SQUASH",
        transformations = transforms,
    )
