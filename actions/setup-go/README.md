# Setup Go: bounded cache timer lifetime

This explicitly maintained distribution is `actions/setup-go` at
`924ae3a1cded613372ab5595356fb5720e22ba16` (v6.5.0, Node 24), with **one change**:
the cache download `Promise.race` clears its timer in `finally`, including when
the download rejects. The upstream success-only `then` leaves a 600-second timer
alive after some transfer failures. The first transport error is not fixed by
this change. Cache restore/save, tokens, permissions and download limits retain
upstream behavior.

Use `creekxi2026/ci-runner/actions/setup-go@<reviewed full commit SHA>` with the
same inputs as upstream. No downloaded official action is silently rewritten at
runtime, no global Node timer is monkeypatched, and no network download is added
by this fork. Bundles are committed because GitHub JavaScript actions execute
their distribution directly; they are not installed into runner images.

`upstream.json` records the original file SHA256 digests. The MIT license is
retained. Run `python3 scripts/verify-setup-go-patch.py` to reverse the one patch
in memory and verify every original file byte. To reproduce, download those four
files from the pinned upstream commit, then run that script with `--apply`.
Run `node scripts/check-cache-timer.cjs` for real child-process lifetime checks.

Track https://github.com/actions/toolkit/issues/2470. Remove this fork and return
callers to a fixed, pinned upstream action after the same regression passes.
