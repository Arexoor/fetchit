#!/usr/bin/env python3
"""Exercise source snapshot creation in an isolated Git repository."""
from pathlib import Path
import json
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / 'build/podman6/prepare-source.py'

class Snapshot(unittest.TestCase):
    def test_committed_bytes_manifest_and_refusal(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            source = root / 'repo'
            source.mkdir()
            def git(*args):
                return subprocess.check_output(['git', '-C', str(source), *args], stderr=subprocess.DEVNULL).decode().strip()
            git('init')
            git('config', 'user.name', 'Test')
            git('config', 'user.email', 'test@example.invalid')
            (source / 'tracked.txt').write_text('committed\n')
            git('add', 'tracked.txt')
            git('commit', '-m', 'fixture')
            revision = git('rev-parse', 'HEAD')
            output = root / 'context'
            def prepare(path):
                return subprocess.run(['python3', str(SCRIPT), str(source), str(path), revision], capture_output=True, text=True)
            result = prepare(output)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual((output/'source/tracked.txt').read_text(), 'committed\n')
            self.assertEqual(json.loads((output/'source-manifest.json').read_text())['revision'], revision)
            self.assertNotEqual(prepare(output).returncode, 0, 'Must not overwrite an existing context')
            (source/'tracked.txt').write_text('dirty\n')
            self.assertNotEqual(prepare(root/'dirty-context').returncode, 0, 'Must reject dirty source')

if __name__ == '__main__':
    unittest.main(verbosity=2)
