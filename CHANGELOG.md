# Changelog

Notable changes to shoulder-daemon. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[semantic versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.3.0] - 2026-09-05

### Added

- `shoulderd monitor` follows the daemon's log and shows only the facts moving: stored,
  superseded, merged and dropped by the tidying pass, refused or failed writes, and advice
  queued and injected, one line each with the text. It opens on the last twenty and waits
  for more; `--all`, `--no-follow` and `--json` change that.
- A fact written with `shoulderd fact add` or `fact update` is logged like one the model
  deduced, with `origin=cli`, so the monitor shows it too.
- The Claude Code plugin links the daemon it fetches into `~/.local/bin`, so `shoulderd`
  is a command in a terminal and not only a path the plugin knows. It says so once if that
  directory is not on `PATH`.
- The README has a section on configuration and tweaking: pickiness level by level,
  monitoring, and the choice of storage backend.

### Changed

- The daemon logs to a file by default, `~/.local/share/shoulder-daemon/shoulderd.log`, as
  well as to stderr. It used to log to stderr alone unless `SHOULDER_LOG` named a file, and
  the adapters start it with stderr closed, so a plugin install kept no log at all.
  `SHOULDER_LOG` still moves the file; `SHOULDER_LOG=stderr` turns it off. A file past 8 MB
  is moved to `.1` at the next start.
- The decision pass speaks in a second case. It used to inject only when a stored fact
  contradicted what the assistant was about to do, so a fact that said how this codebase
  does the thing just asked for stayed in the store while the assistant searched the
  repository for the same answer. Now such a fact is surfaced at the prompt, before the
  search starts. A live test pins it on every configured provider beside the contradiction
  and silence cases.
- A model call that takes more than five seconds is logged as a warning with the session,
  the step and the error if there was one. Stalls on the way to the provider used to leave
  a trace only when they outlived the twenty-second client timeout, and none at all when
  they came in just under it.

## [0.2.0] - 2026-09-04

### Added

- A memory store inside the daemon. It keeps facts in one JSON file -
  `~/.local/share/shoulder-daemon/facts.json`, or wherever `SHOULDER_MEMORY_PATH` points -
  and it is what runs when no `SHOULDER_MEMORY_URL` is set, so an install that starts
  nothing else still remembers. A daemon whose file cannot be opened logs the reason and
  falls back to storing nothing rather than refusing to start.
- Recall by meaning with nothing installed. An embedding table is compiled into the binary
  and a fact is ranked by the rarity-weighted mean of its words, which matches a question
  worded differently from the fact that answers it. The weights are the first 40,000 short
  lower-case words of the public-domain `glove.6B.100d` vectors at one signed byte per
  dimension; `relay/internal/memory/vectors/NOTICE` records the source and the generator
  beside it rebuilds the table.
- The shared secret is generated rather than asked for. On first start the daemon makes a
  token, keeps it in `~/.local/share/shoulder-daemon/token`, and writes it into its env
  file and into `env.SHOULDER_TOKEN` of `~/.claude/settings.json`, leaving the rest of that
  file as it was. Until it sees one correct token it accepts hooks without one, because the
  editor that launched it read its environment before the value existed; after that first
  correct header every request must carry it. Setting `SHOULDER_TOKEN` yourself overrides
  all of it and nothing of yours is written.
- The daemon reads its own env file. Every setting can live in
  `~/.config/shoulder-daemon/env` (or `$SHOULDER_ENV_FILE`), which the CLI and the OpenCode
  adapter already read, so a key set in a login shell is no longer invisible to a daemon an
  editor started from a desktop launcher. The process environment still wins over the file.
- `shoulderd doctor` says whether anything is being remembered: `ok`, `none`, or
  `unreachable` with the store's error. It asks the daemon, which does a real read against
  the backend, because a URL that resolves proves nothing about a backend that refuses every
  request.
- The decision pass sees the whole turn on Claude Code. The Stop hook carries only the last
  assistant message, so the daemon reads the rest from the session transcript the hook
  names, accepted only when it is an absolute path under `.claude/projects` ending in
  `.jsonl`. When the file cannot be read it keeps the hook's message and says so once per
  session, and counters report how often each of those happened.
- `make memory` starts mcp-memory-service from the compose file, which now carries it
  behind a profile: `make up` leaves it alone, and pointing the daemon at it still takes
  `SHOULDER_MEMORY_URL`.
- A `compare` test suite that loads the built-in store and mcp-memory-service with the same
  corpus and prints where each ranked the record that answers each question. It is a
  measurement to run after touching scoring or the embedding table, not an assertion;
  `CONTRIBUTING.md` describes it with the other three suites.

### Changed

- With no memory service named the daemon uses its own store instead of storing nothing.
  The warning that told you to set `SHOULDER_MEMORY_URL` is gone, and the error a refused
  `fact add` returns now says the daemon has no store at all and names both
  `SHOULDER_MEMORY_PATH` and `SHOULDER_MEMORY_URL`.
- The idle timer is on by default at 60 minutes, where it was off. A harness that dies
  without sending `SessionEnd` no longer leaves a daemon on the machine until the next
  reboot; `SHOULDER_IDLE_EXIT_MINUTES=0` restores the old behaviour.
- Model connectors share one HTTP client that pings idle HTTP/2 connections and drops the
  ones that do not answer. A silently dropped connection used to queue every later request
  on a dead stream and burn the full 20-second timeout, one call after another, until the
  process restarted; it now costs one failed call and a fresh dial.
