Quadlet
=======

The ``quadlet`` method deploys a complete Git directory into the host's Quadlet
search path. The host's installed Podman generator creates systemd services.
FetchIt validates the bundle before changing live files, reloads systemd once per
batch, and optionally starts or restarts workloads.

Use Podman **5.7 or newer within major version 5**, cgroup v2, and a running host
systemd manager. The helper uses the host's generator and binaries, so it validates
against the installed Podman rather than FetchIt's Go library version. Podman 6
is not supported by this method.

Quadlet configuration
---------------------

.. code-block:: yaml

   targetConfigs:
     - name: application
       url: https://github.com/your-org/application.git
       branch: main
       quadlet:
         - name: web
           schedule: "*/1 * * * *"
           targetPath: deploy/quadlet
           root: true
           start: true
           restart: true

``name`` and ``schedule`` follow the normal method configuration. ``targetPath``
must select a dedicated directory containing the complete bundle. Quadlet source
units belong at the bundle root; supporting files and drop-ins may be nested.
Bundles are limited to 32 MiB of file contents and 4,096 files; exceeding a limit
returns an error before allocating the file contents or starting a helper. Keep
large build contexts in a separate build pipeline or reduce the selected bundle.

``glob`` is deliberately unsupported because filtering can omit dependencies.
Symlinks, uninstantiated templates, and duplicate generated service names are
rejected. Concrete instance files such as ``web@blue.container`` are allowed.

``root`` defaults to false. ``start`` defaults to false; set it to true to start
desired services after deployment. ``restart`` defaults to false and implies ``start``; it restarts
workload services whenever the selected bundle changes, including supporting-file
changes. Network and volume units are started, rather than restarted, to preserve
resources used by running containers. Changing the configuration of an existing
network or volume may require a new resource/unit name and an explicit migration;
FetchIt does not migrate resource contents.

The generator recognizes ``.container``, ``.network``, ``.volume``, ``.pod``,
``.kube``, ``.image``, ``.build``, and ``.artifact``. FetchIt uses Podman's naming
helpers, including ``ServiceName=`` overrides and overrides in drop-ins. The
integration suite exercises container/network/volume bundles; test advanced
resource types and their inputs on your host before relying on them. FetchIt
copies all supporting files, including relative YAML, build contexts, environment
files, and ``.conf`` drop-ins. Absolute references still refer to host paths.

Boot activation comes from the source unit's ``[Install]`` section, for example
``WantedBy=multi-user.target`` for a system service or ``WantedBy=default.target``
for a user service. FetchIt does not run ``systemctl enable`` for generated units.
``start: false`` does not remove boot activation specified by ``[Install]``.

Automatic subgroup discovery
----------------------------

Set ``scanDepth`` on a **Quadlet method**, not on the Git target or other method
kinds. The default ``0`` retains exact-directory behavior, including source units
at ``targetPath`` itself. A positive depth instead treats ``targetPath`` as a
parent and discovers independent groups on **every scheduled Git poll**:

.. code-block:: yaml

   targetConfigs:
     - name: applications
       url: https://github.com/your-org/homelab.git
       branch: main
       quadlet:
         - name: applications
           schedule: "*/1 * * * *"
           targetPath: services
           scanDepth: 1
           root: true
           start: true
           restart: true
           cleanupOnRemoval: true

The example's directory name is arbitrary: ``targetPath: service`` selects
``service`` exactly; FetchIt does not pluralize or otherwise rename it. For
rootless operation replace ``root: true`` with the explicit host paths below.
No per-child configuration entries are created.

* ``scanDepth: 1`` examines direct child directories only. A child qualifies only
  when it contains a **direct** ``.container``, ``.volume`` or ``.network`` file.
  ``services/a/a.container``
  selects ``services/a``; ``services/a/deep/a.container`` does not select it.
* ``scanDepth: N`` considers depths 1 through N inclusive, counting directory
  edges below the parent. The parent itself is never selected in scanning mode.
  The value must be a nonnegative YAML integer representable by the controller;
  booleans, quoted numbers, fractional values and negative values are rejected.
  There is no arbitrary depth cap; traversal is iterative.
* Stop descending when a group root is found, so groups never overlap. All of
  that group's recursive content remains part of its bundle tree. A nested
  Quadlet source unit inside an already selected group is still a validation
  error, not a second group or silently ignored file.
