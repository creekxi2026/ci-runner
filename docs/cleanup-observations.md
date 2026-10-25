# Durable terminal evidence

`JOB_CLEANUP_JOURNAL=on` enables the host-only `ci-cleanup-observation/v1`
journal. An optional service `teardown` declaration adds a project verifier and
an optional host admission gate. Defaults remain off/absent. This code has not
replaced a resident controller. The generic journal is not itself a project
terminal receipt: a reviewed project collector interprets its boolean checks.

## Storage and trust

All paths below are subdirectories of the existing fixed work volume:

| Path | Owner, mode, writer |
| --- | --- |
| `cleanup-v1/<owner>/<runner>.json` | root 0444, root 0700 parents; controller only |
| `admissions-v1/<owner>/<runner>/<service>.json` | root 0444, root 0700 parents; trusted host issuer only |
| `jobs/<runner>/service-private/<service>/spec.json` | root 0400; original reviewed spec, before provisioning |
| `jobs/<runner>/service-private/<service>/terminal/proof.json` | root 0444; pinned teardown adapter only |

Neither worker nor service receives journal/admission mounts. Project collectors
receive an exact owner's journal **read-only**, never the volume root. All reads
are no-follow, regular-file, one-link, bounded to 65536 bytes; journal/admission
reads also enforce root ownership and exact modes. Unknown/duplicate JSON keys
and incorrect types fail closed. Private service parents remain root 0700.

Journal writes use exclusive temp files, file fsync, rename and directory fsync.
The owner flock excludes other controllers; a local mutex serializes updates.
Retries fsync even unchanged content and synchronize newly created ancestors.
Temporary crash files are unlinked, never followed. A failed final write can be
retried after the lease has already been removed. Startup reconciliation uses
fresh scoped resource absence checks and never deletes arbitrary journal names.

## Service extension

A new controller accepts this optional member in a service v1 spec:

```json
{"teardown":{"checks":["resource-absent","credential-disabled"],"admission":true}}
```

There are 1–16 unique safe check IDs. The entire declaration participates in the
catalog fingerprint. Old catalogs retain their previous encoding/fingerprint;
old controllers reject this new member, so install the new controller before
enabling it. A declared teardown requires the cleanup journal to be enabled.
The original spec is persisted before any service starts; recovery never selects
an adapter from a changed catalog for an old lease.

If admission is required, provisioning waits for a host-issued envelope with
exact fields `version=1`, `lease_id`, `owner_id`, `job_id` (runner name),
`service_id`, `fingerprint`, and project-owned object `payload`. The controller
checks the envelope against its allocation and commits its canonical SHA256 to
`admission_digests[service]` **before** creating the service. A changed admission
or one first seen after cleanup is rejected. The project issuer must establish
payload provenance; root-chowning a worker result does not make it trusted.

After cancellation/join of provisioning and confirmed worker absence, the
controller invokes the original pinned `/ci-adapter teardown`. The request is
the same v1 envelope as init. The adapter receives init's private secrets RO,
consumer outputs RO, its request RO and a distinct `/terminal` RW directory.
It shares only the existing private service network namespace. It has no
capabilities, Docker API, restart policy or logs, and uses the existing 2/16
adapter budget. Its deadline is cleanup_started_at + startup_seconds, persisted
across retries/restarts rather than extended indefinitely.

The only accepted output is `/terminal/proof.json`:

```json
{"version":1,"checks":{"resource-absent":true,"credential-disabled":true}}
```

The map must contain exactly the declared keys, all booleans true. No free text,
errors, credentials, endpoint, SQL or arbitrary payload is copied. The controller
adds the pinned adapter/config identities and fsyncs `service_proofs[service]`
before destroying the service or private job data. A temporary server inspection
error, adapter setup I/O error or incomplete adapter run retains the owned service,
secrets and checkpoint for retry within the original deadline. Restart never
extends that deadline. Once it expires, ordinary owned resource reclamation can
continue without proof; a previously completed successful witness can still be
transferred. Confirmed missing/stopped servers and malformed verification also
produce **no proof**. An init failure may also be unprovable;
this must remain unknown rather than be translated into project success.

## Order and recovery

```mermaid
sequenceDiagram
    participant H as Trusted host issuer/collector
    participant C as Controller
    participant A as Pinned project adapter
    participant J as Durable owner journal
    H->>C: Persist exact allocation admission
    C->>J: Commit admission digest before service start
    C->>A: init / check
    Note over C: Job completes, fails, cancels or expires
    C->>C: Cancel/join provisioning; delete worker
    C->>J: Persist cleanup start and allocation/native observations
    C->>A: teardown, bounded original spec
    A-->>C: Whitelisted boolean proof, or no proof
    C->>J: Fsync proof before deleting its source
    C->>C: Delete service, private data, network; deregister/release
    C->>J: Fresh absence checks; allocation_released_at
    H->>J: Read after worker and private disk are gone
    H->>H: Join exact native job and project admission; publish receipt
```

