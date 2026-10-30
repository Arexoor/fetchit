#!/usr/bin/env python3
"""Create a new context from a clean, exact committed checkout (no credentials)."""
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import sys

source, base, revision = Path(sys.argv[1]).resolve(), Path(sys.argv[2]).resolve(), sys.argv[3]
def git(*args):
    return subprocess.check_output(['git', '-C', str(source), *args])
assert git('rev-parse', 'HEAD').decode().strip() == revision, 'Expected exact HEAD commit'
assert not git('status', '--porcelain'), 'Refusing dirty/untracked source'
assert not base.exists(), 'Use a new build context; never overwrite an old build'
assert not base.is_relative_to(source), 'Build outside the source checkout'
base.mkdir(parents=True)
hashes = {}
for filename in git('ls-files', '-z').decode().split('\0'):
    if not filename:
        continue
    target = base / 'source' / filename
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_bytes(git('show', revision + ':' + filename))
    target.chmod((source / filename).stat().st_mode & 0o777)
    hashes[filename] = hashlib.sha256(target.read_bytes()).hexdigest()
for name in ('Containerfile', '.containerignore', 'build.sh', 'verify.py'):
    shutil.copy2(Path(__file__).parent / name, base / name)
(base / 'source-manifest.json').write_text(json.dumps({'revision': revision, 'files_sha256': hashes}, indent=2) + '\n')
print('Prepared committed source:', revision, 'files:', len(hashes))
