# The decision model contract

shoulder-daemon's relay contains no model and no opinion about what advice
should be. Once per turn boundary, off the session's hot path, it hands a
rendered window of the session and whatever its memory backend matched to one
provider, lets that provider look things up if it wants to, and acts on the JSON
that comes back at the end. Anything that can answer that prompt is a valid
decision model.

Almost every provider speaks the OpenAI chat completions wire format, so a
provider is three things: a base URL, an auth header and a model id. That is why
the boundary is this small:

```go
type Provider interface {
	Name() string
	Complete(ctx context.Context, system, user string) (string, error)
	Chat(ctx context.Context, msgs []Message, tools []Tool) (Message, error)
}
```

`Complete` asks one question and takes one answer, which is what the digest and
the CLI message path want. `Chat` carries a message list and a tool list, because
the decision step is an agent rather than a single question: it can search memory
again, or read back what this session has been about, before it commits to an
answer. A provider that can't do tool calls still works. It never asks for one,
and the loop ends on its first reply.

## Choosing one

`SHOULDER_LLM` names a preset:

| `SHOULDER_LLM` | Base URL | Default model | Key variable |
|---|---|---|---|
| `gemini` | `https://generativelanguage.googleapis.com/v1beta/openai` | `gemini-flash-lite-latest` | `GEMINI_API_KEY` |
| `glm` | `https://api.z.ai/api/paas/v4` | `glm-4.7-flash` | `GLM_API_KEY` |
| `glm-coding` | `https://api.z.ai/api/coding/paas/v4` | `glm-5.3-flash` | `GLM_API_KEY` |
| `openrouter` | `https://openrouter.ai/api/v1` | `google/gemini-2.5-flash-lite` | `OPENROUTER_API_KEY` |
| `openai` | `https://api.openai.com/v1` | `gpt-5.2-mini` | `OPENAI_API_KEY` |
| `local` | `http://127.0.0.1:11434/v1` | `qwen2.5-coder:7b` | none |
| `opencode-go` | `https://opencode.ai/zen/go/v1` | `glm-5.3-flash` | `OPENCODE_API_KEY` |

Each base URL already includes the provider's version segment, because every
provider spells it differently, and getting it wrong is a silent 404 rather than
an error that says what happened.

Four traps are encoded in the presets rather than left to be rediscovered:

- **`gemini`** is an AI Studio key against the OpenAI compatibility layer.
  Vertex AI is a different base URL and wants an OAuth token, not this key.
  `gemini-flash-lite-latest` is a moving alias - pin a dated id if a silent
  model change would matter to you.
- **`glm` and `glm-coding` are not interchangeable.** A Coding Plan key returns
  401 against the pay-as-you-go base URL. `glm-coding` also sends
  `"thinking": {"type": "disabled"}`: reasoning is on by default there and is
  pure waste on a classification task. Measured on `glm-5.3-flash`, omitting the
  field spent 54 reasoning tokens to answer "OK" in 3.
- **`openai`** is Platform billing. A ChatGPT subscription does not grant API
  access.
- **`opencode-go`** is a subscription gateway in front of open-weight coding
  models, billed monthly rather than per token, which is what makes it a
  reasonable place to put a job that runs on every turn. It's an ordinary
  OpenAI-compatible HTTP endpoint: `OPENCODE_API_KEY` goes out as a Bearer
  token, and nothing shells out to the `opencode` CLI or reads its config. It's
  a separate namespace from OpenCode Zen, which carries the frontier models.

### Anything else

`local` is only a preset pointing at Ollama's default port. Override the base
URL and it covers everything else that serves `/v1/chat/completions`:

```bash
# vLLM
SHOULDER_LLM=local
SHOULDER_LLM_BASE_URL=http://127.0.0.1:8000/v1
SHOULDER_LLM_MODEL=Qwen/Qwen2.5-Coder-7B-Instruct
SHOULDER_LLM_KEY=<whatever you passed to --api-key, if any>

# LiteLLM
SHOULDER_LLM=local
SHOULDER_LLM_BASE_URL=http://127.0.0.1:4000/v1
SHOULDER_LLM_MODEL=shoulder          # a model_name from your LiteLLM config
SHOULDER_LLM_KEY=sk-…                # your LiteLLM virtual key
```

