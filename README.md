# ci-runner

On-demand Linux ARM64 GitHub Actions runners for Docker/OrbStack, using the official non-Kubernetes [scale-set client](https://github.com/actions/scaleset) (public preview).

The default is a **trusted, manually dispatched test fleet**. Other trusted private fleets can explicitly opt into supported events; defaults and the public smoke workflow remain manual-only. Production adoption requires independent audit and explicit cutover authorization.

## Quick start

Requirements: an ARM64 Linux Docker daemon (OrbStack on Apple Silicon), Compose, and authorized repository administration credentials.

1. Download `compose.yaml` and `env.txt` from the deployment artifact of a **successful** `Cloud ARM64 images` workflow. Rename `env.txt` to `.env`. GitHub artifact downloads require a GitHub login.
2. Set `GITHUB_CONFIG_URL` and a unique `SCALE_SET_NAME` in `.env`. Create `.secrets/github-token` with the authorized token; restrict it to mode `0600`. Never commit credentials or add them to runner environments.
3. Pull all three digest-pinned images, then start the controller:

```sh
# Run in the private directory containing compose.yaml and .env.
set -a; . ./.env; set +a
docker compose pull
docker pull "$RUNNER_IMAGE"
docker pull "$POSTGRES_IMAGE"
docker compose up -d
```

Use the selected scale-set label in `runs-on`. The included smoke workflow defaults to four successful jobs with hard concurrency **three**. Set `fail_slot4=true` only when deliberately exercising failed-job cleanup.

The [blank configuration template](.env.example) is also available for manual setup. All image digests and `IMAGE_REVISION` must come from the same successful build's environment; the controller rejects floating workload images, wrong architectures and a mismatched runner revision.

### Image publication and deployment snapshots

The cloud workflow publishes three separate GHCR packages: `ghcr.io/<owner>/ci-runner-controller`, `ghcr.io/<owner>/ci-runner-runner`, and `ghcr.io/<owner>/ci-runner-postgres`. Each package has only the `latest` tag; no source-revision or build-ID tags are generated. The full source SHA remains in OCI labels and the deployment artifact.

Publication runs are serialized without cancelling an in-progress run. ARM64 candidates are pushed by digest without tags. All three publication digests must validate before any `latest` promotion begins. Promotions preserve the single-manifest digest rather than creating a new index. Only after all promotions succeed does the workflow generate and upload the deployment artifact.

Registry multi-tag publication is **not atomic**: a failed promotion can leave a mixture of old and new `latest` tags. Failed candidate pushes do not change `latest`, and failed promotions do not produce a deployment artifact. The successful artifact—not the mutable tags—selects a coherent deployment. Its `env.txt` fixes all three role-package references as `ghcr.io/<owner>/ci-runner-<role>@sha256:...` for that deployment. Jobs never resolve `latest`; existing deployments keep their fixed snapshots until explicitly updated. Older untagged digests are not a guaranteed registry archive, so retain the selected images locally or in an approved archive if long-term rollback is required.

## Lifecycle and storage

- Only the controller runs without demand. Each job receives a private runner, proxy and internal IPv4/IPv6 network. PostgreSQL is **opt-in per job**, not created by default. There are **no persistent CI volumes or shared writable caches**.
- A failed new runner or transient GitHub polling/acknowledgement failure does not tear down other jobs. Startup adopts existing live jobs before acquiring a GitHub session. Cleanup and lifetime checks operate independently of GitHub polling.
- Idle, unassigned runners expire after `RUNNER_IDLE_TIMEOUT_SECONDS` (default **300**). All runners have an absolute creation-based lifetime of `RUNNER_MAX_LIFETIME_SECONDS` (default **3600**); controller restart does not reset it. The job-start hook prevents idle reclamation racing assignment; its job-writable marker is not a security credential.
- Graceful stop stops scheduling and drains work until it exits or reaches its lifetime. Compose allows up to the configured lifetime for this drain. A hard controller crash preserves live job containers. Actual removal can be delayed by Docker unavailability; labeled cleanup state is retained for recovery.
- Runner root filesystems are read-only. `/home/runner` uses a **2 GiB** per-job tmpfs and `/tmp` a **128 MiB** tmpfs. PostgreSQL data uses a **512 MiB** tmpfs. tmpfs consumes the container's memory budget; a job can fail on memory/workspace exhaustion rather than consume unbounded host disk.
- Runner memory is limited to **1536 MiB**, PostgreSQL to **512 MiB**, proxy to **128 MiB**, and controller to **256 MiB**. Logs rotate at 2 × 5 MiB per container. Job resources are reclaimed after exit, completion or timeout, with failed cleanup retried.
- The pinned Go 1.26.3 and Node 24.14.0 toolchains are image-managed. Each job seeds private toolcache links to read-only image binaries; setup-go/setup-node can reuse these versions without downloading them. Other requested versions still require public network access.
- Runner images default to `GOPROXY=https://goproxy.cn` with `GOSUMDB=sum.golang.org`; checksum verification remains enabled. Workflows can override these defaults. Set appropriate `GOPRIVATE`/`GONOPROXY`/`GONOSUMDB` before requesting private modules to avoid disclosing private module paths to public services. This setting affects Go modules only, not GitHub, Node or other traffic.
- Immutable images remain in the Docker daemon until explicitly retired. Updates are cloud-built; no local build cache is needed. Review and remove exact unused CI image references when appropriate; there is no global pruning or cleanup of other deployments.

## Network and trust boundary

`RUNNER_ALLOWED_EVENTS` is a comma-separated explicit allowlist. Empty/unset means
`workflow_dispatch` only. Supported names are `workflow_dispatch`, `push`,
`pull_request`, `pull_request_target`, and `schedule`; unknown names, empty list
elements and wildcards fail startup closed. A trusted private fleet can set
`push,pull_request,pull_request_target,schedule,workflow_dispatch` without widening
any existing public fleet. Capacity remains three regardless of event selection.

This allowlist is **not source/fork attestation**. The pinned scale-set SDK supplies
owner/repository, workflow reference, run ID and event name, but no fork/head
repository or actor trust/approval metadata. Do not enable PR events for untrusted
workloads on that basis. Repository-level registration plus privately controlled
workflow/ref/actor policy must be independently audited before cutover, especially
for `pull_request_target` workflows that check out or execute PR-controlled code.
No additional source trust is inferred or invented by the controller.

Set `PUBLIC_EGRESS_DOH_URL=https://dns.alidns.com/resolve` in the deployment `.env` to use a trusted HTTPS JSON DNS resolver inside the per-job public gateway. This does not modify Mac/OrbStack/system DNS or give the workload direct DNS access. TLS verification stays enabled; all returned IPv4/IPv6 addresses must still be public, and failed HTTPS DNS does not fall back to synthetic system answers. This resolver handles workload destinations, not the controller's own GitHub API DNS. Leave it empty to retain ordinary DNS.

Job INPUT/OUTPUT are deny-by-default. Outbound traffic is limited to the job's HTTP/CONNECT proxy (public TCP/80 and TCP/443 only) and its PostgreSQL TCP/5432. The proxy checks every DNS answer, rejects nonpublic addresses, and dials a verified numeric address without resolving again. SSH, UDP and arbitrary external database access are unavailable.

A trusted deployment may set `PUBLIC_EGRESS_UPSTREAM_PROXY` to an existing credential-free HTTP proxy (for example, `http://host.docker.internal:7897` on a Mac with that proxy). Only the per-job gateway receives this setting. DNS validation still happens first and the upstream CONNECT target is the verified numeric IP, never the job-supplied hostname. Direct egress remains the default; this does not change host or OrbStack network settings.

A short-lived NET_ADMIN helper installs namespace firewall rules and is removed before registration. Jobs have all capabilities dropped, no sudo, no host mounts, no Docker socket and no controller administration token. Docker service/container Actions are not supported. The controller itself has Docker-root-equivalent authority; do not expose its socket or run untrusted controller source. Containers share the VM kernel; this is not a separate VM boundary against kernel exploits.

## Optional PostgreSQL

Run `ci-postgres` only in jobs that need a disposable database. In GitHub Actions it appends connection settings to `$GITHUB_ENV` for **subsequent steps**. For the current step use `ci-postgres <command> ...`, for example `ci-postgres psql -v ON_ERROR_STOP=1 -c 'SELECT 1'`. It does not print credentials or require `eval`.

The fixed per-job request creates at most one bounded PostgreSQL companion with a generated password; repeated requests reuse it. The controller waits for readiness outside the janitor, adds only this database's TCP/5432 namespace allowance, and publishes configuration atomically. It never accepts arbitrary images, Docker commands or another job's database through the request. Without a request there is no database container, database environment or TCP/5432 allowance. Cleanup includes requested databases. A controller restart conservatively preserves ambiguous job claims until the original hard lifetime.

`POSTGRES_DATABASE_PREFIX` defaults to empty, preserving the generic `ci` database
and bootstrap-role behavior. A non-empty operator prefix enables a trusted
controller-provisioned **pair**: `<prefix><24 lowercase hex>` and that primary name
plus `_staging`, both owned by the same non-admin login role (`NOSUPERUSER`,
`NOCREATEDB`, `NOCREATEROLE`, `NOREPLICATION`, `NOBYPASSRLS`, no role memberships).
Prefixes must start with a lowercase letter, contain only lowercase letters,
digits or underscores, end in underscore and be at most 31 bytes, keeping both
database identifiers within PostgreSQL's 63-byte limit. A private application
fleet may configure `closet_ai_test_`; no application scripts are baked into images.

Names derive from the deployment/job identity; the login password derives with
HMAC from bootstrap state preserved privately in the owned disposable cluster.
Retries and controller recovery reuse the same pair/password. Changed prefixes,
foreign container ownership, partial pairs, inconsistent ownership or elevated
role privileges fail closed without publishing worker configuration or silently
repairing state. Failed partial creation requires disposal of the job/cluster,
not manual repair inside a running job. Do not change lease mode/prefix while jobs
are active. Cluster data is disposable, not a persistent recovery database.

With a configured prefix workers receive **only the non-admin** credentials:
`CI_DATABASE_NAME`, `CI_DATABASE_COMPANION_NAME`, `DATABASE_URL`,
`CI_DATABASE_HOST`, `PGHOST`, `PGPORT`, `PGDATABASE`, `PGUSER`, `PGPASSWORD`.
Both databases are verified using that login before publishing the mode-0600
environment file atomically. Secrets use scoped Docker exec environment and SQL
stdin, never process argv or stdout/logs. The privileged provisioning session
also disables PostgreSQL statement/error/duration logging so rejected ownership
or privilege checks cannot leak password-bearing SQL into server logs.
`$GITHUB_ENV` exports are whitelisted;
connection settings are secret-bearing files and must not be printed/uploaded.
All provisioning/verification/publication shares the existing 45-second readiness
budget and immutable job lifetime; the janitor never waits on SQL readiness.

## Updates and development

Download a new successful deployment artifact. Update the existing `.env` **image digest pair, source revision and PostgreSQL digest**, preserving private settings and credentials. Pull all selected images before `docker compose up -d`; do not replace `.env` wholesale with a generic template. Runner automatic updates are enabled separately from image/toolchain publishing.

Base images and PostgreSQL are digest pinned. The exact official PostgreSQL dependency is mirrored by the cloud build into GHCR, so the execution host does not need Docker Hub access. Apt uses fixed, signed repository snapshots; signature checks remain enabled (snapshot expiry checks are intentionally disabled). Refresh snapshots and dependencies through a reviewed image rebuild, not mutable installation during jobs.

```sh
go test -race ./...
go vet ./...
python3 -m unittest discover -s tests -v
```

Dependabot update-generation should use GitHub-hosted runners, not this ARM fleet: its self-hosted requirements specify Linux x64 and Docker. Keep product PR/security checks and their triggers intact.
