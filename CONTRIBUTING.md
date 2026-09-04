# Contributing

Development happens on [GitLab](https://gitlab.com/quittymr/shoulder-daemon) and on
[GitHub](https://github.com/QuittyMR/shoulder-daemon). Both are live: open an issue or
a change on whichever you already have an account for, and it will be answered there.
The two are kept in sync, so please do not open the same thing twice.

## Getting a checkout running

```bash
git clone https://gitlab.com/quittymr/shoulder-daemon.git
cd shoulder-daemon
make build          # bin/shoulderd and bin/advisor-echo
make test
make lint           # golangci-lint, the same version and config CI runs
make cover          # coverage per module
```

`make doctor` builds and then runs `shoulderd doctor`, which reports whether the daemon,
the adapters and the settings on this machine actually agree with each other. Run it
before you believe a manual test.

`make install-plugins` replaces the copy of the adapter your harness loaded with the one
in this checkout. Editing `adapters/` without it changes nothing your editor sees, and
the failure is silent - the stale copy keeps posting to the address it was built against.

Running from a checkout, a container or a service manager is covered in
[docs/INSTALL.md](docs/INSTALL.md).

## House rules

**The standard library is the dependency budget.** Neither `relay` nor `advisor-echo`
has a `go.sum`, and both should stay that way. A change that adds a module needs to
argue for itself in the merge request; a memory or model backend belongs behind the
existing interfaces, not in `go.mod`. The one piece of third-party material in the
tree is `relay/internal/memory/vectors/vectors.bin`, public-domain GloVe weights
with their provenance in the NOTICE beside it; `mkvectors.go` there rebuilds it
from the published file in minutes, so it is data with a recipe rather than a blob
nobody can account for.

**Nothing new goes on the hook path.** The relay answers a harness hook while the user's
turn is open. Network calls and synchronous disk I/O there are the one thing that breaks
the design, and `make bench` is the check - it runs `BenchmarkHookRoundTrip`, and a
change that moves that number needs to say why in the merge request.

**Comments explain why, never what.** The existing ones name the trap the code is
avoiding. Do not restate the logic and do not describe the session that produced the
change.

**Tests come with the behaviour.** Every package that has behaviour has a `_test.go`
beside it, and `make test` runs all of them with no setup. Three suites need more than
that and are behind build tags, so a contributor with none of it installed still gets a
green run:

```bash
cd relay
go test -tags integration ./integration/...                        # a real editor
SHOULDER_MEMORY_URL=… go test ./internal/memory/ -run Live         # a real memory service
SHOULDER_MEMORY_URL=… go test -tags compare ./internal/memory/ -v  # both stores, side by side
```

The **integration** suite drives a real Claude Code or OpenCode through the adapters and
asserts on what the daemon saw: sessions observed, hooks authenticated, the daemon
started and stopped, and — in `integration/memory_test.go` — that a fact learned in one
turn is recalled for a later one worded differently, that facts survive the daemon
exiting between sessions, that a local fact never reaches another project's session,
that an unreadable store costs the facts and nothing else, that two projects at once
keep their own places, and that the generated token reaches the harness and is then
enforced. It needs the editor on `PATH` and will not run in CI. The OpenCode half drives
a free model by default and skips itself when that endpoint is not answering, which it
often is not; `SHOULDER_IT_MODEL` points it at one that is, and a run against a paid
model costs a few one-word turns.

The **live** tests exercise a connector against a running memory service, and skip
without `SHOULDER_MEMORY_URL`.

The **compare** suite is a measurement rather than an assertion: it loads the built-in
store and mcp-memory-service with the same corpus, asks the same questions, and prints
where each ranked the record that answers each one. Run it after anything that touches
scoring, the embedding table or deduplication — the numbers move, and a change that
improves one group of questions usually costs another. `TestCompareIdentifierFamily` is
the case worth keeping honest: eight facts that differ only in a port number, which a
store that reads meaning and not identifiers will happily collapse into one.

**Commit messages say what the change is for.** Lower case, no prefix taxonomy, and a
line that describes the intent rather than the mechanism - `key a project by its root
commit` rather than `change project ID hashing`. Keep the diff to one coherent change.

## Before you open a merge request

```bash
make lint && make test && make bench
```

CI runs the same three on both platforms, plus `govulncheck`, and on GitLab coverage, SAST
and secret detection. The live provider suite runs on a weekly schedule from `main` only.

## Cutting a release

One number has to agree in three places: the tag, `adapters/claude-code/.claude-plugin/plugin.json`,
and a `## [X.Y.Z]` section in `CHANGELOG.md`. `make release-check TAG=vX.Y.Z` proves it and
prints the notes that will go on the release.

```bash
make release-check TAG=v0.2.0
make release TAG=v0.2.0
```

`make release` runs `scripts/tag-release.sh`, which creates three tags and pushes them to every
remote: `v0.2.0`, which the pipelines build from, and `relay/v0.2.0` and `advisor-echo/v0.2.0`,
which are what the Go module proxy reads - each module lives in a subdirectory, and Go only
recognises a nested module's tag when it carries the directory as a prefix. Without those two,
`go install ...@latest` keeps resolving to a pseudo-version of `main`.

Both pipelines then build the binaries, push the images, and create the release with the
changelog section as its notes. The plugin picks the new binary up on the next editor start
once the old daemon has exited.

New harness support means a new directory under `adapters/`, a matching entry in
`scripts/install-plugins.sh`, and captured hook payload fixtures under
`testdata/hook-payloads/<harness-version>/`. A new decision model means a connector in
`relay/internal/llm/` and a row in the connector table in the README. A new memory
backend means implementing the five-method interface in `relay/internal/memory/` and
passing the conformance suite in `conformance.go` — the built-in store passes the
same suite as the one that talks to a service.

## Security

Do not open a public issue for a vulnerability. See [SECURITY.md](SECURITY.md).
