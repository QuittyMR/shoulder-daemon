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
