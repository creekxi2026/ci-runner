# CI runner cache

A persistent cache for trusted CI jobs. The volume is named `ci-deps-linux-arm64`;
there is no application, machine-local, issue or version suffix in its name.
The schema/owner/repository/trust-lane labels match the controller's contract.
Initialize with the controller's exact configuration values; an existing volume
with different labels is rejected. Source code, controller jobs and this profile
all use the root-level `cache-init.py` and `cache-env.sh`.

Application lockfiles and dependency seeds are explicit preparation inputs;
they are not requirements of this runtime profile.

## Usage

```sh
python3 profiles/cache/create-cache.py \
  --owner "$DEPLOYMENT_ID" --repository "$GITHUB_CONFIG_URL" \
  --lane "$DEPENDENCY_CACHE_TRUST_LANE"
docker compose -f profiles/cache/compose.yaml run --rm verify
```

A new volume starts with empty package-manager caches and an empty optional tools
directory. It does not require an application lockfile or seed image. The helper
uses an already available generic Linux runner image (`--image` selects another);
it does not download an image implicitly. The supplied Compose recipe pins the
current ARM64 runner. Choose a matching runner image for another architecture.

To import an existing, idle trusted CI cache without redownloading:

```sh
python3 profiles/cache/create-cache.py \
  --owner "$DEPLOYMENT_ID" --repository "$GITHUB_CONFIG_URL" \
  --lane "$DEPENDENCY_CACHE_TRUST_LANE" --import-volume EXISTING_CACHE
```

The source must be a managed CI cache with the `data` layout used by this profile.
The CLI refuses a source used by running containers. Keep writers stopped for the
whole migration. It copies into a staging directory, verifies every regular file's
SHA256 and every symlink target, then publishes the directory atomically. It never
deletes the source. `--rename cache/old=cache/new` can rename an immediate internal
namespace after verification. Both source and target tool/input directories must
be trusted; this command does not turn untrusted build outputs into trusted ones.

Repeated initialization leaves matching volumes intact. Compose declares the
volume external, so removing jobs does not remove data. Set `--volume NAME` and
`CI_RUNNER_CACHE_VOLUME=NAME` together when an independently isolated trust domain
needs its own volume. Do not create a volume per ordinary job. A different repository/deployment/trust
scope must use a separate explicit name; labels prevent accidental cross-scope adoption.

## Layout and permissions

| Subdirectory | Use | Access |
| --- | --- | --- |
| `data/npm/_cacache` | Package archives/index | UID/GID 1001, read/write |
| `data/gomod` | Go module downloads and extracted modules | UID/GID 1001, read/write |
| `data/go-build/<target>-<Go>-<ABI>` | Compiled packages/test binaries | UID/GID 1001, read/write |
| `data/jest/node<major>` | Jest transforms; Jest also keys source/config | UID/GID 1001, read/write |
| `data/pip` | pip download/wheel cache | UID/GID 1001, read/write |
| `data/tools` | Optional reviewed tools/wheelhouse | Root-owned, job mount read-only |

The task sources `cache-env.sh` (the Compose entrypoint does this). Node/Go/Python
versions are detected from installed tools; no application or toolchain version is
hardcoded into the environment script. Python in the optional tools directory is
registered only when present. Setup markers use the detected x64/ARM64 architecture.
`CI_CACHE_ABI` in Compose identifies the supplied base's native-library ABI. Update
it when native libraries change; Go additionally keys source, compiler and flags.
A Jest caller can use `CI_JEST_CACHE_DIR` as its `cacheDirectory`.

npm logs and npx installations remain in private HOME. Application source,
node_modules, writable virtualenvs, credentials, Git configuration and databases
remain private. Root-owned optional tool distributions can be read by every trusted
job; make a private copy for tools requiring writes. No agent HOME or agent cache
is mounted or chowned.

The initial imported contents cover the application previously used for acceptance.
They do not imply that every future repository's dependencies are already present.
Warm only missing lockfile entries once; subsequent jobs reuse the downloaded data.
For fail-fast offline acceptance use `npm_config_offline=true`, `GOTOOLCHAIN=local`
and `GOPROXY=off`. The supplied Compose check has networking disabled; authenticated
checkout and current vulnerability databases still require separate network access.

## Scope and cleanup

This is the preparation and local-verification entrypoint for the controller's
cache contract, not a parallel scheme. `DEPENDENCY_CACHE_VOLUME` and
`CI_RUNNER_CACHE_VOLUME` should select the same volume. Updated controllers mount
that volume automatically; standalone Compose mounts it explicitly.

Previously deployed controllers still use the older hash-named v1 flat-layout
volume until the updated image/configuration is delivered. This source change
neither publishes an image nor switches them. Keep their old volume while they
reference it. Prewarmed standalone data can be migrated to the unified volume,
verified, then its redundant standalone volume retired. Do not retire a live
controller's volume as part of standalone cleanup. Mixed-event cache stays off.

Cache size is not a hard quota. Keep one persistent copy for this trust domain,
measure growth, and prune package-manager-owned caches in an idle maintenance
window. Do not share writable build caches with untrusted PR jobs, and do not
use broad Docker volume pruning as cache maintenance.
