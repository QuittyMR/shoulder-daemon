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
