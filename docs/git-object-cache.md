# Controller-owned Git object cache

The controller prepares the fleet repository's default Actions workspace before
starting its worker. Standard pinned `actions/checkout` still fetches and checks
out its requested ref/SHA, with its normal credentials and history settings. A
cache hit grants no source, workflow, protected-branch or release authority.
The controller never switches a shared working tree or executes cached source.

## Storage and compatibility

`GIT_OBJECT_CACHE_MODE=auto` (default) enables this cache for private disk jobs.
Legacy shared-writable dependency-cache fleets skip it; explicit `on` rejects
that incompatible combination. `off` restores normal cold checkout. This cache
does not require a dependency seed and does not replace Go/Jest or dependency
caches. The controller image adds Git/Python from the existing signed Debian
snapshot; its Compose memory budget is 1 GiB for pack processing.

The existing `ci-deps-linux-arm64-cache` volume must be mounted read-write only
in the controller at `/var/lib/ci-runner/cache`. Startup checks its local driver,
empty driver options, four existing schema/owner/repository/lane labels and
actual mount identity. Its root must be UID 0 and not group/world writable.
The legacy volume labels identify the pool, not permission to mix repositories.
Data lives under `git-v1/<deployment-id>/<sha256(repository + newline + refs)>/`.
Every namespace is UID 0 mode 0700. Changing repo, owner or refs creates an
independent namespace; the previous namespace is retained for operator review.

`GIT_OBJECT_CACHE_REFS` is a comma-separated operator selection of one to eight
literal `refs/heads/...` names, default `refs/heads/main`. The only remote is the
fleet's validated `https://github.com/OWNER/REPOSITORY`; there are no worker-
supplied URLs, refspecs or import paths. Add a protected integration branch when
appropriate. Non-selected branches and tags are still fetched normally by the
worker. A deleted cache source fails refresh and retains the validated previous
snapshot; it never substitutes a newer SHA for the job's request.

Private files live in the existing work volume under
`jobs/<job-id>/home/_work/<repository-name>/<repository-name>/.git`, UID/GID 1001.
The worker sees only its existing private home/tmp mounts. No persistent cache
mount, hardlink, shared worktree metadata or alternate-object path is exposed.
The controller installs a valid detached HEAD without expanding source blobs;
normal checkout's reset/checkout materializes source inside the limited worker.
Custom checkout paths and other repositories remain cold. LFS payloads and
submodule repositories are outside this cache.

## Trust, publication and cleanup

Only the trusted controller fetches from GitHub. The token enters the helper on
stdin, then only the fetch child's environment; it is absent from URLs, process
arguments, disk configuration, workers and logs. System/global Git config,
credential helpers, hooks, redirects and protocols other than HTTPS are disabled.
Git verifies fetched objects. The helper copies only validated regular loose
objects and pack/index/reverse-index files; it reconstructs config and refs.
It rejects shared links, unexpected object metadata and shallow cache snapshots.

A per-namespace `flock` covers update, copy and collection. A fresh bare staging
repository receives private objects and incrementally fetches selected refs.
The helper validates content with `git fsck`, renames the snapshot and atomically
publishes `index.json`. A crash before publication leaves the previous index;
the next refresh removes staging under the same lock. An invalid index, corrupt
snapshot or permission mismatch fails cold instead of importing data.

Refreshes occur at most once per minute. Each namespace keeps one published
snapshot plus one staging copy. More than 32 packs triggers repack/prune in
staging. After publication old snapshots are removed under the lock. Active
jobs already own all their files, so GC cannot invalidate them. No worker data
is ever promoted back, including on successful protected jobs.

The object budget is 2 GiB/100,000 copied files per snapshot. Preparation requires
5 GiB free; fetch checks its object size and 1 GiB reserve every 250 ms. This is
a monitored cache budget, not a filesystem quota; a write can exceed the limit
between checks. Repack transient data, private copies and source expansion also
consume disk. A Git subprocess has 180 seconds and the whole helper four minutes,
including lock waiting. Cancellation kills/reaps its Git process group and cleans
staging/private output; controller crash recovery retains the existing job-lease
cleanup. A failed private cleanup fails provisioning rather than starting a
worker with uncertain state.

`FICLONE` is attempted for every copied file. Unsupported filesystems use ordinary
private byte copies. `cloned_bytes` and `copied_bytes` report logical bytes, not
physical allocation. `received_pack_bytes` counts new received pack payload
files, excluding HTTP/TLS overhead and pack indexes; it is not total wire traffic.
`fetch_seconds` includes transfer and index-pack. `prepare_seconds` also includes
lock waiting, validation and private initialization, but excludes Actions queue
time and the worker's checkout. Logs report source ref hashes and snapshot ID.

## Rollout and rollback

1. Verify the fixed volumes' labels/ownership, current Compose configuration and
   active jobs. Use a reviewed digest pair and the matching deployment artifact.
2. Set the fleet's selected refs and cache mode in its operator environment. Drain
   affected workers through the existing controller shutdown contract before
   recreating only that controller; do not interrupt other pools or active jobs.
3. Verify controller logs, normal exact-SHA PR/push jobs, full-history consumers,
   dependency caches and the application's existing non-production release gate.
   Compare cold/warm runs at the same source and record queue time separately.
4. To disable, set `GIT_OBJECT_CACHE_MODE=off` in the operator environment and,
   after draining, run the deployment's normal `docker compose up -d controller`.
   Keep the coherent previous image pair/config for an image rollback. No volume
   deletion is required; jobs return to normal cold `actions/checkout`.

For a corrupt namespace, disable/drain first, verify the exact deployment/repo/ref
key and absence of a running helper, then quarantine only that key directory
within `git-v1/<deployment-id>/`. Re-enable to create a cold snapshot. Do not
remove the fixed volume, its other namespaces, dependency data or live job data.
The task's deployment record must supply the actual directory/key and Compose
location; this generic contract intentionally does not choose an operator host.

## Verification boundaries

`python3 -m unittest discover -s tests -p test_git_cache.py -v` executes real Git
fixtures for cold/warm/incremental pack reuse, parallel heads, pinned history,
deleted source branch, force-push/GC with live copies, corruption, byte-copy
fallback, failure cleanup and subprocess timeouts. Set `CI_GIT_CACHE_TEST_ROOT`
to a task-private directory on the existing cache volume to test cross-volume
cloning; place TMPDIR in a task-private directory on the fixed work volume.
Tests remove their private directories; callers must remove only their own
temporary container/root directories. Never create per-test volumes on the
shared host. Native Actions and deployment evidence remain separate gates.
