#!/usr/bin/env python3
"""Bounded image smoke checks; never starts the FetchIt controller."""
import hashlib
import json
import os
from pathlib import Path
import struct
import subprocess

base = Path(__file__).resolve().parent
image = os.environ.get('FETCHIT_VERIFY_IMAGE') or (base / 'image.id').read_text().strip()
assert os.getuid() != 0, 'Run with rootless Podman'
manifest = json.loads((base / 'source-manifest.json').read_text())
checks = []
with (base / 'verify.log').open('w') as log:
    def run(args, expected=0):
        log.write('$ ' + ' '.join(args) + '\n')
        result = subprocess.run(args, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=120)
        log.write(result.stdout + '\nexit=' + str(result.returncode) + '\n')
        log.flush()
        print(result.stdout, end='', flush=True)
        assert result.returncode == expected, (args, result.returncode)
        checks.append({'command': args, 'exit_code': result.returncode})
        return result.stdout
    inspect = json.loads(run(['podman', 'image', 'inspect', image]))[0]
    (base / 'image-inspect.json').write_text(json.dumps([inspect], indent=2) + '\n')
    image_id = inspect['Id']
    def container(entry, *args):
        return run(['podman', 'run', '--rm', '--pull=never', '--network=none', '--entrypoint', entry, image_id, *args])
    assert 'fetchit version 0.0.0' in container('/usr/local/bin/fetchit', '--version')
    assert 'start' in container('/usr/local/bin/fetchit', '--help')
    container('/usr/local/bin/fetchit', 'start', '--help')
    container('/usr/local/bin/fetchit', 'host-artifact', '--help')
    container('/usr/local/bin/sops', '--version')
    container('/bin/bash', '-euc', 'test -x /usr/local/bin/fetchit; test -x /usr/local/bin/entry.sh; test -x /usr/local/bin/sops; for cmd in rsync tar chroot flock find grep sed sort stat cp env; do command -v "$cmd"; done; rpm -q rsync tar coreutils-single util-linux findutils grep sed device-mapper-libs libseccomp; sha256sum /usr/local/bin/fetchit /usr/local/bin/sops /usr/local/bin/entry.sh')
    name = 'fetchit-build-extract-podman6'
    run(['podman', 'create', '--name', name, '--pull=never', '--network=none', '--entrypoint', '/usr/local/bin/fetchit', image_id, '--version'])
    try:
        run(['podman', 'cp', name + ':/usr/local/bin/fetchit', str(base / 'fetchit')])
    finally:
        run(['podman', 'rm', name])
    binary = (base / 'fetchit').read_bytes()
    script = (base / 'source/pkg/engine/quadlet-host.sh').read_bytes()
    assert binary.count(script) == 1, 'Exact patched script must be embedded once'
    assert b'Experimental FetchIt test build: allowing host Podman 6' in binary
    assert binary[:5] == b'\x7fELF\x02', 'Expected ELF64 binary'
    endian = '<' if binary[5] == 1 else '>'
    phoff = struct.unpack_from(endian + 'Q', binary, 32)[0]
    phentsize, phnum = struct.unpack_from(endian + 'HH', binary, 54)
    types = [struct.unpack_from(endian + 'I', binary, phoff + i * phentsize)[0] for i in range(phnum)]
    assert 3 not in types, 'Static binary must not have PT_INTERP'
    result = {
        'image_id': image_id,
        'image_digest': inspect.get('Digest'),
        'image_size_bytes': inspect.get('Size'),
        'source_revision': manifest['revision'],
        'source_modified_from_upstream': ['pkg/engine/quadlet-host.sh'],
        'fetchit_sha256': hashlib.sha256(binary).hexdigest(),
        'embedded_script_sha256': hashlib.sha256(script).hexdigest(),
        'embedded_script_exact_occurrences': binary.count(script),
        'static_elf_no_interpreter': True,
        'checks': checks,
    }
    (base / 'verification.json').write_text(json.dumps(result, indent=2) + '\n')
    log.write(json.dumps(result, indent=2) + '\n')
    print(json.dumps({k:v for k,v in result.items() if k != 'checks'}, indent=2))