- The compose file mounts Claude Code transcripts read-only at the host path the hook
  payload names, keeps the daemon's facts on a named volume that survives `down`, maps the
  container user to yours under rootless podman, and turns off SELinux confinement for that
  one container: a labelled home directory is refused to a confined container whatever its
  uid, and relabelling a home directory is the worse alternative. A facts volume created
  before the user mapping is owned by the old id and has to be re-owned once;
  `docs/INSTALL.md` has the command.
- README and `docs/INSTALL.md` are rewritten around the install that needs no store, no
  model to pull and no token. The README keeps the two-step plugin install and the model
  choice; the binary by hand, a memory service instead of the built-in store, and every
  setting there is moved to `docs/INSTALL.md`.
- Linting runs the house golangci-lint set - govet, gosec, revive, gocritic, gofumpt and
  goimports - with this repository's exclusions written beside their reasons, and the README
  badge points at `.golangci.yml`. Held back: `fieldalignment`, whose autofix reorders struct
  fields under positional literals, and the three complexity caps, which sixteen functions
  still exceed.
- `replay` and the live hook path share one transcript reader, so a replayed session is
  parsed by the same code as a real one. No flags changed.

### Fixed

- A consult could wedge a session. The advisor claim was released after the "consult over"
  signal rather than before it, so the next event, posted by whoever waits on that signal,
  was skipped as already in flight, and with nothing else coming the session waited forever.
  GitLab's shared runners under the race detector hit it about half the time.
- The daemon's log file is created readable only by its owner, where it was world-readable.
- `advisor-echo` serves with a header read timeout, so a client that sends headers slowly
  can no longer hold a connection open indefinitely.
- Four places where an error was shadowed by an inner assignment, and test fixtures written
  with loose permissions.

### Removed

- The Codecov badge and integration. GitLab computes coverage itself and its badge already
  works, so a third-party service holding a token for a number the pipeline prints anyway
  is gone.
- The Go Report Card badge, replaced by the linter-config badge.

## [0.1.1] - 2026-09-03

### Added

- `make release TAG=vX.Y.Z` creates the three tags a release needs - the release tag and one
  per Go module - and pushes them to every remote. Without the module tags `go install` never
  resolves `@latest` to a release.

### Fixed

- A pipeline test that gave a consult two seconds before calling it stalled, which GitLab's
  shared runners exceeded under the race detector. The v0.1.0 tag pipeline failed on it, so
  that version has a GitHub release only; this one has both.

## [0.1.0] - 2026-09-03

The first tagged release.

### Added

- A local Go daemon that watches a coding session over harness hooks, keeps a store of
  facts, and injects the relevant ones back into the session. The working agent is never
  asked to manage its own memory.
- Adapters for **Claude Code** (marketplace plugin, HTTP hooks, a `SessionStart` script
  that revives the daemon) and **OpenCode** (a plugin that gets the daemon's env file and
  reports session end). Both are tested against a real harness under the `integration`
  build tag.
- Model connectors for Gemini, OpenRouter, z.ai (`glm` and `glm-coding`), OpenCode Go,
  OpenAI, and local Ollama, with `SHOULDER_LLM` taking a comma-separated fallback chain.
- A memory interface with five methods and a conformance suite, so a backend can be
  swapped without touching the pipeline. Running with no memory URL stores nothing.
- Fact supersession and consolidation: a new fact that contradicts a stored one settles
  the collision rather than appending beside it, and the store is tidied instead of only
  growing.
- The `shoulderd` command line - `message`, `fact add/list`, `digest`, `doctor`,
  `config`, `help`. `config` reads and changes log level, pickiness, provider and model on
  a running daemon with no restart.
- Pickiness control over how readily a turn is judged worth acting on.
- `replay`, which re-runs whatever a provider hung off a tool call.
- An injection budget, redaction before anything leaves the machine, and per-project scope
  keyed by the repository's root commit so a rename or a move does not orphan its facts.
- `shoulder_hook_latency_seconds` and the rest of the Prometheus metrics.
- `scripts/install-plugins.sh`, which owns the settings that name a path, and
  `make update`, which rebuilds, reinstalls the adapters and restarts the daemon in the
  order that works.
- Documentation: [architecture](docs/ARCHITECTURE.md), [install](docs/INSTALL.md), and the
  [advisor protocol](docs/ADVISOR.md) for bringing your own decision model.
- `shoulderd version`, and `doctor` reporting the build, where it came from, and whether a
  newer release exists.
- The Claude Code plugin fetches the release binary for its platform on first start,
  checksum-verified, so the plugin is the whole install.
- Release binaries for Linux, macOS and Windows on amd64 and arm64, with `SHA256SUMS`, on
  every tag; multi-arch images at `ghcr.io/quittymr/shoulder-daemon` and
  `registry.gitlab.com/quittymr/shoulder-daemon`.

### Fixed

- A dead launch no longer wedges every start that follows it.
- The daemon waits before believing a session's last goodbye, so a harness that pauses is
  not mistaken for one that quit.
- Advice is delivered while it can still change what the agent does, rather than after the
  turn has committed.
- A fact that mixes a lasting claim with a momentary one is rejected instead of stored.
- The ranked envelope from the by-tag endpoint is decoded correctly, and `searchByTag`
  results are unwrapped properly.
- Session notes are remembered as the store accepted them, not as they were offered.

[Unreleased]: https://github.com/QuittyMR/shoulder-daemon/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/QuittyMR/shoulder-daemon/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/QuittyMR/shoulder-daemon/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/QuittyMR/shoulder-daemon/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/QuittyMR/shoulder-daemon/releases/tag/v0.1.0
