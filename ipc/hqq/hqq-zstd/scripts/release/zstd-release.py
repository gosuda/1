#!/usr/bin/env python3
"""Automates tracking, pinning, tagging, and hashing upstream Zstandard releases."""

from __future__ import annotations

import argparse
import hashlib
import io
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import urllib.request

EXPECTED_PLATFORMS = (
    ("darwin", "amd64", "zstd_server_darwin_amd64"),
    ("darwin", "arm64", "zstd_server_darwin_arm64"),
    ("linux", "amd64", "zstd_server_linux_amd64"),
    ("linux", "arm64", "zstd_server_linux_arm64"),
    ("windows", "amd64", "zstd_server_windows_amd64.exe"),
    ("windows", "arm64", "zstd_server_windows_arm64.exe"),
)

ZSTD_RELEASES_LATEST_API = "https://api.github.com/repos/facebook/zstd/releases/latest"
ZSTD_RELEASE_TAG_API = "https://api.github.com/repos/facebook/zstd/releases/tags/v{version}"
SEMVER_RE = re.compile(r"^v?(\d+)\.(\d+)\.(\d+)$")
TAG_RE = re.compile(r"^v(\d+)\.(\d+)\.(\d+)$")

LICENSE_MARKERS = (
    "BSD License",
    "Redistributions of source code must retain the above copyright notice",
    "Neither the name Facebook, nor Meta",
)


def repo_root(start: Path | None = None) -> Path:
    if start is not None:
        return start.resolve()
    return Path(__file__).resolve().parents[2]


def parse_zstd_version(version: str) -> tuple[int, int, int]:
    m = SEMVER_RE.fullmatch(version.strip())
    if not m:
        raise ValueError(f"invalid zstd version {version!r}: expected X.Y.Z")
    return int(m.group(1)), int(m.group(2)), int(m.group(3))


def zstd_version_number(version: str) -> int:
    major, minor, patch = parse_zstd_version(version)
    return major * 10000 + minor * 100 + patch


def read_pinned_version(root: Path) -> str:
    version_file = root / "third_party" / "zstd" / "VERSION"
    for line in version_file.read_text(encoding="utf-8").splitlines():
        if line.startswith("version="):
            return line.split("=", 1)[1].strip()
    raise ValueError(f"{version_file} has no version= line")


def github_headers() -> dict[str, str]:
    headers = {
        "Accept": "application/vnd.github+json",
        "User-Agent": "gosuda-hqq-zstd-release",
    }
    token = os.environ.get("GH_TOKEN") or os.environ.get("GITHUB_TOKEN")
    if token:
        headers["Authorization"] = f"Bearer {token}"
    return headers


def fetch_json(url: str) -> dict:
    req = urllib.request.Request(url, headers=github_headers())
    with urllib.request.urlopen(req, timeout=30) as resp:
        return json.load(resp)


def download_bytes(url: str) -> bytes:
    req = urllib.request.Request(url, headers={"User-Agent": "gosuda-hqq-zstd-release"})
    with urllib.request.urlopen(req, timeout=120) as resp:
        return resp.read()


def latest_zstd_release() -> tuple[str, dict]:
    release = fetch_json(ZSTD_RELEASES_LATEST_API)
    if release.get("draft") or release.get("prerelease"):
        raise RuntimeError(f"latest release {release.get('tag_name')!r} is draft/prerelease")
    tag = release.get("tag_name", "")
    major, minor, patch = parse_zstd_version(tag)
    version = f"{major}.{minor}.{patch}"
    archive = f"zstd-{version}.tar.gz"
    assets = {a["name"]: a for a in release.get("assets", [])}
    if archive not in assets:
        raise RuntimeError(f"release {tag} does not publish {archive}")
    return version, release


def parse_sha256_file(text: str, filename: str) -> str:
    for line in text.splitlines():
        fields = line.strip().split()
        if len(fields) == 2 and fields[1] == filename:
            digest = fields[0].lower()
            if re.fullmatch(r"[0-9a-f]{64}", digest):
                return digest
    raise ValueError(f"no valid SHA-256 entry for {filename!r}")


