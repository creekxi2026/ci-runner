# Historical stage diagnostic archives

The custom ci-stage-v1 producer and SDK patch are retired; this reader is retained
for existing evidence, not as a new task prerequisite or deployment gate.

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

## Offline validation

Run `python3 -m unittest discover -s tests -p test_diagnostic_archive_local.py -v`.
The optional layered replay still requires historical event artifacts; current
controllers do not produce them. No archived data is rewritten by this change.
