package memory

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory/vectors"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
)

// writeBoth stores two facts in that order against the compiled-in table and
// says what happened to the second. The table is the one that ships, so this
// is the behaviour of a default install rather than of a stub.
func writeBoth(t *testing.T, first, second string) error {
	t.Helper()
	ctx := context.Background()
	l, openErr := NewLocal(filepath.Join(t.TempDir(), "facts.json"), vectors.Embedder{})
	if openErr != nil {
		t.Fatalf("open: %v", openErr)
	}
	if _, storeErr := l.Store(ctx, Record{Content: first, Scope: scope.Global}); storeErr != nil {
		t.Fatalf("store %q: %v", first, storeErr)
	}
	_, err := l.Store(ctx, Record{Content: second, Scope: scope.Global})
	return err
}

// TestTwoFactsOneUnseenWordApartAreBothKept is the defect the vocabulary guard
// exists for, against the model that has it.
//
// "frontend", "backend" and "cloudflare" are none of them in the compiled-in
// table, so both sentences reduce to the same three words it does hold and
// embed to the identical vector: the cosine is exactly 1.0000, which is above
// every threshold there is, and the words in common cannot save the pair
// because one content word apart is nearly all of them. Without the guard the
// second write is refused as a restatement of the first, and a refusal is a
// supersede: the caller replaces the first fact with the second and the store
// silently holds one where it was told two.
func TestTwoFactsOneUnseenWordApartAreBothKept(t *testing.T) {
	for _, c := range []struct{ name, first, second string }{
		{"same polarity", "the frontend is deployed to Cloudflare", "the backend is deployed to Cloudflare"},
		{"opposite polarity", "the backend is deployed to Cloudflare", "the frontend is not deployed to Cloudflare"},
		{"another pair of the same shape", "the frontend build runs in CI", "the backend build runs in CI"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := writeBoth(t, c.first, c.second); err != nil {
				t.Fatalf("%q was refused against %q: %v; a refusal is a supersede, so one of the two facts would be deleted",
					c.second, c.first, err)
			}
		})
	}
}

// TestARestatementIsStillRefusedWhenTheUnseenWordsMatch is the control. The
// guard is not "an unknown word anywhere stops the embedding being believed":
// both texts here carry the same unknown word, so the model read the whole of
// the difference between them, and its verdict stands.
func TestARestatementIsStillRefusedWhenTheUnseenWordsMatch(t *testing.T) {
	const claim = "the config file is required"
	if _, ok := unknownWords(vectors.Embedder{}, claim)["config"]; !ok {
		t.Fatalf("%q has no unknown word in it; the case would not test the guard at all", claim)
	}
	err := writeBoth(t, claim, "the config file is not required")
	var dup *ErrDuplicateSemantic
	if !errors.As(err, &dup) {
		t.Fatalf("got %v, want ErrDuplicateSemantic: the denial has to collide or the store keeps the claim and its denial at once", err)
	}
}

// blindEmbedder puts every text at the same angle — so the cosine between any
// two of them is exactly 1 — and cannot see the words it was told about. It
// separates the two halves of the question: the embedding says restatement,
// and the vocabulary says the embedding never read the difference.
type blindEmbedder struct {
	angledEmbedder
	unseen map[string]bool
}

func (b blindEmbedder) Knows(word string) bool { return !b.unseen[word] }

// TestTheGuardIsAPropertyOfTheEmbedder: one pair of writes, one cosine of 1,
// and two opposite verdicts, decided by nothing but whether the embedder can
// say which words it could not see.
func TestTheGuardIsAPropertyOfTheEmbedder(t *testing.T) {
	ctx := context.Background()
	const (
		first  = "the frontend is deployed to acme"
		second = "the backend is deployed to acme"
	)
	// Five tokens of six in common, which is 0.833: over the lexical floor the
	// dense measure needs as a second opinion, and under the threshold that
	// refuses a write on words alone. So the embedding decides, or the guard
	// stops it deciding.
	seen := angledEmbedder{id: "blind-v1", angles: map[string]float64{first: 0, second: 0}}

	write := func(t *testing.T, emb Embedder) error {
		t.Helper()
		l, openErr := NewLocal(filepath.Join(t.TempDir(), "facts.json"), emb)
		if openErr != nil {
			t.Fatalf("open: %v", openErr)
		}
		if _, storeErr := l.Store(ctx, Record{Content: first, Scope: scope.Global}); storeErr != nil {
			t.Fatalf("store: %v", storeErr)
		}
		_, err := l.Store(ctx, Record{Content: second, Scope: scope.Global})
		return err
	}

	t.Run("an embedder that does not say is believed", func(t *testing.T) {
		var dup *ErrDuplicateSemantic
		if err := write(t, seen); !errors.As(err, &dup) {
			t.Fatalf("got %v, want ErrDuplicateSemantic: at a cosine of 1 with nothing said about the vocabulary, the write is a restatement", err)
		}
	})
	t.Run("an embedder that reports the words it could not see is not", func(t *testing.T) {
		blind := blindEmbedder{angledEmbedder: seen, unseen: map[string]bool{"frontend": true, "backend": true}}
		if err := write(t, blind); err != nil {
			t.Fatalf("the second fact was refused: %v; the words the pair differs in are the ones the model could not read", err)
		}
	})
	t.Run("and is believed again when the words it could not see match", func(t *testing.T) {
		blind := blindEmbedder{angledEmbedder: seen, unseen: map[string]bool{"acme": true}}
		var dup *ErrDuplicateSemantic
		if err := write(t, blind); !errors.As(err, &dup) {
			t.Fatalf("got %v, want ErrDuplicateSemantic: the unseen word is the same on both sides, so the model read the difference", err)
		}
	})
}

