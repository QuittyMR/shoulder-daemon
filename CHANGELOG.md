# Changelog

Notable changes to shoulder-daemon. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[semantic versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Subagent prompts and results are observed as part of the session: the
  `Agent` tool call's prompt and the `SubagentStop` answer are consulted like
  the user's own prompt and answer end, under the parent's session id. The
  plugin now registers `SubagentStart`, the first hook to carry the id Claude
  Code gave the agent; the relay records it as an `agent_start` and pairs it
  with the `Agent` call that spawned it, oldest of the type first. Advice from
  a subagent's prompt is queued at the action level, addressed to that call
  and from there to the id, and lands at the subagent's start when it is
  ready by then or at its next tool call; neither the main thread nor a
  sibling takes it. A subagent's injections are charged to the session's
  character cap and leave the main thread's event gap alone. A subagent's event
  may add findings and facts but never rules or preferences: those are
  dropped before the write, counted in
  `shoulder_facts_agent_rule_dropped_total`, and logged with their content. A
  subagent's stop does not advance the session's event counter and does not start the
  periodic tidy. A neutral event that names an `agent_id` is taken as the
  agent's whether or not it also sends `origin`.
- An optional triage step in front of the decision model, backed by TypeSafe's
  Jev (System One). With `SHOULDER_TRIAGE=jev` and `TYPESAFE_API_KEY`, every
  event is first classified as needing nothing, a new fact, a change to a stored
  fact, or a stored fact repeated to the session. A confident "repeat" injects
  the stored fact as it is at the next tool call, and a confident "nothing" at
  pickiness `balanced` or stricter settles it, both without calling the
  decision model; everything else, including "nothing" at `eager` or `open`, a
  verdict under `SHOULDER_JEV_MIN_CONFIDENCE` (0.6) and any triage failure,
  goes to the decision model unchanged. Triage is cut off after a quarter of
  `ADVISOR_TIMEOUT_SECONDS`, at most 5s, so a stalled Jev does not use up the
  decision model's time, and its latency is reported as
  `shoulder_hook_latency_seconds{event="triage"}`. Triage also runs with no
  `SHOULDER_LLM`, where a fact it wants written is counted in
  `shoulder_triage_unhandled_total` rather than dropped. `shoulderd config
  show` reports the triage in use.
- `shoulderd doctor` reports the decision model on an `llm:` line and compares
  the provider, the model where the env file names one, and the store with
  what the env file asks for. `/v1/cli/config` and `/v1/cli/memory` now say
  where each value came from - the env file, the process environment,
  `config set`, or nothing - and which file the daemon read. A difference in a
  value from the file or from nothing is a daemon started before the file was
  edited or from another file, and fails as `MISMATCH`; one from the process
  environment or `config set` was chosen over the file and is printed as a
  note. Doctor also fails when there is no decision model, and when the store
  failed to open at start, which it reports with the reason rather than as no
  store. A triage with no decision model reads `llm: none (triage only)` and
  passes. While nothing listens, it shows the end of the start command's
  output. `--liveness` is unchanged.
- `shoulderd env path|get|set|unset` reads and writes the env file with the
  grammar the daemon reads it with; `set` changes a setting where it first
  stands, takes a secret on standard input and writes through a link. The
  setup skill's `env-set.sh` and `make install-plugins` now go through it, and
  the token the daemon generates is written single-quoted, so every value they
  write reads back as itself. A leading `~/` in `SHOULDER_ENV_FILE` is home
  everywhere the file is resolved.
- `make up-check` prints whether `make up` would recreate the relay, and the
  names of the variables behind the answer, without doing anything.
- `make check-stack` runs the compose stack through `make up` under a project,
  a port, a volume, an env file, a `deploy/.env` and a state directory of its
  own, with a stub model and a stub store, and fails unless compose reads that
  env file and no other, `make up` recreates a relay that is stale or predates
  the hash and leaves a current one alone, and doctor reports the configured
  model and store as coming from the file. It leaves a running install alone
  and removes what it created.

### Changed

- The word "turn" is gone from the project: the unit is the event, one
  utterance by the user or by an agent, and nothing groups a prompt with its
  answer. Renamed with it:
  - the wire kind `turn_end` is `answer_end`. `/v1/events` still accepts
    `turn_end` from an adapter installed before this, and records it as an
    `answer_end`; the OpenCode adapter sends the new name;
  - the setting `BUDGET_MIN_TURN_GAP` is `BUDGET_MIN_EVENT_GAP`. The old key
    is still read while the new one is unset, and the daemon logs one line
    at start naming the replacement;
  - the metric `shoulder_advice_suppressed_turn_gap_total` is
    `shoulder_advice_suppressed_event_gap_total`, with no alias;
  - in the advice a neutral adapter is answered with, `created_turn` and
    `ttl_turns` are `created_event` and `ttl_events`; in the `/v1/sessions`
    listing, `turn` is `main_events`; in the log, the `turn` attribute of
    `advice queued` is `main_events`, which `shoulderd monitor` prints as
    `event N`;
  - the decision prompt's `<recent-turn>` tag is `<recent-events>`, the state
    sent to Jev names the window `recent_events`, and both prompts speak of
    events;
  - `replay -max-turns` is `replay -max-answers`.
- The session's counter, which advice is aged and budgeted in, advances on
  every prompt of the user and on every answer end of the main thread, where
  it advanced on the answer end alone; a subagent's prompt and stop still
  leave it alone. Everything counted in it is doubled so that a session
  behaves as before: the default of `BUDGET_MIN_EVENT_GAP` is 6 where it was 3,
  advice expires 4 counts after it was written where it was 2, and the
  periodic tidy runs every 10 where it was 5. A gap set in
  an env file keeps its number and now spans half as much of the session:
  double it to keep the gap it had. The tidy runs at the first answer end in
  each span of 10, so a prompt whose answer was interrupted does not put it
  off. A note delivered before the first answer ended did not open the gap;
  it does now. A note written at an answer end is current for the next two
  prompts, where it was three.
- Every prompt and every answer end is consulted, the main thread's and a
  subagent's alike, and the consults of one session run concurrently. Before,
  one consult ran per session at a time and a prompt or an answer end that
  arrived meanwhile was dropped, so agents spawned together were not all
  advised and an answer end behind a slow consult lost its facts.
  `shoulder_advisor_skipped_inflight_total` is gone with the drop, and
  `advisor_in_flight` is gone from the session summary. Only the rewrite of
  the session's keyword record is serialised, one consult at a time. Advice
  whose text is already pending for the same session and addressee is not
  queued twice and is counted in `shoulder_advice_duplicate_total`.
- The fact categories are now `finding` (something a session established by
  looking), `fact` (a durable truth about the project or the machine), `rule`
  (a constraint or decision that governs how work is done here) and
  `preference` (how the person wants work or communication done; private).
  A `rule` or a `preference` is stored only when the user said it: the
  decision prompt refuses one from an agent line or from what the assistant
  concluded, and `facts.UserOnly` names the two for the pipeline to enforce on
  a consult that comes from a subagent. The old names map forward on read, on
  `fact add`, on migration and in the model's reply: `decision`, `constraint`
  and `correction` become `rule`, `structure` and `reference` become `fact`.
  The docs store files new records in `FINDINGS`, `FACTS`, `RULES` and
  `USER.shoulder.md`, writes every category under its current name, and keeps
  reading and correcting in place the `ARCHITECTURE`, `DECISIONS`,
  `CONVENTIONS` and `REFERENCES.shoulder.md` files an older daemon wrote.
- Recall searches each scope for the recent prose as a whole and once more for
  each of its sentences, up to sixteen, and merges the hits by record with the
  best score kept, so a stored fact that matches one sentence closely is found
  rather than averaged away by the rest of the window. A sentence ends at a
  line break or at a terminator that ends a word, so a file name, a version
  or a hostname is not cut in two; a one-word piece is not searched, and a
  window over the cap keeps its newest sentences. The searches run four at a
  time; the cut is counted in `shoulder_recall_sentences_dropped_total`.
- The built-in store returns a legacy category under its current name on
  every read, as the docs store and the memory service already did, so a
  `facts.json` written by an older daemon no longer puts `decision` or
  `structure` in front of the decision model or in `shoulderd fact list`.
- The registry no longer holds facts recorded explicitly for an event: nothing
  ever recorded one, so the path that reconciled them with the model's was
  dead, and an event triage settles now writes nothing.

### Fixed

- The container reads the daemon's one env file, `$SHOULDER_ENV_FILE` or
  `${XDG_CONFIG_HOME:-~/.config}/shoulder-daemon/env`, the file the docs tell
  you to write and the CLI and the adapters read, and no other. It used to read
  `deploy/.env` instead, so an install configured as documented ran with no
  model and the built-in store, silently. `deploy/.env` is no longer read for
  the daemon's settings: move them into the env file. The Makefile resolves
  the path; compose run by hand needs `SHOULDER_ENV_FILE` set and refuses to
  start without it. `make up` and `make update` name, without values,
  whatever `deploy/.env` still sets, so a setting left there is not lost
  unannounced. `ADVISOR_BASE_URL`, `ADVISOR_MODEL`,
  `ADVISOR_API_KEY` and `ADVISOR_SYSTEM_PROMPT`, which had no effect, are no
  longer read at all.
- The stack is run with podman and podman-compose by name. It needed podman
  already, for `userns_mode: keep-id`; `docker compose` and `podman compose`,
  which picks docker-compose when it is installed, are no longer used.
- The daemon, the CLI and the OpenCode adapter read the env file with the
  grammar podman-compose reads it with, python-dotenv's: a ` # comment` after
  an unquoted value is dropped, a `#` inside quotes is kept, double quotes take
  escapes such as `\n` and `\"`, single quotes take only `\'` and `\\`, and
  `${NAME}` and `${NAME:-default}` are expanded. They used to keep the comment
  and the backslashes and leave `${...}` alone, so a containerised daemon and
  doctor, or a bare daemon and compose, could read one line two ways.
- `make up` recreates the relay when its env file changed since the relay was
  created, by the file's hash, which compose puts in the relay's environment.
  It passes `--no-recreate`, so an edited file used to reach a running install
  only after a `make down`. The relay is recreated alone, with `--no-deps`; the
  memory service is not restarted. A relay created before this release is
  recreated once, and two sessions starting together are kept apart with
  `flock`.
- `make update` fails when a relay exists and could not be recreated, rather
  than reporting success; an install with no stack is still skipped.
- The plugin keeps the start command's output in
  `~/.local/state/shoulder-daemon/up.log` instead of discarding it, says when
  the start failed, and waits thirty seconds before trying again.

## [0.4.1] - 2026-09-24

### Added

- A `setup-shoulder-daemon` skill ships with the Claude Code plugin. It reads
  `shoulderd doctor` and the env file first so it asks only what is still open,
  then interviews the user over the decision model, the store and how recall
  ranks, and configures all of it from inside the session: it starts the
  mcp-memory-service container and waits for it to answer before writing
  `SHOULDER_MEMORY_URL`, restarts the daemon through the plugin's own script,
  and finishes on a `doctor` run rather than on its own say-so. An API key is
  copied from the environment by name and never printed, and when there is none
  the skill stops and hands the user the line to run rather than writing a
  placeholder that reports as configured and fails on the first turn.

### Fixed

- `make update` and `make up` no longer drop a running mcp-memory-service.
  Compose acts only on the services its profiles select, so either command
  recreated the relay and left it pointing at a store that was no longer there -
  which the daemon reported as healthy while it dropped every search. Both now
  select the memory profile whenever the store's volume exists, so the first
  `up` after a `make down` brings the store back too. `make up`, which is what
  `SHOULDER_START_CMD` runs every time the daemon idles out and a session brings
  it back, passes `--no-recreate`, because podman-compose otherwise recreates a
  healthy relay and makes the store pay its model load on every session start.
  `make down` removed the store either way and now says so explicitly.
- Stopping the daemon no longer cuts writes off half way. It exits when the last
  session ends or it has been idle, and until now it did so while requests,
  the advice for a turn and the tidying pass were still writing: a fact the
  model had already decided could be lost, a tidying pass could leave both
  wordings of a merged fact behind, and a stray temporary file could be left
  beside the store. The daemon now cancels what has not decided yet, lets what
  has decided finish writing, waits for the last tidying pass before going idle,
  and closes the store only after all of it, within a fixed shutdown budget.
  `shoulderd memory migrate` and `shoulderd learn` cut off by a stop now say so
  and exit non-zero instead of reporting a partial copy as complete, and a
  partly read document is never removed by `--replace`.

## [0.4.0] - 2026-09-06

### Added

- The OpenCode adapter ships as the `shoulder-daemon` npm package, so
  `opencode plugin shoulder-daemon` installs it and OpenCode lists it under
  that name. OpenCode labels a plugin with the spec it was installed by, so a
  file copied into the plugin directory appears as its path; the package is
  what makes the name appear instead, and it carries the version the tag built.
  Copying the file still works and runs identical code.

- `SHOULDER_MEMORY=docs` keeps facts as markdown in the repository they are about:
  one bullet per fact under `docs/*.shoulder.md` in the worktree, committed and
  reviewed like any other file, with preferences in a `USER.shoulder.md` the daemon
  keeps out of git. Facts that follow the person go under `SHOULDER_GLOBAL_DOCS`
  (default `~/.local/share/shoulder-daemon/docs`); `SHOULDER_DOCS_DIR` renames the
  subdirectory. Hand edits are honoured, and recall ranks exactly as the JSON store
  does. `SHOULDER_MEMORY_URL` still wins when set; `shoulderd doctor` says so, and
  for the docs store names the global directory and whether the checkout it was
  typed in holds any shoulder files yet.
- `SHOULDER_EMBEDDING=minilm` ranks the built-in store by a transformer
  (all-MiniLM-L6-v2, run in pure Go) instead of the compiled-in word vectors. The
  model is fetched once into `SHOULDER_MODEL_DIR` in the background; until it is
  there, and on any machine without it, the store works exactly as before. Facts
  written before the model arrived are re-embedded behind the store. The default
  stays `glove`: measured on the benchmark in `docs/INSTALL.md`, the transformer's
  scores need a floor of their own before it can be the default.
- `shoulderd memory migrate --local|--global [--from=PATH]` copies a scope of the
  built-in JSON store into whatever backend the daemon is running now, keeping each
  fact's category, tags, timestamp and privacy. The source file is only read;
  working notes are left behind; a fact the running store already holds is skipped,
  so a second run changes nothing. It prints stored, skipped and failed counts,
  exits 1 if anything was refused, and refuses to migrate the JSON store into
  itself.
- `shoulderd learn --local|--global [--replace|--keep-old] [PATH...]` reads the
  documentation a repository already carries into the store: the markdown at the top of
  the worktree and everything under `docs/` or `doc/`, or the paths you name. Each
  document goes to the decision model a section at a time and what comes back is stored
  under the scope you passed, through the same write path a session uses. `README`,
  `CHANGELOG`, `LICENSE`, `CONTRIBUTING`, the agent instruction files, dot directories,
  `node_modules`, `vendor`, `dist`, `build` and the daemon's own `*.shoulder.md` files
  are left alone. It prints per-document counts and exits 1 if anything was missed.
  `--replace` deletes each document once everything it said is in the store, and only
  from a worktree that was clean when the run started. `LEARN_TIMEOUT_SECONDS` (1800)
  bounds one run.
- Every CLI request and every session write carries the directory it came from
  beside the project identity, so a store that keeps facts with the checkout can
  find the checkout. It is informational: nothing stores or compares it.
- Privacy is an axis of its own, beside the scope. A fact can be local to a project
  and still be about this machine, an account, a path or a habit of yours - "Postgres
  listens on 5433 here" - and a backend that files facts beside a checkout keeps those
  out of what the team commits. The decision model is asked the question directly and
  `shoulderd fact add|update --private` answers it by hand; team conventions are not
  private. Privacy only ever travels forward: a correction of a private fact stays
  private even when the model that wrote it said nothing, a merge of several facts
  inherits the strictest of them, and a session working note is never private. Under
  `SHOULDER_MEMORY=docs`, marking a stored fact private moves its line out of the
  committed file and into the `USER.shoulder.md` git does not carry.
- A scenario benchmark behind the `scenario` build tag measures the whole loop rather
  than retrieval alone: a seeded fact, an agent turn whose own prose has to recall it
  and get it injected, a later turn that contradicts it and has to supersede that exact
  record, and the store's final contents - run against every backend the daemon can be
  built with, with the per-backend numbers in `docs/PERFORMANCE.md`.
- A `/readyz` endpoint reports whether the daemon can actually do its job right
  now, where `/healthz` only says something is listening: a relay whose store
  has died keeps answering `/healthz` with an untroubled ok while every recall
  and write behind it fails, and an adapter had no way to tell. It probes the
  store under a two second budget and caches the verdict for ten seconds, so a
  hook asking before every prompt costs at most one store read across however
  many fire inside that window. It answers 200 with `memory: ok` once the store
  answered, and 503 otherwise: `memory: none` when no backend is configured, so
  nothing the session does will be kept, and `memory: unreachable` with the
  store's own error when one is configured and did not answer in time. Both
  adapters now probe it instead of `/healthz` and start a fresh daemon only when
  the store itself is unreachable, leaving a relay with no store configured
  alone rather than restarting it before every prompt for the rest of the
  session; a relay built before the route existed 404s on it and is treated as
  unknown rather than unwell.

### Changed

- A fact in the `preference` category is marked private wherever it is filed; the
  JSON store and a memory service keep the flag and ignore it.
- The decision and learn prompts ask for a rule to be stored as what is allowed or what is
  forbidden, with none of "not", "never", "don't", "must not" or "no longer" in the stored
  sentence: "never commit secrets" becomes "only commit data that is non-secret", "do not
  use var" becomes "use of var is forbidden", "never force push to main" becomes "force
  pushes to main are forbidden". Measured over six pairs of rules written both ways, a
  reversal worded as a denial of the rule collides with the affirmative form in four pairs
  of six under the word table and five of six under the transformer, and with the
  prohibition in none - and a collision is the only way the pipeline is told which record
  to supersede. It costs the word table two questions of eighteen at rank one and two in
  the top three, the transformer one and none, and no unrelated rule about the same
  subject is wrongly refused under either.
- The decision prompt speaks about a stored fact that bears on the operation a turn is
  about to perform whether that fact forbids it, permits it, or says where or how, and
  about a stored preference the assistant's own reply has just broken. It named only
  contradictions and procedures before, and stayed silent on both of those. Measured on
  the loop benchmark, advice reaches the outbox where one is due in 64-79% of the turns
  that call for it under the built-in stores, against 50-57% before, and in 57% against
  mcp-memory-service, against 50%: a permission recalled before a force push is surfaced
  under every backend, where it was surfaced under none.

### Fixed

- The built-in store's re-embedding pass did not start on a store with no file yet,
  so with `SHOULDER_EMBEDDING=minilm` every fact of the first session kept the
  word-vector embedding until the next start.
- Two facts differing only in a word the compiled-in word vectors have never seen
  are no longer read as one fact restated. "the frontend is deployed to Cloudflare"
  and "the backend is deployed to Cloudflare" embedded to the identical vector,
  because none of the three words is in the table, so the second was refused as a
  duplicate and superseded the first: a fact deleted on the default install. The
  words a model could not look up now have to match before its similarity may
  declare a restatement. `SHOULDER_EMBEDDING=minilm` is unaffected — a WordPiece
  tokeniser has no word outside its vocabulary — and recall is unchanged for both.

## [0.3.0] - 2026-09-05

### Added

- `shoulderd monitor` follows the daemon's log and shows only the facts moving: stored,
  superseded, merged and dropped by the tidying pass, refused or failed writes, and advice
  queued and injected, one line each with the text. It opens on the last twenty and waits
  for more; `--all`, `--no-follow` and `--json` change that.
- A fact written with `shoulderd fact add` or `fact update` is logged like one the model
  deduced, with `origin=cli`, so the monitor shows it too.
- The Claude Code plugin links the daemon it fetches into `~/.local/bin`, so `shoulderd`
  is a command in a terminal and not only a path the plugin knows. It says so once if that
  directory is not on `PATH`.
- The README has a section on configuration and tweaking: pickiness level by level,
  monitoring, and the choice of storage backend.

### Changed

- The daemon logs to a file by default, `~/.local/share/shoulder-daemon/shoulderd.log`, as
  well as to stderr. It used to log to stderr alone unless `SHOULDER_LOG` named a file, and
  the adapters start it with stderr closed, so a plugin install kept no log at all.
  `SHOULDER_LOG` still moves the file; `SHOULDER_LOG=stderr` turns it off. A file past 8 MB
  is moved to `.1` at the next start.
- The decision pass speaks in a second case. It used to inject only when a stored fact
  contradicted what the assistant was about to do, so a fact that said how this codebase
  does the thing just asked for stayed in the store while the assistant searched the
  repository for the same answer. Now such a fact is surfaced at the prompt, before the
  search starts. A live test pins it on every configured provider beside the contradiction
  and silence cases.
- A model call that takes more than five seconds is logged as a warning with the session,
  the step and the error if there was one. Stalls on the way to the provider used to leave
  a trace only when they outlived the twenty-second client timeout, and none at all when
  they came in just under it.

## [0.2.0] - 2026-09-04

### Added

- A memory store inside the daemon. It keeps facts in one JSON file -
  `~/.local/share/shoulder-daemon/facts.json`, or wherever `SHOULDER_MEMORY_PATH` points -
  and it is what runs when no `SHOULDER_MEMORY_URL` is set, so an install that starts
  nothing else still remembers. A daemon whose file cannot be opened logs the reason and
  falls back to storing nothing rather than refusing to start.
- Recall by meaning with nothing installed. An embedding table is compiled into the binary
  and a fact is ranked by the rarity-weighted mean of its words, which matches a question
  worded differently from the fact that answers it. The weights are the first 40,000 short
  lower-case words of the public-domain `glove.6B.100d` vectors at one signed byte per
  dimension; `relay/internal/memory/vectors/NOTICE` records the source and the generator
  beside it rebuilds the table.
- The shared secret is generated rather than asked for. On first start the daemon makes a
  token, keeps it in `~/.local/share/shoulder-daemon/token`, and writes it into its env
  file and into `env.SHOULDER_TOKEN` of `~/.claude/settings.json`, leaving the rest of that
  file as it was. Until it sees one correct token it accepts hooks without one, because the
  editor that launched it read its environment before the value existed; after that first
  correct header every request must carry it. Setting `SHOULDER_TOKEN` yourself overrides
  all of it and nothing of yours is written.
- The daemon reads its own env file. Every setting can live in
  `~/.config/shoulder-daemon/env` (or `$SHOULDER_ENV_FILE`), which the CLI and the OpenCode
  adapter already read, so a key set in a login shell is no longer invisible to a daemon an
  editor started from a desktop launcher. The process environment still wins over the file.
- `shoulderd doctor` says whether anything is being remembered: `ok`, `none`, or
  `unreachable` with the store's error. It asks the daemon, which does a real read against
  the backend, because a URL that resolves proves nothing about a backend that refuses every
  request.
- The decision pass sees the whole turn on Claude Code. The Stop hook carries only the last
  assistant message, so the daemon reads the rest from the session transcript the hook
  names, accepted only when it is an absolute path under `.claude/projects` ending in
  `.jsonl`. When the file cannot be read it keeps the hook's message and says so once per
  session, and counters report how often each of those happened.
- `make memory` starts mcp-memory-service from the compose file, which now carries it
  behind a profile: `make up` leaves it alone, and pointing the daemon at it still takes
  `SHOULDER_MEMORY_URL`.
- A `compare` test suite that loads the built-in store and mcp-memory-service with the same
  corpus and prints where each ranked the record that answers each question. It is a
  measurement to run after touching scoring or the embedding table, not an assertion;
  `CONTRIBUTING.md` describes it with the other three suites.

### Changed

- With no memory service named the daemon uses its own store instead of storing nothing.
  The warning that told you to set `SHOULDER_MEMORY_URL` is gone, and the error a refused
  `fact add` returns now says the daemon has no store at all and names both
  `SHOULDER_MEMORY_PATH` and `SHOULDER_MEMORY_URL`.
- The idle timer is on by default at 60 minutes, where it was off. A harness that dies
  without sending `SessionEnd` no longer leaves a daemon on the machine until the next
  reboot; `SHOULDER_IDLE_EXIT_MINUTES=0` restores the old behaviour.
- Model connectors share one HTTP client that pings idle HTTP/2 connections and drops the
  ones that do not answer. A silently dropped connection used to queue every later request
  on a dead stream and burn the full 20-second timeout, one call after another, until the
  process restarted; it now costs one failed call and a fresh dial.
- The compose file mounts Claude Code transcripts read-only at the host path the hook
  payload names, keeps the daemon's facts on a named volume that survives `down`, maps the
  container user to yours under rootless podman, and turns off SELinux confinement for that
  one container: a labelled home directory is refused to a confined container whatever its
  uid, and relabelling a home directory is the worse alternative. A facts volume created
  before the user mapping is owned by the old id and has to be re-owned once;
  `docs/INSTALL.md` has the command.
- README and `docs/INSTALL.md` are rewritten around the install that needs no store, no
  model to pull and no token. The README keeps the two-step plugin install and the model
  choice; the binary by hand, a memory service instead of the built-in store, and every
  setting there is moved to `docs/INSTALL.md`.
- Linting runs the house golangci-lint set - govet, gosec, revive, gocritic, gofumpt and
  goimports - with this repository's exclusions written beside their reasons, and the README
  badge points at `.golangci.yml`. Held back: `fieldalignment`, whose autofix reorders struct
  fields under positional literals, and the three complexity caps, which sixteen functions
  still exceed.
- `replay` and the live hook path share one transcript reader, so a replayed session is
  parsed by the same code as a real one. No flags changed.

### Fixed

- A consult could wedge a session. The advisor claim was released after the "consult over"
  signal rather than before it, so the next event, posted by whoever waits on that signal,
  was skipped as already in flight, and with nothing else coming the session waited forever.
  GitLab's shared runners under the race detector hit it about half the time.
- The daemon's log file is created readable only by its owner, where it was world-readable.
- `advisor-echo` serves with a header read timeout, so a client that sends headers slowly
  can no longer hold a connection open indefinitely.
- Four places where an error was shadowed by an inner assignment, and test fixtures written
  with loose permissions.

### Removed

- The Codecov badge and integration. GitLab computes coverage itself and its badge already
  works, so a third-party service holding a token for a number the pipeline prints anyway
  is gone.
- The Go Report Card badge, replaced by the linter-config badge.

## [0.1.1] - 2026-09-03

### Added

- `make release TAG=vX.Y.Z` creates the three tags a release needs - the release tag and one
  per Go module - and pushes them to every remote. Without the module tags `go install` never
  resolves `@latest` to a release.

### Fixed

- A pipeline test that gave a consult two seconds before calling it stalled, which GitLab's
  shared runners exceeded under the race detector. The v0.1.0 tag pipeline failed on it, so
  that version has a GitHub release only; this one has both.

## [0.1.0] - 2026-09-03

The first tagged release.

### Added

- A local Go daemon that watches a coding session over harness hooks, keeps a store of
  facts, and injects the relevant ones back into the session. The working agent is never
  asked to manage its own memory.
- Adapters for **Claude Code** (marketplace plugin, HTTP hooks, a `SessionStart` script
  that revives the daemon) and **OpenCode** (a plugin that gets the daemon's env file and
  reports session end). Both are tested against a real harness under the `integration`
  build tag.
- Model connectors for Gemini, OpenRouter, z.ai (`glm` and `glm-coding`), OpenCode Go,
  OpenAI, and local Ollama, with `SHOULDER_LLM` taking a comma-separated fallback chain.
- A memory interface with five methods and a conformance suite, so a backend can be
  swapped without touching the pipeline. Running with no memory URL stores nothing.
- Fact supersession and consolidation: a new fact that contradicts a stored one settles
  the collision rather than appending beside it, and the store is tidied instead of only
  growing.
- The `shoulderd` command line - `message`, `fact add/list`, `digest`, `doctor`,
  `config`, `help`. `config` reads and changes log level, pickiness, provider and model on
  a running daemon with no restart.
- Pickiness control over how readily a turn is judged worth acting on.
- `replay`, which re-runs whatever a provider hung off a tool call.
- An injection budget, redaction before anything leaves the machine, and per-project scope
  keyed by the repository's root commit so a rename or a move does not orphan its facts.
- `shoulder_hook_latency_seconds` and the rest of the Prometheus metrics.
- `scripts/install-plugins.sh`, which owns the settings that name a path, and
  `make update`, which rebuilds, reinstalls the adapters and restarts the daemon in the
  order that works.
- Documentation: [architecture](docs/ARCHITECTURE.md), [install](docs/INSTALL.md), and the
  [advisor protocol](docs/ADVISOR.md) for bringing your own decision model.
- `shoulderd version`, and `doctor` reporting the build, where it came from, and whether a
  newer release exists.
- The Claude Code plugin fetches the release binary for its platform on first start,
  checksum-verified, so the plugin is the whole install.
- Release binaries for Linux, macOS and Windows on amd64 and arm64, with `SHA256SUMS`, on
  every tag; multi-arch images at `ghcr.io/quittymr/shoulder-daemon` and
  `registry.gitlab.com/quittymr/shoulder-daemon`.

### Fixed

- A dead launch no longer wedges every start that follows it.
- The daemon waits before believing a session's last goodbye, so a harness that pauses is
  not mistaken for one that quit.
- Advice is delivered while it can still change what the agent does, rather than after the
  turn has committed.
- A fact that mixes a lasting claim with a momentary one is rejected instead of stored.
- The ranked envelope from the by-tag endpoint is decoded correctly, and `searchByTag`
  results are unwrapped properly.
- Session notes are remembered as the store accepted them, not as they were offered.

[Unreleased]: https://github.com/QuittyMR/shoulder-daemon/compare/v0.4.1...HEAD
[0.4.1]: https://github.com/QuittyMR/shoulder-daemon/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/QuittyMR/shoulder-daemon/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/QuittyMR/shoulder-daemon/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/QuittyMR/shoulder-daemon/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/QuittyMR/shoulder-daemon/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/QuittyMR/shoulder-daemon/releases/tag/v0.1.0
