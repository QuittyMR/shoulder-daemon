---
name: setup-shoulder-daemon
description: Interview the user and configure shoulder-daemon on their behalf - decision model and its key, memory backend, ranking model, and the mcp-memory-service container when they want one - running every download and container start here, then restarting the daemon and proving it with doctor. Use when the plugin has just been installed, when someone asks to set up, configure, reconfigure, or fix shoulder-daemon, when they want to change its model, provider, pickiness or memory store, or when `shoulderd doctor` reports NONE, UNREACHABLE, STALE, REJECTED, or no decision model.
allowed-tools: Bash, Read, Write, AskUserQuestion
---

# Setting up shoulder-daemon

Everything about the daemon is environment driven, and it reads one file itself
so that nothing has to be exported by whoever launches it:
`$SHOULDER_ENV_FILE`, or `${XDG_CONFIG_HOME:-~/.config}/shoulder-daemon/env`.
Configuring the daemon means writing that file and restarting. Anything already
in the process environment wins over the file, which is the one way a setting
you wrote can appear not to take.

Do the whole thing here. The user should not have to open a terminal, except
for the single case in "Keys" below where they must, and you tell them exactly
what to type.

## 1. Preflight, before you ask anything

Run these first and let the answers cancel questions. A skill that asks about
something already configured looks like it did not look.

```bash
# $CLAUDE_PLUGIN_ROOT is set for a plugin's own components; the fallback is for
# a checkout, where this file may have been read directly.
S="${CLAUDE_PLUGIN_ROOT:-adapters/claude-code}/skills/setup-shoulder-daemon/scripts/env-set.sh"
[ -x "$S" ] || S="$(find ~/.claude "$PWD" -name env-set.sh -path '*setup-shoulder-daemon*' 2>/dev/null | head -1)"
"$S" path                                    # the file you are about to write
[ -f "$("$S" path)" ] && cat "$("$S" path)" | sed 's/=.*/=<set>/'   # names only, never values
command -v shoulderd || echo "no shoulderd on PATH"
shoulderd doctor 2>&1 || true
shoulderd config show 2>&1 || true           # provider, model and pickiness of a running daemon
```

Never print the contents of the env file as-is: it holds API keys and this
session's transcript is stored on disk. The `sed` above is why.

Read the report:

- `relay: ...` not answering, or no `shoulderd` at all - the daemon is fetched
  at the next session start by the plugin's own `scripts/ensure-daemon.sh
  --fetch`. Run that script now rather than downloading anything yourself; it
  verifies the release checksum, links the binary into `~/.local/bin`, and takes
  the lock that stops two editors starting two daemons.
- `plugin: STALE` - the harness is running the copy of the adapter it took at
  install time. Reinstall the plugin, or `make install-plugins` from a checkout.
  Configuration will not fix it and it makes every later check unreliable.
- `auth: N REJECTED` - `SHOULDER_TOKEN` differs between the daemon and the
  harness. Never write a token yourself: the daemon generates one and syncs it
  into the env file. Remove any `SHOULDER_TOKEN` line the user added by hand and
  restart.
- `memory: UNREACHABLE` - a store is configured that cannot be read. That is
  question 2 below, and it is urgent: sessions look normal while nothing is kept.

## 2. The interview

Use AskUserQuestion, one question at a time, recommended option first. Skip any
question preflight already answered, and say what you found instead of asking.

**Q1 - the decision model.** This is the only setting the daemon cannot work
without; with none it observes every turn and stays silent. `SHOULDER_LLM`
names a connector and takes a comma-separated list to fail over through.

| Connector | Endpoint | Default model | Key |
|---|---|---|---|
| `gemini` | Google AI | `gemini-flash-lite-latest` | `GEMINI_API_KEY` |
| `openrouter` | OpenRouter | `google/gemini-2.5-flash-lite` | `OPENROUTER_API_KEY` |
| `glm` | z.ai | `glm-4.7-flash` | `GLM_API_KEY` |
| `glm-coding` | z.ai coding plan | `glm-5.3-flash` | `GLM_API_KEY` |
| `opencode-go` | OpenCode Go | `glm-5.3-flash` | `OPENCODE_API_KEY` |
| `openai` | OpenAI | `gpt-5.2-mini` | `OPENAI_API_KEY` |
| `local` | Ollama on `127.0.0.1:11434` | `qwen2.5-coder:7b` | none |

Recommend by latency, not by quality: the pass runs while the user's turn is
open and advice that lands after the assistant has already chosen what to do is
worth nothing. A flash-tier model beats a better one that thinks for twenty
seconds. Put whichever connectors already have a key first in the list, and
offer `local` to anyone who wants no key at all - check `curl -sf
http://127.0.0.1:11434/api/tags` before offering it, and say the model has to be
pulled with `ollama pull` if it is not there.

**Q2 - where facts are kept.** Three options, and the default is genuinely fine:

- **Built-in store** (`SHOULDER_MEMORY=local`, the default). One JSON file at
  `~/.local/share/shoulder-daemon/facts.json`. Nothing to install, nothing to
  keep running, no container. Recommend this unless the user asks for more.
- **Docs store** (`SHOULDER_MEMORY=docs`). The same facts as markdown bullets
  under `docs/*.shoulder.md` in each checkout, committed and read by the whole
  team. Offer it when the user talks about sharing facts with colleagues.
