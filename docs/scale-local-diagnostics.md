# Local Scale diagnostics (RUIF-93, group 33)

The additive `local` object on `ci-stage-v1` belongs only to the inner Scale
logical **end** event. Its existing op_id, parent_op_id and logical_attempt bind
it to the outer retry; the outer end remains propagation, with no local object.
No error is wrapped, parsed, formatted or replaced to produce this observation.
The side channel does not repair or bypass any message, identity or cleanup gate.

The SDK validates the closed origin/reason pairs before retaining a value copy;
unknown is explicit. Invalid observations count as loss, never enter the writer.
All optional numbers/booleans are emitted as null when not observed. Numbers are
non-nil Available/Started/Completed counts, and a **one-based original handler
slice index**, including preceding nil entries. This ordinal is not a message ID,
runner ID, job ID, workflow ID or business attribution. Message nil leaves counts
null; a present, empty message gives zero. `acquired` reflects the existing
messageProgress read/write, not whether this invocation made an acquire request.

`name_valid` comes from the actual cleanup or input validator; those validators
retain their distinct original rules. `state_exists` reuses the existing state()
result (including its existing lazy initialization). `journal_exists=true` means
its existing open succeeded, false means ENOENT; other open errors remain null.
`journal_safe` describes the existing descriptor metadata/size validation, not
JSON correctness or ownership fields within the record. A failed fstat is null; a subsequent read clears earlier journal metadata before observing it again.
No file, Docker, HTTP, environment or raw message read is added for diagnostics.
These are observations at those original operations, not a later atomic snapshot.

## Return-point map and callers

All existing callers retain nil-observation wrappers. The only enabled path is
Scale -> observeSchedulerObserved / observeInputAssignmentObserved. Scheduler
passes the same optional observation into readObserved/updateObserved and its
identity callback. Other recovery/cleanup callers keep the original wrappers.
Input publication's independent assignJobInputs path keeps its original wrapper
and validation; inputAssignment delegates to the same validator with nil.

| Actual return condition | origin / reason | Caller / propagation |
| --- | --- | --- |
| cleanup name validator fails | cleanup_journal / invalid_cleanup_identity | readObserved -> scheduler; update's repeated guard uses same reason |
| cleanup OpenFile fails | cleanup_journal / cleanup_open_failed | readObserved -> scheduler/update; ENOENT consumed by original success/create path clears this tentative failure |
| fstat, regular/single-link/uid/mode/size check fails | cleanup_journal / unsafe_cleanup_record | readObserved -> scheduler/update |
| existing bounded stream read errors or exceeds 65536 | cleanup_journal / invalid_cleanup_record_size | decodeRecord on the already opened descriptor -> readObserved |
| strict JSON/canonical decode fails | cleanup_journal / invalid_cleanup_record | decodeRecord -> readObserved |
| schema/owner/job differs | cleanup_journal / foreign_cleanup_record | decodeRecord -> readObserved |
| scheduler repository/run/runner/job identity fails | scheduler_observation / invalid_scheduler_identity | observeSchedulerObserved -> Scale |
| old/new scheduler identity or result conflicts | scheduler_observation / scheduler_identity_changed | update's original callback -> scheduler -> Scale |
| update marshal/size, no-change sync, temp removal/open/write/chmod/fsync/close/rename/directory sync returns error | cleanup_journal / cleanup_update_failed | updateObserved fallback only when no deeper classification exists; original error returned unchanged |
| input name validator fails | input_assignment / invalid_input_runner | inputAssignmentObserved -> input observer -> Scale |
| input runner/run/repository identity fails | input_assignment / invalid_input_assignment | same validator; independent assignJobInputs uses nil wrapper |
| input spec Stat fails except consumed ENOENT | input_assignment / input_spec_stat_failed | input observer -> Scale |
| assignment ReadFile fails except consumed ENOENT | input_assignment / input_assignment_read_failed | input observer -> Scale |
| old sealed bytes differ | input_assignment / sealed_input_changed | input observer -> Scale |
| assignment b.write returns any error | input_assignment / input_assignment_write_failed | fallback at actual write boundary; no claim about a deeper file operation |
| completion cleanup retains the job | completion_cleanup / completion_cleanup_pending | Scale after its original cleanup/snapshot; no claim about cleanup's internal root cause |
| cancellation, acquire, demand/reconcile/start or otherwise unclassified error | unknown / unknown | existing Scale return; no inherited child classification or guessed cause |
| successful Scale (including nil/initial/done) | none / none | same inner end; unexecuted fields remain null |

