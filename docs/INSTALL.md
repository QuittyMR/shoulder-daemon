# Installing shoulder-daemon

Two things have to be true before shoulder-daemon does anything useful: the relay is running, and
Claude Code is calling it.

The Claude Code adapter (`adapters/claude-code/`) is almost entirely static. Installing it means
installing the plugin (a `hooks/hooks.json` whose per-event entries are `type: "http"` callbacks to
a relay on `127.0.0.1:8787`, plus one `SessionStart` script that starts that relay when nothing is
answering) and doing two things on the host: setting a token, and - only if you have to - adjusting
one settings key. Nothing here needs Node, Python, Go, or any other runtime; the one script wants
`bash` and `curl`.

Sections 1 to 4 cover the adapter. Sections 5 and 6 cover the relay it talks to and the command
line you use to talk to it yourself.

## 1. Set `SHOULDER_TOKEN` before starting Claude Code

```bash
export SHOULDER_TOKEN="$(openssl rand -hex 32)"
claude
```

Put the `export` in your shell profile (or wherever you set env vars for the terminal Claude Code
runs in) so it is present every time, not just this once.

**Why this matters:** Claude Code sends **no authentication header by default** - verified in
Phase 0 (`spikes/results/PHASE0-FINDINGS.md`, S1). Every hook request arrives at the relay with
only `Accept, Accept-Encoding, Connection, Content-Length, Content-Type, User-Agent`. Without a
token configured, the relay's `127.0.0.1:8787` listener is open to **any local process** that can
reach loopback - not just Claude Code. The token is what turns that into an authenticated channel:
`hooks/hooks.json` sends it as `X-Shoulder-Token: ${SHOULDER_TOKEN}`, and the relay must
reject requests that don't present the matching value.

If you run several Claude Code projects against the same relay, use the same token for all of them;
the relay authenticates the caller, not the project.

## 2. Check `allowedHttpHookUrls` - the silent-block trap

If you (or an enterprise policy) have set `allowedHttpHookUrls` in any Claude Code settings file
(`~/.claude/settings.json`, project `.claude/settings.json`, or a managed policy file), **every**
shoulder-daemon hook is silently blocked unless `http://127.0.0.1:8787/*` matches one of its patterns.
There is no error, no crash, and no user-visible warning - Claude Code fails open and the session
continues exactly as if the plugin weren't installed. The only trace is one line in a debug log
(see step 3 for how to capture it):

```
[WARN] HTTP hook blocked: http://127.0.0.1:8787/v1/hooks/claude-code/UserPromptSubmit does not match any pattern in allowedHttpHookUrls
```

If `allowedHttpHookUrls` is **undefined**, all URLs are permitted and **most users need to do
nothing for this step** - Phase 0 (S1/S3) ran with no allowlist at all and every hook fired. Check
whether the key exists first:

```bash
grep -r allowedHttpHookUrls ~/.claude/settings.json .claude/settings.json 2>/dev/null
```

If it exists, add the loopback pattern to it rather than replacing the list:

```json
{
  "allowedHttpHookUrls": [
    "http://127.0.0.1:8787/*"
  ]
}
```

(Merge `"http://127.0.0.1:8787/*"` into whatever array is already there - don't drop existing
entries.)

## 3. Verify the install actually works

Run a throwaway prompt with Claude Code's own hook debug logging turned on:

```bash
claude -p 'say hello' --debug hooks --debug-file /tmp/shoulderd-hooks.log
```

Then inspect the log:

```bash
grep -E 'HTTP hook|allowedHttpHookUrls|hook error' /tmp/shoulderd-hooks.log
```

What to look for:

- `Hooks: HTTP hook POST to http://127.0.0.1:8787/v1/hooks/claude-code/UserPromptSubmit` (and the
  other `type: "http"` events) - confirms each hook actually fired.
- `Hooks: HTTP hook response status 200` - confirms the relay answered.
- **Absence** of `HTTP hook blocked: ... does not match any pattern in allowedHttpHookUrls` -
  confirms step 2 isn't silently discarding every hook.
- **Absence** of `Hooks: HTTP hook error: connect ECONNREFUSED 127.0.0.1:8787` - confirms the relay
  is actually running and reachable; if you see this, the plugin is installed correctly but the
  relay container isn't up (Claude Code fails open, so your prompt still completes normally - you
  just get no advisory injection).
- `Hooks: env var $SHOULDER_TOKEN not in allowedEnvVars, skipping interpolation` - should
  **not** appear; if it does, the `hooks.json` you have installed is stale (missing the per-hook
  `allowedEnvVars` field) or has been hand-edited incorrectly.

