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