def verify_and_extract_archive(archive_bytes: bytes, version: str) -> tuple[str, str]:
    digest = hashlib.sha256(archive_bytes).hexdigest()
    prefix = f"zstd-{version}"
    license_member = f"{prefix}/LICENSE"
    header_member = f"{prefix}/lib/zstd.h"
    license_text: str | None = None
    header_text: str | None = None

    with tarfile.open(fileobj=io.BytesIO(archive_bytes), mode="r:gz") as tf:
        for member in tf.getmembers():
            if member.name.startswith("/") or ".." in Path(member.name).parts:
                raise ValueError(f"unsafe archive member path: {member.name!r}")
            if member.name == license_member:
                f = tf.extractfile(member)
                if f is None:
                    raise ValueError(f"cannot read {license_member}")
                license_text = f.read().decode("utf-8")
            elif member.name == header_member:
                f = tf.extractfile(member)
                if f is None:
                    raise ValueError(f"cannot read {header_member}")
                header_text = f.read().decode("utf-8")

    if license_text is None:
        raise ValueError(f"missing {license_member} in archive")
    if header_text is None:
        raise ValueError(f"missing {header_member} in archive")

    for marker in LICENSE_MARKERS:
        if marker not in license_text:
            raise ValueError(f"{license_member} does not contain expected BSD marker {marker!r}")

    maj_m = re.search(r"^\s*#\s*define\s+ZSTD_VERSION_MAJOR\s+(\d+)", header_text, re.M)
    min_m = re.search(r"^\s*#\s*define\s+ZSTD_VERSION_MINOR\s+(\d+)", header_text, re.M)
    rel_m = re.search(r"^\s*#\s*define\s+ZSTD_VERSION_RELEASE\s+(\d+)", header_text, re.M)
    if not (maj_m and min_m and rel_m):
        raise ValueError(f"could not parse ZSTD_VERSION_* macros from {header_member}")
    header_ver = f"{int(maj_m.group(1))}.{int(min_m.group(1))}.{int(rel_m.group(1))}"
    if header_ver != version:
        raise ValueError(f"{header_member} reports {header_ver}, expected {version}")

    return digest, license_text


def pin_version(root: Path, version: str | None = None, release_meta: dict | None = None) -> str:
    if version is None:
        version, release_meta = latest_zstd_release()
    else:
        major, minor, patch = parse_zstd_version(version)
        version = f"{major}.{minor}.{patch}"
        if release_meta is None:
            release_meta = fetch_json(ZSTD_RELEASE_TAG_API.format(version=version))

    archive = f"zstd-{version}.tar.gz"
    assets = {a["name"]: a for a in release_meta.get("assets", [])}
    if archive not in assets:
        raise RuntimeError(f"release v{version} has no asset {archive}")

    tar_url = assets[archive]["browser_download_url"]
    if not tar_url.startswith("https://"):
        raise ValueError(f"unexpected tarball url {tar_url!r}")
    archive_bytes = download_bytes(tar_url)
    digest, license_text = verify_and_extract_archive(archive_bytes, version)

    sha_asset = assets.get(f"{archive}.sha256")
    if sha_asset is not None:
        sha_text = download_bytes(sha_asset["browser_download_url"]).decode("utf-8")
        expected = parse_sha256_file(sha_text, archive)
        if digest != expected:
            raise ValueError(f"SHA-256 mismatch for {archive}: got {digest}, asset says {expected}")

    api_digest = assets[archive].get("digest") or ""
    if api_digest.startswith("sha256:"):
        expected_api = api_digest.removeprefix("sha256:").lower()
        if digest != expected_api:
            raise ValueError(f"SHA-256 mismatch against GitHub API digest: got {digest}, API says {expected_api}")

    pin_dir = root / "third_party" / "zstd"
    pin_dir.mkdir(parents=True, exist_ok=True)
    version_content = (
        "# Zstandard source pinned for the bindgen zstd example (bindgen/examples/zstd).\n"
        "# Read by scripts/release/fetch-zstd.sh. Not vendored in git.\n"
        "# Dual-licensed (BSD-3-Clause or GPL-2.0-only); this project uses the BSD-3-Clause\n"
        "# option. LICENSE in this directory is the BSD text of the pinned release.\n"
        "# SHA256 is the digest of the official release tarball; the release also\n"
        f"# publishes it as {archive}.sha256 next to the tarball.\n"
        f"version={version}\n"
        f"archive={archive}\n"
        f"url={tar_url}\n"
    )
    (pin_dir / "VERSION").write_text(version_content, encoding="utf-8")
    (pin_dir / "SHA256").write_text(f"{digest}  {archive}\n", encoding="utf-8")
    (pin_dir / "LICENSE").write_text(license_text, encoding="utf-8")
    return version