* Resource-only directories containing direct ``.volume`` or ``.network`` files
  are groups. Config/drop-in-only directories and other unit extensions alone
  do not qualify. Removing the final direct **recognized unit** (``.container``,
  ``.volume`` or ``.network``) retires the whole group, even if auxiliary files
  remain. Removing only the last container cleans up that container but keeps
  remaining resource definitions. Removing the directory also retires the group.
  Owned runtime units and deployed supporting files are cleaned up; persistent
  data volumes and networks are not deleted by FetchIt. Resources remain
  start-only, not restartable.
* An empty/missing parent is idle, not disabled: subsequent polls discover new
  groups automatically. Files outside selected groups, including a parent
  README, do not restart groups. Recursive config, drop-in, executable-mode or
  encrypted-credential changes restart only the affected group when enabled.

Each poll fetches one desired Git revision. Deterministic, bounded child names
include a digest of the parent identity and child path. Folder renames are
remove/add operations, never implicit ownership or credential-namespace
migrations. Generated systemd service names and container names must still be
unique on the host: separate ownership directories are not systemd namespaces.
Groups must not rely on cross-group restart ordering. Retired groups are removed
first; desired groups are applied in alphabetical path order. You may explicitly
name a resource directory ``00-resources`` to apply it before application groups;
FetchIt never adds prefixes or constructs a dependency graph.

Discovery does **not** add cross-group Quadlet file-reference support. Every
candidate is generator-validated separately with ``QUADLET_UNIT_DIRS`` restricted
to its staged bundle, before any group is applied. Thus ``Volume=shared.volume:/data``
or ``Network=shared.network`` in another group fails preflight, including on first
boot; alphabetical application cannot fix this. Keep native file references and
resource definitions in the same group. Alternatively, explicitly name resources
with ``VolumeName=`` / ``NetworkName=`` and use those literal runtime names in
consumers (without the ``.volume`` / ``.network`` suffix). This avoids generator
file lookup but does not create systemd dependency edges or guarantee resource
availability after reboot. Boot ordering, readiness, and coordinated changes
remain the operator's responsibility.

The host keeps a private parent inventory and per-child ownership receipts.
The parent inventory is read before fetching Git, then checked under a parent
lock. A write-ahead inventory records all old/new child paths before applying
any group; a unique transaction generation prevents stale/overlapping polls
from adopting later partial work. All candidates are parsed and generator-tested
before any retiring group is removed. Group operations reuse the normal
ownership checks, receipt CAS, recursive tree comparison and batched lifecycle
operations. The parent's applied revision advances only after all groups succeed.
If activation fails midway, already successful groups can remain updated; the
next poll resumes from their receipts, including after controller recreation.
This is **not** an atomic application transaction across different groups.

Keep the Git object cache and host control state for recovery. Missing historical
objects or invalid receipts fail closed rather than guessing ownership. The
parent inventory is bounded to 512 KiB; the existing per-bundle file/byte limits
still apply. ``cleanupOnRemoval`` uses the recorded child ownership journal,
not a scan of arbitrary host directories. Switching between exact-directory
and scanning modes is an ownership change; enable cleanup of the old method or
perform an explicit migration. Do not use both modes over overlapping paths.

Automatic target-level ``rollback`` / bad-commit tracking is not supported in
scanning mode; configuration must not silently request that policy. Use a new
Git commit to restore the desired groups. Exact-directory mode keeps its existing
recovery behavior.

Host access and rootless operation
----------------------------------

FetchIt needs access to the host Podman socket. The Quadlet helper runs through
that socket with only the chroot, file access, ownership, and permission
capabilities needed for host operations; it does not request privileged mode. It mounts the host root
read-only to use the actual generator, libraries, and systemd private socket,
and mounts the configuration parent writable. It shares the host PID namespace
so systemd can authenticate the client. This is host administration
access; use trusted repositories and the existing commit-verification option
where appropriate. Files are installed into a method-owned subdirectory, not
into another method's directory. Rootful hosts must have ``/etc/containers``;
rootless hosts must have the configured configuration directory already created.

For rootless operation, use the intended user's Podman socket and provide their
host paths explicitly:

.. code-block:: yaml

   quadlet:
     - name: web
       schedule: "*/1 * * * *"
       targetPath: deploy/quadlet
       root: false
       hostHome: /home/operator
       hostConfigHome: /home/operator/.config
       hostRuntimeDir: /run/user/1234
       start: true
       restart: true

The host user manager must be running. Enable lingering for that user if workloads
must survive logout. FetchIt does not infer host paths from its container's UID,
``HOME``, or ``XDG_RUNTIME_DIR``. Rootless files are installed beneath
``hostConfigHome/containers/systemd``; system files beneath
``/etc/containers/systemd``.