`llama.cpp`'s `llama-server` works the same way with `SHOULDER_LLM_MODEL` set to
whatever it reports at `/v1/models`.

### Failover chains

A comma-separated `SHOULDER_LLM` is tried left to right, first success wins:

```bash
SHOULDER_LLM=glm-coding,gemini
```

This exists so a subscription-backed provider can lead and a metered one can
cover its outages, without the decision step becoming a single point of failure.
A cancelled or expired context is not retried down the chain - it would fail
every provider identically.

`SHOULDER_LLM_BASE_URL`, `SHOULDER_LLM_MODEL` and `SHOULDER_LLM_KEY` apply only
when exactly one provider is named. In a chain each provider takes its key from
its own preset variable, because an override would otherwise be sent to a
provider it does not belong to.

### Changing it while the daemon runs

`shoulderd config set --provider=NAME` swaps the provider without a restart, effective on the next
decision pass; add `--model=ID` in the same call to also pin a model, otherwise the provider's own
default applies - model ids don't carry between providers, so naming one alone can't keep the old
model. The provider's key must already be in the daemon's environment; `config set` cannot supply
one. Like the overrides above, `--model` on its own is refused when the provider in use is a
comma-separated chain, since the chain's providers don't share model ids either. `shoulderd config
show` (or a bare `shoulderd config`) reports the provider and model actually in use, and none of it is
persisted: a restart returns to whatever `SHOULDER_LLM` and its overrides say. `docs/INSTALL.md`
covers the other two settings this same command reaches - the log level and the pickiness.

## The request

```
POST {base}/chat/completions
Content-Type: application/json
Authorization: Bearer {key}          # omitted when the preset has no key

{
  "model": "…",
  "temperature": 0.2,
  "max_tokens": 1200,
  "messages": [
    { "role": "system", "content": "<the decision prompt>" },
    { "role": "user",   "content": "<the turn window and the recalled facts>" }
  ],
  "tools": [
    { "type": "function", "function": { "name": "search_memory",   … } },
    { "type": "function", "function": { "name": "session_history", … } }
  ]
}
```

That is one round trip, and a decision pass is up to four of them. When the reply
carries tool calls, the relay answers each one, appends the assistant message and
the tool results to `messages`, and posts again. The fourth reply is the last one
read: a model still asking for tools there gets no further step, its pending calls
are never run, and whatever text it had already produced is what gets parsed.
That case counts `shoulder_decision_steps_exhausted_total` and logs a warning, so
a model that never converges is visible rather than merely quiet.

`ADVISOR_TIMEOUT_SECONDS` (default 90) bounds the decision pass end to end rather
than one request: every model round trip the step cap allows, plus the memory
lookup that happens between each pair of them. Sized for a single question, the
model that actually uses the tools it was given is exactly the one that gets cut
off. Each individual response body is read up to 1 MB and no further. That
variable keeps its `ADVISOR_` name from the reference-advisor wiring;
`ADVISOR_BASE_URL`, `ADVISOR_MODEL`, `ADVISOR_API_KEY` and
`ADVISOR_SYSTEM_PROMPT` are left over from the same era and no longer reach this
path.

The user message is two sections:

```
<recent-turn>
<user>refactor the parser</user>
<tool name="Read">/src/parser.go</tool>
<result name="Read">package parser…</result>
<tool name="Bash">go test ./...</tool>
<result name="Bash" error="true">FAIL parser_test.go:41</result>
<assistant>I rewrote the tokenizer loop.</assistant>
</recent-turn>

<stored-facts>
id=a3f19c4e8b2d7015 scope=local category=structure: the main branch is master
id=7c02b8de41af9330 scope=global category=preference: prefers terse answers
</stored-facts>
```

The turn window is deliberately lossy: the model needs the shape of the work,
not a transcript. Tool arguments are reduced to the one field that identifies
what the call touched - the command, the file path, the search pattern and where
it looked - and tool results are clipped at 1500 characters. The window keeps at
most `WINDOW_EVENTS` events (default 40) inside `WINDOW_CHARS` characters
(default 12000), rendered oldest first and dropped from the oldest end when the
character budget runs out.