Only the first deepest confirmed failure is retained. Expected absent journals
are cleared when the original caller consumes ENOENT; a later update write error
therefore receives cleanup_update_failed. Each new event clears prior event
metadata. After all message handlers succeed, handler details clear before demand
or start so those errors cannot borrow a prior handler's observations.

## Consumer and version binding

`scripts/diagnostic_archive.py` is the generic, offline companion reader, derived
from the accepted owner-probe-operation-v2 `input/package/source/diagnostic_archive.py`
(original SHA256 `0e8afd76eb5d4a09bfb03617c800a2119ebb7fde2ca496a3405987233702aa3b`).
The group-32 reader has identical bytes. The companion accepts exactly the old
key set or that set plus the closed `local` object; unexpected keys/types/enums,
wrong event placement and all old identity/sequence/cap checks still fail.
Ordinary stderr is discarded and JSON-shaped stderr remains strict. The event
object, including local, survives stdout/stderr filtering and events.ndjson.
Deduplicated accepted local observations/failures and fixed reasons additionally
appear in manifest.local_scale; old input without local retains the old manifest
shape. Histories and frozen inputs are never rewritten or reinterpreted.

Python 3.9 accepts only certain fractional timestamp widths. The reader now pads
or truncates fractional seconds to six digits **only for cursor arithmetic**;
original Go/Docker timestamp text is retained. This fixes real Go output with
four/five fractional digits and does not weaken the event timestamp grammar.

The reader must accompany this producer revision; the prior reader rejects its
new fields. This source candidate is not a runnable deployment bundle: real
matching controller/worker digests, metadata/binary binding and a fresh accepted
operation input remain required before any new window. Frozen installer,
executor c290ea06, SOURCE 4f864ae2 and historical owner02 are not changed.

## Reproducible local entries

Run root and nested race/vet/build independently (root ./... excludes the nested
module). For a narrow layered replay with Go 1.26.9:

```sh
mkdir -p /path/to/private-evidence/events
R33_EVENT_OUTPUT=/path/to/private-evidence/events go test \
  -run '^TestDiagnosticScale|^TestDiagnosticCleanupRecord|^TestDiagnosticLocalListen' -count=1 -v .
R33_EVENT_OUTPUT=/path/to/private-evidence/events \
R33_ARCHIVE_OUTPUT=/path/to/private-evidence python3 -B -m unittest discover \
  -s tests -p test_diagnostic_archive_local.py -v
go -C third_party/scaleset test -race -run '^TestDiagnostic' ./...
python3 -B -m unittest discover -s tests -p test_diagnostic_module.py -v
```

The Scale matrix uses real handlers, synthetic files and a rejecting transport;
it compares nil/on/full/failed/short/panicking/blocked writers, two attempts and
exact injected error identity/Is/As, ordered request traces and message/resource
state. The listener test executes the actual listen/retry path with one poll,
two local failures, one acquire and no ack. The bounded-read size/read-fault
return is tested through its extracted stream helper, without a filesystem race.
The companion test executes actual foreground subprocess pipes with finite Docker
executables, samples stdout/stderr and verifies lossless event preservation and
manifest consumption. These fixtures are not native GitHub/daemon/permission or
business deployment evidence, and cannot recover owner02's missing branch.
