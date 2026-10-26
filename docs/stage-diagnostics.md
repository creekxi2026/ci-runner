# Retired custom stage diagnostics

The controller no longer emits ci-stage-v1 events or uses a patched scaleset SDK.
It depends on the exact original upstream module recorded in go.mod and go.sum.
Nonempty CI_DIAGNOSTIC_PROFILE or CI_DIAGNOSTIC_OWNER_UID settings reject startup
before credential or GitHub access, rather than silently dropping requested evidence.
Remove a retired diagnostic profile only after closing any window that needs it;
retain its prior matching image pair to finish historical work if required.

This retirement does not alter retries, cancellation, scheduler acknowledgments,
cleanup journals, service checks or bounded proxy logs. Historical stage records
remain readable by the [offline archive reader](scale-local-diagnostics.md).