// TestAWordAndItsPluralAreOneUnknown pins the decision about inflection,
// which is a decision and not an accident. Against the compiled-in table
// ordinary English is present in every form these facts use, so what reaches
// this comparison is jargon — and English negates with do-support, which moves
// the verb: "deduplicates" becomes "deduplicate", and neither is in the table.
// Read literally the pair differs in a word the model could not see and the
// contradiction is never refused, which leaves the store holding a claim and
// its denial at once.
func TestAWordAndItsPluralAreOneUnknown(t *testing.T) {
	emb := vectors.Embedder{}
	t.Run("a verb and its third person", func(t *testing.T) {
		a := unknownWords(emb, "the store deduplicates on write")
		b := unknownWords(emb, "the store does not deduplicate on write")
		if !sameWords(a, b) {
			t.Fatalf("unknown words %v and %v, want one set: the two texts differ in polarity, not in a fact", a, b)
		}
	})
	t.Run("a noun and its plural", func(t *testing.T) {
		a := unknownWords(emb, "the namespace is created by hand")
		b := unknownWords(emb, "the namespaces are created by hand")
		if !sameWords(a, b) {
			t.Fatalf("unknown words %v and %v, want one set", a, b)
		}
	})
	t.Run("but two different words are two unknowns", func(t *testing.T) {
		a := unknownWords(emb, "the frontend is deployed to Cloudflare")
		b := unknownWords(emb, "the backend is deployed to Cloudflare")
		if sameWords(a, b) {
			t.Fatalf("unknown words %v and %v read as one set; a fold this coarse would delete a fact", a, b)
		}
	})
	t.Run("case is not a difference", func(t *testing.T) {
		a := unknownWords(emb, "Cloudflare serves it")
		b := unknownWords(emb, "cloudflare serves it")
		if !sameWords(a, b) {
			t.Fatalf("unknown words %v and %v, want one set: the tokeniser lower cases and the table is lower case", a, b)
		}
	})
	t.Run("a word of two characters is left whole", func(t *testing.T) {
		if got := oneForm("js"); got != "js" {
			t.Fatalf("oneForm(%q) = %q; folding it leaves a token the tokeniser can never produce", "js", got)
		}
	})
}

// TestAnEmbedderThatSaysNothingLeavesTheGuardInert, because a nil vocabulary
// is the shipping state of every embedder that is not the compiled-in table.
func TestAnEmbedderThatSaysNothingLeavesTheGuardInert(t *testing.T) {
	if got := unknownWords(nil, "the frontend is deployed to Cloudflare"); got != nil {
		t.Fatalf("unknownWords(nil, ...) = %v, want nothing", got)
	}
	if !sameWords(unknownWords(nil, "the frontend is deployed to Cloudflare"), unknownWords(nil, "the backend is deployed to Cloudflare")) {
		t.Fatal("two texts read as differing in an unknown word by an embedder that was never asked")
	}
}

// TestFallbackReportsTheVocabularyOfWhicheverModelAnswers, because the
// composite is two models and the words a vector is blind to belong to the one
// that produced it.
func TestFallbackReportsTheVocabularyOfWhicheverModelAnswers(t *testing.T) {
	primary := newLoadingEmbedder("big-model", 8)
	f := NewFallback(primary, blindEmbedder{
		angledEmbedder: angledEmbedder{id: "small-model"},
		unseen:         map[string]bool{"cloudflare": true},
	})

	if f.Knows("cloudflare") {
		t.Fatal("before the model arrives the fallback's vocabulary is the one that answers, and it does not have this word")
	}
	primary.arrive()
	if !f.Knows("cloudflare") {
		t.Fatal("after the model arrives the fallback's word list is not the one that produced the vector; an embedder that cannot say is taken to have seen every word")
	}
}
