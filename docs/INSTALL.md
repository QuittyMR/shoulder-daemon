# Installing shoulder-daemon

Two things have to be true before shoulder-daemon does anything useful: the relay is running, and
Claude Code is calling it.

The Claude Code adapter (`adapters/claude-code/`) is almost entirely static. Installing it means
installing the plugin (a `hooks/hooks.json` whose per-event entries are `type: "http"` callbacks to
a relay on `127.0.0.1:8787`, plus one `SessionStart` script that starts that relay when nothing is
answering) and doing two things on the host: setting a token, and - only if you have to - adjusting
one settings key. Nothing here needs Node, Python, Go, or any other runtime; the one script wants
`bash` and `curl`.

Sections 1 to 4 cover the adapter. Sections 5 to 7 cover the relay it talks to, the command
line you use to talk to it yourself, and the settings that command can change on a running daemon.

## 1. The token, which you do not have to set

The daemon generates a token on first start, keeps it in
`~/.local/share/shoulder-daemon/token`, and writes it into `env.SHOULDER_TOKEN` of
`~/.claude/settings.json` and into its own env file. Nothing else in your setup changes, and the
CLI reads the same value, so a terminal that has sourced nothing can still talk to the daemon. You
do not have to do anything here.

**Why there is a token at all:** Claude Code sends **no authentication header by default** -
verified against Claude Code 2.1.251, whose captured hook payloads are in
`testdata/hook-payloads/2.1.251/`. Every hook request arrives at the relay with
only `Accept, Accept-Encoding, Connection, Content-Length, Content-Type, User-Agent`. Without a
token, the relay's `127.0.0.1:8787` listener is open to **any local process** that can reach
loopback - and to any page open in a browser, for which localhost is not special. `hooks.json`
sends the value as `X-Shoulder-Token: ${SHOULDER_TOKEN}` and the relay rejects requests that do not
present it.

An editor reads its environment when it launches, so the session that started the daemon may
predate the value being written. Until the daemon sees one correct token it accepts hooks without
one, and from that moment it requires it; the window closes on the first hook of the first session
that has it, not on a timer.

Setting `SHOULDER_TOKEN` yourself overrides all of this and nothing is written to your files. Do
that if you run several machines against one relay, or if you would rather the value came from your
own secret store. The relay authenticates the caller, not the project, so every project on one
machine uses one token.

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
`CLAUDE_PLUGIN_ROOT` Claude Code ever executes, it runs on `SessionStart` and again before each
prompt, and all it does is start the relay when nothing is answering on `SHOULDER_ADDR`:

- It needs `bash` and `curl` and nothing else. **No Node, Python, or Go on the host.**
- It runs `shoulderd` off `PATH`, or the copy it fetched itself. On a machine with neither, the
  `SessionStart` run downloads the latest release binary for the platform into
  `${XDG_DATA_HOME:-~/.local/share}/shoulder-daemon/bin`, verifies it against the published
  `SHA256SUMS`, and starts it; a failed or tampered download leaves nothing behind. Set
  `SHOULDER_START_CMD` when your relay runs under a container or a service manager -
  `export SHOULDER_START_CMD="cd /path/to/shoulder-daemon && make up"` is the usual shape - and
  nothing is fetched. `SHOULDER_RELEASE_BASE` points the fetch at a mirror.
- Several editors launching together start exactly one daemon. It takes an atomic `mkdir` lock under
  `$XDG_RUNTIME_DIR` (or `/tmp`), and whoever loses waits for the winner rather than racing to bind.
- It always exits 0 and never blocks. `hooks.json` allows the `SessionStart` run 60 seconds, which
  is only ever spent on the one first download; the per-prompt run gets 5 and never downloads. A
  session that can't reach the relay simply has no advisory context.

Nothing in the plugin ever stops the relay. Section 5 covers how it stops itself.

## 5. Run the relay

The relay is one static binary with no third-party dependencies. Build it and start it:

```bash
make build
./bin/shoulderd
```

It listens on `SHOULDER_ADDR` (default `127.0.0.1:8787`) and logs to stderr and to
`~/.local/share/shoulder-daemon/shoulderd.log`, one JSON record per line. `SHOULDER_LOG` moves the
file; `SHOULDER_LOG=stderr` writes no file, for a daemon whose output something else collects. A
file past 8 MB is moved to `.1` at the next start. It never logs to stdout: on the command-hook
fallback path stdout belongs to the harness.