The `<assistant>` block is the whole turn's text, not only its last message.
Claude Code's Stop hook carries only the final text block, so the daemon reads
the rest from the session transcript the hook names; when that file cannot be
read, the hook's message is all it has, and it says so once per session in the
log. `<thinking>` blocks appear only when the harness supplies reasoning text.
Claude Code does not: every thinking block it persists has an empty body.
OpenCode's `ReasoningPart` does carry text, so the same model sees more from an
OpenCode session than a Claude Code one.

The stored facts are what a semantic search over the memory backend matched,
capped at eight. More is not better here: the model has to notice one
contradiction, not read a filing cabinet. The search query is built from the
recent prose only - the tool traffic is the bulk of a window but it drags a
semantic search towards whichever files were touched rather than towards what
was actually said.

Recall reads both the session's project and the user's global knowledge, so a
preference recorded in one repository reaches every other one.

### The two tools

The decision model is an agent, and the normal case is calling neither tool.

`search_memory({"query": "…", "limit": 5, "min_score": 0.0})` searches the stored
facts again, over the same two scopes the first search read. The results of that
first search are already in the prompt above, so this one exists to look again
more broadly: different words, a larger limit, a lower floor. `query` is the only
required argument. `limit` is clamped to 25 - omitted, zero, or larger, and it
becomes 25 - because the number comes from the model and sizes the next prompt,
and an unbounded one lets a single call spend the whole context window.
`min_score` drops matches the backend scored below it, and a record the backend
left unscored is never dropped. It answers `(nothing matched)` when nothing does.

`session_history({})` returns the keywords from every earlier turn of this
session, in order, on one comma-separated line. It's what makes a bare "do it" or
"same for the other one" readable: the turn alone doesn't say what it's about and
what came before does. It answers `nothing yet` on the first turn.

A tool that fails hands its error text back to the model rather than ending the
pass, because the model can retry with different arguments. Every call of either
counts `shoulder_decision_tool_call_total`. Both answer about this session only:
`search_memory` reads the scopes this session may read, and `session_history`
reads the note this session has been keeping.

## The reply

A standard chat completion, and only `choices[0].message` is read: its
`tool_calls` while the loop is still running, its `content` at the end. That
final content must be a JSON object with three fields:

```json
{
  "inject": "",
  "keywords": [],
  "facts": [
    {
      "content": "the integration tests need a live Postgres on 5544",
      "category": "structure",
      "scope": "local",
      "tags": [],
      "supersedes": ""
    }
  ]
}
```

**`inject`** is what to say to the session, and empty is the correct and most
common answer. The prompt asks for one short note, spoken in two cases: a stored
fact contradicts what the assistant just said or is about to do, or a stored
fact says how this codebase does the thing just asked for, which the assistant
would otherwise spend the turn searching for.

**`keywords`** are the terms the model took from the turn and from whatever it
just injected: nouns and identifiers - file paths, function and type names,
commands, packages, the subject being worked on - rather than verbs. They're
folded into one running note per session, and that note is what `session_history`
reads back, so a bare "do it" on a later turn still means something.

That note is a stored record like any other, with one difference that decides
everything about it: its kind is `session`, where a fact carries the zero value.
A session record is local by definition, filed under the project this session is
running in, and it's rewritten once per turn rather than appended to - each turn
supersedes the last one, so there's one record per session and not one per turn.
A read that names no kind is asking for facts, and recall, a digest and
`shoulderd fact list` all name none, so the note never appears in any of them and
is never quoted back to a person as something the daemon learned. It's worth
having on the next turn and noise a week later. Writes count
`shoulder_session_keywords_stored_total` the first time and
`shoulder_session_keywords_superseded_total` after that; a session whose
directory doesn't resolve to a project has nowhere to file it and counts
`shoulder_session_keywords_no_project_total` instead.

How many a turn may add is cut after parsing rather than trusted to the model:
eight for a short turn, twenty-five for a long one, split at roughly 500 tokens
of rendered turn. The model is told those numbers too, and they're enforced
anyway, because a note that grows at whatever rate the model picks is a prompt
nobody sized.