Use rootless operation unless the workloads require root-owned host resources.
The helper administers the selected user's filesystem and systemd manager; a
rootful helper administers the entire host. The helper uses only the capabilities
listed above and writable configuration parent, but authored units can invoke
host commands. Use a dedicated deployment user/host where isolation is needed.

Rootless host paths must be canonical absolute paths, not ``/``, with no traversal
or embedded newlines. Configure the intended user's home, configuration, and
runtime directories, and verify their ownership and permissions before deployment.
Do not select shared or sensitive configuration parents. FetchIt validates path
syntax and its own managed directory; this is not an arbitrary host-path sandbox
or a policy allowlist for commands authored in units.

``helperImage`` optionally selects the helper image. The default is
``quay.io/fetchit/fetchit:latest``. Use an image built from a FetchIt release with
Quadlet support: it must provide ``sh``, ``chroot``, ``flock``, ``cp``, ``find``,
``grep``, ``sed``, and ``sort``. For deployments using a registry, configure an immutable image digest in
``helperImage`` when available. Local development can use the image tag built from
the same checkout. Helper images and Git contents are trusted host administration
inputs; reducing container capabilities does not sandbox the Quadlet services.

For production, replace the mutable default with an approved immutable digest:

.. code-block:: yaml

   helperImage: quay.io/fetchit/fetchit@sha256:REPLACE_WITH_APPROVED_DIGEST

This is a placeholder, not a usable image reference. Obtain the actual digest
from the image built from your reviewed source revision. Verify any available
signature/provenance against your organization's trusted signer policy before
allowing the deployment; a digest pins content but does not establish who built
it. FetchIt does not perform signature verification or enforce an image allowlist.
Do not assume that this project publishes a signature for every development image.
Test the new engine/helper pair on a disposable host, then update the pinned digest
explicitly. Keep the persistent volume and host journals through that upgrade.

New actions are carried in the FetchIt binary,
so no changes to the legacy Systemd helper image are required.

Updates, removal, and recovery
------------------------------

Change detection is scoped to the complete ``targetPath`` subtree, not to the
repository commit. FetchIt compares go-git tree object IDs (the Git equivalent of
an empty recursive diff): names, additions/deletions, nested contents, ciphertext,
and Git executable bits all contribute. This uses existing commit objects and
never invokes shell Git with an interpolated path. All candidate bundles still
pass the normal path, size, file-type and source-name validation before planning.

An unrelated commit still reaches the host helper, but a matching applied
receipt permits only an atomic receipt update under the namespace ownership lock,
without copying files, daemon-reload, or service restarts. The Git applied tag
advances only after that succeeds. The helper retains compare-and-set checks;
a missing receipt or pending partial operation takes the full reconciliation path.
Startup rechecks the receipt even when Git has not changed. This is not periodic
host drift detection: a receipt is evidence of a successful managed operation,
not a live filesystem or application-health audit.

New receipts additionally record the lifecycle settings identity and Git subtree
ID. ``helperImage`` remains part of the applied configuration tag, but is excluded
from the lifecycle identity: the controller supplies the embedded script and the
image supplies its execution tools, not application configuration. A helper-only
reference change can adopt a matching tree/lifecycle receipt without restarting
apps; this does not assert that two helper images have identical bytes. Host paths,
root mode, start/restart settings and bundle/repository identity remain significant.
Old two-line receipts remain readable; helper-only changes without the new proof
conservatively reconcile. Same-settings, unchanged-tree updates can upgrade the
receipt without a restart. Initial deployment without a receipt always reconciles.

When any bundle file changes, all eligible workload services in that bundle are
reconciled according to ``start``/``restart``; no per-service dependency graph is
introduced. Changes to a volume definition therefore reconcile the bundle but
**do not migrate or replace an existing volume**. Inspect resource semantics and
perform any migration explicitly. FetchIt never prunes or removes volume data.

FetchIt stops removed services before deleting their source files. Renaming a
file or changing ``ServiceName=`` retires the old service. It refuses to overwrite
a service owned outside the method's bundle. Removing the complete selected
Git directory removes its deployed files and stops its managed services.
By default, removing a method from FetchIt's configuration retains its services.
Set ``cleanupOnRemoval: true`` while the method is still configured to register
persistent cleanup intent; removing it then cleans its journal-owned bundle on
reload/startup. See :doc:`lifecycle` for receipts, retries, and retained resources.
Without opt-in, remove its source units first while the method is still scheduled.