`LOG_LEVEL` takes `debug`, `info`, `warn` or `error` and defaults to `info`. An unrecognised value
falls back to `info` rather than refusing to start, because a typo in a log setting is a poor reason
to have no daemon. At `debug` every hook arrival is logged; at `info` you still get every fact
stored, every fact superseded, and every piece of advice queued and injected, with its text.
`shoulderd monitor` follows the file and shows only those lines; section 8 covers it.

`SHOULDER_PICKINESS` sets how reluctant the decision model is to store a new fact: `eager`, `open`,
`balanced`, `careful`, `strict`, or the numbers `0` to `4` behind them, low to high, and it defaults
to `balanced` (2). Higher stores less and takes only a rule the user stated outright; lower stores
more, including a rule only implied, and leans on the existing consolidation pass to clean up after
itself. Like `LOG_LEVEL`, an unrecognised value falls back to the default rather than refusing to
start. It's read at boot but not fixed there - section 8 covers changing it, and the other three live
settings, on a daemon that's already running.

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
export SHOULDER_MEMORY_URL=http://127.0.0.1:8100   # optional; without it the built-in store is used
export SHOULDER_MEMORY_KEY=…          # if your memory service requires one
```

A comma-separated `SHOULDER_LLM` is a failover chain tried left to right. With `SHOULDER_LLM`
unset the relay logs a warning naming the available presets and never speaks; with
`SHOULDER_MEMORY_URL` unset it remembers in its own file and says where. Neither is fatal, and
neither affects your coding session. The full variable table is in
the repository README; `docs/ADVISOR.md` covers the model side in detail.

`make up` runs the same binary under `deploy/docker-compose.yml` with host networking, which is
what keeps the listener on loopback with no port published to any other interface. It also mounts
`~/.claude/projects` read-only, where Claude Code writes each session's transcript: the Stop hook
carries only the last thing the assistant said in a turn, and the transcript is where the rest of
it is read from. Those files are mode 600, so the container runs its nonroot user as you
(`userns_mode: keep-id`) and, because an SELinux host labels them as your home and refuses a
confined container whatever its uid, with SELinux confinement off for this one container
(`label=disable`). A `facts` volume created before that mapping existed is owned by the
old one and must be re-owned once, or the daemon cannot write its store:

```bash
podman unshare chown -R 0:0 "$(podman volume inspect shoulder-daemon_facts -f '{{.Mountpoint}}')"
```

Without the mount the daemon still runs; it logs once per session that the transcript is
unreadable and sees only the last message of each turn.

To check the relay itself rather than the hooks:

```bash
./bin/shoulderd doctor            # relay, metrics, the store, and which hook events have ever fired
./bin/shoulderd doctor --json     # the same, machine-readable
./bin/shoulderd doctor --liveness # only "is the process serving?", for container healthchecks
```

`--liveness` exists because a correctly running relay has seen no hooks at all until somebody
starts a coding session, so a healthcheck must ask the weaker question.

## 6. Where the facts go

The daemon has a store inside it and uses it unless told otherwise, so this section is optional.
Everything it learns is held in memory and written to one JSON file — `facts.json` under
`~/.local/share/shoulder-daemon/`, or wherever `SHOULDER_MEMORY_PATH` points — as a whole file
through a temporary one, so an interrupted write leaves the previous facts rather than half of the
new ones. It is mode 600, because everything you have said in front of an agent is in it.

Recall is by embedding, with the model compiled into the binary: 40,000 GloVe word vectors at 100
dimensions, quantised to a signed byte each, mean-pooled with rarity weighting and the vocabulary's
common direction projected back out, so a question is answered by what it means and not by which
words it repeats: "where does this get deployed" recalls "we ship to the staging cluster", which
shares not one word with it. It adds about 4MB to the binary and needs nothing at runtime — no
download, no key, no service, no network — so it works the moment the daemon is installed, on a
machine with nothing else on it. Vectors are computed once per record and stored beside
it, tagged with the model that produced them, so a later table cannot be compared against an
earlier one's numbers. A record whose vector is missing or stale is scored on words in common
instead of being lost.

What it is not is a transformer. Word order is lost and negation is invisible, so "releases ship on
Fridays" and "releases never ship on Fridays" read as the same claim — which is why the second one
collides with the first and supersedes it rather than being stored beside it. Sentences of three or
four words are too short to place well; a turn's worth of text is not.

`shoulderd doctor` reports which store is in use on its `memory:` line, and the daemon names the
table and its vocabulary size at startup.

**How much you give up.** Measured, not asserted:
`relay/internal/memory/compare_test.go` loads both stores with the same twenty facts and asks the
same fifteen questions - plain recall, families of facts that differ by one identifier, direction,
paths and identifiers, long facts, negation, synonymy - and reports where each put the record that
answers each question:

```bash
podman run -d --rm --name mem --network host -e MCP_MODE=http -e MCP_HTTP_HOST=127.0.0.1 \
  -e MCP_HTTP_PORT=8101 -e MCP_ALLOW_ANONYMOUS_ACCESS=true \
  docker.io/doobidoo/mcp-memory-service:11-slim
