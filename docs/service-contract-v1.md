# Generic job services v1

The opt-in v2 [private input contract](private-inputs.md) adds operator-owned
persistent resource mounts to separately authenticated dispatch pools. It does
not change the service v1 envelope or enable mounts for ordinary PR workers.

The optional [controller cleanup journal](cleanup-observations.md) retains
infrastructure observations after job deletion. It does not extend this adapter
ABI with teardown or implement a project terminal receipt. Missing project
verification must remain unknown; resource disappearance is insufficient.

This is the implemented runner-side service v1 contract. Consuming projects own
their adapters and workflows; operators coordinate upgrades for each deployment.
The previous parameterized database API has been removed from the core.
The controller must not interpret database names,
SQL, framework/tool names, or application environment variables.

The separate [Git object cache](git-object-cache.md) is controller-owned storage,
not a service or worker-selected catalog capability. It seeds only each job's
private default checkout path; it adds no worker mount, credential, service
environment, admission bypass or successful-verification claim. Projects retain
ownership of checkout SHA/history and protected-source validation.

## Catalog and trust

An operator installs a reviewed service catalog in a controller-only read-only
mount. Candidate jobs can select a catalog service ID, never supply catalog
content, an image, an initializer, a Docker option or a host path. Catalog and
adapter revisions come from a protected, reviewed source; checking out a PR does
not change them. No administrative credential from a deployment database is used.

The v1 catalog is an object with `version: 1` and a `services` map. Each entry has
exactly these resolved fields (unknown fields are rejected):

| Field | Meaning |
|---|---|
| `image` | Service image reference pinned with `@sha256:<64 lowercase hex>` |
| `adapter_image` | Initialization/check image, pinned the same way |
| `config` | Public, project-owned JSON settings; no credentials or script paths |
| `ports` | Sorted unique list of TCP ports, integers 1..65535 |
| `resources` | `memory_bytes`, `cpu_millis`, `pids`, `data_bytes`, `startup_seconds` positive integers |
| `storage` | List of container data mount targets, validated against operator policy; no host paths |
| `service_env` | Static string map; application interpretation belongs to its image |
| `secret_files` | Map of generated secret IDs to `service`/`init` recipients; no worker recipient |

Service/secret IDs match `[a-z][a-z0-9-]{0,47}`. Every resource limit and storage
target is constrained by an operator policy independently of catalog validation.
v1 allows at most one service instance per catalog ID per job and a bounded total
per job. Filesystem capacities are enforced or provisioning fails; they are not
advisory requests. An empty catalog is valid and requires no service images.
All execution images are pre-pulled and verified Linux ARM64 images. The generic
runner distribution publishes controller/runner images, not database images.
Each generated secret is 32 cryptographically random bytes encoded as 64 lowercase
hex characters plus a newline, persisted once for the lease. A recipient list is
a nonempty sorted subset of `["init", "service"]`; unknown recipients fail.
Files are mounted at `/run/ci-service-secrets/<secret-id>`, read-only to each
declared recipient. Service configuration can refer to those fixed paths through
its static `service_env`; the runner does not interpret application env keys.

## Worker-visible files

Before any job command runs, the controller mounts a **job-private directory**
read-only at `/run/ci-services` and sets:

```text
CI_SERVICES_FILE=/run/ci-services/index.json
CI_SERVICES_FINGERPRINT=<64 lowercase hex>
```

`index.json` is immutable throughout the job and exists even when no service is
requested. All fields below are required; the service map may be empty:

```json
{
  "version": 1,
  "lease_id": "<64 random lowercase hex, generated once and persisted for this job>",
  "owner_id": "<deployment identity>",
  "job_id": "<controller-owned job identity>",
  "fingerprint": "<CI_SERVICES_FINGERPRINT>",
  "services": {
    "example-db": {
      "image": "<resolved digest-pinned reference>",
      "adapter_image": "<resolved digest-pinned reference>",
      "config_digest": "<sha256 of canonical config JSON>",
      "ready_file": "/run/ci-services/services/example-db/ready.json"
    }
  }
}
```

Service-specific state is published separately; replacing the index is forbidden.
Only after initialization and the consumer-level check succeed does the controller
atomically publish `ready.json` with exactly these fields:

