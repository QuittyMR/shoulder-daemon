# Phase 0 findings — Claude Code 2.1.251

Harness: `spikes/listener` (Go), `spikes/settings/*.json` supplied via `claude -p --settings`,
`--debug hooks --debug-file`. Raw captures in `spikes/captures/`, fixtures in
`testdata/hook-payloads/2.1.251/`.

## Verdicts

| ID | Question | Verdict |
|---|---|---|
| S1 | Do `type:"http"` hooks fire? | **Yes**, for `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `Stop`, `SessionEnd`. **No** for `SessionStart`. |
| S2 | Does Claude Code fail open on an unreachable hook URL? | **Yes.** `ECONNREFUSED` logged at ERROR, session completes, correct reply. |
| S3 | Allowlist and auth headers | Allowlist undefined = permitted. Non-matching = blocked + fail-open. Literal headers work; env interpolation needs **per-hook** `allowedEnvVars`. |
| S4 | Does `additionalContext` reach the model? | **Yes**, from both `UserPromptSubmit` and `PreToolUse`. Turn returned to the user in both. |
| S5 | Does `Stop` carry `last_assistant_message`? | **Yes**, complete text. |
| S6 | Is the `timeout` field enforced for HTTP hooks? | **Yes, in seconds.** Client aborts the request and fails open. |

## S1 — event coverage and payload shape

Fired, with listener-side service time 78–339 µs:

| Event | Body keys |
|---|---|
| `UserPromptSubmit` | `cwd, hook_event_name, permission_mode, prompt, prompt_id, session_id, transcript_path` |
| `PreToolUse` | `cwd, hook_event_name, permission_mode, prompt_id, session_id, tool_input, tool_name, tool_use_id, transcript_path` |
| `PostToolUse` | above + `duration_ms, tool_response` |
| `Stop` | `background_tasks, cwd, hook_event_name, last_assistant_message, permission_mode, prompt_id, session_crons, session_id, stop_hook_active, transcript_path` |
| `SessionEnd` | `cwd, hook_event_name, prompt_id, reason, session_id, transcript_path` |

**Blocking constraint:** `SessionStart` refuses HTTP hooks.

```
[DEBUG] Skipping HTTP hook http://127.0.0.1:8787/v1/hooks/claude-code/SessionStart
        — HTTP hooks are not supported for SessionStart
```

The guard in the 2.1.251 binary is exactly:

```js
Ue = r === "SessionStart" || r === "Setup"
   ? De.filter((Ne) => { if (Ne.hook.type === "http") { n(`Skipping HTTP hook ${Ne.hook.url} — HTTP hooks are not supported for ${r}`); return false } return true })
   : De
```

So `SessionStart` and `Setup` are the *only* two excluded events.

No authentication header is sent by default. Claude Code sends only
`Accept, Accept-Encoding, Connection, Content-Length, Content-Type, User-Agent`.

## S2 — fail-open

```
[ERROR] Hooks: HTTP hook error: connect ECONNREFUSED 127.0.0.1:8787
[DEBUG] "Hook UserPromptSubmit (UserPromptSubmit) error:\nconnect ECONNREFUSED 127.0.0.1:8787"
```

Session exit 0, reply correct. Wall clock, same prompt, 3 runs each:
listener down 10.76 / 12.04 / 8.45 s (mean 10.42); listener up 7.69 / 8.13 / 9.68 s (mean 8.50).
The ranges overlap and the spread is dominated by model latency. **No systematic stall was
measurable**, which is consistent with loopback `ECONNREFUSED` returning immediately. The dangerous
case is not a refused connection but a hung listener, and S6 covers it.

## S3 — allowlist and headers

- `allowedHttpHookUrls` **undefined** → all URLs permitted. S1/S4 ran with no allowlist at all.
- `allowedHttpHookUrls: ["https://example.com/*"]` → blocked, fail-open:
  `[WARN] HTTP hook blocked: http://127.0.0.1:8787/... does not match any pattern in allowedHttpHookUrls`
- `allowedHttpHookUrls: ["http://127.0.0.1:8787/*"]` → permitted.
- Literal header values arrive verbatim.
- Env interpolation: `${VAR}` and `$VAR` are both recognised. `{{VAR}}` and `${env:VAR}` pass
  through as literal text. Interpolation requires the **per-hook** field `allowedEnvVars`:

```
[WARN] Hooks: env var $GC_TOKEN not in allowedEnvVars, skipping interpolation
```

The settings-level `httpHookAllowedEnvVars` alone is **not** sufficient — per the binary's own
description it is a policy allowlist that each hook's `allowedEnvVars` is intersected with.
Working shape:

```json
{ "type": "http", "url": "http://127.0.0.1:8787/v1/hooks/claude-code/UserPromptSubmit",
  "timeout": 2, "headers": { "X-Guardian-Token": "${GUARDIAN_CLAW_TOKEN}" },
  "allowedEnvVars": ["GUARDIAN_CLAW_TOKEN"] }
```

## S4 — injection

`UserPromptSubmit` returning
`{"hookSpecificOutput":{"hookEventName":"UserPromptSubmit","additionalContext":"… GCNONCE-4A11 …"}}`
→ model replied `GCNONCE-4A11`. Same via `PreToolUse` → `GCNONCE-4B22`. Both sessions exited 0 with
`Stop` and `SessionEnd` firing normally, i.e. the turn was not consumed.

## S6 — timeout enforcement

Listener instrumented to detect client disconnect via `r.Context()`.

| Config | Delay | Server observed | Client gone |
|---|---|---|---|
| `timeout: 2` | 8000 ms | aborted at ~1–2 s | **true** |
| `timeout: 10` | 8000 ms | served full 8 s | false |

`timeout` is in **seconds** and is enforced for HTTP hooks; on expiry Claude Code abandons the
request and continues. A hung relay therefore costs at most `timeout` seconds per hook, bounded.

## Incidental discoveries

- Hooks support an `if` condition field; it is evaluated only for tool events
  (`Hook if condition "…" cannot be evaluated for non-tool event …`).
- Async hooks are detected from the **first line** of hook output
  (`Hooks: Checking first line for async: …`); the recognised field set is
  `{"async","hookEventName","behavior"}`.
- `--settings <file>` **merges** with user and global settings rather than replacing them — an
  unrelated global `SessionStart` command hook still fired during every spike run.

## Design consequences

1. **Drop the `SessionStart` hook entirely.** Open the session lazily on the first event that
   arrives. This removes the only thing that would have forced a host-side script, so the Claude
   Code adapter stays a pure `hooks.json` of URLs.
2. **Set `timeout: 2` on every hook.** Worst case for a wedged relay is 2 s per hook rather than
   unbounded.
3. **Ship the token as `${GUARDIAN_CLAW_TOKEN}` with per-hook `allowedEnvVars`.** No secret in the
   committed settings file.
4. **Install and `doctor` must check `allowedHttpHookUrls`.** If a user or an enterprise policy has
   set it, loopback must be added or every hook is silently blocked — with only a WARN in a debug
   log that nobody reads.