SHOULDER_MEMORY_URL=http://127.0.0.1:8101 go test -tags compare ./internal/memory/ -run TestCompare -v
```

At the time of writing the built-in store answers 11 of 15 with the right fact first and 14 of 15
within the top three; mcp-memory-service answers 14 of 15 first. The four it puts second or lower
are questions whose only link to the stored fact is that two words are related in meaning, which a
mean of word vectors barely represents and a transformer does. It wins one the service loses. Run it
yourself before believing either number, and run `TestCompareIdentifierFamily` too: that one is the
scenario a store like this fails worst, eight facts that differ only in a port number, and it is
what the numeric guard in the store exists for.

**Using mcp-memory-service instead.** Set `SHOULDER_MEMORY_URL` and the daemon uses
[mcp-memory-service](https://github.com/doobidoo/mcp-memory-service) for everything instead of its
own file. It recalls better, as the numbers above show, because a transformer sits behind it, and
it can be shared between machines. Start it:

```bash
docker run -d --name shoulder-memory --network host --restart unless-stopped \
  -e MCP_MODE=http -e MCP_HTTP_HOST=127.0.0.1 -e MCP_HTTP_PORT=8100 \
  -e MCP_ALLOW_ANONYMOUS_ACCESS=true -v shoulder-memory:/app/data \
  docker.io/doobidoo/mcp-memory-service:11-slim
