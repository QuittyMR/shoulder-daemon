package vectors

import (
	"context"
	"testing"
)

func embed(t *testing.T, text string) []float32 {
	t.Helper()
	v, err := Embedder{}.Embed(context.Background(), text)
	if err != nil {
		t.Fatalf("embed %q: %v", text, err)
	}
	return v
}

func similarity(a, b []float32) float64 {
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot
}

func TestTheTableIsCompiledIn(t *testing.T) {
	words, err := Words()
	if err != nil {
		t.Fatalf("the shipped table did not load: %v", err)
	}
	dims, err := Dims()
	if err != nil {
		t.Fatal(err)
	}
	if words < 10000 || dims != 100 {
		t.Fatalf("%d words at %d dimensions; the table is not the one this was built for", words, dims)
	}
}

// The reason for having a model at all: two sentences that share no word can
// still be about the same thing, and nothing that counts words will say so.
func TestMeaningBeatsWordsInCommon(t *testing.T) {
	cases := []struct{ query, near, far string }{
		{
			query: "where does this get deployed",
			near:  "we ship to the staging cluster",
			far:   "the office cat is called Biscuit",
		},
		{
			query: "what database do the tests need",
			near:  "the integration suite requires a running Postgres",
			far:   "releases are cut on the last Thursday of the month",
		},
		{
			query: "how much detail should the reply have",
			near:  "prefers terse answers with no preamble",
			far:   "the CI runner is self-hosted",
		},
	}
	for _, tc := range cases {
		q := embed(t, tc.query)
		near, far := similarity(q, embed(t, tc.near)), similarity(q, embed(t, tc.far))
		if near <= far {
			t.Errorf("%q: %q scored %.3f and %q scored %.3f", tc.query, tc.near, near, tc.far, far)
		}
	}
}

// Every mean of word vectors points in roughly the same direction, so without
// the common component removed an unrelated fact scores around 0.6 and no
// threshold anywhere can separate the two.
func TestUnrelatedTextIsActuallyUnrelated(t *testing.T) {
	got := similarity(
		embed(t, "the main branch is called master"),
		embed(t, "lunch is at one"),
	)
	if got > 0.5 {
		t.Errorf("unrelated sentences scored %.3f", got)
	}
}

func TestTextWithNoKnownWordsHasNoVector(t *testing.T) {
	if v := embed(t, "zzqq xkcdd"); v != nil {
		t.Errorf("invented words produced a vector of %d dimensions; the store scores those on the characters instead", len(v))
	}
}

func TestAVectorIsAUnitVector(t *testing.T) {
	v := embed(t, "the deploy target is the staging cluster")
	if got := similarity(v, v); got < 0.999 || got > 1.001 {
		t.Errorf("a vector against itself is %.6f, want 1", got)
	}
}

func TestARuinedTableIsAnErrorRatherThanNumbers(t *testing.T) {
	if got := parse([]byte("not a vector table at all")); got.err == nil {
		t.Fatal("a table that is not one must not load")
	}
	broken := make([]byte, len(blob))
	copy(broken, blob)
	// A truncated table read as though it were whole is rows of somebody
	// else's bytes, scored and ranked as if they meant something.
	if got := parse(broken[:len(magic)+16]); got.err == nil {
		t.Fatal("a truncated table must not load")
	}
}

// Three or four words carry too little for a mean of word vectors to place
// them, and a query that short is one the words in common have to answer. This
// is a limit worth pinning: the store reads it as a weak result rather than a
// strong one, which is the behaviour it depends on.
func TestAVeryShortQueryIsNotConfidentlyPlaced(t *testing.T) {
	q := embed(t, "keep it short")
	related := similarity(q, embed(t, "prefers terse answers with no preamble"))
	if related > 0.45 {
		t.Errorf("a three-word query scored %.3f, which the store would treat as a match", related)
	}
}

// Knows is what makes this table answer memory.Vocabulary, and the store's
// guard against deleting a fact is built on its answers being right about this
// table rather than about English.
func TestKnowsReportsWhatIsActuallyInTheTable(t *testing.T) {
	var e Embedder
	// Ordinary English, present in the inflections these facts use, which is
	// why the store is free to fold a trailing s on the words that are not.
	for _, w := range []string{"deploy", "deploys", "deployed", "service", "services", "branch", "branches"} {
		if !e.Knows(w) {
			t.Errorf("Knows(%q) = false, want true", w)
		}
	}
	// The words the defect was measured on. All three are outside the table,
	// which is why two sentences that differ only in them embed identically.
	for _, w := range []string{"frontend", "backend", "cloudflare", "namespace", "deduplicate"} {
		if e.Knows(w) {
			t.Errorf("Knows(%q) = true, want false", w)
		}
	}
	if e.Knows("Cloudflare") != e.Knows("cloudflare") {
		t.Error("Knows disagrees with itself about case; the table is lower case and so is the tokeniser")
	}
}
