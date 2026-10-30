#!/usr/bin/env python3
"""Unit-test the actual shell gate; stub version only, no host integration claim."""
from pathlib import Path
import subprocess
import unittest

ROOT = Path(__file__).resolve().parents[1]

class VersionGate(unittest.TestCase):
    def test_supported_and_rejected_versions(self):
        helper = (ROOT / 'pkg/engine/quadlet-host.sh').read_text()
        gate = helper.split('version=$(hostcmd podman --version)', 1)[1].split('[ "$(hostcmd stat', 1)[0]
        for version, accepted in [('5.6.2', False), ('5.7.0', True), ('5.10.0', True),
                                  ('6.0.0', True), ('6.2.0-rc1', True), ('7.0.0', False),
                                  ('4.9.0', False), ('unknown', False)]:
            with self.subTest(version=version):
                result = subprocess.run(['sh', '-eu', '-c',
                    'fail() { printf "%s\\n" "$*" >&2; exit 1; }; version="$1";\n' + gate,
                    'gate-test', 'podman version ' + version], text=True, capture_output=True)
                self.assertEqual(result.returncode == 0, accepted, result.stderr)
                if accepted and version.startswith('6.'):
                    self.assertIn('Experimental FetchIt test build', result.stderr)

if __name__ == '__main__':
    unittest.main(verbosity=2)