A clean run shows six `HTTP hook POST to ...` lines (`UserPromptSubmit`, `PreToolUse`,
`PostToolUse`, `PostToolUseFailure`, `Stop`, `SessionEnd` - `PreCompact` only fires around context
compaction, so a short prompt won't trigger it) each followed by a `200` response status, and no
`blocked` or `ECONNREFUSED` lines. `SessionStart` is a command hook rather than an HTTP one, so it
never appears as an `HTTP hook POST` line at all; what proves it worked is that the relay is up.

## 4. One script, and nothing else executable

The plugin directory is a manifest (`.claude-plugin/plugin.json`,
`.claude-plugin/marketplace.json`), a hooks file (`hooks/hooks.json`) whose per-event entries are
all `type: "http"`, and one script, `scripts/ensure-daemon.sh`. That script is the only thing under
`CLAUDE_PLUGIN_ROOT` Claude Code ever executes, it runs on `SessionStart` alone, and all it does is
start the relay when nothing is answering on `SHOULDER_ADDR`:

- It needs `bash` and `curl` and nothing else. **No Node, Python, or Go on the host.**
- It runs `shoulderd` off `PATH`. Set `SHOULDER_START_CMD` when your relay runs under a container or
  a service manager - `export SHOULDER_START_CMD="cd /path/to/shoulder-daemon && make up"` is the
  usual shape. With neither of those it prints a `go install` line to stderr and gives up.
- Several editors launching together start exactly one daemon. It takes an atomic `mkdir` lock under
  `$XDG_RUNTIME_DIR` (or `/tmp`), and whoever loses waits for the winner rather than racing to bind.
- It always exits 0 and never blocks, and `hooks.json` allows it 5 seconds. A session that can't
  reach the relay simply has no advisory context.

Nothing in the plugin ever stops the relay. Section 5 covers how it stops itself.

## 5. Run the relay

The relay is one static binary with no third-party dependencies. Build it and start it:

```bash
make build
./bin/shoulderd
```

It listens on `SHOULDER_ADDR` (default `127.0.0.1:8787`) and logs to stderr unless `SHOULDER_LOG`
names a file. It never logs to stdout: on the command-hook fallback path stdout belongs to the
harness.

`LOG_LEVEL` takes `debug`, `info`, `warn` or `error` and defaults to `info`. An unrecognised value
falls back to `info` rather than refusing to start, because a typo in a log setting is a poor reason
to have no daemon. At `debug` every hook arrival is logged; at `info` you still get every fact
stored, every fact superseded, and every piece of advice queued and injected, with its text.

It stops when the last session ends: the harness sends `SessionEnd`, and if no
other session is open the daemon shuts down. The adapters start it again the
next time an editor launches. `SHOULDER_IDLE_EXIT_MINUTES` is a backstop for a
harness that dies without saying goodbye, off by default.

Under a service manager, set the restart policy so a deliberate exit is not
undone. `deploy/docker-compose.yml` uses `restart: on-failure` for that reason;
`unless-stopped` would bring the daemon back seconds after every shutdown.

Started with nothing else configured, the relay observes and stays silent. Two variables turn it
into something that thinks:

```bash
export SHOULDER_LLM=gemini            # or glm-coding, glm, openrouter, openai, opencode-go, local
export GEMINI_API_KEY=…               # the key variable belongs to the preset you chose
export SHOULDER_MEMORY_URL=http://127.0.0.1:8100
export SHOULDER_MEMORY_KEY=…          # if your memory service requires one
```

A comma-separated `SHOULDER_LLM` is a failover chain tried left to right. With `SHOULDER_LLM`
unset the relay logs a warning naming the available presets and never speaks; with
`SHOULDER_MEMORY_URL` unset it warns that nothing will be recalled or stored and runs on the no-op
connector. Neither is fatal, and neither affects your coding session. The full variable table is in
the repository README; `docs/ADVISOR.md` covers the model side in detail.

`make up` runs the same binary under `deploy/docker-compose.yml` with host networking, which is
what keeps the listener on loopback with no port published to any other interface.

To check the relay itself rather than the hooks:

```bash
./bin/shoulderd doctor            # relay, metrics, and which hook events have ever fired
./bin/shoulderd doctor --json     # the same, machine-readable
./bin/shoulderd doctor --liveness # only "is the process serving?", for container healthchecks
```

`--liveness` exists because a correctly running relay has seen no hooks at all until somebody
starts a coding session, so a healthcheck must ask the weaker question.

## 6. Talk to it directly: local and global knowledge

Everything shoulder-daemon stores is either **local** to one project or **global** to you.

- **local** means it belongs to this codebase and is recalled only here. The project is the root of
  the git worktree you are in, so every subdirectory of one checkout shares a single memory.
  *The main branch is called master. The integration tests need a live Postgres.*
- **global** means it follows you into every repository. *Prefers terse answers. Always runs the
  linter before pushing.*

There is no default and nothing guesses. A record that arrives without a scope is rejected and
counted, because a guess means one project's memory eventually poisons another's. The command line
is where that rule becomes visible:

```bash
shoulderd message "this is my git repository"
shoulderd message --update "we cut the v2 migration from this release"
shoulderd message --no-update "remind me what the deploy target is"

shoulderd fact add  --local  --category=structure "the integration tests need a live Postgres on 5544"
shoulderd fact add  --global --category=preference --tag=style "prefers terse answers"
shoulderd fact update --local  --id=<id> "the integration tests need a live Postgres on 5544"
shoulderd fact list  --local  --limit=20

shoulderd digest              # both scopes, as prose
shoulderd digest --local
shoulderd digest --global
```

`fact add` and `fact update` require `--local` or `--global`; omitting it is an error that names
both. `message` and `fact list` default to the project you are standing in, and a bare `digest`
covers both scopes. The project is the root of the git worktree, or the working directory itself
when that is not a checkout.

Every subcommand is a thin HTTP client against the running relay. It reads the address from
`SHOULDER_ADDR` (override with `--addr`) and the token from `SHOULDER_TOKEN`. If nothing is
listening you get one line saying so and a non-zero exit - the CLI does not start a daemon for you.

## Where configuration lives

Everything is environment driven, which raises the only awkward question here:
whose environment. A daemon you start by hand inherits the shell you typed in.
A daemon started by an editor adapter, a container or a service manager does
not, and the failure is quiet - it comes up, reports itself healthy, observes
every turn and has no model to ask, so it stays silent and looks like it is
simply never finding anything to say.

Keep the settings in a file and point at it:

```bash
mkdir -p ~/.config/shoulder-daemon
cat > ~/.config/shoulder-daemon/env <<'EOF'
SHOULDER_LLM=glm-coding,gemini
GLM_API_KEY=...
GEMINI_API_KEY=...
SHOULDER_TOKEN=a-shared-secret
SHOULDER_MEMORY_URL=http://127.0.0.1:8100
SHOULDER_MEMORY_KEY=...
EOF
chmod 600 ~/.config/shoulder-daemon/env
export SHOULDER_ENV_FILE=~/.config/shoulder-daemon/env
```

`deploy/docker-compose.yml` reads `${SHOULDER_ENV_FILE:-.env}`, so with that
variable set `make up` uses your file, and without it falls back to `deploy/.env`
next to the compose file. Both are gitignored. If you have an old `deploy/.env`
lying around from an earlier experiment, the fallback will find it and use it in
preference to nothing, which is how a daemon ends up running on settings you
forgot you wrote.

`SHOULDER_TOKEN` has to match on both sides: the daemon checks it, and the
adapter sends it as `X-Shoulder-Token`. When they differ every hook is rejected
and the session carries on as though nothing were installed, because hooks fail
open. `shoulderd doctor` reports that as `auth: N REJECTED`.

An adapter can only pass on what its editor exported to it, so both
`SHOULDER_ENV_FILE` and `SHOULDER_START_CMD` belong wherever your editor sets
environment at startup. For Claude Code that is the `env` block in
`~/.claude/settings.json`:

```json
{
  "env": {
    "SHOULDER_TOKEN": "a-shared-secret",
    "SHOULDER_ENV_FILE": "/home/you/.config/shoulder-daemon/env",
    "SHOULDER_START_CMD": "make -C /home/you/src/shoulder-daemon up"
  }
}
```

## Running from a checkout

```bash
git clone https://gitlab.com/quittymr/shoulder-daemon && cd shoulder-daemon
cp deploy/.env.example deploy/.env      # add SHOULDER_LLM and a key
make up                                 # container; or: make build && ./bin/shoulderd
```

With a container or a systemd unit, point the plugin at it:
`export SHOULDER_START_CMD="cd /path/to/shoulder-daemon && make up"`.

## Every setting

Everything is environment driven. The only two you need:

| Variable | Purpose |
|---|---|
| `SHOULDER_LLM` | `gemini`, `glm`, `glm-coding`, `openrouter`, `openai`, `opencode-go`, `local`. Comma-separate for a failover chain. |
| `SHOULDER_MEMORY_URL` | Base URL of your memory backend. Unset means it observes and answers but stores nothing. |

Then `SHOULDER_TOKEN` (shared secret for the hooks, strongly advised),
`SHOULDER_ADDR`, `SHOULDER_MEMORY_KEY`, `SHOULDER_LOG`, `SHOULDER_DRY_RUN`, and
the `WINDOW_*`, `BUDGET_*` and `ADVISOR_*` tuning knobs. See
[docs/INSTALL.md](docs/INSTALL.md).

**Models.** Gemini, GLM (pay-as-you-go and Coding Plan), OpenRouter, OpenCode Go,
OpenAI, and any OpenAI-compatible endpoint including a local Ollama. Comma-
separate `SHOULDER_LLM` for a failover chain. More coming.

**Memory.** [mcp-memory-service](https://github.com/doobidoo/mcp-memory-service)
today. Backends sit behind a five-method `Connector` interface that names nothing
specific to any product; `memory.TestConnector` is an exported conformance suite
a new one can run against itself. More coming.

## After an update

```bash
git pull
make update
```

`make update` rebuilds the binary and the container image, replaces each
harness's installed copy of the adapter, and restarts the daemon. Then restart
your editor so it reloads the plugin, and run `shoulderd doctor`.

The step that is easy to miss is the adapter. A harness runs the copy it took
when the plugin was installed, not your checkout, so editing the adapter here
changes nothing it loads. The failure is silent: the old copy keeps posting to
whatever address and header it was built against, so hooks either never arrive
or are rejected while the source on disk looks correct. `shoulderd doctor`
reports that as `plugin: STALE`.
