#!/usr/bin/env python3
"""Unit tests for scripts/release/zstd-release.py."""

from __future__ import annotations

import hashlib
import importlib.util
import io
from pathlib import Path
import tarfile
import tempfile
import unittest

SCRIPT_PATH = Path(__file__).resolve().parent / "zstd-release.py"
SPEC = importlib.util.spec_from_file_location("zstd_release", SCRIPT_PATH)
assert SPEC is not None and SPEC.loader is not None
zstd_release = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(zstd_release)


def make_zstd_tarball(
    version: str,
    license_text: str,
    header_text: str,
    extra_members: list[tuple[str, bytes]] | None = None,
) -> bytes:
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz") as tf:
        files = [
            (f"zstd-{version}/LICENSE", license_text.encode("utf-8")),
            (f"zstd-{version}/lib/zstd.h", header_text.encode("utf-8")),
        ]
        if extra_members:
            files.extend(extra_members)
        for name, data in files:
            info = tarfile.TarInfo(name=name)
            info.size = len(data)
            tf.addfile(info, io.BytesIO(data))
    return buf.getvalue()


VALID_LICENSE = (
    "BSD License\n\n"
    "Redistributions of source code must retain the above copyright notice, this\n"
    "list of conditions and the following disclaimer.\n\n"
    "Neither the name Facebook, nor Meta Platforms, Inc. nor the names of its\n"
    "contributors may be used to endorse or promote products derived from this\n"
    "software without specific prior written permission.\n"
)

VALID_HEADER_158 = (
    "#define ZSTD_VERSION_MAJOR 1\n"
    "#define ZSTD_VERSION_MINOR 5\n"
    "#define ZSTD_VERSION_RELEASE 8\n"
)


class ZstdReleaseTests(unittest.TestCase):
    def test_zstd_version_number(self) -> None:
        self.assertEqual(zstd_release.zstd_version_number("1.5.7"), 10507)
        self.assertEqual(zstd_release.zstd_version_number("v1.5.8"), 10508)
        self.assertEqual(zstd_release.zstd_version_number("2.0.1"), 20001)

    def test_compute_next_tag(self) -> None:
        tags = ["v0.1.10507", "v0.2.10507", "v0.3.10507"]
        self.assertEqual(zstd_release.compute_next_tag(tags, "1.5.7"), "v0.4.10507")
        self.assertEqual(zstd_release.compute_next_tag(tags, "1.5.8"), "v0.3.10508")
        self.assertEqual(zstd_release.compute_next_tag(tags + ["v0.3.10508"], "1.5.8"), "v0.4.10508")
        self.assertEqual(zstd_release.compute_next_tag([], "1.5.7"), "v0.1.10507")

    def test_verify_and_extract_archive(self) -> None:
        arc = make_zstd_tarball("1.5.8", VALID_LICENSE, VALID_HEADER_158)
        digest, lic = zstd_release.verify_and_extract_archive(arc, "1.5.8")
        self.assertEqual(digest, hashlib.sha256(arc).hexdigest())
        self.assertEqual(lic, VALID_LICENSE)

    def test_verify_rejects_traversal_and_bad_version(self) -> None:
        arc = make_zstd_tarball(
            "1.5.8",
            VALID_LICENSE,
            VALID_HEADER_158,
            extra_members=[("zstd-1.5.8/../../evil", b"x")],
        )
        with self.assertRaises(ValueError):
            zstd_release.verify_and_extract_archive(arc, "1.5.8")

        wrong_hdr = make_zstd_tarball(
            "1.5.8",
            VALID_LICENSE,
            "#define ZSTD_VERSION_MAJOR 1\n#define ZSTD_VERSION_MINOR 5\n#define ZSTD_VERSION_RELEASE 7\n",
        )
        with self.assertRaises(ValueError):
            zstd_release.verify_and_extract_archive(wrong_hdr, "1.5.8")

    def test_update_hashes_and_docs(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp) / "repo"
            dist = Path(tmp) / "dist"
            (root / "third_party" / "zstd").mkdir(parents=True)
            (root / "docs").mkdir(parents=True)
            dist.mkdir(parents=True)

            (root / "third_party" / "zstd" / "VERSION").write_text(
                "version=1.5.8\narchive=zstd-1.5.8.tar.gz\nurl=https://example.invalid/zstd-1.5.8.tar.gz\n",
                encoding="utf-8",
            )
            (root / "README.md").write_text(
                "official C `libzstd` (`1.5.7`)\n\tVersion           = \"v0.3.10507\"\n\tZstdVersionNumber = 10507\nZstandard (`1.5.7`)\n",
                encoding="utf-8",
            )
            (root / "docs" / "ARCHITECTURE.md").write_text(
                'ZSTD["Official libzstd 1.5.7"]\nDetail String (zstd 1.5.7)\nCalling the identical `libzstd` `1.5.7` C implementation\nFetch the pinned Zstandard source (`v1.5.7`)\n',
                encoding="utf-8",
            )

            for _, _, fname in zstd_release.EXPECTED_PLATFORMS:
                (dist / fname).write_bytes(fname.encode("utf-8"))

            zstd_release.update_hashes_and_embedded(root, dist, "v0.3.10508", "1.5.8", update_docs=True)

            hashes_text = (root / "hashes.go").read_text(encoding="utf-8")
            self.assertIn('Version           = "v0.3.10508"', hashes_text)
            self.assertIn("ZstdVersionNumber = 10508", hashes_text)
            self.assertIn("(Zstandard 1.5.8)", hashes_text)
            for _, _, fname in zstd_release.EXPECTED_PLATFORMS:
                self.assertTrue((root / "internal" / "embedded" / "bin" / fname).is_file())

            readme_text = (root / "README.md").read_text(encoding="utf-8")
            self.assertIn("`1.5.8`", readme_text)
            self.assertIn('"v0.3.10508"', readme_text)
            self.assertIn("10508", readme_text)

            arch_text = (root / "docs" / "ARCHITECTURE.md").read_text(encoding="utf-8")
            self.assertIn("Official libzstd 1.5.8", arch_text)
            self.assertIn("Detail String (zstd 1.5.8)", arch_text)
            self.assertIn("(`v1.5.8`)", arch_text)
            self.assertIn("identical `libzstd` `1.5.7` C implementation", arch_text)


if __name__ == "__main__":
    unittest.main()
