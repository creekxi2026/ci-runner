# Controller cleanup observations (partial RUIF-106)

`JOB_CLEANUP_JOURNAL=on` enables a controller-only infrastructure journal. It is
**not** a `release-pg-terminal/v1` provider and must not unblock a service-specific
cleanup gate. The default is off. This change has not been deployed.

The current adapter ABI only supports `init` and `check`. It provides neither a
project cleanup verifier nor trusted project execution/admission records. This
PR preserves facts that would otherwise disappear while those missing interfaces
are resolved. It does not assert database absence, role absence, credential
revocation, or successful tests from removal of a container or directory.

## Implemented interface

Records live in the existing fixed volume at
`cleanup-v1/<deployment-owner>/<runner-name>.json`, outside `jobs/<runner-name>`.
Directories are root-owned 0700; records are root-owned 0444, at most 65536 bytes.
Only a trusted host collector may read them. No worker mount, worker publication
API, arbitrary output path or worker deletion API is added. `acquire`, `ready`,
`exec`, service catalog v1 and its fingerprint are unchanged.

The closed `ci-cleanup-observation/v1` object contains:

| Field | Evidence and limits |
| --- | --- |
| `owner_id`, `runner_name` | Controller allocation identity; runner name is **not** a native numeric job ID |
| `native` | Optional scheduler repository, run ID, opaque scheduler job ID, runner ID and result; no inferred source SHA or attempt |
| `service_index` | Optional controller-private index captured before deletion; absence stays null |
| `runner_before_cleanup` | Allowlisted Docker state: observed, running, OOMKilled, exit code and validated finish timestamp |
| `cleanup_started_at` | Durable pre-deletion checkpoint |
| `allocation_released_at` | Only after normal cleanup/deregistration, data removal and pool release, plus fresh scoped absence checks |

`native=null` and `observed=false` mean unknown. `observed=false` makes every other
runner-state field unusable as evidence. A scheduler result is a scheduler fact,
not an authenticated release result. Unknown scheduler enums become `unknown`;
cleanup cannot turn a failed/canceled result into a success. A delayed completion
can add the result after cleanup. Conflicting identities or outcomes are rejected.

Writes use exclusive temporary files, file fsync, atomic rename and directory
fsync. Retries synchronize the directory even when bytes already match. Initial
ancestor directories are synchronized before accepting writes. The shared-work
owner lock excludes another controller; a journal mutex serializes local writers.
Readers reject final symlinks, hard links, non-regular files, writable/wrong-owner
files, duplicate JSON keys and oversized data. Temporary crash files are unlinked,
never followed. Names are generated from validated runner IDs.

```mermaid
sequenceDiagram
    participant S as Scheduler
    participant C as Controller
    participant J as Private journal
    participant D as Docker / fixed work volume
    S->>C: JobStarted / JobCompleted (trusted SDK)
    C->>J: Persist whitelisted scheduler identity/result
    C->>J: Persist pre-cleanup state + private service index
    C->>D: Existing owned resource / job data cleanup
    C->>D: Deregister runner and release pool lease
    C->>D: Check exact job/owner containers, networks and paths absent
    C->>J: Persist allocation_released_at
    Note over J: Survives job directory removal; not PG terminal receipt
```

## Failure, restart and storage rules

A failed initial journal commit postpones cleanup, keeping the allocation for
retry. A failed final commit keeps an in-memory finalization state, so retries do
not need the already removed lease. On restart, the ordinary controller recovery
handles surviving allocations; pending journal entries with no remaining
allocation are reconciled with read-only Docker and filesystem checks. Unknown
Docker responses never establish absence. Startup refuses a journal whose pending
allocation cannot be reconciled. Operators must investigate rather than delete
its journal to force successful evidence.

