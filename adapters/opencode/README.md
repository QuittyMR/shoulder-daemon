# shoulder-daemon for OpenCode

Install it from npm, which is also what makes OpenCode list it under a name:

```bash
opencode plugin shoulder-daemon
```

That resolves the package, detects its `./server` entrypoint, and adds
`"shoulder-daemon"` to the `plugin` array of your OpenCode config. Pass
`--global` to write the user config instead of the project one.

Or copy the one file into `~/.config/opencode/plugins/`, or into
`.opencode/plugins/` for a single project. OpenCode loads both.

```bash
mkdir -p ~/.config/opencode/plugins
curl -o ~/.config/opencode/plugins/shoulder-daemon.js \
  https://gitlab.com/quittymr/shoulder-daemon/-/raw/main/adapters/opencode/shoulder-daemon.js
```

The two routes run identical code. They differ in what `/status` shows and in
who updates it: the npm package carries the version and upgrades with
`opencode plugin shoulder-daemon`, while a copied file is yours to refresh.

The plugin starts the daemon at load if nothing is answering, using an atomic
lock so that several editors opening together start exactly one. Set
`SHOULDER_START_CMD` if yours runs under a container or a service manager.

## What it does

| OpenCode | neutral event |
|---|---|
| `chat.message` | `user_prompt`, and the advice it gets back is held for the next request |
| `tool.execute.before` | `tool_call` |
| `tool.execute.after` | `tool_result` |
| `session.idle` | `turn_end`, carrying the assistant text and its reasoning |
| `session.compacted` | `compact` |
| `session.deleted` | `session_end` |

Advice is injected through `experimental.chat.system.transform`. That is the
only mechanism OpenCode re-evaluates on every request, so it is also the only
one that survives compaction: the others are replaced by the summary.

## Why it is written so defensively

OpenCode awaits every hook with no timeout, and dispatches through
`Effect.promise`, which treats a rejection as an unrecoverable defect rather
than a typed error. No call site catches it. **A plugin that throws takes the
user's turn down with it.**

Claude Code fails open on our behalf with a two-second hook timeout. Here that
is the plugin's job, so every hook body is wrapped and cannot rethrow, every
request carries an `AbortSignal` deadline, and everything except the prompt hook
is sent without being awaited. A stopped or wedged daemon costs an OpenCode
session nothing.

## Reasoning

OpenCode is the only harness that gives this anything. Its `ReasoningPart`
carries real text, accumulated per stream chunk, and the adapter forwards it as
`thinking` on the turn. Claude Code persists thinking blocks with an empty body,
so the same field is always empty there. How much arrives depends on the
provider - OpenCode inserts empty placeholders for models that emit none.

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `SHOULDER_ADDR` | `127.0.0.1:8787` | where the daemon is listening |
| `SHOULDER_TOKEN` | unset | must match the daemon's |
| `SHOULDER_TIMEOUT_MS` | `250` | deadline on every call to the daemon |
| `SHOULDER_START_CMD` | unset | how to start the daemon; plain `shoulderd` when unset |
