# Performance: recall, latency and memory

Performance here means three things, and they pull against each other: how often the
fact that answers a question is the first one recalled, how long the recall takes while
the agent's turn is open, and how much the daemon holds resident next to an editor. This
page says what each lever costs and buys, measured, so the choice is yours rather than
the default's.

## What is measured

`relay/internal/memory/compare_test.go` loads each store with the same small corpus of
facts and asks it the same questions: plain recall, families of facts that differ by one
identifier, direction, paths and identifiers, long facts, negation, synonymy. It reports
where each store put the record that answers each question. It runs against the built-in
store alone by default and against a live mcp-memory-service when `SHOULDER_MEMORY_URL`
is set:

```bash
cd relay
go test -tags compare ./internal/memory/ -run TestCompare -v
SHOULDER_EMBEDDING=minilm SHOULDER_MODEL_DIR=/tmp/models \
  go test -tags compare,minilm ./internal/memory/ -run TestCompare -v
```

`TestCompareIdentifierFamily` is the second measurement: eight facts that differ only in a
port number, which is the scenario an embedding fails worst, and every configuration
below writes and answers all eight.

The corpus is small and hand-written. Read the numbers as the shape of the trade-off, not
as a promise about your facts; run it yourself before believing any of them.

Retrieval is half the question. A benchmark query is a question ("which branch should I
rebase onto"); a production query is the turn's own prose ("committing now"), and a fact
that ranks first for the one may never be reached by the other. Worse, ranking says
nothing about whether the loop closes.
`relay/internal/pipeline/scenario_bench_test.go` measures the whole loop instead. Each
scenario seeds a fact through the connector, drives real turns through `Pipeline.Consult`
against the configured decision model, and scores four things per turn: whether the seed
came back from the search the turn's prose ran, whether advice reached the outbox where
one was due, whether the turn that contradicts the seed superseded that record rather
than storing a second fact beside it, and whether the store ends the scenario holding
exactly what it should. The scenarios cover both scopes and every category the decision
prompt knows, plus two that must produce nothing: an unrelated turn that must not be
spoken over, and two turns of ordinary work whose session note must not become knowledge.
Every scenario is a fresh store and a fresh session, and every backend is built the way
`cmd/shoulderd` builds it. Nothing fails the run; the table is the output.

```bash
cd relay
SHOULDER_MODEL_DIR=/tmp/models go test -tags scenario ./internal/pipeline/ -run TestScenarioBenchmark -v
```

It needs `SHOULDER_LLM` and its key - read from the environment or the daemon's env file -
and skips without them. mcp-memory-service is measured as well when `SHOULDER_MEMORY_URL`
is set. Measured against gemini-flash-lite:

| | `glove` | `minilm` | docs + `glove` | mcp-memory-service |
|---|---|---|---|---|
| Seed recalled by the turn's own prose | 81% | 100% | 81% | 90% |
| Advice reached the outbox where one was due | 64-71% | 71-79% | 64% | 57% |
| Contradiction superseded the right record | 100% | 100% | 100% | 100% |
| Store left in the expected final state | 92-100% | 92-100% | 92-100% | 100% |
| Decision pass, mean / worst | 0.8 s / 1.7 s | 0.8 s / 1.7 s | 0.8 s / 1.6 s | 0.9 s / 1.5 s |

One run is one sample per cell, and the injection row is two. Recall and superseding
repeated exactly every time; the injection and final columns moved by one turn between
runs, which is the decision model's own variance and not the store's. The worst decision
pass is under two seconds, with an occasional four- to five-second provider stall that the
daemon logs as a slow model call.

Read them with the failures in mind. Superseding is the strongest link, and recall is the
one the ranker moves: the word table misses every "committing now" turn the transformer
catches, which is the retrieval benchmark's blind spot made visible.

The injection column is the weakest, and it moved when the rule was rewritten rather than
when anything about the store changed. It used to name two cases to speak in - a fact that
contradicts the turn, and a fact that says how this codebase does the thing asked for -
and every miss was a turn it did not cover: a fact that permits the operation, or a
preference the assistant's own reply had just broken. It now names both, and the
permission turn is the one that moved. "force push this to the release branch" over a
stored "force pushing to the release branch is allowed" was silent under every backend
before and is answered under every backend now, in both runs; the commit message written
in the past tense over a stored preference for the imperative is answered wherever the
preference was recalled at all, three backends of four. Two turns did not move. "deploy
this" over "deploys go to us-east-1" is still answered in about half the runs, and
"committing now" over "secrets can be committed into this repository" is still silent
everywhere, including under the ranker that recalls the seed every time - the model does
not read an ordinary commit as the operation that permission is about, which is a
judgement rather than a bug, and the reason it stays visible here.

The final column no longer flatters the wording. It asks that a stored rule carry no
negator anywhere in it, through the same word list `internal/memory` judges polarity by,
so "Secrets must not be committed into this repository." now fails it where an
opening-only check passed it. Under that stricter reading every fact left in every store
in both runs was affirmative, including the one the owner's scenario is named for: asked
to withdraw a permission with "Don't commit secrets here", every backend stored some form
of "Only commit data that is non-secret." The benchmark prints each stored sentence with
its verdict beside it; read those rather than the percentage.

Two properties of mcp-memory-service show up in this loop and in no retrieval benchmark. A
versioned update leaves the sentence it replaced in the server's duplicate index, so a
fact that has been corrected can never be written again verbatim, and a session working
note is refused when any other session has already written the same keywords. Both are
silent skips on the write path.

## The rankers

| | Compiled-in word table (`glove`, default) | Transformer in process (`minilm`) | mcp-memory-service |
|---|---|---|---|
| Right fact first | ~65% | ~95% | ~95% |
| Right fact in the top three | ~85% | ~95% | ~95% |
| Query latency | under 1 ms | ~12 ms | ~17 ms over HTTP |
| Daemon resident memory | ~18 MB | ~227 MB | ~18 MB, plus the container |
| Needs at install | nothing | a 91 MB download, once | a running container |
| Where it fails | a question whose only link to the fact is a word the table has never seen, or two words merely related in meaning | the one synonymy question every store fails | the one synonymy question every store fails |

The word table is 40,000 word vectors compiled into the binary, mean-pooled with rarity
weighting. It works the moment the daemon is installed, on a machine with nothing on it,
and it is what the daemon uses until told otherwise. What it cannot do is read word order
or negation, and it is blind to any word outside its table: to it, "frontend" and
"backend" are the same absence of a word, which is why the store compares such words
literally before it lets the embedding call two facts the same.

`SHOULDER_EMBEDDING=minilm` replaces the table with all-MiniLM-L6-v2 run in pure Go inside
the daemon. It matches the memory service on this benchmark without a container, and
costs an order of magnitude more resident memory plus a background download on first
start. Until the model has arrived the store ranks with the table, and re-embeds what was
written meanwhile once it is there.

The memory service is the choice when the facts must be shared between machines, or when
the daemon's own memory footprint matters more than running one more container.

## Thresholds are per model

Everything the store decides by similarity - the floor under which a search returns
nothing, the similarity at which a write is refused as a restatement, and the lower one
at which a claim's negation is refused so it supersedes the claim - is a number chosen by
a sweep against the model that produces the vectors. The table's numbers are wrong for
the transformer and the reverse, by enough to either delete facts or keep contradictions,
so each embedder carries its own set and the store looks them up by the model that scored
the record. Switching embedders changes how well the store recalls, not what it refuses.

## The other lever: the decision model

Recall is a small part of the time a turn waits. The decision pass runs while the turn is
open and calls the model you configured with `SHOULDER_LLM`; advice that arrives after the
assistant has chosen what to do is worth nothing. A flash-tier model answering in under a
second beats a better one that thinks for twenty, because deciding whether a turn
contradicts a stored fact is classification, not authorship. The
`shoulder_hook_latency_seconds` metric with `event="advisor"` reports what the pass costs
you; the README's connector table lists the defaults.

## Where to go from here

The failures the benchmark still shows are the same under every ranker: a claim whose
negation is worded in other terms, and a question linked to its fact only by synonymy.
Neither is a threshold problem. The first is now attacked at write time: the decision and
learn prompts ask for a rule to be stated as what is allowed or what is forbidden, with no
negator anywhere in the sentence - "never commit secrets" stored as "only commit data that
is non-secret", "do not use var" as "use of var is forbidden", "never force push to main"
as "force pushes to main are forbidden" - and `TestCompareAffirmativeForm` measures what
that buys over six pairs of rules written both ways, under both rankers.

What it buys is contradiction handling, which is what decides whether the store ends up
holding a rule and its denial at once. A reversal worded as a denial of the rule ("use of
var is not forbidden") collides with the prohibition in none of the six pairs, and with
the affirmative form in four of six under the word table and five of six under the
transformer; only a collision tells the pipeline which record to supersede. The pair that
fails under both is the one whose affirmative shares almost no words with its reversal
("only commit data that is non-secret" against "committing secrets is not forbidden"),
which is the same lexical failure as before, not a polarity one. A reversal that shares
few words with the rule ("secrets may be committed") is caught in at most one pair under
either form.

It is not free under the word table. The affirmative form puts the rule first two
questions fewer out of eighteen and in the top three two fewer, against a prohibition that
manages twelve; under the transformer it costs one question at rank one and nothing in the
top three. Distinctness costs nothing either way - an unrelated rule about the same
subject is kept under both forms, in every pair, under both rankers. Wording every rule
this way is therefore a trade the transformer takes for free and the word table pays a
little recall for, and it is made because a store that quietly holds a rule and its denial
is worse than one that ranks the rule third. The second failure needs a model that reads
the question and the fact together, which none of these rankers do.
