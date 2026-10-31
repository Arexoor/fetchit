#!/usr/bin/env python3
"""Opt-in real native helper apply/remove test; no engine/API/Git claim.

Run as a disposable rootless user with an active systemd user manager, Podman
socket, cgroup v2 and SELinux Enforcing. FETCHIT_NATIVE_HELPER_IMAGE must name an
already available compatible helper image. The helper script comes from this
checkout, NOT from that image. No image build, pull, workload start or prune.
"""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
import uuid

ROOT = Path(__file__).resolve().parents[1]


@unittest.skipUnless(os.environ.get('FETCHIT_NATIVE_HELPER_IMAGE'), 'opt-in native host test')
class NativeRootlessHelper(unittest.TestCase):
    def test_inactive_apply_receipt_and_owned_removal(self):
        self.assertNotEqual(os.geteuid(), 0, 'Run as the disposable rootless user')
        home = Path.home()
        parent = Path(os.environ.get('XDG_CONFIG_HOME', str(home / '.config')))
        runtime = os.environ['XDG_RUNTIME_DIR']
        image = os.environ['FETCHIT_NATIVE_HELPER_IMAGE']
        identity = 'fetchit-native-test-' + uuid.uuid4().hex
        unit = identity + '.service'
        live = parent / 'containers/systemd' / identity
        control = parent / ('.' + identity)
        containers = []

        def run(args, check=True):
            p = subprocess.run(args, cwd=home, capture_output=True, text=True, timeout=90)
            if check:
                self.assertEqual(p.returncode, 0, f'{args[0]} failed (output withheld)')
            return p

        self.assertEqual(run(['getenforce']).stdout.strip(), 'Enforcing')
        self.assertEqual(run(['podman', 'info', '--format={{.Host.Security.Rootless}}']).stdout.strip(), 'true')
        self.assertEqual(run(['podman', 'info', '--format={{.Host.CgroupManager}}']).stdout.strip(), 'systemd')
        self.assertEqual(run(['systemctl', '--user', 'is-active', 'podman.socket']).stdout.strip(), 'active')
        run(['podman', 'image', 'exists', image])
        helper = (ROOT / 'pkg/engine/quadlet-host.sh').read_text()
        revision = '1' * 40
        with tempfile.TemporaryDirectory(prefix=identity + '-') as tmp:
            bundle = Path(tmp) / 'quadlet-bundle'
            (bundle / 'files').mkdir(parents=True)
            (bundle / 'files/probe.env').write_text('FETCHIT_PUBLIC_MARKER=native-test\n')
            (bundle / (identity + '.container')).write_text(
                '[Container]\nImage=docker.io/library/alpine:3.22\nPull=never\n'
                'Exec=sleep infinity\nEnvironmentFile=./files/probe.env\n')
            empty = Path(tmp) / 'empty/quadlet-bundle'
            empty.mkdir(parents=True)

            def invoke(removal):
                name = identity + ('-remove' if removal else '-apply')
                cid = run(['podman', 'create', '--pull=never', '--name', name,
                    '--cap-drop=ALL', '--cap-add=SYS_CHROOT', '--cap-add=DAC_OVERRIDE',
                    '--cap-add=FOWNER', '--cap-add=CHOWN', '--security-opt=label=disable',
                    '--pid=host', '-v', '/:/host:ro,rbind,rslave',
                    '-v', f'{parent}:/host{parent}:rw',
                    '--env', f'HOME={home}', '--env', f'XDG_CONFIG_HOME={parent}',
                    '--env', f'XDG_RUNTIME_DIR={runtime}', '--entrypoint', '/bin/sh', image,
                    '-ceu', helper, 'quadlet', str(parent), runtime, identity,
                    unit if removal else '', '' if removal else unit, 'false', '',
                    revision if removal else '0' * 40, revision, 'native-test',
                    str(removal).lower()]).stdout.strip()
                containers.append(cid)
                run(['podman', 'cp', str(empty if removal else bundle), cid + ':/tmp/quadlet-bundle'])
                run(['podman', 'start', '--attach', cid])
                self.assertEqual(run(['podman', 'inspect', '--format={{.State.ExitCode}}', cid]).stdout.strip(), '0')

            try:
                invoke(False)
                self.assertIn('---' + unit + '---', (control / 'validation.log').read_text())
                self.assertEqual((live / 'files/probe.env').read_text(), 'FETCHIT_PUBLIC_MARKER=native-test\n')
                self.assertEqual((control / 'receipt').read_text(), revision + '\nnative-test\n')
                self.assertEqual(run(['systemctl', '--user', 'show', unit, '--property=SourcePath', '--value']).stdout.strip(), str(live / (identity + '.container')))
                self.assertEqual(run(['systemctl', '--user', 'show', unit, '--property=ActiveState', '--value']).stdout.strip(), 'inactive')
                invoke(True)
                self.assertEqual(run(['systemctl', '--user', 'show', unit, '--property=LoadState', '--value'], check=False).stdout.strip(), 'not-found')
            finally:
                for cid in containers:
                    run(['podman', 'rm', '--force', cid])
                for path in (live, control):
                    if path.exists():
                        shutil.rmtree(path)
                run(['systemctl', '--user', 'daemon-reload'])
        self.assertFalse(live.exists())
        self.assertFalse(control.exists())
        self.assertEqual(run(['getenforce']).stdout.strip(), 'Enforcing')


if __name__ == '__main__':
    unittest.main(verbosity=2)