```json
{
  "version": 1,
  "state": "ready",
  "lease_id": "<same as index>",
  "owner_id": "<same as index>",
  "job_id": "<same as index>",
  "service_id": "example-db",
  "fingerprint": "<same as index>",
  "image": "<same as index service>",
  "adapter_image": "<same as index service>",
  "config_digest": "<same as index service>",
  "endpoint": {"host": "<private numeric IPv4 or IPv6>", "ports": [5432]},
  "outputs": {"consumer": "/run/ci-services/services/example-db/outputs/consumer.json"}
}
```

Port 5432 and `example-db` are examples, not defaults. The output payload is opaque
project-owned JSON. No credentials/DSN appear in index/ready files. v1 publishes
one regular UTF-8 JSON output file, `consumer.json`, at most 64 KiB; its internal
schema belongs to the adapter/consumer. The controller parses only the outer
service envelope. Paths are controller-generated, never adapter-supplied.

The mount and metadata directories are controller-owned, non-writable to workers;
index/ready files are UID 0 mode 0444, directories UID 0 mode 0755. The output
file is UID 1001 mode 0400 inside that read-only mount. Bootstrap files are in a
separate private mount absent from the worker. Reject symlinks, hard links,
non-regular output files, traversal, unexpected files and oversized output.
Finish publishing the output before the atomic ready-file rename. Consumers
reject unsupported versions, inconsistent identities/digests and paths outside
the fixed read-only mount, rather than trusting an environment variable alone.
No descriptor from a previous job can be reused. Never log/copy the output into
an artifact; the project wrapper masks credential values before any Actions env
export. The descriptor is not permission to expose its referenced contents.

## Worker commands and lifetime

```text
ci-service acquire SERVICE_ID [--timeout SECONDS]
ci-service ready SERVICE_ID [--timeout SECONDS]
ci-service exec SERVICE_ID [--timeout SECONDS] -- COMMAND [ARG...]
```

Timeout defaults to 45 seconds; accepted values are integers 1..300 and the job's
remaining lifetime always wins. `acquire` requests only a catalog ID, waits for
ready, and prints only the absolute ready-file path followed by newline. `ready`
waits for an already requested service and has no stdout; an unrequested service
is unavailable. `exec` acquires, then replaces itself with COMMAND **in the worker,
with the worker's UID/capabilities**, setting `CI_SERVICE_FILE` to ready.json. It
does not exec inside a service, initializer or host. Consumers read their opaque
output and map it to project variables themselves. No PG-specific CLI remains
in the generic image after cutover.

Infrastructure exit codes: 64 invalid arguments; 65 missing/invalid catalog or
service ID; 69 unrequested/failed service; 70 ownership/protocol mismatch;
124 wait timeout; 130 interruption. `exec` otherwise returns the command's exit
status. Errors contain codes/IDs only, never raw adapter output or credentials.
Duplicate/concurrent requests for `(owner_id, job_id, lease_id, service_id)` share
one provision operation and endpoint. A failed initialization stays failed for
that lease; retrying acquire cannot silently create a replacement. Client wait
timeout stops waiting, not the controller's bounded initialization. No manual
release API in v1: cleanup is owned by the controller when the job ends/expires.

The lease identity and private bootstrap files are persisted before provisioning.
Recovery verifies ownership, catalog/fingerprint and existing state; mismatch
fails rather than adopting another instance. Worker exit/cancellation/lifetime
expiry reclaims every owned service/adapter/network/data allocation independently
of adapter success or project cleanup SQL. No sibling service/host access is
granted. Services have no public egress in v1; only the requesting worker and its
initialization/check containers can reach the declared ports. Checks and all
initialization share the configured startup deadline and resource budget.

## Adapter ABI v1

The adapter image provides `/ci-adapter`. The controller starts it with `init`,
then `check`, in disposable isolated containers using the pinned image; jobs
cannot replace this command. Both read a root-owned read-only
`/run/ci-adapter/request.json` containing `version: 1`, `lease_id`, `owner_id`,
`job_id`, `service_id`, `endpoint`, `config`, and `secret_files` (ID to private
absolute file path). Every field is required; the check receives an empty
`secret_files` map. Files are generated by the controller, not copied from a PR.

