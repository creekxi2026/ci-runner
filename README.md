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

The [blank configuration template](.env.example) is also available for manual setup. All image digests and `IMAGE_REVISION` must come from the same successful build's environment; the controller rejects floating workload images, wrong architectures and a mismatched runner revision. All three image pushes must succeed before the deployment artifact is published. Partial candidate pushes never update default deployment references.

## Lifecycle and storage

- Only the controller runs without demand. Each job receives a private runner, PostgreSQL, proxy and internal IPv4/IPv6 network. There are **no persistent CI volumes or shared writable caches**.
- A failed new runner or transient GitHub polling/acknowledgement failure does not tear down other jobs. Startup adopts existing live jobs before acquiring a GitHub session. Cleanup and lifetime checks operate independently of GitHub polling.
- Idle, unassigned runners expire after `RUNNER_IDLE_TIMEOUT_SECONDS` (default **300**). All runners have an absolute creation-based lifetime of `RUNNER_MAX_LIFETIME_SECONDS` (default **3600**); controller restart does not reset it. The job-start hook prevents idle reclamation racing assignment; its job-writable marker is not a security credential.
- Graceful stop stops scheduling and drains work until it exits or reaches its lifetime. Compose allows up to the configured lifetime for this drain. A hard controller crash preserves live job containers. Actual removal can be delayed by Docker unavailability; labeled cleanup state is retained for recovery.
- Runner root filesystems are read-only. `/home/runner` uses a **2 GiB** per-job tmpfs and `/tmp` a **128 MiB** tmpfs. PostgreSQL data uses a **512 MiB** tmpfs. tmpfs consumes the container's memory budget; a job can fail on memory/workspace exhaustion rather than consume unbounded host disk.
- Runner memory is limited to **1536 MiB**, PostgreSQL to **512 MiB**, proxy to **128 MiB**, and controller to **256 MiB**. Logs rotate at 2 × 5 MiB per container. Job resources are reclaimed after exit, completion or timeout, with failed cleanup retried.
- The pinned Go 1.26.3 and Node 24.14.0 toolchains are image-managed. Each job seeds private toolcache links to read-only image binaries; setup-go/setup-node can reuse these versions without downloading them. Other requested versions still require public network access.
- Runner images default to `GOPROXY=https://goproxy.cn` with `GOSUMDB=sum.golang.org`; checksum verification remains enabled. Workflows can override these defaults. Set appropriate `GOPRIVATE`/`GONOPROXY`/`GONOSUMDB` before requesting private modules to avoid disclosing private module paths to public services. This setting affects Go modules only, not GitHub, Node or other traffic.
- Immutable images remain in the Docker daemon until explicitly retired. Updates are cloud-built; no local build cache is needed. Review and remove exact unused CI image references when appropriate; there is no global pruning or cleanup of other deployments.

## Network and trust boundary

Job INPUT/OUTPUT are deny-by-default. Outbound traffic is limited to the job's HTTP/CONNECT proxy (public TCP/80 and TCP/443 only) and its PostgreSQL TCP/5432. The proxy checks every DNS answer, rejects nonpublic addresses, and dials a verified numeric address without resolving again. SSH, UDP and arbitrary external database access are unavailable.

A trusted deployment may set `PUBLIC_EGRESS_UPSTREAM_PROXY` to an existing credential-free HTTP proxy (for example, `http://host.docker.internal:7897` on a Mac with that proxy). Only the per-job gateway receives this setting. DNS validation still happens first and the upstream CONNECT target is the verified numeric IP, never the job-supplied hostname. Direct egress remains the default; this does not change host or OrbStack network settings.

A short-lived NET_ADMIN helper installs namespace firewall rules and is removed before registration. Jobs have all capabilities dropped, no sudo, no host mounts, no Docker socket and no controller administration token. Docker service/container Actions are not supported. The controller itself has Docker-root-equivalent authority; do not expose its socket or run untrusted controller source. Containers share the VM kernel; this is not a separate VM boundary against kernel exploits.

## Updates and development

Download a new successful deployment artifact. Update the existing `.env` **image digest pair, source revision and PostgreSQL digest**, preserving private settings and credentials. Pull all selected images before `docker compose up -d`; do not replace `.env` wholesale with a generic template. Runner automatic updates are enabled separately from image/toolchain publishing.

Base images and PostgreSQL are digest pinned. The exact official PostgreSQL dependency is mirrored by the cloud build into GHCR, so the execution host does not need Docker Hub access. Apt uses fixed, signed repository snapshots; signature checks remain enabled (snapshot expiry checks are intentionally disabled). Refresh snapshots and dependencies through a reviewed image rebuild, not mutable installation during jobs.

```sh
go test -race ./...
go vet ./...
python3 -m unittest discover -s tests -v
```

Dependabot update-generation should use GitHub-hosted runners, not this ARM fleet: its self-hosted requirements specify Linux x64 and Docker. Keep product PR/security checks and their triggers intact.