Pre-cleanup runner inspect may say running or unknown. Only the final absence
checks establish that all job processes are gone. `native` is the SDK's exact
repository/run/runner plus opaque scheduler job ID and result; it does **not**
invent a numeric Actions job ID, source SHA or attempt. Delayed completion can
update the durable journal after ordinary cleanup; conflicting outcomes fail.
Project collectors must authenticate the numeric native identity separately.

## Retention and rollback

Retain incomplete journals/admissions until investigation resolves them. Keep
completed journal, admission, exported receipt and verified consumer result for
30 days and archive their hashes together. Reclamation is an operator operation:
check exact owner/lease/native attempt, archive digest and successful consumer
record, verify no active runner/lease/consumer mount refers to it, then remove
only those exact regular files and empty per-job/per-lease directories. Do not
prune shared caches, fixed volumes or another owner's records. There is no
automatic destructive retention API. Bound the initial pilot to 1000 records /
64 MiB and stop new pilot dispatch for archival review before that bound; the
host sweep refuses >1000 journals and existing cleanup must remain enabled.

Drain before changing controller or catalog; do not kill resident jobs for an
upgrade. Rollback disables new terminal admissions, lets owned cleanup finish,
and restores the retained old image/catalog after drain. Keep new journals and
admissions for the matching collector. Old readers do not understand the new
optional proof/admission fields; do not silently discard them to downgrade.

## Evidence boundary

Core unit/race tests cover identity, serialization, concurrent writes, filesystem
safety, absent/unknown resources and fsync/restart paths. The optional generic
`TestServiceTerminalRuntime` accepts an external project fixture and uses real
Docker operations without embedding project SQL. Its test-only HTTP transport
prefixes volume subpaths below an owned directory of the existing fixed volume.
Local image IDs bypass registry admission only in that test. Native SDK/API data
in this fixture are explicitly synthetic. The project fixture verifies real PG
cleanup and its unchanged fixed final consumer in a separate UID1001 RO mount.
Native trusted workflow execution and fleet rollout require separate acceptance.

## Events outside the allocation namespace

With the journal enabled, Scale handles a started/completed event whose name
fails `validJobName` by committing a routing disposition before continuing to
later events and current demand. This uses the local allocation invariant:
creation generates `ci-job-<8 hex>-<3 hex>`, shared leases validate it, and
restart recovery admits only that namespace. It does not infer absence from
missing memory state, match a resource by scheduler runner ID, infer the event's
upstream cause, or claim that any resource was cleaned. A contradictory local
state entry outside the namespace fails closed. Valid-name events retain their
existing journal, assignment, cleanup and retry rules, including when only a
durable journal or lease remains after restart.

Dispositions are owner-only files at
`cleanup-v1/<owner>/unaddressable-<sha256>.audit` (0444, root 0700 parents).
The closed `ci-unaddressable-event/v1` record contains `schema`, `owner_id`,
`handler` (started/completed), `reason=outside_runner_namespace`, and
`event_sha256`. The digest covers the canonical JSON encoding of owner,
handler and the typed SDK event; raw names, native identity, body and results
are never stored in this record or used in its path. The fingerprint is for
local deduplication, not proof of scheduler authenticity or a secret redaction
protocol. No diagnostic schema or collector receipt fields change.

The existing owner flock and journal mutex serialize writes. Publication uses
exclusive temporary creation, file fsync, rename and directory fsync; retries
validate an existing exact owner/mode/single-link/canonical record and repeat
directory fsync. A stale `.new` name is unlinked without following it. A write,
validation or sync error returns from Scale and prevents ack; a later event or
reconcile failure also prevents ack. Redelivery revalidates the same record;
a process crash before ack leaves either an unpublished temp or a durable
idempotent record. Only after all events and demand succeed may listen ack, and
ack failures retain the existing retry behavior. There is no pending cleanup
hidden in the disposition: leases, labeled resources and per-allocation
journals retain all original janitor/recovery responsibilities, independent of
message delivery. No message queue or replay of the fingerprint is needed.

`.audit` and `.audit.new` do not enter the existing `.json` allocation recovery
or project receipt readers; retain the audit records with owner evidence and
include them in the existing pilot archival budget (1000 records / 64 MiB).
They are not automatically deleted. Operators must archive them explicitly;
do not overwrite old evidence or replay a consumed observation window when
installing a new candidate. A new controller revision still needs its matching
reviewed image pair and newly generated operation inputs before rollout.
