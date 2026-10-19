# Contributing to lockmq

Thanks for your interest. Bug reports, fixes and new test cases are welcome.
For security issues, follow [SECURITY.md](SECURITY.md) instead of opening an
issue.

## Before you start

- For anything beyond a small fix, open an issue first to agree on the
  approach. lockmq is a security tool: features that loosen a default, add a
  way around the console, call a broker endpoint outside the allowlist, or
  add a dependency need a clear case.
- Code, comments, docs and commit messages are in English.
- lockmq is built on [lockkit](https://github.com/DavidGodefroid/lockkit):
  the console, approvals, configuration review, IPC, secrets and PII
  primitives live there. A change to them is a lockkit pull request.
- By contributing you agree that your work is licensed under the Apache
  License 2.0.

## Build

```sh
CGO_ENABLED=0 go build ./cmd/lockmq
```

Requires Go 1.26 or later. Every target builds with `CGO_ENABLED=0`; keep
dependencies pure Go.

To work on lockmq and lockkit together, use an uncommitted `go.work`:

```
go 1.26.0

use (
	.
	../lockkit
)
```

## Tests

```sh
go vet ./...
go test ./...
```

Unit tests need no broker: the management API is served by
`httptest.Server` with fabricated JSON fixtures.

### Integration tests

Integration tests run RabbitMQ 3.13 and 4.1 in Docker with the stream
plugin enabled. Containers are bound to `127.0.0.1` and removed at the end.

```sh
go test -tags integration ./test/integration/
```

Never point a test at a real or remote broker.

### Fuzzing

Fuzz targets run nightly in CI; run one locally with
`go test -run '^$' -fuzz '^FuzzMaskNoPanic$' -fuzztime 30s ./internal/msgmask/`.
A failing input is written under the package's `testdata/fuzz/`; commit it
with the fix, so `go test` replays it from then on.

## Pull requests

- Write the failing test first, then the fix.
- Keep `gofmt`, `go vet`, `staticcheck` and `govulncheck` clean.
- Keep the security invariants: no secret in argv, environment, files, logs,
  the socket protocol or error strings; no protocol method that loosens the
  policy; no broker call outside the allowlist; fail closed on anything
  ambiguous.
- One logical change per pull request, with a commit message that explains
  why.

Pull requests target `main`. The CI checks (`test`, `lint`, `integration`)
must pass. Pull requests are squash-merged: the title becomes the commit
subject and the description its body. Use a conventional prefix (`feat:`,
`fix:`, `docs:`, `chore:`, ...).

Participation is governed by the [code of conduct](CODE_OF_CONDUCT.md).

## Releases

Releases are built by GoReleaser (`.goreleaser.yaml`) when a `v*` tag is
pushed: `.github/workflows/release.yml` runs the unit tests, then builds
linux and darwin on amd64 and arm64, signs `checksums.txt` with cosign
(keyless), attaches an SBOM per archive and the Linux packages. To check
the configuration locally without signing or publishing:

```sh
goreleaser check
goreleaser release --snapshot --clean --skip=sign
```

The output goes to `dist/`, which is ignored by git.
