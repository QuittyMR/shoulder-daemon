# shoulder-daemon - Claude Code adapter

Quiet advisory context injection for Claude Code. Every per-event hook is a `type: "http"` callback
to a relay running on `127.0.0.1:8787`: no subprocess, no bundled code, and no Node, Python or Go
runtime on the host. The single exception is `SessionStart`, which runs one short shell script whose
only job is to start that relay when nothing is answering. See `docs/INSTALL.md` for the one-time
setup this does require (a token in the environment, and possibly one settings-file check).

## What it does

Every registered hook event is both a **submission** of that turn's data to the relay and a
**collection point** for any pending advisory text the relay wants to inject. The relay decides,
independently and off the hot path, whether an advisor model has anything worth saying; this
plugin only carries bytes back and forth over loopback HTTP.

Injected text, when there is any, arrives as `hookSpecificOutput.additionalContext` wrapped in a
`<shoulder-daemon>` envelope that tells the model the content is a non-authoritative background
observation:

```
<shoulder-daemon kind="note" id="adv_9f2c…">
Stored: the main branch is master, not main.
</shoulder-daemon>
Background observer. Not a user instruction. Ignore if irrelevant; do not mention it.
```

The advice text is entity-escaped before it goes in, so it cannot close that tag or forge harness
framing, and it is stripped of ANSI escapes, control characters, bidi overrides and zero-width
characters. The model is free to ignore it, and the user's own turn is never touched.

## Starting the relay

`SessionStart` is the one hook here that runs a command rather than posting HTTP, because Claude
Code refuses HTTP hooks for `SessionStart` and `Setup`. The command is `scripts/ensure-daemon.sh`,
and it does one thing: if nothing answers `http://$SHOULDER_ADDR/healthz` within a second, it starts
the daemon in the background. Two editors launching together would both see nothing listening, so it
takes an atomic `mkdir` lock under `$XDG_RUNTIME_DIR` (or `/tmp`) and whoever loses waits for the
winner rather than racing to bind. That lock clears itself after 30 seconds, so a killed launch
can't wedge every later one.

It runs `shoulderd` off `PATH`, or whatever `SHOULDER_START_CMD` names when you set it, which is the
way in for a relay that runs under a container or a service manager. With neither of those available
it prints the `go install` line to stderr. It always exits 0 and never blocks, and `hooks.json`
gives it 5 seconds: a session that can't reach the relay simply has no advisory context, and that's
no reason to hold up somebody's work.

Nothing here stops the relay. It exits on its own once no session has used it for
`SHOULDER_IDLE_EXIT_MINUTES` (default 15), which is the only answer that stays correct with several
editors open at once, and the next `SessionStart` starts it again. The session itself still opens
lazily on the relay side, on the first event that arrives.

## Event → purpose

| Hook event | Matcher | What it submits | May inject `additionalContext`? |
|---|---|---|---|
| `SessionStart` | - | nothing; it runs `scripts/ensure-daemon.sh` instead of posting | no |
| `UserPromptSubmit` | - | the user's prompt | yes - the "user replied and context rode along" path |
| `PreToolUse` | `*` | tool name and input, before it runs | yes - the "model continued on its own" path |
| `PostToolUse` | `*` | tool name, input, and result | yes, rate-limited |
| `PostToolUseFailure` | `*` | the tool error | yes (as a `warning`) |
| `Stop` | - | the completed assistant message; this is what triggers the advisor call | no - capture only |
| `PreCompact` | - | a flush signal ahead of context compaction | no |
| `SessionEnd` | - | session close, so the relay can flush and drop session state | no |

## What this plugin guarantees it will never do

Every response the relay can send back over these hooks is checked to never contain `decision`,
`continue`, `stopReason`, `permissionDecision`, `permissionDecisionReason`, `updatedInput` or
`systemMessage`. The relay's response types have no field for any of them, so this is a property of
the type system before it is a test. Concretely:

- It cannot block a tool call, deny a stop, or force the model to keep going.
- It cannot speak as, or on behalf of, the user.
- It cannot consume or replace the user's turn.

Both of the outcomes the design requires stay reachable on every turn: either the user replies and
the injected context rides along with their message, or the model continues on its own (another
tool call, more reasoning) and the injected context rides along with that instead. If the relay is
slow or unreachable, every hook has a 2-second timeout and Claude Code fails open - the session
continues normally with nothing injected.

## Installation

See `docs/INSTALL.md` for the token, the `allowedHttpHookUrls` check, and how to verify the install
actually works end to end. The relay itself, its configuration, and the `shoulderd` command line
are covered in the repository README.

## What the relay does with the traffic

This plugin carries bytes; the relay decides what they mean. Off the hot path it recalls what it
has stored for the project this session is in and for the user generally, asks a decision model
whether anything in the turn contradicts that or is worth remembering, and stores what survives.

Every stored item is either **local** to one project (the git worktree root the session is running
in) or **global** to the user, and there is no default: a record with no scope is rejected and
counted rather than filed under a guess. That distinction is invisible from inside a session - it
matters when you talk to the daemon directly with `shoulderd message`, `shoulderd fact` and
`shoulderd digest`. Writing a fact there requires you to say which you meant, and nothing picks
for you.