Generator, copy, reload, and service failures are reported and leave the applied
Git commit unchanged. Persistent helper state tracks services from interrupted
applications so the next run can retry safely, including when the desired commit
changes. Application is not an atomic transaction. Git targets may opt into best-effort
``rollback`` when they contain only Raw, Kube, or Quadlet methods; it is disabled
by default. See :doc:`lifecycle` for limits. Network and volume resources may remain
on the host after their units are removed; FetchIt does not force-delete data.
On startup, FetchIt checks the host receipt for an interrupted application, even
when the saved Git revision already matches the desired revision. After success,
an unchanged Git commit is a no-op; this method does not continuously repair host drift.

Resource retention during cleanup
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

FetchIt stops the bundle's journal-owned generated services, removes its deployed
source units and supporting files, and reloads systemd. It does not separately
run ``podman volume rm``, ``podman network rm``, or an image prune.

For the checked-in ``web.container``, ``web.network``, and ``web.volume`` example,
the generated container is removed on service stop, but the named network and
named volume remain with their data. Podman's standard ``.volume`` generator does
not delete the volume when its service is stopped. ``.network`` retains its
network by default; ``NetworkDeleteOnStop=true`` changes that behavior to delete
it. Custom ``ExecStop``/``ExecStopPost`` and ``.kube`` teardown settings can also
remove resources. Review the generated units with ``systemctl cat`` before
turning on cleanup. See the `Podman Quadlet reference
<https://docs.podman.io/en/v5.8.0/markdown/podman-systemd.unit.5.html>`_.

To preserve persistent data, keep volume-deletion commands out of stop hooks,
back up named volumes before removal, and inspect them afterward with
``podman volume ls`` and ``podman volume inspect NAME`` (add ``sudo`` for rootful).
Keep the volume until a separate, explicit data-retirement decision. Deleting
source units does not make a destructive authored stop hook safe.

See the `runnable example <https://github.com/containers/fetchit/tree/main/examples/quadlet>`_
and the `Podman 5.8 Quadlet manual <https://docs.podman.io/en/v5.8.0/markdown/podman-systemd.unit.5.html>`_.

Run the example from a checkout
-------------------------------

These commands assume Linux, Go with automatic toolchain downloads enabled, and
Podman 5.7--5.x. Fedora 44 is the supported Fedora baseline for the Go dependency
update; Ubuntu 26.04 runs the Quadlet lifecycle workflow. Run the example against
an image built from this checkout until a release containing Quadlet is published.

Install host dependencies and build the image:

.. code-block:: bash

   # Fedora:
   sudo dnf install -y podman crun git golang
   # Ubuntu alternative:
   # sudo apt-get update
   # sudo apt-get install -y podman crun git golang

   git clone https://github.com/containers/fetchit.git
   cd fetchit
   go mod vendor
   # Use sudo podman for a rootful deployment, ordinary podman for rootless.
   # Replace amd64 with arm64 on an ARM64 host.
   sudo podman build --build-arg ARCH=amd64 -t localhost/fetchit:quadlet .

Use ``examples/quadlet/quadlet.yaml`` as the configuration. Set
``helperImage: localhost/fetchit:quadlet`` on the method so the helper uses the same
local image. Set the repository URL and branch to the Git revision containing your
bundle. During development, use the feature branch containing the example; after
merge, ``main`` contains it. A published release can use the default helper image.

For rootful deployment:

.. code-block:: bash

   sudo systemctl enable --now podman.socket
   mkdir -p "$HOME/.fetchit-quadlet"
   cp examples/quadlet/quadlet.yaml "$HOME/.fetchit-quadlet/config.yaml"
   # Edit config.yaml: set helperImage and the appropriate Git branch.
   sudo podman run -d --name fetchit-quadlet \
     --security-opt label=disable \
     -v fetchit-quadlet-volume:/opt \
     -v "$HOME/.fetchit-quadlet/config.yaml:/opt/mount/config.yaml:ro" \
     -v /run/podman/podman.sock:/run/podman/podman.sock \
     localhost/fetchit:quadlet

For rootless deployment, build the image into the user's image store instead of
root's store, then configure ``root: false``, ``hostHome``, ``hostConfigHome``, and
``hostRuntimeDir`` with that host user's paths. Display the values with:

