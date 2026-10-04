# ci-runner

On-demand ARM64 GitHub Actions runners for Docker/OrbStack. Uses the official non-Kubernetes [scale-set client](https://github.com/actions/scaleset), currently public preview. Cloud builds publish separate `controller` and `runner` targets to GHCR.

**Pre-audit deployment:** only `workflow_dispatch` jobs are acquired. This repository's smoke workflow deliberately fails slot 4 to test failed-job cleanup. No product workflow migration is included.

## Start

Requirements: Linux ARM64 Docker daemon (OrbStack on Apple Silicon), Compose, repository administrator credentials and public registry access.

1. Copy `compose.yaml` and `.env.example` into a private deployment directory; rename `.env.example` to `.env`.
2. Set the repository URL and unique scale-set name in `.env`. Create `.secrets/github-token` (mode 0600) containing an authorized repository administration token. Never commit it or put it in runner environment variables.
3. Pull images first:

```sh
docker compose pull
docker pull ghcr.io/creekxi2026/ci-runner:runner
docker pull postgres:17-bookworm
docker compose up -d
```

Use `runs-on: ci-mac-arm64-smoke` for manually dispatched test jobs. Change the label in both deployment and workflow together. The hard capacity limit is three. With no demand, only the controller remains.

Update with `docker compose pull`, `docker pull` for the runner, and `docker compose up -d`. Prefer digest-pinned images in deployed `.env`. Runner automatic updates are enabled independently of image publishing.

## Isolation and storage

- Each job has its own internal IPv4/IPv6 network, runner, PostgreSQL and public-only HTTP/CONNECT proxy. Job OUTPUT is deny-by-default except loopback, its proxy TCP/3128 and PostgreSQL TCP/5432. A short-lived NET_ADMIN helper installs rules in the runner network namespace, then exits before registration; jobs have all capabilities dropped and no sudo.
- Proxy resolves once, rejects any nonpublic result, and dials the verified address directly. Ports 80/443 only. SSH, UDP and arbitrary external database access are intentionally unavailable. Use HTTPS checkout; workflows needing other protocols require separately reviewed support.
- No host Docker socket, host mount or administration token reaches jobs. Service/container Actions requiring Docker are not supported. Controller has Docker-root-equivalent authority; never expose it to job networks or run untrusted controller source.
- PostgreSQL data is bounded to a 512 MiB per-job tmpfs, with no published port. Job resources are removed on completion regardless of result, on runner exit, graceful stop and controller restart. No shared writable cache or persistent job volumes.
- Runner writable layers are disposed after each job. Docker daemon disk quotas are not imposed; logs are rotated to 2 × 5 MiB per container. Images occupy daemon storage until explicitly retired. No global pruning.

The namespace firewall and proxy need actual-host verification; configuration alone is not a security certification. Containers share the Linux VM kernel, so this is not a VM boundary against kernel exploits. Public fork events, PR target and workflow_run are not acquired. Production event/trust support and default migration require independent audit.

## Development

```sh
go test ./...
python3 -m unittest discover -s tests -v
docker compose --env-file .env.example config --quiet
```

Dependabot update-generation should use GitHub-hosted runners, not this ARM fleet: GitHub's self-hosted requirements specify Linux x64 and Docker. Update-generation does not count toward included Actions minutes; ordinary CI on Dependabot PRs follows normal Actions billing. Keep PR/security required checks and their triggers intact.
