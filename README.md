# ci-runner

On-demand Linux ARM64 GitHub Actions runners for Docker/OrbStack, using the official non-Kubernetes [scale-set client](https://github.com/actions/scaleset) (public preview).

This is a **trusted, manually dispatched test fleet**. It does not migrate product workflows or acquire public fork, PR, push or workflow-run events. Production adoption requires independent audit and explicit cutover authorization.

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

The [blank configuration template](.env.example) is also available for manual setup. All image digests and `IMAGE_REVISION` must come from the same successful build's environment; the controller rejects floating workload images, wrong architectures and a mismatched runner revision. Both image pushes must succeed before the deployment artifact is published. Partial candidate pushes never update default deployment references.

## Lifecycle and storage

- Only the controller runs without demand. Each job receives a private runner, PostgreSQL, proxy and internal IPv4/IPv6 network. There are **no persistent CI volumes or shared writable caches**.
- A failed new runner or transient GitHub polling/acknowledgement failure does not tear down other jobs. Startup adopts existing live jobs before acquiring a GitHub session. Cleanup and lifetime checks operate independently of GitHub polling.
- Idle, unassigned runners expire after `RUNNER_IDLE_TIMEOUT_SECONDS` (default **300**). All runners have an absolute creation-based lifetime of `RUNNER_MAX_LIFETIME_SECONDS` (default **3600**); controller restart does not reset it. The job-start hook prevents idle reclamation racing assignment; its job-writable marker is not a security credential.
- Graceful stop stops scheduling and drains work until it exits or reaches its lifetime. Compose allows up to the configured lifetime for this drain. A hard controller crash preserves live job containers. Actual removal can be delayed by Docker unavailability; labeled cleanup state is retained for recovery.
- Runner root filesystems are read-only. `/home/runner` uses a **2 GiB** per-job tmpfs and `/tmp` a **128 MiB** tmpfs. PostgreSQL data uses a **512 MiB** tmpfs. tmpfs consumes the container's memory budget; a job can fail on memory/workspace exhaustion rather than consume unbounded host disk.
- Runner memory is limited to **1536 MiB**, PostgreSQL to **512 MiB**, proxy to **128 MiB**, and controller to **256 MiB**. Logs rotate at 2 × 5 MiB per container. Job resources are reclaimed after exit, completion or timeout, with failed cleanup retried.
- Immutable images remain in the Docker daemon until explicitly retired. Updates are cloud-built; no local build cache is needed. Review and remove exact unused CI image references when appropriate; there is no global pruning or cleanup of other deployments.

## Network and trust boundary

Job INPUT/OUTPUT are deny-by-default. Outbound traffic is limited to the job's HTTP/CONNECT proxy (public TCP/80 and TCP/443 only) and its PostgreSQL TCP/5432. The proxy checks every DNS answer, rejects nonpublic addresses, and dials a verified numeric address without resolving again. SSH, UDP and arbitrary external database access are unavailable.

A short-lived NET_ADMIN helper installs namespace firewall rules and is removed before registration. Jobs have all capabilities dropped, no sudo, no host mounts, no Docker socket and no controller administration token. Docker service/container Actions are not supported. The controller itself has Docker-root-equivalent authority; do not expose its socket or run untrusted controller source. Containers share the VM kernel; this is not a separate VM boundary against kernel exploits.

## Updates and development

Download a new successful deployment artifact. Update the existing `.env` **image digest pair, source revision and PostgreSQL digest**, preserving private settings and credentials. Pull all selected images before `docker compose up -d`; do not replace `.env` wholesale with a generic template. Runner automatic updates are enabled separately from image/toolchain publishing.

Base images and PostgreSQL are digest pinned. Apt uses fixed, signed repository snapshots; signature checks remain enabled (snapshot expiry checks are intentionally disabled). Refresh snapshots and dependencies through a reviewed image rebuild, not mutable installation during jobs.

```sh
go test -race ./...
go vet ./...
python3 -m unittest discover -s tests -v
```

Dependabot update-generation should use GitHub-hosted runners, not this ARM fleet: its self-hosted requirements specify Linux x64 and Docker. Keep product PR/security checks and their triggers intact.