Journal storage is diagnostic retention, not a cache. No worker can read a sibling
record or remove one. Do not mount the owner directory into a candidate job.
For an authorized collector, use a root-run container with only the exact owner
subpath mounted read-only; never the fixed-volume root. The existing PG consumer
requires a different, per-service publication root with 0755 directories and
`<lease_id>.json`; this journal is intentionally incompatible with that interface.

Retention policy for an eventual approved pilot: retain unresolved records until
resolution; export completed records to the controlled evidence archive and keep
them locally for 30 days. Prune only released records after checking the export
digest, absence of that runner's active lease/resources, exact owner, regular file
and age. Stop scheduling new pilot jobs when journal storage reaches 64 MiB or
1000 records, then archive/review; existing cleanup must continue. Automatic
retention/admission enforcement is **not implemented** in this partial PR, so a
long-running production enablement is not ready. No fixed volumes, active leases,
shared caches or other owners' directories may be pruned.

## Missing project provider contract

The pinned consumer is closet-ai
`6f6260153a22fcf140c9fdab52b2d1b7b6975899`,
`scripts/ci/release/lease_cleanup.py`. Its binding requires exactly repository,
source_sha, run_id/run_attempt/job_id, lease_id/owner_id/service_id/fingerprint,
instance_id/input_digest, producer source/workflow/policy/executor/run identity,
and terminal_outcome. The current scheduler record does not provide the complete
binding. Its opaque job UUID must not be substituted for the Actions job number.

Before a complete provider can be implemented, the project-owned integration
needs an authoritative admission/execution record, not a candidate manifest or
the latest attempt returned after a restart. A protected collector must join
the scheduler assignment to the exact authenticated Actions job/attempt/source
and the fixed trusted planner/executor identity. Missing or conflicting joins
must remain waiting. This is a W05 provider dependency, not a reason to defer the
contract to W07 or weaken T06.

The project also needs a digest-pinned teardown/verifier adapter. Before service
destruction it must disable login, revoke credentials, terminate existing sessions,
drop the two owned databases and role, and independently check catalog absence
and rejected authentication using the previous synthetic credential. Unknown,
partially initialized, unreachable or unexpectedly owned objects must not produce
affirmative claims. SQL/naming and release schema serialization belong to that
project adapter, not this generic repository. Both the selected adapter spec and
its admission record must be persisted before provisioning so recovery cannot
run a changed catalog/provider on an old lease.

Only after both project verification and infrastructure release may a trusted
provider render `release-pg-terminal/v1`, preserving the original test outcome,
with a fresh `finished_at`. Store pending verification separately from private
job data; publish atomically to a per-service read-only directory. The existing
single lease can cover several services, so sharing a `<lease>.json` root across
services would overwrite identities. A host collector must consume the receipt
after runner exit/cancellation, without depending on a dead runner.

## Validation and rollout boundary

Unit tests cover changed/late identity, failure preservation, concurrency,
links/modes/owners/size, partial/unknown resource state, crash-file replacement,
restart reconciliation, finalization after lease deletion and fsync retries.
The isolated ARM64 runtime test exercises actual success (exit 0), failure
(exit 7) and forced cancellation (exit 137) of a non-business dummy worker,
actual network/private directory/pool cleanup, and retained root-owned evidence.
Scheduler IDs/results in that runtime test are explicitly synthetic. It has no
PostgreSQL database or application credential. The fixed consumer rejects these
generic records; missing PG receipt stays waiting.

This partial PR is not a deployable solution to W05-T06. A concrete activation
plan must first pin the finished controller, project provider/adapter, admission
collector, full schema/config and immutable image digests, then obtain the
existing deployment authorization. Drain current jobs without restarting them;
retain the current controller image/config and fixed work volume; canary one
success/failure/cancelled job, verify actual SQL/session/credential observations
and post-run collector reads, then extend. If evidence fails, stop new admission,
allow owned cleanup, revert the exact retained controller/config after drain and
preserve pending journal records. Never use rollback to claim missing evidence.
