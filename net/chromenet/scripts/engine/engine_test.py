#!/usr/bin/env python3
import argparse
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import engine


class ToolchainTest(unittest.TestCase):
    def source(self, root: Path, *, mold: bool) -> Path:
        src = root / 'src'
        (src / 'tools/clang/scripts').mkdir(parents=True)
        (src / 'tools/rust').mkdir(parents=True)
        (src / 'tools/clang/scripts/update.py').write_text(
            "CLANG_REVISION = 'clang-fixture'\nCLANG_SUB_REVISION = 1\n")
        (src / 'tools/rust/update_rust.py').write_text(
            "RUST_REVISION = 'rust-fixture'\nRUST_SUB_REVISION = 1\n")
        deps = "'gn_version': 'gn-fixture',\n'ninja_version': 'ninja-fixture',\n"
        if mold:
            deps += ("'package': 'chromium/buildtools/third_party/mold/mold',\n"
                     "'version': 'mold-fixture',\n")
        (src / 'DEPS').write_text(deps)
        return src

    def test_optional_mold_on_both_hosts(self) -> None:
        for has_mold in (False, True):
            for host, target in ((engine.LINUX_HOST, 'linux-amd64'),
                                 (engine.MAC_HOST, 'darwin-arm64')):
                with self.subTest(mold=has_mold, host=host), tempfile.TemporaryDirectory() as work:
                    src = self.source(Path(work), mold=has_mold)
                    revs = engine.source_revisions(str(src))
                    self.assertEqual(revs['mold'], 'mold-fixture' if has_mold else None)
                    expected = has_mold and host == engine.LINUX_HOST
                    archives = engine.toolchain_files(revs, [host], with_win=False)
                    self.assertEqual(any(name.startswith('cipd/mold/') for name, _ in archives), expected)
                    with patch.object(engine, 'host_of', return_value=host), \
                            patch.object(engine, 'read_pins', return_value={}), \
                            patch.object(engine, 'fetch_pinned'), \
                            patch.object(engine.shutil, 'copyfile'), \
                            patch.object(engine, 'run'), \
                            patch.object(engine, 'unzip') as unzip:
                        engine.cmd_toolchain(argparse.Namespace(work=work, targets=target))
                    self.assertEqual(
                        any('cipd/mold/' in call.args[0] for call in unzip.call_args_list),
                        expected,
                    )


class ReleaseTagTest(unittest.TestCase):
    def test_encode_chromium_version(self):
        self.assertEqual(engine.encode_chromium_version('157.0.8103.78'), '101570810307800')
        self.assertEqual(engine.encode_chromium_version('157.0.8103.105'), '101570810310500')
        self.assertEqual(engine.encode_chromium_version('158.0.10000.1'), '101581000000100')
        self.assertEqual(engine.encode_chromium_version('157.0.8103.78', rev=2), '101570810307802')

    def test_monotonic_semver_ordering(self):
        versions = [
            '157.0.8103.78',
            '157.0.8103.99',
            '157.0.8103.100',
            '157.0.8103.250',
            '157.0.8104.0',
            '157.0.10000.0',
            '158.0.8200.1',
        ]
        encoded = [int(engine.encode_chromium_version(v)) for v in versions]
        # Must exceed the existing 14-digit v0.2.10157081033800 tag.
        self.assertGreater(encoded[0], 10157081033800)
        self.assertEqual(encoded, sorted(encoded))
        self.assertEqual(len(set(encoded)), len(encoded))

    def test_next_release_tag(self):
        tags = ['v0.1.10157081033800', 'v0.2.10157081033800']
        self.assertEqual(
            engine.next_release_tag('157.0.8103.78', tags),
            'v0.2.101570810307800',
        )
        self.assertEqual(
            engine.next_release_tag('157.0.8103.78', tags + ['v0.2.101570810307800']),
            'v0.2.101570810307801',
        )
        self.assertEqual(
            engine.next_release_tag('157.0.8103.105', tags + ['v0.2.101570810307801', 'v0.3.101570810307800']),
            'v0.3.101570810310500',
        )
        self.assertEqual(engine.next_release_tag('157.0.8103.78', []), 'v0.1.101570810307800')

    def test_invalid_inputs(self):
        for bad in ('157.1.8103.78', '157.0.8103', '157.0.8103.1000', '157.0.100000.1'):
            with self.assertRaises(SystemExit):
                engine.encode_chromium_version(bad)
        with self.assertRaises(SystemExit):
            engine.next_release_tag('157.0.8103.78', ['v0.2.101580820000100'])


if __name__ == '__main__':
    unittest.main()
