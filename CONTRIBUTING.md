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
existing interfaces, not in `go.mod`.

**Nothing new goes on the hook path.** The relay answers a harness hook while the user's
turn is open. Network calls and synchronous disk I/O there are the one thing that breaks
the design, and `make bench` is the check - it runs `BenchmarkHookRoundTrip`, and a
change that moves that number needs to say why in the merge request.

**Comments explain why, never what.** The existing ones name the trap the code is
avoiding. Do not restate the logic and do not describe the session that produced the
change.

**Tests come with the behaviour.** Every package that has behaviour has a `_test.go`
beside it. Live provider and live memory tests skip themselves when their credentials
are absent, and the harness integration suite is behind the `integration` build tag:

```bash
cd relay && go test -tags integration ./integration/...
```

Those need a real Claude Code or OpenCode on `PATH` and will not run in CI.

**Commit messages say what the change is for.** Lower case, no prefix taxonomy, and a
line that describes the intent rather than the mechanism - `key a project by its root
commit` rather than `change project ID hashing`. Keep the diff to one coherent change.

## Before you open a merge request

```bash
make lint && make test && make bench
```

CI runs the same three on both platforms, plus `govulncheck`, coverage, and on GitLab SAST
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
passing the conformance suite in `conformance.go`.

## Security

Do not open a public issue for a vulnerability. See [SECURITY.md](SECURITY.md).
