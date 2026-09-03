# Changelog

Notable changes to shoulder-daemon. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[semantic versioning](https://semver.org/spec/v2.0.0.html).

No version has been tagged yet. Everything below is what `main` has accumulated since the
project started, and it is what a first `v0.1.0` would contain.

## [Unreleased]

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
