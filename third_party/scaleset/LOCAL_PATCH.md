# Local diagnostic patch (RUIF-93)

This is a **locally patched** complete snapshot of
`github.com/actions/scaleset v0.4.1-0.20260916214619-e6daac702355`.
The upstream module ZIP is authenticated by
`h1:+DtFsbhUIxDt1vJL+xMPCPTZzE7nw+K4KTqqwUOFqIc=`.
That h1 describes the original ZIP, **not this patched tree**. LICENSE is the
unmodified upstream MIT license. UPSTREAM.json records the original file hashes,
ZIP SHA256 and individual patch hashes.

The nil-default WithDiagnostics option installs bounded typed enqueue operations
and observes the existing retry client's request hook and retry policy result.
It does not replace the HTTP transport, consume response bodies, change retry
policy results/timeouts/error handlers, or alter refresh locks/requests/returns.
The upstream logger stays Discard unless the caller explicitly changes it using
the existing option. No arbitrary callback or I/O executes in refresh locks.

The diagnostic writer is one separate goroutine with a bounded queue, a 2 KiB
line limit and a 20 MiB output cap. Close takes a caller deadline; a physically
blocked writer can remain until process exit. Loss/writer failure makes evidence
incomplete, and never changes business return values. The salt is supplied in
memory; raw session IDs never enter the event queue. Only closed stages, enums,
numbers and trusted frozen identity fields are serialized.

Root `go test ./...` does not test this nested module. Its full race/vet entries
are explicitly included in `.github/workflows/images.yml`. Run them locally too.
The Docker build copies this tree before go mod download, so the root replace
has the same source closure in a container. This is not an upstream release or
an authorization to publish or install a controller/runner pair.
