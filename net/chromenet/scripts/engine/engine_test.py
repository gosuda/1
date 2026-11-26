#!/usr/bin/env python3
import unittest

import engine


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