```

From a checkout, `make memory` starts the same service out of `deploy/docker-compose.yml`. Use the
`-slim` tags: the unsuffixed ones are amd64 only. The first start downloads an ONNX embedding model
and takes a few minutes. Then point the daemon at it:

```bash
echo 'SHOULDER_MEMORY_URL=http://127.0.0.1:8100' >> ~/.config/shoulder-daemon/env
```

The daemon picks it up the next time it starts, which is the next editor session once the current
ones end. `shoulderd doctor` then reports `memory:  ok (mcp-memory-service)`.

**Anonymous access or a key.** `MCP_ALLOW_ANONYMOUS_ACCESS=true` is what makes writes work at all;
without it the store answers 401 to every request and the daemon logs a write it never made. On a
listener bound to 127.0.0.1 it grants what a local database file already grants: anything on this
machine can read it. To require a key instead, set `MCP_API_KEY` on the store and the same value as
`SHOULDER_MEMORY_KEY` on the daemon, and leave anonymous access off - with it on, a key is checked
first and then not required, which is a key that protects nothing.

Nothing above the connector boundary knows which of the two is running, and which of the
service's own backends sits behind it (SQLite-vec, Cloudflare, Milvus) is invisible from here. A
third backend is a five-method `Connector` interface in `relay/internal/memory/` that names nothing
specific to any product, and `memory.TestConnector` is a conformance suite a new one runs against
itself - the built-in store passes the same suite as the service - so a connector is a small,
self-contained piece of work; see [CONTRIBUTING.md](../CONTRIBUTING.md). If you want a particular
store, say so: open an issue on [GitLab](https://gitlab.com/quittymr/shoulder-daemon/-/issues) or
[GitHub](https://github.com/QuittyMR/shoulder-daemon/issues), or mail thomas@lumea-technologies.com.
Naming the one you use is the fastest way to get it built, and a patch is welcome too.

## 7. Talk to it directly: local and global knowledge

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
`SHOULDER_ADDR` (override with `--addr`) and the token from `SHOULDER_TOKEN`, the env file, or the
generated token file, in that order, so a terminal that has sourced nothing still works. If nothing
is listening you get one line saying so and a non-zero exit - the CLI does not start a daemon for
you.

## 8. Change settings on a running daemon

Four of the settings above don't need a restart: the log level, the pickiness, and the provider and
model that answer. `shoulderd config` reads them; `shoulderd config set` turns them.

```bash
shoulderd config                                  # log level, pickiness, provider, model in use
shoulderd config set --pickiness=strict
shoulderd config set --provider=gemini --model=gemini-2.5-flash-lite
```

`config set` changes only the flags it is given, takes effect on the next turn, and does not
interrupt anything already in flight. It is all-or-nothing: a request naming a value that doesn't
exist - an unknown level, an unknown pickiness, a provider with no key in the daemon's environment, a
model its provider doesn't have - is refused with the reason, and every setting is left exactly as it
was. Naming `--provider` resets the model to that provider's own default, because model ids don't
carry between providers, and `--model` is refused for a comma-separated failover chain, since the
providers in one don't share model ids either - the same restriction `SHOULDER_LLM_MODEL` has in
`docs/ADVISOR.md`. The provider's API key must already be in the daemon's environment; `config set`
can't supply one.

None of it is written down. A restart returns to whatever the environment says, so the env file stays
the single description of how the daemon is meant to run.

Both commands are a thin client over `GET /v1/cli/config` and `PATCH /v1/cli/config`, which honour
`SHOULDER_TOKEN` like every other CLI route.

To see what a setting is doing, watch the facts move:

```bash
shoulderd monitor                 # the last twenty movements, then every new one as it happens
shoulderd monitor --all           # the whole file first
shoulderd monitor --no-follow     # print and exit
shoulderd monitor --json          # the raw records
```

It reads the log file from section 5 - `--log=PATH` names another - and keeps the lines about
facts: stored, superseded, merged and dropped by the tidying pass, refused or failed writes, and
advice queued and injected. Each line carries the time, the scope and project or the session, the
category and the text, and the ids involved. A fact typed with `shoulderd fact add` shows as
`[cli]`. A daemon on `SHOULDER_LOG=stderr` writes no file, so there is nothing for `monitor` to
read; a container's log is `make logs` instead.

## Where configuration lives

Everything is environment driven, which used to raise an awkward question: whose
environment. A daemon you start by hand inherits the shell you typed in; a daemon started by an
editor adapter, a container or a service manager does not, and the failure is quiet - it comes up,
reports itself healthy, observes every turn and has no model to ask, so it stays silent and looks
like it is simply never finding anything to say.

So there is one file, and the daemon reads it itself. Nothing has to be exported for it to be found:

```bash
mkdir -p ~/.config/shoulder-daemon
cat > ~/.config/shoulder-daemon/env <<'EOF'
SHOULDER_LLM=glm-coding,gemini
GLM_API_KEY=...
GEMINI_API_KEY=...
SHOULDER_MEMORY_URL=http://127.0.0.1:8100
SHOULDER_MEMORY_KEY=...
EOF
chmod 600 ~/.config/shoulder-daemon/env
```

Restart the daemon and it is running on those settings. `$SHOULDER_ENV_FILE` names a different file
if you keep yours elsewhere, and anything already in the process environment wins over the file, so
a value you exported deliberately is never overridden by one you may have forgotten. The CLI reads
the same file, which is why `shoulderd doctor` works in a terminal that has sourced nothing.

Switching the store is that file and a restart: add `SHOULDER_MEMORY_URL` for a memory service,
remove it for the one built into the daemon.

`deploy/docker-compose.yml` reads `${SHOULDER_ENV_FILE:-.env}`, so with that
variable set `make up` uses your file, and without it falls back to `deploy/.env`
next to the compose file. Both are gitignored. If you have an old `deploy/.env`
lying around from an earlier experiment, the fallback will find it and use it in
preference to nothing, which is how a daemon ends up running on settings you
forgot you wrote.

`SHOULDER_TOKEN` has to match on both sides: the daemon checks it, and the
adapter sends it as `X-Shoulder-Token`. The daemon keeps them in step by
generating the value and writing it into both places, so this only comes apart
when somebody sets it themselves in one of them. When they differ every hook is
rejected and the session carries on as though nothing were installed, because
hooks fail open. `shoulderd doctor` reports that as `auth: N REJECTED`.

`make install-plugins`, which `make update` runs, writes the path-dependent
settings itself: `SHOULDER_START_CMD` for the checkout it is run from, a
generated `SHOULDER_TOKEN` if the file has none, and the same three values into
the `env` block of `~/.claude/settings.json`. Moving or renaming the checkout is
therefore repaired by running it again, rather than by hunting a stale absolute
path through two config files.

The OpenCode adapter reads the same file for any `SHOULDER_` variable its process does not already
have. An editor started from a desktop launcher inherits the session environment rather than your
login shell's, so exports are usually missing from it, and that file is what keeps such a session
observed anyway.

The Claude Code adapter cannot: its hooks carry `${SHOULDER_TOKEN}` and the editor interpolates it
from its own environment before any of our code runs. That is why the daemon writes the token into
`~/.claude/settings.json` rather than expecting you to. `SHOULDER_START_CMD` belongs there too when
you run the daemon from a checkout, since the editor is what runs it:

```json
{
  "env": {
    "SHOULDER_TOKEN": "a-shared-secret",
    "SHOULDER_ENV_FILE": "/home/you/.config/shoulder-daemon/env",
    "SHOULDER_START_CMD": "make -C /home/you/src/shoulder-daemon up"
  }
}
```

## Getting the daemon yourself

The Claude Code plugin fetches it for you. Every other route to the same binary:

- **Release binary** - every [release](https://github.com/QuittyMR/shoulder-daemon/releases/latest)
  carries Linux, macOS and Windows builds for amd64 and arm64 beside a `SHA256SUMS`; the
  [GitLab release](https://gitlab.com/quittymr/shoulder-daemon/-/releases) carries the same files.
- **`go install`** - into `$(go env GOPATH)/bin`, which needs to be on your `PATH`:

  ```bash
  go install gitlab.com/quittymr/shoulder-daemon/relay/cmd/shoulderd@latest
  ```

- **Container** - `ghcr.io/quittymr/shoulder-daemon` and
  `registry.gitlab.com/quittymr/shoulder-daemon`, both multi-arch.
  [`deploy/docker-compose.yml`](../deploy/docker-compose.yml) runs it on host networking so hooks
  still reach `127.0.0.1:8787`, and keeps its store on a volume.

`shoulderd version` says which one you have and where it came from, and `shoulderd doctor` tells you
when a newer release exists. A `shoulderd` already on `PATH` is used in preference to the fetched
one, so none of these is ever second-guessed by the plugin.

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
| `SHOULDER_MEMORY_URL` | Base URL of a memory service. Unset means the store built into the daemon. |
| `SHOULDER_MEMORY_PATH` | Where that built-in store writes. Defaults to `~/.local/share/shoulder-daemon/facts.json`. |

Then `SHOULDER_TOKEN` (generated for you; set it only to override),
`SHOULDER_ADDR`, `SHOULDER_MEMORY_KEY`, `SHOULDER_PICKINESS`, `SHOULDER_LOG` (the log file;
`~/.local/share/shoulder-daemon/shoulderd.log`, or `stderr` for none), `LOG_LEVEL`,
`SHOULDER_DRY_RUN`, `SHOULDER_IDLE_EXIT_MINUTES` (60; zero turns it off) and the `WINDOW_*`,
`BUDGET_*` and `ADVISOR_*` tuning knobs, all of which belong in the env file
described under "Where configuration lives".

**Models.** Gemini, GLM (pay-as-you-go and Coding Plan), OpenRouter, OpenCode Go,
OpenAI, and any OpenAI-compatible endpoint including a local Ollama. Comma-
separate `SHOULDER_LLM` for a failover chain. More coming.

**Memory.** The store built into the daemon by default, and
[mcp-memory-service](https://github.com/doobidoo/mcp-memory-service) when
`SHOULDER_MEMORY_URL` is set; section 6 covers both. Backends sit behind a
five-method `Connector` interface that names nothing specific to any product;
`memory.TestConnector` is an exported conformance suite a new one can run
against itself. More coming - ask for the one you want.

## After an update

```bash
git pull
make update
```

`make update` rebuilds the binary and the container image, replaces each
harness's installed copy of the adapter, and restarts the daemon. Then restart
your editor so it reloads the plugin, and run `shoulderd doctor`.

Updating a container install from before the transcript mount, run the
volume re-owning command from section 5 once, or the daemon comes up unable
to write its store.

The step that is easy to miss is the adapter. A harness runs the copy it took
when the plugin was installed, not your checkout, so editing the adapter here
changes nothing it loads. The failure is silent: the old copy keeps posting to
whatever address and header it was built against, so hooks either never arrive
or are rejected while the source on disk looks correct. `shoulderd doctor`
reports that as `plugin: STALE`.