def list_git_tags(root: Path) -> list[str]:
    out = subprocess.check_output(
        ["git", "tag", "-l", "v*.*.*"],
        cwd=root,
        text=True,
    )
    return [line.strip() for line in out.splitlines() if line.strip()]


def compute_next_tag(existing_tags: list[str], version: str) -> str:
    cver = zstd_version_number(version)
    parsed: list[tuple[int, int, int]] = []
    tag_set = set(existing_tags)
    for t in existing_tags:
        m = TAG_RE.fullmatch(t)
        if m:
            parsed.append((int(m.group(1)), int(m.group(2)), int(m.group(3))))

    if not parsed:
        return f"v0.1.{cver}"

    wmaj, wmin, wcver = max(parsed)
    if cver > wcver:
        candidate = f"v{wmaj}.{wmin}.{cver}"
        while candidate in tag_set:
            wmin += 1
            candidate = f"v{wmaj}.{wmin}.{cver}"
        return candidate

    wmin += 1
    candidate = f"v{wmaj}.{wmin}.{cver}"
    while candidate in tag_set:
        wmin += 1
        candidate = f"v{wmaj}.{wmin}.{cver}"
    return candidate


def sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def update_hashes_and_embedded(
    root: Path,
    dist_dir: Path,
    tag: str,
    version: str | None = None,
    update_docs: bool = False,
) -> None:
    if not TAG_RE.fullmatch(tag):
        raise ValueError(f"invalid release tag {tag!r}: expected v<major>.<minor>.<cver>")
    if version is None:
        version = read_pinned_version(root)
    cver = zstd_version_number(version)

    entries: list[tuple[str, str, str]] = []
    sums_lines: list[str] = []
    for goos, goarch, filename in sorted(EXPECTED_PLATFORMS, key=lambda x: x[2]):
        src = dist_dir / filename
        if not src.is_file():
            raise FileNotFoundError(f"missing binary {src}")
        digest = sha256_file(src)
        entries.append((goos, goarch, digest))
        sums_lines.append(f"{digest}  {filename}\n")

    sums_path = dist_dir / "SHA256SUMS"
    sums_path.write_text("".join(sums_lines), encoding="utf-8")

    # Sort map entries by "<GOOS>/<GOARCH>" to match gofmt/existing hashes.go ordering.
    entries.sort(key=lambda e: f"{e[0]}/{e[1]}")
    map_lines = "".join(
        f'\t"{goos}/{goarch}": mustHash("{digest}"),\n'
        for goos, goarch, digest in entries
    )

    hashes_go = (
        "package zstd\n\n"
        'import "gosuda.org/hqq/secureexec"\n\n'
        "// Version follows the v<WrapperMajor>.<WrapperMinor>.<NormalizedCVersion> scheme,\n"
        f"// where {cver} corresponds to ZSTD_VERSION_NUMBER (Zstandard {version}).\n"
        "const (\n"
        f'\tVersion           = "{tag}"\n'
        f"\tZstdVersionNumber = {cver}\n"
        ")\n\n"
        '// ReleaseHashes maps "<GOOS>/<GOARCH>" to the SHA-256 digest of the prebuilt\n'
        "// zstd_server binary published with this module version.\n"
        "var ReleaseHashes = map[string]secureexec.Hash{\n"
        f"{map_lines}"
        "}\n\n"
        "// ExpectedHash returns the pinned SHA-256 digest of zstd_server for goos/goarch.\n"
        "func ExpectedHash(goos, goarch string) (secureexec.Hash, bool) {\n"
        '\th, ok := ReleaseHashes[goos+"/"+goarch]\n'
        "\treturn h, ok\n"
        "}\n\n"
        "func mustHash(s string) secureexec.Hash {\n"
        "\th, err := secureexec.ParseHash(s)\n"
        "\tif err != nil {\n"
        "\t\tpanic(err)\n"
        "\t}\n"
        "\treturn h\n"
        "}\n"
    )
    hashes_path = root / "hashes.go"
    hashes_path.write_text(hashes_go, encoding="utf-8")
    if shutil.which("gofmt"):
        subprocess.run(["gofmt", "-w", str(hashes_path)], check=True)

    bin_dir = root / "internal" / "embedded" / "bin"
    bin_dir.mkdir(parents=True, exist_ok=True)
    for _, _, filename in EXPECTED_PLATFORMS:
        shutil.copy2(dist_dir / filename, bin_dir / filename)

    if update_docs:
        update_documentation(root, tag, version, cver)


