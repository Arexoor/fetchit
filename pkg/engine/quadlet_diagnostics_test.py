"""Executable private diagnostics contract; no live application changes."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

HERE = Path(__file__).parent

class Diagnostics(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.parent = self.root / 'config'
        self.parent.mkdir()
        self.log = self.parent / 'fetchit/quadlet.log'

    def run_helper(self, script, operation='apply'):
        wrapper = HERE / 'quadlet-diagnostics.sh'
        command = ['sh', '-ceu', script]
        if wrapper.exists():
            command = ['sh', '-eu', str(wrapper), str(self.parent), operation, 'test-group', 'revision-test'] + command
        return subprocess.run(command, env=dict(os.environ, FETCHIT_QUADLET_HOST_ROOT='/'), text=True, capture_output=True)

    def test_failure_before_generator_is_durable_and_private(self):
        result = self.run_helper("echo PRIVATE_EARLY_FAILURE >&2; exit 23")
        self.assertEqual(result.returncode, 23)
        self.assertTrue(self.log.exists(), 'helper stderr discarded: no durable central diagnostic log')
        self.assertIn('PRIVATE_EARLY_FAILURE', self.log.read_text())
        self.assertIn('exit=23', self.log.read_text())
        self.assertNotIn('PRIVATE_EARLY_FAILURE', result.stdout + result.stderr)
        self.assertEqual(self.log.stat().st_mode & 0o777, 0o600)
        self.assertEqual(self.log.parent.stat().st_mode & 0o777, 0o700)

    def test_inspection_preserves_receipt_and_previous_failure(self):
        self.run_helper('echo PRIVATE_START_FAILURE >&2; exit 7')
        result = self.run_helper("printf 'FETCHIT_RECEIPT absent\\n'; echo PRIVATE_INSPECT >&2", 'inspect')
        self.assertEqual(result.returncode, 0)
        self.assertEqual(result.stdout, 'FETCHIT_RECEIPT absent\n')
        self.assertNotIn('PRIVATE_', result.stderr)
        self.assertIn('PRIVATE_START_FAILURE', self.log.read_text())
        self.assertIn('PRIVATE_INSPECT', self.log.read_text())

    def test_symlinks_are_rejected_without_modifying_target(self):
        target = self.root / 'untouched'
        target.write_text('unchanged')
        self.log.parent.mkdir()
        self.log.symlink_to(target)
        result = self.run_helper('echo SHOULD_NOT_RUN')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('FETCHIT_DIAGNOSTICS_UNAVAILABLE', result.stdout + result.stderr)
        self.assertEqual(target.read_text(), 'unchanged')

    def test_previous_output_is_bounded_and_existing_directory_mode_preserved(self):
        self.log.parent.mkdir(mode=0o755)
        self.log.write_text('x' * (2 << 20))
        result = self.run_helper('echo NEW_RUN')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertLess(self.log.stat().st_size, (1 << 20) + 1024)
        self.assertIn('NEW_RUN', self.log.read_text())
        self.assertEqual(self.log.parent.stat().st_mode & 0o777, 0o755)

    def test_host_failures_before_generator_and_after_successful_validation(self):
        from quadlet_recovery_test import HostRecovery, A, B, SCRIPT
        for phase in ['early', 'start']:
            with self.subTest(phase=phase):
                fixture = HostRecovery()
                fixture.setUp()
                try:
                    chroot = fixture.root / 'bin/chroot'
                    original = chroot.read_text()
                    if phase == 'early':
                        chroot.write_text('#!/bin/sh\necho PRIVATE_EARLY_COMMAND >&2\nexit 29\n')
                    else:
                        chroot.write_text(original.replace('*) echo "$*" >> "$LOG";;', '*) echo "$*" >> "$LOG"; if [ "$1" = restart ]; then echo PRIVATE_SYSTEMCTL_START >&2; exit 31; fi;;'))
                    args = ['sh', '-ceu', SCRIPT, 'quadlet', '/etc/containers', '', 'fetchit-test', 'app.service', 'app.service', 'true', 'app.service', A, B, 'config']
                    wrapped = ['sh', '-eu', str(HERE/'quadlet-diagnostics.sh'), '/etc/containers', 'apply', 'fetchit-parent', B] + args
                    result = subprocess.run(wrapped, env=fixture.env, capture_output=True, text=True)
                    self.assertNotEqual(result.returncode, 0)
                    log = fixture.root / 'etc/containers/fetchit/quadlet.log'
                    data = log.read_text()
                    self.assertIn('PRIVATE_EARLY_COMMAND' if phase == 'early' else 'PRIVATE_SYSTEMCTL_START', data)
                    self.assertIn('group=fetchit-test operation=apply', data)
                    self.assertNotIn('PRIVATE_', result.stdout + result.stderr)
                    if phase == 'start':
                        self.assertIn('SYNTHETIC_PRIVATE_MARKER', data)
                finally:
                    fixture.doCleanups()

if __name__ == '__main__':
    unittest.main()