`init` receives only its own service bootstrap files and writes
`/outputs/consumer.json`. The service can receive explicitly declared bootstrap
files; neither worker nor check receives them. `check` mounts `/outputs` read-only
and verifies the actual consumer credentials/behavior against its own instance.
The private adapter output stays UID 0 (mode 0400 recommended); adapters must not
chown it to the worker UID. Both adapter phases run as UID 0 with all capabilities
dropped. Only the controller copies a validated result into the worker publication
tree as UID 1001 mode 0400. Internal and worker-visible output files are distinct.
Success requires both exits to be zero, valid output, and ownership validation.
Adapters get no Docker socket, host paths, other job mounts, privileged mode or
controller/GitHub/cloud credentials. Their stdout/stderr are not forwarded to
job/controller logs or artifacts; only sanitized phase and exit status are kept.
Application naming, SQL, permissions and repeat/partial-initialization semantics
are implemented and tested in the trusted adapter. General CLI cannot run an
arbitrary adapter script. An adapter digest/config change creates a new static
fingerprint, never a retroactive change to a running job.

## Fingerprint v1

The controller calculates SHA-256 over canonical UTF-8 JSON of this exact object:

```text
{
  "version": 1,
  "adapter_abi": 1,
  "platform": "linux/arm64",
  "controller_image": <resolved immutable reference>,
  "runner_image": <resolved immutable reference>,
  "tools_seed": <immutable seed hex or empty string>,
  "dependency_seed": <immutable seed hex or empty string>,
  "policy_digest": <sha256 of resolved isolation/network/resource/output policy>,
  "catalog": <entire validated, resolved version-1 catalog>
}
```

v1 JSON accepts only ASCII strings/keys, booleans, null, arrays, objects, and
integers within +/-9007199254740991; duplicate keys, floats and unknown fields
are rejected. Canonical encoding sorts object keys lexicographically, preserves
array order, uses compact `,`/`:` separators, standard JSON escaping and no final
newline. Materialize every field/default before hashing; `secret_files` contains
recipient declarations, never generated values. Equivalent port lists are sorted
during validation. The config digest uses the same canonical encoding.

The entire catalog is hashed, not only acquired services: route can compute the
same fingerprint as the later database job without starting any services.
Resolved controller policy includes network rules, resource ceilings, mount/output
rules and admission/trust policy revisions; secrets and arbitrary machine paths
are excluded. A static change to image, adapter, layout/config, limits, policy,
platform or seeds changes the fingerprint. Random lease/job IDs, endpoint IPs,
passwords, timestamps and assigned host ports are never hashed. Runtime envelope
validation, not cache identity, authenticates those per-job values. Deployment
identity remains an independently checked binding; it is not a secret.

The consumer bumps its receipt/context version to 2, replaces the PG-image field
with `services_fingerprint`, and rejects old/missing context for reuse. Continue
checking source/base/workflow/event/ref/classification and actual latest job
evidence. All stages compare their fingerprint with route; mid-run deployment
changes fail closed. Existing separate runner/seed fields may remain for clarity.

## Migration acceptance

The consuming project owns its adapter, service declaration, tool manifest,
wrapper, service-specific validation and verification receipts. The runner owns
the generic catalog validator, CLI/files, adapter lifecycle, fingerprint and
cleanup. For deployments still using the former interface, retain the pinned
controller and project interface until both sides pass migration acceptance.
Do not deploy the intermediate suffix-parameter change as the final architecture.

Validate two concurrent isolated jobs; non-admin consumer credentials; partial,
foreign, changed or extra-owned database rejection in the project suite; output
and log secrecy; init failure/cancellation/recovery cleanup; deterministic
fingerprints and changed-input/old-receipt rejection. Use a non-database dummy
service for generic runner tests. Then drain, retain rollback config/images,
switch the reviewed catalog/adapter/controller/consumer together, and confirm a
real project database job. Staging/production persistent databases are untouched.

## Operator policy and current implementation

`SERVICES_CATALOG_FILE` and `SERVICES_POLICY_FILE` point to root-owned, non-group/
world-writable files in a controller-only read-only mount. Policy has exactly
`version: 1`, `max_services` (0..4), `max_resources` (the same five resource fields),
`storage_targets` (exact allowlist), and `trust_revision` (reviewed policy revision).
The canonical resolved policy digest also binds the network, output, tmpfs and
adapter isolation revisions and resource-sharing rule. See `examples/services`
for an empty deployment; each consuming project owns its populated catalog.