def update_documentation(root: Path, tag: str, version: str, cver: int) -> None:
    readme_path = root / "README.md"
    if readme_path.is_file():
        readme = readme_path.read_text(encoding="utf-8")
        readme = re.sub(
            r"(official C `libzstd` \(`)\d+\.\d+\.\d+(`\))",
            rf"\g<1>{version}\2",
            readme,
        )
        readme = re.sub(
            r'(Version\s*=\s*")v\d+\.\d+\.\d+(")',
            rf"\g<1>{tag}\2",
            readme,
        )
        readme = re.sub(
            r"(ZstdVersionNumber\s*=\s*)\d+",
            rf"\g<1>{cver}",
            readme,
        )
        readme = re.sub(
            r"(Zstandard \(`)\d+\.\d+\.\d+(`\))",
            rf"\g<1>{version}\2",
            readme,
        )
        readme_path.write_text(readme, encoding="utf-8")

    arch_path = root / "docs" / "ARCHITECTURE.md"
    if arch_path.is_file():
        arch = arch_path.read_text(encoding="utf-8")
        arch = re.sub(
            r"(Official libzstd )\d+\.\d+\.\d+",
            rf"\g<1>{version}",
            arch,
        )
        arch = re.sub(
            r"(Detail String \(zstd )\d+\.\d+\.\d+(\))",
            rf"\g<1>{version}\2",
            arch,
        )
        arch = re.sub(
            r"(Fetch the pinned Zstandard source \(`v)\d+\.\d+\.\d+(`\))",
            rf"\g<1>{version}\2",
            arch,
        )
        arch_path.write_text(arch, encoding="utf-8")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=None, help="Repository root directory")
    sub = parser.add_subparsers(dest="cmd", required=True)

    sub.add_parser("latest", help="Print the latest upstream facebook/zstd version")

    p_pin = sub.add_parser("pin", help="Pin third_party/zstd to latest or specified version")
    p_pin.add_argument("--version", default="", help="Zstandard version (e.g. 1.5.7); default latest")

    p_tag = sub.add_parser("next-tag", help="Compute the next Go module release tag")
    p_tag.add_argument("--zstd-version", default="", help="Zstandard version (default: pinned VERSION)")

    p_hash = sub.add_parser("update-hashes", help="Update hashes.go and internal/embedded/bin from dist/")
    p_hash.add_argument("--dist", type=Path, required=True, help="Directory with built zstd_server_* binaries")
    p_hash.add_argument("--tag", required=True, help="Release tag (e.g. v0.4.10507)")
    p_hash.add_argument("--zstd-version", default="", help="Zstandard version (default: pinned VERSION)")
    p_hash.add_argument("--update-docs", action="store_true", help="Also update version numbers in README.md and docs/ARCHITECTURE.md")

    args = parser.parse_args(argv)
    root = repo_root(args.root)

    if args.cmd == "latest":
        version, _ = latest_zstd_release()
        print(version)
        return 0

    if args.cmd == "pin":
        ver = args.version.strip() or None
        pinned = pin_version(root, ver)
        print(pinned)
        return 0

    if args.cmd == "next-tag":
        ver = args.zstd_version.strip() or read_pinned_version(root)
        tags = list_git_tags(root)
        print(compute_next_tag(tags, ver))
        return 0

    if args.cmd == "update-hashes":
        ver = args.zstd_version.strip() or read_pinned_version(root)
        update_hashes_and_embedded(root, args.dist, args.tag, ver, update_docs=args.update_docs)
        return 0

    return 2


if __name__ == "__main__":
    raise SystemExit(main())