**`facts`** are durable statements the turn established that are not already
stored - something that would still be true and useful in another session next
month. `category` must be one of `decision`, `constraint`, `preference`,
`correction`, `structure`, `reference`. That vocabulary is closed, and anything
outside it is dropped rather than passed through: shoulder-daemon can only
recall on categories it knows, and it will not hand a backend a value whose
treatment it cannot see. `supersedes` names the id of a stored fact this one
replaces, and is honoured only when that fact sits in the same scope and
project as this one. A correction crossing that line is not a correction.

**`scope`** is required on every fact and is either `local` or `global`. Local
means it is about this codebase and is noise everywhere else; global means it is
about the user and follows them between repositories. There is no default: a
fact with no scope, or an unrecognised one, is dropped by the pipeline and
counted as `shoulder_facts_missing_scope_total`. The model is asked to choose,
and choosing wrongly is recoverable; not choosing is not.

### Silence

All of these mean "inject nothing": an empty string, whitespace only, and the
tokens `NOOP` and `none` (both case-insensitive, surrounding whitespace ignored).
A response with no `choices` is also silence, and so is a loop that ended with
the model having produced no text at all.

### Tolerance

Small models mangle JSON, so `ParseDecision` strips code fences, leading prose
and trailing commentary, and takes the outermost braces. A reply cut off before
its closing brace still yields its injection if that field was already complete;
the facts are dropped in that case, because a half-written fact must never be
stored. A reply that cannot be parsed at all is treated as silence, never as an
error worth disturbing the session over.

## What happens to an injection

Text that survives is:

1. entity-escaped - every `<`, `>` and `&` - so it can never close the advisory
   envelope or forge harness framing;
2. stripped of ANSI escapes, control characters, bidi overrides and zero-width
   characters;
3. truncated to `BUDGET_MAX_CHARS` (default 800);
4. put through the budget gate, which by default permits one note every three
   turns and 4000 characters per session, and holds it for at most two turns
   before it expires.

A model cannot make itself heard more often by talking more. Write the prompt to
stay quiet.

The prompt that produces all of this is `prompts.Decision`, in
`relay/internal/prompts`. Changing it means changing the JSON contract above, so
it is code rather than configuration.

## Failure is the relay's problem, not the session's

A non-2xx status, a malformed body, an oversized body, a connection refused, or
a response slower than `ADVISOR_TIMEOUT_SECONDS` all produce exactly one
outcome: no injection, one counter increment, one log line. The coding session
is never affected, because nothing in it was ever waiting on this call.

A provider that fails part way through the loop is not a total loss: text the
model had already produced outlives the round trip that failed, and is parsed as
if the model had stopped there. Only a pass with nothing at all to show for it is
reported as an error.

The same is true of the memory backend: a failed search is counted and the pass
continues with no recall rather than being abandoned. A tool call that fails is
not even that much - the error text goes back to the model as the tool's result.

Set `SHOULDER_DRY_RUN=1` to run the entire pipeline, log every injection and
every fact it would have written, and perform neither. It is the right way to
evaluate a new model or a changed prompt against real sessions.

## The reference advisor

`advisor-echo/` is a minimal OpenAI-compatible server that exists to prove the
wiring, not to give good advice. `ECHO_MODE=noop` always answers `NOOP`;
`fixed` returns `ECHO_TEXT`; `echo` summarises the last user message.
`ECHO_DELAY_MS` and `ECHO_FAIL_RATE` let you watch the relay absorb a slow or
failing backend without the session noticing. It serves
`POST /v1/chat/completions` and `/healthz`, listening on `ADVISOR_ADDR` (default
`:9090`), so:

```bash
SHOULDER_LLM=local
SHOULDER_LLM_BASE_URL=http://127.0.0.1:9090/v1
```

`ECHO_FAIL_RATE=1.0` is the quickest proof that a completely broken backend
costs the coding session nothing.

## Privacy

The turn window contains the user's prompts, the assistant's replies, the
commands it ran and the output it saw. All of it goes to whichever provider you
configured. If that is a hosted model, source code leaves the machine. The
`local` preset and `advisor-echo` are there so that it does not have to.