Limits bound the aggregate server/adapter/helper budget: server 12/16, adapter
2/16, namespace 1/16, firewall 1/16. Only one init/check runs at a time; all stages
share the persisted startup deadline. Minimum memory is 256 MiB, CPU 160 millis,
PIDs 64 and data 4096 bytes. Data targets split the declared tmpfs capacity;
container `/run` and `/tmp` each have 16 MiB caps charged to the memory budget.
Adapters have private disk scratch for their one output; the trusted adapter is
responsible for keeping scratch bounded, while publication rejects outputs over
64 KiB. Service-declared anonymous Docker volumes not covered by catalog storage,
and all adapter-declared anonymous volumes, are rejected.

In v1 the service/init/check containers share a dedicated network namespace
behind a firewall installed before application startup. Each has its own root
filesystem and PID namespace. The namespace exposes only declared ports to the
requesting worker, cannot initiate host/sibling/public traffic, and blocks Docker
embedded DNS. The image entrypoint may use limited filesystem/UID capabilities
to drop privilege; it never gets NET_ADMIN. Init/check have all capabilities
removed. No service restart policy is installed: stopped tmpfs services fail
closed instead of resetting data under the same lease. Controller recovery
preserves a live instance and rejects changed configuration/ownership.

## Native cache persistence is separate from service leases

The optional incremental cache policy is operator-owned generic directory metadata,
not a service adapter API. Neither service acquisition nor project outputs can
publish caches or supply trusted source identity. The controller validates the
GitHub run/job/branch independently and removes workers and service writers before
reading cache directories. No service/worker receives the snapshot volume mount.
Disabling/missing caches cannot bypass service validation or change test results.
Traversal metadata is bounded by actual JSON-escaped path lengths plus per-entry
overhead; ordinary hashed filenames do not consume a worst-case escaping budget.
See the README native incremental cache contract for bounds, private copies,
compatibility, rollback and fixed-volume ownership. Consumer-specific compiler or
transform paths belong in the operator policy, never this generic core.

## Optional host admission and teardown

A service may declare `teardown: {"checks": ["resource-absent"], "admission": true}`.
It requires `JOB_CLEANUP_JOURNAL=on`; the declaration is fingerprinted and the
original spec persists before provisioning. Admission binds an operator-issued
per-lease payload before service creation. Cleanup runs the pinned adapter after
worker absence, with init secrets RO and separate `/terminal` outputs, then
persists only the exact allowlisted boolean checks before deleting private data.
Missing proof stays unknown while ordinary resource reclamation continues.
See [terminal evidence](cleanup-observations.md) for the envelope, ordering,
bounds, persistence, compatibility and retention contract. Old controllers
reject this optional catalog field; old catalogs require no changes.


## Trusted job inputs

The optional `ci-job-inputs/v1` policy is independent of service payload semantics.
It preserves the service v1 lifecycle and does not grant an adapter teardown
ownership. `fleet.start` seals `ci-job-input-spec/v1` (name, owner, volume, complete
Docker spec) under `inputs-v1/<owner>/<runner>/spec.json`, then calls the reviewed
adapter's `prepare`. The spec retains JIT credentials and stays root-only through
its 0700 parent; workers never receive it. There is one controller Docker create.

The adapter receives bounded stdin `{schema:"ci-job-input-call/v1", operation,
directory}` and no output is logged. `prepare` returns 0 before creation. The
controller publishes `ci-job-assignment/v1` from authenticated scale-set events
(repository, run ID, opaque scheduler job ID, runner name/ID, event). It does not
invent attempt or numeric Actions job ID. The project adapter must resolve those
with its authenticated provider and existing owner release before publication.
Maintenance callback identity must be independently bound by that adapter.

Only configured subdirectories are mounted RO; a separate generic gate directory
is mounted at `/run/ci-input-gate`. Existing firewall/network readiness runs first.
The job-start hook then validates root-owned 0444 `ready.json` against native
repository/run/attempt, after the adapter has checked numeric job/runner identity
and published project input. Missing input waits up to 900s; invalid input fails.
The binding schema is `ci-job-input-ready/v1` with repository, run_id, attempt and
positive numeric job_id. Candidates cannot create bindings or replace the hook
before the first step. No owner/private spec/token/journal mount enters workers.