.. code-block:: bash

   printf 'hostHome: %s\nhostConfigHome: %s\nhostRuntimeDir: /run/user/%s\n' \
     "$HOME" "${XDG_CONFIG_HOME:-$HOME/.config}" "$(id -u)"
   mkdir -p "${XDG_CONFIG_HOME:-$HOME/.config}" "$HOME/.fetchit-quadlet"
   sudo loginctl enable-linger "$(id -un)"
   export XDG_RUNTIME_DIR="/run/user/$(id -u)"
   systemctl --user enable --now podman.socket
   podman build --build-arg ARCH=amd64 -t localhost/fetchit:quadlet .
   # Prepare ~/.fetchit-quadlet/config.yaml with the rootless fields above.
   podman run -d --name fetchit-quadlet \
     --security-opt label=disable \
     -v fetchit-quadlet-volume:/opt \
     -v "$HOME/.fetchit-quadlet/config.yaml:/opt/mount/config.yaml:ro" \
     -v "$XDG_RUNTIME_DIR/podman/podman.sock:/run/podman/podman.sock" \
     localhost/fetchit:quadlet

Set the example's ``WantedBy=default.target`` when running it as a user service.
The named FetchIt volume preserves Git repositories and applied-commit tags across
container recreation. Recreating FetchIt without that volume loses its Git cleanup
baseline; keep it for updates and retries. Rootful and rootless Podman image,
container, network, and volume stores are separate, so consistently use the same
mode for building, running, inspecting, and removing resources.

Inspect services and diagnose failures
--------------------------------------

The generated default names are:

.. list-table:: Generated service names
   :header-rows: 1

   * - Source
     - Service
   * - ``web.container`` / ``web.kube``
     - ``web.service``
   * - ``web.network``
     - ``web-network.service``
   * - ``web.volume``
     - ``web-volume.service``
   * - ``web.pod``
     - ``web-pod.service``
   * - ``web.image``
     - ``web-image.service``
   * - ``web.build``
     - ``web-build.service``
   * - ``web.artifact``
     - ``web-artifact.service``

``ServiceName=`` overrides these names and omits the ``.service`` suffix in the
source value. Drop-ins with the same filename follow Podman's specificity rules;
selected drop-ins are merged alphabetically. The generator manages dependencies
such as ``Network=web.network`` and ``Volume=web.volume:/data``.

For the rootful example:

.. code-block:: bash

   sudo podman logs fetchit-quadlet
   sudo systemctl status web.service
   sudo systemctl cat web.service
   sudo systemctl show web.service --property=SourcePath
   sudo journalctl -u web.service --no-pager -n 100
   sudo podman exec systemd-web cat /data/message

For rootless operation, use ordinary ``podman`` and add ``--user`` to the
``systemctl`` and ``journalctl`` commands. ``SourcePath`` identifies the managed
Quadlet subdirectory, which also contains supporting files. The helper writes
``validation.log`` in a sibling hidden control directory beneath the configured
parent. FetchIt logs contain generator output when validation fails and the
helper's exit status when a host command fails.

If a service is not generated, inspect the validation log for missing required
keys or options unsupported by the host's Podman version. If startup fails, inspect
the service journal for image-pull errors, unavailable host paths, or a failed
``ExecStartPre``. Fix the desired Git bundle or the missing host prerequisite; the
same commit is retried on the next schedule because it was not recorded as applied.
For user-manager connection failures, confirm that the configured host home and
runtime paths belong to the socket's user and that their user manager is running.
An ownership error means another bundle or regular systemd service already uses
the generated name; choose a distinct ``ServiceName=``.

To update workloads, push changes to the selected Git directory and wait for the
configured schedule. To remove workloads, delete their Quadlet sources in Git and
leave FetchIt running until it applies the removal. Only then remove the FetchIt
container or method configuration. Network/volume data retention and configuration
reload behavior follow the earlier sections; deleting the FetchIt container alone
does not stop its host-managed services.

Configuration changes and concurrent controllers
------------------------------------------------

Applied-commit tags include the bundle path, branch, host paths, and activation
settings. Changing ``start``, ``restart``, or ``helperImage`` therefore triggers
application on configuration reload, even if the Git commit is unchanged. The
host directory identity stays stable for activation changes, preserving ownership.
Changing the root/user scope, host paths, branch, or bundle path changes ownership.
With cleanup enabled, the old identity is retired and cleaned up. Without it,
remove the old sources while their previous method is still configured. FetchIt
does not migrate resource contents to a different manager or path. Trailing
slashes in ``targetPath`` do not change ownership.
The existing :doc:`methods` configuration-reload mechanism also applies to Quadlet.

Use one active configuration owner for each bundle. The host lock serializes helper
operations, and its applied receipt rejects a stale plan whose expected revision
no longer matches the host. A second controller with the same configuration should
refresh its applied Git state before retrying a stale-plan error. Do not deliberately
run competing controllers with different settings for the same managed bundle.
Helper-container removal errors also fail application; inspect stopped helper
containers with ``podman ps -a`` and resolve the removal error before retrying.
