# Setting up shoulder-daemon, for an agent

You are a coding agent, running inside Claude Code or OpenCode, and a person has
asked you to install and set up shoulder-daemon for them. This page is the whole
procedure. Follow it top to bottom, run every command yourself, and stop only at
the one step marked **the person does this**.

Three rules hold throughout:

1. **Never take an API key through the conversation.** Transcripts are written
   to disk, and shoulder-daemon itself reads transcripts. The key is typed by the
   person into a command that reads it from standard input; you never see it.
2. **Never print the env file.** It holds keys. Print the names of its settings,
   never their values.
3. **Nothing is done until `shoulderd doctor` says so.** You report what doctor
   printed, not what you wrote.

## 1. Find out what is already there

```bash
command -v claude && claude --version          # Claude Code?
command -v opencode && opencode --version      # OpenCode?
uname -s; uname -m                             # the release binary is named after these
command -v shoulderd && shoulderd version      # a daemon already installed?
shoulderd doctor 2>&1 || true                  # what a running one reports
ENV="$(shoulderd env path 2>/dev/null || echo "${XDG_CONFIG_HOME:-$HOME/.config}/shoulder-daemon/env")"
echo "$ENV"
[ -f "$ENV" ] && sed -n 's/^[[:space:]]*\(export[[:space:]]\{1,\}\)\{0,1\}\([A-Za-z_][A-Za-z0-9_]*\)[[:space:]]*=.*/\2/p' "$ENV"   # setting names only
```

Let the answers cancel steps. A daemon that is installed is not installed
again; a setting that is in the file is not asked about. If both harnesses are
present, ask the person which one, or do both: steps 2 and 5 differ per harness,
everything else is shared.

## 2. Install the hooks

**Claude Code.** The plugin is the whole install: it fetches the daemon on the
next session start, links it into `~/.local/bin`, and starts it whenever a
session needs one.

```bash
claude plugin marketplace add QuittyMR/shoulder-daemon
claude plugin install shoulder-daemon@shoulder-daemon
```

The hooks load when the editor next starts, so the session you are in does not
yet observe anything; say so in the report.

**OpenCode.** The plugin is one file. `~/.config/opencode/plugins/` is for every
project, `.opencode/plugins/` in a repository is for that one; OpenCode loads
both.

```bash
mkdir -p ~/.config/opencode/plugins
curl -fsSL -o ~/.config/opencode/plugins/shoulder-daemon.js \
  https://gitlab.com/quittymr/shoulder-daemon/-/raw/main/adapters/opencode/shoulder-daemon.js
```

## 3. Get the daemon

Skip this if step 1 found `shoulderd` on `PATH`: a binary already there is used
in preference to anything fetched, so `go install` or a package manager is never
second-guessed.

**Claude Code** has a script for it in the installed plugin. It downloads the
release for this platform, checks it against the published `SHA256SUMS`, links
it into `~/.local/bin/shoulderd`, and starts it:

```bash
ENSURE="$(find ~/.claude/plugins/cache -name ensure-daemon.sh -path '*shoulder-daemon*' 2>/dev/null | sort | tail -1)"
"$ENSURE" --fetch
```

The cache holds the copy of the plugin the harness runs, one directory per
version; the newest is the one to use.

**OpenCode**, or anywhere the script is not at hand, does the same by hand.
`os` is `linux` or `darwin`, `arch` is `amd64` or `arm64`:

```bash
os=$(uname -s | tr '[:upper:]' '[:lower:]'); arch=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
base=https://github.com/QuittyMR/shoulder-daemon/releases/latest/download
tmp=$(mktemp -d) && cd "$tmp"
curl -fsSL -o "shoulderd_${os}_${arch}" "$base/shoulderd_${os}_${arch}" && curl -fsSL -o SHA256SUMS "$base/SHA256SUMS"
grep " shoulderd_${os}_${arch}\$" SHA256SUMS | sha256sum -c -     # shasum -a 256 -c on macOS
mkdir -p ~/.local/bin && install -m 755 "shoulderd_${os}_${arch}" ~/.local/bin/shoulderd && cd - && rm -rf "$tmp"
```

`~/.local/bin` has to be on `PATH`; if `command -v shoulderd` still finds
nothing, tell the person the one line that fixes it:
`export PATH="$HOME/.local/bin:$PATH"`. With Go installed,
`go install gitlab.com/quittymr/shoulder-daemon/relay/cmd/shoulderd@latest` is
the alternative, into `$(go env GOPATH)/bin`.

## 4. Configure

Everything the daemon knows comes from one file, which it reads itself at start
wherever it was started from: `shoulderd env path` prints it, normally
`~/.config/shoulder-daemon/env`. Write it only with `shoulderd env set`, which
replaces a setting in place and carries every other line across, including the
`SHOULDER_TOKEN` the daemon generates for the hooks. Never write it with an
editor or a shell redirect.

**The decision model** is the one setting the daemon cannot work without; with
none it observes every session and stays silent. Ask the person which connector,
one question, recommended option first. Recommend by speed, not quality: the
decision runs while their prompt is being answered, and advice that lands after
the agent has chosen what to do is worth nothing, so a flash-tier model beats a
better one that thinks for twenty seconds.