The coordinator alternates durable active-runner/receipt cursors and executes one
scope with a 60s process-group deadline per turn. It advances before invocation;
75 rotates/retries, 0/2 settles. The active lane uses the controller’s current
runner set, including gates already released into an in-job wait; stale backlog
cannot consume those live jobs’ wait window. Reopen resumes both cursors. New admission stops
above 1000 retained scope records; existing scopes continue below the 10000-entry
inventory ceiling. Orphans/settled records remain diagnostic evidence pending
operator retention review, never deletion authority. Controller disk/proxy/service/
registration/job/lease/terminal/teardown ownership is unchanged. Adapter exit does
not authorize a business release or independent acceptance.

A rollback stops new demand first, preserves active-job mounts and receipt
reconciliation, and retains compatible project readers. Disabling inputs is not
a way to run workflows requiring them. Independent cross-repository review and
explicit installation are required before enabling this optional interface.

### Authenticated allocation sessions (opt-in v2)

`ci-job-inputs/v2` retains the sealed spec, assignment, gate and cleanup ownership
above. Before Listener starts, the image-owned runner launcher interposes on the
pinned Actions Runner 2.337.0 `Runner.Worker spawnclient` inherited anonymous pipe.
It reads the first `NewJobRequest` frame (int32 message type, int32 byte length,
UTF-16LE JSON) before starting the actual Worker. The initial message is bounded
to 8 MiB. Unknown types, malformed frames and authentication timeout fail closed.
This is a version-specific protocol adapter: a runner upgrade requires protocol
regression evidence before enabling v2 with that version.

The controller creates `/run/ci-intake/session.sock` inside this job's root-owned
RO input subpath. It accepts once and removes the socket name before executing
the pinned policy command. The runner sends one bounded JSON line:
`{schema:"ci-job-intake/v1",message:<original allocation object>}`. The controller
supplies the adapter with `{schema:"ci-job-input-session/v1",directory,message}`
over stdin. The directory comes from the controller, never from that message.
The generic core does not interpret project credentials or grant state authority.
The installed adapter must authenticate the original allocation against the
controller assignment, live provider and its exact installed workflow policy.

The adapter's first stdout byte must be ASCII `0`; only that byte is forwarded.
It must publish the normal immutable gate before acknowledging. Until then no
Worker, hook, checkout or workflow step starts. All other stdout/stderr is
discarded. Subsequent input, EOF, cancellation, session timeout, controller exit
or runner reclamation revoke the helper process group. The controller waits for
session teardown before closing its input root. The socket has no reconnect,
credential refresh or arbitrary-command protocol. Recovery never revives a
consumed token; a new assigned job must establish its own session. The v1 timer
adapter is disabled for v2 scopes, so no token-file fallback runs concurrently.

V2 additionally permits explicitly declared RO executable mounts below a
project-owned `/opt/<namespace>/<subdirectory>`; core runtime/tool paths remain
reserved. The policy command's root-owned ancestry and exact SHA256 are checked
at startup and invocation. A project must separately pin its executable closure,
private operation allowlist, provider checks and state publication guards. A v2
policy does not itself provide those project guarantees or deployment authority.

The [version-specific Worker protocol](worker-intake-protocol.md) specifies
message validation, in-session bounded waiting, native exit/cancellation and
repeatable offline executable checks. A project returning 75 must keep the same
helper alive and wait before its first `0`; the one-shot bridge does not reconnect
or retry an exited helper. Writes to the host socket and Worker pipe remain
interruptible. Worker group cleanup includes descendants after leader exit.

Optional pre-Worker stage observations follow
[intake diagnostics (v2 records / v1 producer wire)](intake-diagnostics-v1.md); they grant no service authority
and preserve the one-use intake and ready protocol.
Provider decode/run comparison reasons remain helper observations only; the
project retains its strict decoder, identity predicates and gate authority.

V2 job intake requires [allocation startup session binding](intake-startup-binding.md)
before the unchanged helper/provider gate. This is a one-use existing-channel
capability, separate from diagnostic observations and OS process identity.
