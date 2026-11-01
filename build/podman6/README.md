# Experimental Podman 6 snapshot build

This fork owns the experimental source, build recipes and portable development tests.
Upstream: https://github.com/containers/fetchit, base commit
`2089acc2efeeccb44dfb4549fab3bff1f7cb815d`. This branch includes experimental
Podman 6 gating, folder-scoped reconciliation, batched lifecycle operations, and
conditional generic helper prefetch. This is **not official Podman 6 support**
or a Go-bindings port; the bindings remain on major 5.

## Build (Linux amd64, rootless Podman)

Use a clean checkout at the exact experimental release commit. Python 3 and Git
prepare a new context outside the checkout. No local credentials, `.git`, VM
reports or ignored files are copied. Set the context to a location accessible to
the rootless build user; when preparing as another user, transfer its ownership
before building. Do not reuse the live controller's tag or context.

```sh
python3 tests/test_podman_version_gate.py
python3 tests/test_prepare_podman6_source.py
python3 build/podman6/prepare-source.py "$PWD" "$HOME/fetchit-snapshot-build" "$(git rev-parse HEAD)"
bash "$HOME/fetchit-snapshot-build/build.sh"
python3 "$HOME/fetchit-snapshot-build/verify.py"
```

`FETCHIT_BUILD_IMAGE` overrides the isolated default build tag.
The verifier uses `image.id`, or an explicit `FETCHIT_VERIFY_IMAGE` for source
comparison against a prior image. It never starts the controller: ephemeral
network-disabled smoke containers check CLI/help, SOPS and runtime utilities;
it extracts the binary, verifies the full embedded helper bytes and static ELF.
Verification output describes the selected image and source snapshot, not an
assertion that an older image was compiled from the newer documentation commit.

The recipe retains the tested pinned Go 1.27.1 Alpine and UBI9 minimal bases,
SOPS 3.13.3 checksum, upstream build tags, entrypoint and runtime packages.
It uses direct `go build -mod=readonly` rather than the upstream Makefile's
absent vendored build machinery. `CGO_ENABLED=0`, linux/amd64, bounded compiler
parallelism/memory and `-buildvcs=false -trimpath` are intentional. Native
CGO-backed features are excluded. FetchIt's version still prints upstream's
`0.0.0`; use Git commit and image ID for provenance. RPM repositories are not
frozen: a new image ID is expected, not bit-for-bit reproducibility.

Local checks are not a substitute for full upstream CI or host integration.
The version-gate test stubs only the version input and executes the actual shell
gate; it makes no host/API compatibility claim. Existing upstream Go and
integration suites remain in this repository. Before promoting a new build,
exercise rootless Podman, cgroup v2, SELinux Enforcing, real Quadlet generation,
apply/remove, engine API/Git reconciliation, no-op, invalid-candidate retention,
offline recovery and reboot behavior in a disposable environment. Keep machine
addresses, credentials and deployment evidence outside this public fork.

## Optional native rootless helper regression

The reusable `tests/test_native_rootless_helper.py` covers native helper
apply, generator output, SourcePath ownership, inactive state, receipt and
removal with a random owned namespace. It replaces a deployment-local,
hardcoded experiment; no deployment config, VM addresses, credentials or
private evidence are included. Existing version-gate/snapshot tests already
cover the portable baseline/build aspects, so those were not duplicated.

On a disposable host, log in as the rootless test user (not root), with HOME,
XDG_RUNTIME_DIR and the user bus configured. Require cgroup v2, active user
Podman socket, SELinux Enforcing and a preloaded compatible helper image:

```sh
FETCHIT_NATIVE_HELPER_IMAGE=<preloaded-image-reference> python3 tests/test_native_rootless_helper.py
```

The test executes this checkout's helper script inside that image. It does not
build/pull/start workloads or exercise the Go engine/API/Git path. It creates
short-lived administration helper containers (per-helper label disable,
host mounts), installs an inactive random Quadlet, then removes only its owned
containers/paths and reloads the user manager. Do not run on a production host.
Without the environment opt-in it skips. Outputs omit shared host inventories.
Run portable tests using `python3 -m unittest discover -s tests -p 'test_*.py' -v`.

## Runtime caveats

Set Quadlet `helperImage` to the same reviewed version tag as the engine.
Quadlet resolves/pulls that helper only when deploying (cached images are reused).
Empty or online Quadlet-only configurations no longer prefetch the generic
`quay.io/fetchit/fetchit:latest` image. Leaving `helperImage` unset still selects
that upstream default at deployment; no local alias is required for explicit
helpers. Legacy FileTransfer/Systemd and device-backed paths retain generic
helper prefetch. An explicit Quadlet helper does not override those legacy paths.
A GitHub source release is not a registry publication. Reboot and full target
FCOS acceptance remain separate from local tests.

## Future upstream integration and release workflow

Never move or force-update a published tag. Keep the tested snapshot independent
of newer upstream commits. Preserve existing fork branches and work.

```sh
git remote add upstream https://github.com/containers/fetchit.git # once only
git fetch upstream --tags
git switch -c integration/podman6-next experimental/podman6-snapshot
git merge --no-ff upstream/main
# Resolve deliberately; review API/gate changes, dependency and security changes.
# Run unit/Go tests, rebuild, verify, then disposable-host acceptance above.
# Record exact upstream base, merge commit, image digest and honest limitations.
git push -u origin integration/podman6-next
# After review and validation, choose a NEW unique experimental version:
git tag -a <new-experimental-tag> <tested-commit> -m 'Experimental tested snapshot'
git push origin refs/tags/<new-experimental-tag>
# Create GitHub release with prerelease=true, draft=false, make_latest=false.
# GET the release and tag, dereference annotated tags, verify exact commit.
```

Do not merge upstream into a release just because it is newer; no forced push,
tag replacement, automatic promotion, or silent deployment pin change.