- **mcp-memory-service** (`SHOULDER_MEMORY_URL=http://127.0.0.1:8100`). Better
  recall and shareable between machines, at the cost of a container that has to
  stay up. Section 3 is the whole procedure. Only offer it from a checkout,
  because starting it needs `deploy/docker-compose.yml`.

Facts do not migrate between stores by themselves. If the user is switching an
established install, offer `shoulderd memory migrate --local` (and `--global`)
before you restart on the new one, and say plainly that skipping it strands what
they have.

**Q3 - how recall ranks, and how much it stores.** Both have workable defaults;
ask only if the user is interested or preflight shows they have already tuned
one.

- `SHOULDER_EMBEDDING=glove` (default) is compiled into the binary and needs no
  download. `minilm` is a 91 MB transformer fetched once into
  `~/.cache/shoulder-daemon/models`; recall is better and the store uses `glove`
  until it has arrived, so nothing is broken while it downloads. If they choose
  it, start the daemon and let it fetch, and tell them the first minutes rank on
  `glove`. Ignore it entirely under mcp-memory-service, which embeds its own.
- `SHOULDER_PICKINESS` is `eager, open, balanced, careful, strict` or `0-4`, and
  is how reluctant the store is to keep a new fact. Lower keeps more and needs
  tidying more often; higher keeps the store clean and misses rules the user
  only implied.

## 3. mcp-memory-service, when they chose it

Only reachable from a checkout. Confirm one, then, in this order:

```bash
make memory                    # first start pulls an embedding model, minutes
until curl -sf http://127.0.0.1:8100/api/health >/dev/null; do sleep 5; done
"$S" set SHOULDER_MEMORY_URL http://127.0.0.1:8100
```

Run the wait in the background rather than blocking, and report progress. Do not
write `SHOULDER_MEMORY_URL` before the service answers: a URL pointing at
nothing is worse than no URL, because the daemon accepts it, reports itself
healthy, and drops every search and every write with only a warning in its log.

Then tell the user two things they will otherwise discover the hard way:

- Under rootless podman, `restart: unless-stopped` needs
  `systemctl --user enable --now podman-restart.service` and lingering
  (`loginctl enable-linger`), or the store is gone after a reboot and the daemon
  goes on reporting it as configured. Check both and offer to enable them.
- `make down` removes the store along with the relay, and `make memory` is what
  brings it back. `make up` restarts the relay alone and leaves a running store
  untouched; `make update` selects it, so an update keeps it.

`SHOULDER_MEMORY_KEY` carries the service's API key if it demands one.
`SHOULDER_MEMORY_URL` wins over `SHOULDER_MEMORY`, so do not set both and expect
the second to matter - the daemon warns about exactly this at startup.

## 4. Keys

Never ask the user to paste an API key into this conversation. The transcript is
written to disk, and shoulder-daemon itself reads transcripts.

For the connector they chose, in order:

1. `"$S" has GEMINI_API_KEY` - already in the env file or this environment.
   Nothing to do; say so.
2. In the environment but not the file: `"$S" copy GEMINI_API_KEY`. That reads
   the value by name and writes it with mode 600 without printing it. Do this
   whenever the key is only exported in a shell, because a daemon started by a
   hook does not reliably inherit it.
3. Neither: stop and tell the user, with the exact line to run themselves,
   pointing out they can run it in this session by prefixing it with `!`:

   ```
   ! "$S" set GEMINI_API_KEY sk-...
   ```

   Give them the URL to get one from, then wait. Do not configure around a
   missing key or write a placeholder: an env file with `GEMINI_API_KEY=` in it
   reports as configured and fails on the first turn.

## 5. Write, restart, verify

Write with the helper, never with an editor or a shell redirect - it replaces a
setting in place and carries every other line, including the generated
`SHOULDER_TOKEN` and anything the user wrote by hand, across untouched:

```bash
"$S" set SHOULDER_LLM gemini
"$S" set SHOULDER_MEMORY docs
```

The daemon reads the file only at startup, so restart it. Do not hand-roll a
start command; the plugin's script holds the lock, finds the right binary,
prefers a `shoulderd` on `PATH`, and honours `SHOULDER_START_CMD`:

```bash
pkill -x shoulderd || true                       # or: make up, for a container install
"$CLAUDE_PLUGIN_ROOT"/scripts/ensure-daemon.sh --fetch
until curl -sf --max-time 1 http://127.0.0.1:8787/healthz >/dev/null; do sleep 1; done
shoulderd doctor
```

`shoulderd config set --provider=... --model=... --pickiness=...` changes a
running daemon without a restart, but writes nothing down, so it is for trying a
setting out - not a substitute for the file.

Then verify, and treat the verification as the deliverable:

- `shoulderd doctor` must show `memory: ok (...)` and the provider the user
  chose. `memory: NONE` or `UNREACHABLE` means the work is not done.
- `doctor` also reports hook events it has never seen. Some only appear after
  the user's next turn, so say which ones are still outstanding rather than
  claiming a clean result you have not seen.
- The plugin's hooks only reload when the editor restarts. If you changed
  anything the adapter carries, say so.

## 6. Report

Say what you set, where the file is, what you did not set and why, and the one
thing that needs the user - a restarted editor, a key, a `pull`. Never claim a
setting works because you wrote it; claim it because `doctor` said so.
