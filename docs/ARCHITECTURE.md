# How shoulder-daemon works

The relay absorbs hook traffic in microseconds and does everything slow
somewhere else. That separation is the whole design.

```
  coding harness                shoulderd (relay)              background
  (Claude Code)                                                worker
       │                              │                            │
       ├── POST /v1/hooks/… ─────────▶│  append to the in-memory    │
       │◀───────── {} (immediately) ──│  session window, enqueue    │
       │                              │─────── non-blocking ──────▶ │
       │                              │                             ├─▶ decision model
       │                              │                             │   (SHOULDER_LLM)
       │                              │                             ├─▶ the store: a file of
       │                              │                             │   its own, or a service
       │                              │                             │   (SHOULDER_MEMORY_URL)
       │                              │◀──── at most one short note ┘
       ├── next hook ────────────────▶│
       │◀── additionalContext ────────│
```

A hook handler may only touch in-memory structures. It never calls the model,
never calls the memory backend, and never writes to disk, so it answers at
memory speed even when everything downstream is wedged or unreachable. That
restriction is a test, not a comment: `TestHotPathHasNoSlowDependencies` fails
the build if the hook package acquires a slow dependency, and `make bench`
measures the round trip.

Hooks fail open. If the daemon is stopped, misconfigured, or slow, Claude Code's
two-second hook timeout expires and your session continues exactly as if nothing
were installed.

## What a session is made of

Every adapter maps its harness's hooks onto one neutral vocabulary:
`user_prompt`, `tool_call`, `tool_result`, `tool_failure`, `assistant_message`,
`answer_end`, `compact` and `session_end`. The advisor reads a window of these
rendered as `<user>`, `<assistant>`, `<tool>`, `<result>` and `<compact/>`
lines; the recall query that searches memory is built from the prose among
them, never from tool traffic.

A subagent runs under its parent's session id, so its traffic is part of the
same window. Its prompt is the input of the `Agent` tool call that spawns it,
which the relay records as a `user_prompt` from the agent as well as the tool
call; its start arrives on `SubagentStart`, recorded as an `agent_start`, and
is the first event to carry the id the harness gave it; its answer arrives on
`SubagentStop`, recorded as an `answer_end` from the agent. All carry
`origin: agent` and the subagent's type, and the prompt and the answer render
as `<agent type="…">` and `<agent-result type="…">`. They count as prose for
recall and are advised like the main thread, except that the main thread's
event counter is not advanced by a subagent's prompt or stop. Advice from a subagent's
prompt is addressed to the call that spawned it; the registry pairs that call
with the id the start reports, oldest spawn of the type first, and the advice
is handed only to hooks fired from inside that subagent - at its start, when
it is ready by then, or at its next tool call. Until the id is known it may go
to a subagent of the same type and never to the main thread. Advice with no
address goes to whichever hook can carry it first. A subagent's injections
count against the session's character cap and leave the main thread's event
gap alone.

Every prompt and every answer end starts a consult of its own, the main
thread's and a subagent's alike, and the consults of one session run
concurrently: none is dropped and none waits for another. A consult reads the
window as it stands when it starts, so two that overlap can reach the same
conclusion. Advice whose text is already pending for the same session and
the same addressee is not queued again, counted in
`shoulder_advice_duplicate_total`, and a fact stated twice is settled by the
store, which refuses the near-duplicate and supersedes. The one write that
is serialised is the rewrite of the session's keyword record, behind a lock
that belongs to the session: each consult folds its keywords in and
supersedes the record the consult before it left.

## Local and global

Some of what it learns is about one repository - the main branch is called
master, the integration tests need a live Postgres - and is noise everywhere
else. The rest is about you and how you work - prefers terse answers, always
runs the linter before pushing - and should follow you into every checkout you
open.

Every write says which it is, and there's no default anywhere; a record with no
scope is rejected rather than filed under a guess, because a guess is how one
project's memory ends up poisoning another's. Local is keyed to the root of the
git worktree, stored as a twelve-character hash so a memory service shared
between machines doesn't learn your directory layout. Recall reads both, since
your preferences have to reach whichever project you're actually in.

Write to it without saying which you meant and it tells you to pass `--local`
or `--global`, then exits. It doesn't pick one for you. Reads go the other way
and default to the project your terminal is standing in, because reading the
wrong scope costs you a note and writing to the wrong one costs you a
memory that surfaces where it doesn't belong.
