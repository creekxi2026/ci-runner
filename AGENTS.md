# Runner maintenance boundaries

This repository provides a generic CI runner. Keep project behavior in consuming
repositories and operator-owned declarations, not in controller/runner code.

- Do not add project/repository/service-name branches, database SQL or naming
  rules, application environment exports, or SDK-specific directory assumptions
  to the generic core. Parameterizing a business-specific API is not decoupling.
- Projects own service images, initialization/check adapters, configuration,
  consumer output schemas, tool-directory manifests and deployment workflows.
  The runner treats project config/output as opaque data within the validated
  [service v1 envelope](docs/service-contract-v1.md).
- Retain generic trust and lifecycle enforcement: reviewed digest-pinned inputs,
  per-job isolation, bounded resources, private credentials, atomic read-only
  publication, deterministic fingerprints and cancellation/recovery cleanup.
  A project adapter must not bypass these controls.
- Use a non-business dummy service for core tests. Real project validation may
  be supplied as an external fixture; do not copy its SQL or business source into
  this repository. Publish only controller and runner images here.
- Update the README and service contract with interface changes. Run the relevant
  contract, migration and release regressions; reuse valid runtime evidence when
  runtime behavior is unchanged. Do not rebuild/deploy for documentation alone.
- Obtain an independent review of service-boundary changes and closeout claims.
  Record findings, fixes and evidence limits. An agent review is not a GitHub
  human approval. Static regression checks do not prove all possible future
  business coupling is absent.

## Fixed volumes and test cleanup

- On the shared Docker/OrbStack host, reuse `ci-work-linux-arm64` for all CI
  controllers and `ci-deps-linux-arm64-cache` for the existing opt-in persistent
  dependency cache. Keep dependency-cache trust lanes and per-job isolation;
  sharing a work volume does not authorize sharing writable job data.
- Do not create volumes named after a job, date, test or migration batch on that
  host. Put owned temporary work in private subdirectories of the fixed work
  volume. Never mount the volume root or another job's directory into a worker.
  Tests requiring a dedicated volume must use an isolated disposable Docker
  daemon, or obtain explicit authorization for an exception with a cleanup plan.
  A new persistent volume also requires explicit operator approval.
- Before reusing or migrating storage, check the actual volume, schema, owner,
  permissions, controller mounts and service configuration. Incompatibility is
  a migration problem to resolve explicitly, not a reason to silently introduce
  another date-named volume. Prepare and verify data and configuration first;
  drain affected jobs before switching, retain a working rollback until verified,
  then remove only the retired volume after checking references and live config.
- Track temporary containers, directories, networks and browser sessions created
  by each test. Clean them after success, failure or interruption, and verify
  removal. Record the purpose and removal condition for retained diagnostics or
  rollback material. If execution is interrupted before cleanup, recover those
  exact owned resources on the next run.
- Do not prune globally or remove active jobs, other tasks' resources, fixed
  volumes, Agent Runtime homes or shared caches as test cleanup. Preserve shared
  templates still selected by a deployment or an intentional rollback.
