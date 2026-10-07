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
