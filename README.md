<p align="center">
  <img src="docs/assets/hero.webp" width="900"
       alt="A dark oil painting: a young person in a white collar looks away, while a small winged daemon perched on their shoulder leans in to whisper in their ear.">
</p>

# shoulder-daemon

Persistent memory and context injection for coding agents. A local Go
daemon watches your session, keeps a bucket of facts and injects one short
note into the agent's context when something it holds is worth saying.
The agent has no memory tool to forget to call. Storage and reasoning are both
swappable, and neither has to leave your machine.

- **The agent never manages memory.** It doesn't need to know your categories, doesn't
  check whether a fact is already stored, and can't skip the docs or slurp all
  of them at session start. Only relevant context is injected.
- **Facts don't pile up.** When facts change, they supersede each-other.
  A correction replaces what it corrects, so the agent can trust what it is
  given without checking whether it is current.
- **A small model does the work.** It reads each finished turn, decides whether
  anything is worth saying, writes what is worth keeping, and stays quiet
  otherwise. It can run locally.

## Install

Install the daemon, then the adapter for whichever editor you use. The adapters
start the daemon when the editor launches, and it stops itself after fifteen
minutes with no session. Opening several editors starts one daemon.

```bash
go install gitlab.com/quittymr/shoulder-daemon/relay/cmd/shoulderd@latest
```

**OpenCode** - copy one file into `~/.config/opencode/plugins/`, or into
`.opencode/plugins/` for a single project. OpenCode loads both.

```bash
mkdir -p ~/.config/opencode/plugins
curl -o ~/.config/opencode/plugins/shoulder-daemon.js https://gitlab.com/quittymr/shoulder-daemon/-/raw/main/adapters/opencode/shoulder-daemon.js
```

**Claude Code** - add the marketplace and install the plugin.

```
/plugin marketplace add https://gitlab.com/quittymr/shoulder-daemon
/plugin install shoulder-daemon
```

OpenCode's `ReasoningPart` carries reasoning text and the adapter forwards it as
part of the turn. Claude Code does not expose thinking tokens, so that field is
always empty there.

Select a model:

```bash
export SHOULDER_LLM=gemini          # and GEMINI_API_KEY
export SHOULDER_MEMORY_URL=http://127.0.0.1:8100    # optional; without it nothing is stored
```

Check it with `shoulderd doctor`.

Running from a checkout, a container or a service manager is covered in
[docs/INSTALL.md](docs/INSTALL.md), along with every setting.

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

## Alternatives

| | What triggers it | Where it stores |
|---|---|---|
| **shoulder-daemon** | a small model reads each finished turn | any backend behind a five-method interface |
| [Natural Memory Triggers](https://github.com/doobidoo/mcp-memory-service) | regex and keyword matches on your prompt | SQLite-vec, Cloudflare or Milvus |
| [PowerContext](https://github.com/oceanbase/powercontext) | every prompt | SQLite, or OceanBase for teams |
| [claude-code-semantic-memory](https://github.com/razor-ai/claude-code-semantic-memory) | embedding similarity on your prompt | SQLite with local Ollama embeddings |
| [Letta Claude Subconscious](https://github.com/letta-ai/claude-subconscious) | a background agent reads each finished turn | Letta, self-hosted or theirs |
| [ContextStream](https://github.com/contextstream/mcp-server) | tools the agent chooses to call, plus hooks | their hosted service, API key required |

## Docs

- [How it works](docs/ARCHITECTURE.md) - the hot path, the injection budget, why a hook can't block
- [Install and configure](docs/INSTALL.md)
- [Advisor protocol](docs/ADVISOR.md) - bring your own decision model

## Status

Working and tested against Claude Code 2.1.251 and OpenCode 1.18.25. More model
and memory connectors are coming. Contributing notes and the house rules are in
[docs/INSTALL.md](docs/INSTALL.md); issues and merge requests at
<https://gitlab.com/quittymr/shoulder-daemon>.

## Licence

MIT. See [LICENSE](LICENSE).