| Connector | Endpoint | Default model | Key |
|---|---|---|---|
| `gemini` | Google AI | `gemini-flash-lite-latest` | `GEMINI_API_KEY` |
| `openrouter` | OpenRouter | `google/gemini-2.5-flash-lite` | `OPENROUTER_API_KEY` |
| `glm` | z.ai | `glm-4.7-flash` | `GLM_API_KEY` |
| `glm-coding` | z.ai coding plan | `glm-5.3-flash` | `GLM_API_KEY` |
| `opencode-go` | OpenCode Go | `glm-5.3-flash` | `OPENCODE_API_KEY` |
| `openai` | OpenAI | `gpt-5.2-mini` | `OPENAI_API_KEY` |
| `local` | Ollama on `127.0.0.1:11434` | `qwen2.5-coder:7b` | none |

Offer `local` to anyone who wants no key at all, after
`curl -sf http://127.0.0.1:11434/api/tags` shows Ollama is running. Then:

```bash
shoulderd env set SHOULDER_LLM gemini          # or a comma-separated list to fall back through
```

**The key** - **the person does this.** If the connector needs one, check
whether it is already set, by name only:

```bash
grep -q "^GEMINI_API_KEY=." "$(shoulderd env path)" && echo "key is in the file"
[ -n "${GEMINI_API_KEY:-}" ] && echo "key is in this environment"
```

A key in the file needs nothing. A key only in the environment is copied by
name, without ever being printed: `printf '%s' "$GEMINI_API_KEY" | shoulderd env set GEMINI_API_KEY`.
Neither: stop, and give the person this to run in a terminal of their own:

```bash
shoulderd env set GEMINI_API_KEY      # then paste the key, Enter, Ctrl-D
```

`env set` without a value reads standard input, so the key is never a command
argument anybody can list, and it is never in this conversation. In Claude Code
they can run it in the session by typing `!` before the command. Tell them where
a key comes from (Google AI Studio for Gemini, and so on), and wait. Do not
write a placeholder: a file with `GEMINI_API_KEY=` in it reports as configured
and fails on the first prompt.

**Everything else has a working default.** Facts are kept in one file,
`~/.local/share/shoulder-daemon/facts.json`, ranked by a model compiled into
the binary. Mention, and set only if asked:

- `SHOULDER_MEMORY=docs` keeps the same facts as markdown under
  `docs/*.shoulder.md` in each checkout, committed and read by the whole team.
- `SHOULDER_EMBEDDING=minilm` fetches a 91 MB ranking model once for better
  recall.
- `SHOULDER_PICKINESS` is `eager, open, balanced, careful, strict`: how
  reluctant the daemon is to keep a new fact.

A memory service instead of the file, a container, and every other setting are
in [INSTALL.md](INSTALL.md).

## 5. Start it

The daemon reads the file only at start, so a running one has to be restarted.

```bash
pkill -x shoulderd || true
```

**Claude Code:** `"$ENSURE" --fetch` from step 3 starts it with the lock and
the binary the plugin would use. **OpenCode:** restarting the editor starts it;
to start it now instead, `(nohup shoulderd >/dev/null 2>&1 &)`.

Then wait for it and ask:

```bash
until curl -sf --max-time 1 http://127.0.0.1:8787/healthz >/dev/null; do sleep 1; done
shoulderd doctor
```

Read the report. The work is done when it says `memory: ok (...)` and
`llm: <provider> (<model>)` with the connector the person chose, followed by
`answering since ...`. Anything else names the fault:

- `llm: NONE` - `SHOULDER_LLM` did not reach the daemon; it started before the
  file was written, or from another file. Restart.
- `FAILING since ... The model says: ...` - the model refuses; the quoted line
  is the provider's own answer, usually a wrong or dead key. The person fixes
  the key as in step 4, and you restart.
- `MISMATCH` - the file asks for one thing and the daemon runs another. Restart.
- `memory: UNREACHABLE` or `FAILED TO OPEN` - the store is configured and
  cannot be read; the line says why.
- `hooks: NEVER FIRED` - expected now: the hooks load when the editor restarts
  and fire on the first prompt after that. Say so rather than calling it a fault.

`shoulderd config set --provider=... --pickiness=...` tries a setting on the
running daemon without a restart and without writing it down; the file is
still what counts next start.

## 6. Report

Tell the person, in this order:

1. What doctor said, verbatim for the `memory:` and `llm:` lines.
2. The files, so they know where to go later:
   - the settings: `shoulderd env path` - edit with `shoulderd env set NAME VALUE`, then restart the daemon
   - the hooks: the plugin under `~/.claude/plugins/` (Claude Code) or
     `~/.config/opencode/plugins/shoulder-daemon.js` (OpenCode)
   - the facts: `~/.local/share/shoulder-daemon/facts.json`
   - the log: `~/.local/share/shoulder-daemon/shoulderd.log`, and
     `shoulderd monitor` to watch facts move and the model fail or recover
3. The one thing left to them: restart the editor so the hooks load; or the
   key, if it is still missing, with the command from step 4.
4. What you did not set and why.

Never claim a setting works because you wrote it; claim it because doctor said
so.
