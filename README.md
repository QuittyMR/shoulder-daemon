<p align="center">
  <img src="docs/assets/hero.webp" width="900"
       alt="A dark oil painting: a young person in a white collar looks away, while a small winged daemon perched on their shoulder leans in to whisper in their ear.">
</p>

# shoulder-daemon

[![CI](https://github.com/QuittyMR/shoulder-daemon/actions/workflows/ci.yml/badge.svg)](https://github.com/QuittyMR/shoulder-daemon/actions/workflows/ci.yml)
[![pipeline](https://gitlab.com/quittymr/shoulder-daemon/badges/main/pipeline.svg)](https://gitlab.com/quittymr/shoulder-daemon/-/pipelines)
[![coverage](https://gitlab.com/quittymr/shoulder-daemon/badges/main/coverage.svg?job=test)](https://gitlab.com/quittymr/shoulder-daemon/-/pipelines)
[![release](https://img.shields.io/github/v/release/QuittyMR/shoulder-daemon?sort=semver)](https://github.com/QuittyMR/shoulder-daemon/releases/latest)
[![Go](https://img.shields.io/github/go-mod/go-version/QuittyMR/shoulder-daemon?filename=relay%2Fgo.mod&logo=go&logoColor=white)](relay/go.mod)
[![golangci-lint](https://img.shields.io/badge/linted%20with-golangci--lint-00ADD8?logo=go&logoColor=white)](.golangci.yml)
[![Go Reference](https://pkg.go.dev/badge/gitlab.com/quittymr/shoulder-daemon/relay.svg)](https://pkg.go.dev/gitlab.com/quittymr/shoulder-daemon/relay)
[![licence: MIT](https://img.shields.io/badge/licence-MIT-blue.svg)](LICENSE)

Context advisor for coding agents.
Injects relevant information when needed. Records facts and keeps them up to date as they change.

## How it works

A local Go daemon watches your session, maintains a bucket of facts and injects information
into the agent's session when that context is relevant.
Your agent doesn't need to do anything - so it can't forget to store a fact or consult a knowledgebase.
Storage and reasoning are both modular, and local-only is supported.

- **The working agent never manages memory.** It doesn't need to know how the memory is structured,
  doesn't check whether a fact is already stored, and can't skip the docs or ingest all
  of them at session start. Only relevant context is injected and only when needed.
- **Facts remain up-to-date.** When facts change, they supersede each-other.
  The agent doesn't need to manage this - it just receives up to date information.
- **Retrieval and updates are smart.** A small, low-latency model determines when facts need to be updated,
  injected into the session or created from scratch. Local inference is fully supported.

Something you mention once in passing comes back in a later session, without the agent
having to ask for it.

<p align="center">
  <img src="docs/assets/example.svg" width="760"
       alt="Animated terminal transcript. The user mentions in passing that the branch here is called master rather than main; shoulder-daemon logs that it stored the fact. A week later, in a new session, the user asks to rebase onto the main branch, shoulder-daemon tells the agent the branch is master, and the agent rebases onto master.">
</p>

## Install

**Claude Code** - the plugin is the whole install. On first start it fetches the
daemon for your platform from the latest release, verifies the checksum, links
it into `~/.local/bin` so `shoulderd` works in a terminal, and starts it; after
that it starts the daemon whenever a session needs one and sessions share it.

```
/plugin marketplace add QuittyMR/shoulder-daemon
/plugin install shoulder-daemon
```

`/plugin marketplace add https://gitlab.com/quittymr/shoulder-daemon` does the
same from GitLab. A `shoulderd` already on your `PATH` is used in preference to
the fetched one, so `go install` or a package manager is never second-guessed.

**OpenCode** - the plugin is one file, and the daemon has to be on your `PATH`
from any of the sources below. Copy into `~/.config/opencode/plugins/`, or into
`.opencode/plugins/` for a single project; OpenCode loads both.

```bash
mkdir -p ~/.config/opencode/plugins
curl -o ~/.config/opencode/plugins/shoulder-daemon.js https://gitlab.com/quittymr/shoulder-daemon/-/raw/main/adapters/opencode/shoulder-daemon.js
```

That is the whole install. There is no store to run, no embedding model to pull
and no token to generate: the daemon remembers in a file of its own, ranks with
a model compiled into the binary, and generates the shared secret the hooks need
where the editor reads it.

One more file gives it a decision model, and it is the only thing anybody has to
write by hand:

```bash
mkdir -p ~/.config/shoulder-daemon
cat > ~/.config/shoulder-daemon/env <<'EOF'
SHOULDER_LLM=gemini
GEMINI_API_KEY=...
EOF
chmod 600 ~/.config/shoulder-daemon/env
```

The daemon reads that file itself, so nothing needs exporting and no editor has
to be told about it. Restart yours and `shoulderd doctor` will say what is
still missing. Everything else - the daemon by hand, a memory service instead of
the built-in store, running from a checkout, and every setting there is - lives
in [docs/INSTALL.md](docs/INSTALL.md).

OpenCode is the better informed of the two: Claude Code no longer exposes thinking tokens, so the
decision pass sees less of what the agent is actually doing.

### Choosing a model

`SHOULDER_LLM` names a connector, and takes a comma-separated list to fall back
through. `SHOULDER_LLM_MODEL` and `SHOULDER_LLM_BASE_URL` override the default
model and endpoint.

| Connector | Endpoint | Default model | Key |
|---|---|---|---|
| `gemini` | Google AI | `gemini-flash-lite-latest` | `GEMINI_API_KEY` |
| `openrouter` | OpenRouter | `google/gemini-2.5-flash-lite` | `OPENROUTER_API_KEY` |
| `glm` | z.ai | `glm-4.7-flash` | `GLM_API_KEY` |
| `glm-coding` | z.ai coding plan | `glm-5.3-flash` | `GLM_API_KEY` |
| `opencode-go` | OpenCode Go | `glm-5.3-flash` | `OPENCODE_API_KEY` |
| `openai` | OpenAI | `gpt-5.2-mini` | `OPENAI_API_KEY` |
| `local` | Ollama on `127.0.0.1:11434` | `qwen2.5-coder:7b` | none |

Pick for speed. The decision pass runs while your turn is open, and advice that
arrives after the assistant has chosen what to do is worth nothing, so a
flash-tier model beats a better one that thinks for twenty seconds; deciding
whether a turn contradicts a stored fact is classification, not authorship. On
one machine `gemini-3.5-flash-lite` answered in 0.9s and one coding-plan
endpoint took 29s, so time your own choice - the `shoulder_hook_latency_seconds`
metric with `event="advisor"` reports what the pass is costing you.

Put the choice and its key in `~/.config/shoulder-daemon/env` as above. The
daemon reads that file wherever it was started from - a container, a service
manager or an editor - which is what stops a key that is set in your login shell
from being invisible to the process that needs it.

Use `shoulderd doctor` to verify the validity of your installation and setup.

Running from a checkout, a container or a service manager is covered in
[docs/INSTALL.md](docs/INSTALL.md), along with every setting.

### Updating

```bash
git pull
make update
```

Then restart your editor and run `shoulderd doctor`.

## Use it

```bash
$ shoulderd message "this is my git repository"
main branch is master

$ shoulderd fact add --global "I prefer terse answers with no preamble"
$ shoulderd fact add --local --category=structure "integration tests need a live Postgres"
$ shoulderd fact list --local
$ shoulderd digest                      # narrative summary; --local or --global to narrow
```

`message` records what it learns by default. `--update` forces it, `--no-update`
answers without writing. Writes demand `--local` or `--global`; reads default to
this project, except `digest`, which covers both. `shoulderd help` spells out
each one.
## Configuration and tweaking

Every setting is a line in `~/.config/shoulder-daemon/env`, and the four that
matter day to day can also be turned on a running daemon:

```bash
shoulderd config                        # log level, pickiness, provider and model in use
shoulderd config set --pickiness=careful
shoulderd config set --provider=gemini --model=gemini-2.5-flash-lite
```

`config set` takes effect on the next turn and writes nothing down; a restart
returns to what the env file says.

### Pickiness

Pickiness is how reluctant the decision model is to write a new fact. There is
no right answer: a memory that stores everything fills with noise, and one that
stores nothing is an expensive way to forget. `SHOULDER_PICKINESS` in the env
file sets the starting level, `config set --pickiness` moves it live, and
either takes a name or the number behind it.

| Level | Stores |
|---|---|
| `eager` (0) | anything the turn established, stated or not; when in doubt, store it |
| `open` (1) | rules you state, and ones you clearly imply |
| `balanced` (2) | the default: rules stated or made plain, keeping only the part it is sure of |
| `careful` (3) | only rules you state; when in doubt, nothing |
| `strict` (4) | only a rule it could quote from the turn, in your words |

Lower levels lean on the tidying pass, which the daemon runs every few turns
and when a session ends, and which `shoulderd consolidate` runs by hand. Higher
levels keep the store clean and miss rules you only implied. If the monitor
shows facts that are really history - what you did this turn rather than how
things are done - go up one; if a correction you gave never appears, go down one.

### Monitoring

```bash
shoulderd monitor
```

This follows the daemon's log and shows only the facts moving: stored,
superseded, merged, dropped, refused, and advice queued and injected, one line
each with the text. It opens on the last twenty and waits for more; `--all`
shows the whole file, `--no-follow` prints and exits, `--json` passes the raw
records through.

```
14:02:11  stored      local shoulder-daemon     (structure) "main branch is master"  id=mem_12
14:09:40  queued      session 3f9a1c07 turn 6   "the branch is master, not main"  id=adv_4
14:09:41  injected    session 3f9a1c07 UserPromptSubmit  "the branch is master, not main"  id=adv_4
14:31:05  superseded  global                   [cli]  (preference) "terse answers, no preamble"  supersedes=mem_2
14:40:00  merged      local shoulder-daemon     "integration tests need a live Postgres"  kept=mem_5  replaced=mem_7,mem_9
```

The log itself is `~/.local/share/shoulder-daemon/shoulderd.log`, JSON, one
record per line, and the daemon writes it wherever it was started from. At the
default `info` level it holds every movement above and nothing per hook;
`config set --log-level=debug` adds each hook arrival. `SHOULDER_LOG` moves the
file, and `SHOULDER_LOG=stderr` turns it off for a daemon whose output something
else collects, which is also the one case `monitor` cannot watch. Counters for
the same events are at `/metrics` on the daemon's address.

### Storage backend

Facts are kept by the daemon itself, in
`~/.local/share/shoulder-daemon/facts.json`, with nothing to install or
configure; `SHOULDER_MEMORY_PATH` moves the file. Recall ranks by meaning with
an embedding table compiled into the binary, so a question worded differently
from the fact that answers it still finds it.

[mcp-memory-service](https://github.com/doobidoo/mcp-memory-service) recalls
better and can be shared between machines. It is one container to start and one
setting to point at it:

```bash
make memory                             # from a checkout; first start pulls an embedding model
echo SHOULDER_MEMORY_URL=http://127.0.0.1:8100 >> ~/.config/shoulder-daemon/env
```

Restart the daemon and it uses the service; remove the line and restart to go
back. Facts do not migrate between the two. `SHOULDER_MEMORY_KEY` carries the
service's API key if it demands one; how the two stores compare is measured in
[docs/INSTALL.md](docs/INSTALL.md). Other stores can be added; ask for the one
you use on
[GitLab](https://gitlab.com/quittymr/shoulder-daemon/-/issues) or
[GitHub](https://github.com/QuittyMR/shoulder-daemon/issues).
## Alternatives

| | What triggers it | Where it stores | Why we built this instead |
|---|---|---|---|
| **shoulder-daemon** | A small model reads each turn | a file it manages, embeddings included, or mcp-memory-service | - |
| [Natural Memory Triggers](https://github.com/doobidoo/mcp-memory-service) | regex and keyword matches on your prompt | SQLite-vec, Cloudflare or Milvus | Triggers on preset keywords only |
| [PowerContext](https://github.com/oceanbase/powercontext) | Every prompt | SQLite / OceanBase | Team-focused, no consolidation or supersession, and more invasive, but possibly the best alternative here |
| [claude-code-semantic-memory](https://github.com/razor-ai/claude-code-semantic-memory) | Embedding similarity on your prompt | SQLite/Ollama | Read-only - no learning, no supersession |
| [Letta Claude Subconscious](https://github.com/letta-ai/claude-subconscious) | A background agent reads each finished turn | Letta cloud | Paid option is the clear focus (currently broken for ClaudeCode/SQLite) and very heavy |
| [ContextStream](https://github.com/contextstream/mcp-server) | Tools the agent chooses to call, plus hooks | hosted | No injection, no self-hosting |

## Docs

- [How it works](docs/ARCHITECTURE.md) - the hot path, the injection budget, why a hook can't block
- [Install and configure](docs/INSTALL.md) - the whole install, every setting, and how the built-in store measures up against a memory service
- [Advisor protocol](docs/ADVISOR.md) - bring your own decision model
- [Contributing](CONTRIBUTING.md) - house rules, what a new connector or adapter takes, and the four test suites and what each one is for
- [Security policy](SECURITY.md) - what is in scope, and how to report privately
- [Changelog](CHANGELOG.md)

## Status

Working and tested against Claude Code 2.1.251 and OpenCode 1.18.25. Neither Go
module depends on anything outside the standard library; the one piece of
third-party material is the embedding table, which is public-domain GloVe data
(see `relay/internal/memory/vectors/NOTICE`). More model and memory connectors
are coming - ask for the one you want; no version has been tagged yet.

Development happens on [GitLab](https://gitlab.com/quittymr/shoulder-daemon) and on
[GitHub](https://github.com/QuittyMR/shoulder-daemon). Both are live - open an issue
or a change on whichever you already use, but not on both. The house rules are in
[CONTRIBUTING.md](CONTRIBUTING.md). For a vulnerability, do not open a public issue;
see [SECURITY.md](SECURITY.md).

The Go module path is `gitlab.com/quittymr/shoulder-daemon/relay` on either remote.

## Licence

MIT. See [LICENSE](LICENSE).
