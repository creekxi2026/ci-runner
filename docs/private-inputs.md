# Private resources in a separate authenticated pool

This optional extension to `ci-job-inputs/v2` accepts `private_mounts`. It is
disabled by default, requires a dispatch-only pool, shared work storage, writable
dependency caching off, a digest-pinned project helper, and independent review.
It is not a mechanism for granting resources to a mixed PR pool.

Each entry has exactly `subpath`, `target`, and `read_only`. Sources are directories
on the existing work volume. Paths must be canonical, below at least two volume
components, with root-owned non-writable ancestors and no symlinks. A writable
leaf must be UID1001 mode0700. A read-only leaf must be root-owned and not group
or world writable. Targets are below `/opt/project/` or `/var/lib/project/`.
Mounts cannot overlap each other, existing disk/tool mounts, or publications.
Checks run both on policy loading and each runner preparation. A missing,
changed or incompatible resource refuses creation; it is never repaired by chmod.
The controller mounts only the specified volume subpath, with NoCopy enabled.

The project helper owns repository/workflow/ref/actor/run/attempt/job/registered
runner authorization, source/tool digests, persistent state protocol, and project
configuration. The generic core neither interprets these data nor reads project
credentials. Tests use a non-business dummy directory and publication.

Registration still precedes assignment: the trusted Listener container has these
mounts **before** provider authorization. No candidate Worker runs then. Startup
capability binding, the original Listener allocation, independently sealed
scheduler assignment and the pinned helper gate are mandatory. An event filter
alone is not authorization. A separate pool/label is necessary, and its helper
must reject every unauthorized job before its first stdout byte `0`. Neither
shared UID nor a crafted job payload can bypass the existing one-use handshake.
This relies on the trusted image and Docker/controller administration boundary;
it is not isolation against an administrator or compromised image startup.

The helper's allocation lifetime and cancellation rules remain those in
[the intake protocol](worker-intake-protocol.md). The ordinary controller owns
registration, one container creation, disk/proxy/services and cleanup. Private
resources are operator-owned and remain after job teardown or crash, including
unknown write intents. Reclamation must remove only the job's ordinary resources.
Operators must reconcile persistent state before a new source/tool installation.

Prepare and test policy and files first; drain before changing pool capacity or
images. Keep the prior declarations/image digests, preserve persistent resources,
and stop the new pool on failed readiness. Restoring declarations must not replay
an uncertain write or roll back an application's state. No new volume, privilege,
Docker socket, broad private network route or application deployment is granted
by this interface. Runtime-image publication and onsite enablement are separate.
