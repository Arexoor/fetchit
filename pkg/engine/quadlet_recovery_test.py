"""Offline host-protocol regressions; fake host commands, no live systemd/Podman."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).with_name('quadlet-host.sh').read_text()
A, B, C = ('1' * 40, '2' * 40, '3' * 40)

class HostRecovery(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.control = self.root / 'etc/containers/.fetchit-test'
        self.control.mkdir(parents=True)
        self.bundle = self.root / 'bundle'
        self.bundle.mkdir()
        (self.bundle / 'app.container').write_text('[Container]\nImage=synthetic\n')
        gen = self.root / 'usr/lib/systemd/system-generators/podman-system-generator'
        gen.parent.mkdir(parents=True)
        gen.touch(mode=0o755)
        bin = self.root / 'bin'
        bin.mkdir()
        chroot = bin / 'chroot'
        chroot.write_text('''#!/bin/sh
set -eu
shift 3
while [ "${1#*=}" != "$1" ]; do export "$1"; shift; done
case "$1" in
podman) echo 'podman version 5.8.7';;
stat) echo cgroup2fs;;
*/podman-system-generator)
 echo SYNTHETIC_PRIVATE_MARKER
 [ "$FAIL" != validate ] || exit 1
 echo '---app.service---';;
systemctl)
 shift
 case "$1" in
 show) case "$*" in *LoadState*) echo loaded;; *SourcePath*) echo /etc/containers/systemd/fetchit-test/app.container;; esac;;
 *) echo "$*" >> "$LOG";;
 esac;;
*) exit 90;;
esac
''')
        chroot.chmod(0o755)
        self.env = dict(os.environ, PATH=str(bin)+':'+os.environ['PATH'], FETCHIT_QUADLET_HOST_ROOT=str(self.root), FETCHIT_QUADLET_BUNDLE=str(self.bundle), LOG=str(self.root/'operations'), FAIL='')

    def run_host(self, current, desired, unchanged='false', mode='apply', token='', old='app.service', new='app.service'):
        return subprocess.run(['sh', '-ceu', SCRIPT, 'quadlet', '/etc/containers', '', 'fetchit-test', old, new, 'true', 'app.service', current, desired, 'config', 'false', unchanged, 'lifecycle', ('c' if desired == C else 'a')*40, mode, token], env=self.env, text=True, capture_output=True)

    def ok(self, result):
        self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
        return result.stdout

    def test_validation_diagnostics_are_private(self):
        (self.bundle/'app.container').write_text('[Container]\nImage=synthetic\nEnvironment=TOKEN=SYNTHETIC_PRIVATE_MARKER\n')
        (self.bundle/'invalid.container').write_text('[Container]\nInvalidSyntheticKey=true\n')
        (self.control/'validation.log').write_text('legacy log')
        (self.control/'validation.log').chmod(0o644)
        for failure in ['validate', 'missing-unit']:
            with self.subTest(failure=failure):
                self.env['FAIL'] = failure
                result = self.run_host(A, B, new='app.service\ninvalid.service')
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn('SYNTHETIC_PRIVATE_MARKER', result.stdout+result.stderr)
                self.assertLess(len(result.stdout+result.stderr), 256)
                log = self.control/'validation.log'
                self.assertIn('SYNTHETIC_PRIVATE_MARKER', log.read_text())
                self.assertEqual(log.stat().st_mode & 0o777, 0o600)
                self.assertEqual(self.control.stat().st_mode & 0o777, 0o700)
                self.assertFalse((self.control/'receipt').exists())

    def test_overlapping_process_cannot_replace_newer_receipt(self):
        (self.control/'receipt').write_text(A+'\nconfig\nlifecycle\n'+'b'*40+'\n')
        token = self.ok(self.run_host(A, B, mode='inspect')).split()[1]
        args = ['sh', '-ceu', SCRIPT, 'quadlet', '/etc/containers', '', 'fetchit-test', 'app.service', 'app.service', 'true', 'app.service', A, B, 'config', 'false', 'true', 'lifecycle', 'a'*40, 'apply', token]
        # A second real process has selected its old plan, but has not acquired
        # the host lock yet. Let the newer operation finish before releasing it.
        old = subprocess.Popen(['sh', '-c', 'echo ready; read gate; exec "$@"', 'gate']+args, env=self.env, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        self.assertEqual(old.stdout.readline().strip(), 'ready')
        self.ok(self.run_host(A, C))
        out, err = old.communicate('go\n', timeout=10)
        self.assertNotEqual(old.returncode, 0)
        self.assertIn('Stale Quadlet snapshot', err)
        self.assertTrue((self.control/'receipt').read_text().startswith(C+'\n'))

    def test_crash_receipt_ahead_recovery_and_stale_snapshot(self):
        for receipt_only, pending in [(True, False), (False, False), (False, True)]:
            with self.subTest(receipt_only=receipt_only, pending=pending):
                (self.control/'receipt').write_text(A+'\nconfig\nlifecycle\n'+'b'*40+'\n')
                # Old overlapping controller captured A before B completed.
                stale = self.ok(self.run_host(A, C, mode='inspect')).split()[1]
                self.ok(self.run_host(A, B, unchanged=str(receipt_only).lower()))
                if pending:
                    (self.control/'pending').write_text('removed-at-c.service\n')
                # Controller crashes before updating its Git tag A (or after deleting it).
                snapshot = self.ok(self.run_host(A, C, mode='inspect')).split()
                self.assertEqual(snapshot[2], B)
                token = snapshot[1]
                self.assertNotEqual(token, stale)
                # Fresh controller uses actual B's previous service list, not A's.
                self.ok(self.run_host(B, C, token=token, old='app.service\nremoved-at-c.service'))
                self.assertTrue((self.control/'receipt').read_text().startswith(C+'\n'))
                self.assertIn('stop removed-at-c.service', (self.root/'operations').read_text())
                self.assertFalse((self.control/'pending').exists())
                rejected = self.run_host(A, B, token=stale)
                self.assertNotEqual(rejected.returncode, 0)
                self.assertIn('Stale Quadlet snapshot', rejected.stderr)

if __name__ == '__main__':
    unittest.main()
