package llm

import (
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory"
)

// The live tests share one scenario: a turn that contradicts a stored
// constraint, which every provider worth using must turn into an injection.
const liveContradictionWindow = `<user>deploy this to production</user>
<assistant>Running the deploy script against production now.</assistant>`

var liveContradictionRecall = []memory.Record{
	{ID: "mem_1", Category: "constraint", Content: "Deploys never go straight to production; staging first, always."},
}

// The second case the inject rule names: the turn asks for something the store
// already knows how this codebase does, and the assistant has not yet gone to
// find out. Silence here costs the session the search the fact would have saved.
const liveProcedureWindow = `<user>release a new tag for this version</user>`

var liveProcedureRecall = []memory.Record{
	{ID: "mem_2", Category: "structure", Content: "Releases are cut with make release TAG=vX.Y.Z, which creates the release tag and one tag per Go module and pushes them to every remote."},
}

// The two turns the affirmative-form rule is named for. They are bare on
// purpose: a rule stated in one clause and nothing else is the case the
// rewrite has to survive, because there is no other sentence in the window for
// a model to build an affirmative statement out of.
const (
	liveNeverCommitSecretsWindow = `<user>never commit secrets</user>
<assistant>Understood.</assistant>`
	liveDoNotUseVarWindow = `<user>do not use var</user>
<assistant>Understood.</assistant>`
	liveNeverForcePushWindow = `<user>never force push to main</user>
<assistant>Understood.</assistant>`
	// A prohibition the prompt does not show. The three above are the rule's
	// own worked examples, so a model can pass them by copying what it was
	// shown; this one it has to rewrite itself, and it is the only one of the
	// four that measures the rule rather than the examples.
	liveNoMigrationsWindow = `<user>don't run migrations against production</user>
<assistant>Understood.</assistant>`
)
