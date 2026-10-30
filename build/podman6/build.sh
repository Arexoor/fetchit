#!/usr/bin/env bash
set -euo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")"
[[ $(id -u) != 0 ]] || { printf 'Run using rootless Podman.\n' >&2; exit 1; }
: "${FETCHIT_BUILD_IMAGE:=localhost/fetchit:podman6-snapshot-build}"
revision=$(python3 -c 'import json; print(json.load(open("source-manifest.json"))["revision"])')
podman build --pull=missing --layers=false --force-rm=true --build-arg "SOURCE_REVISION=$revision" --iidfile image.id --tag "$FETCHIT_BUILD_IMAGE" --file Containerfile . 2>&1 | tee build.log
podman image inspect "$(<image.id)" > image-inspect.json
