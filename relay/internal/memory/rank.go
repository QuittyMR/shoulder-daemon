package memory

import (
	"math"
	"sort"
)

// ranker is the scoring every in-process store shares: one set of records, the
// vectors known for them, and the rarity of each word across the set. It is a
// value built for one read and thrown away, because the rarity table depends on
// the set and the set is whatever the caller is holding a lock over.
type ranker struct {
	recs []Record
	vecs map[string]vector
	idf  map[string]float64
	// vocab is what the embedder that scored these vectors could see, when it
	// is able to say. It is nil for an embedder that is not asked or does not
	// answer, and a nil vocabulary makes the guard that uses it inert.
	vocab Vocabulary
}

func newRanker(recs []Record, vecs map[string]vector, emb Embedder) ranker {
	vocab, _ := emb.(Vocabulary)
	return ranker{recs: recs, vecs: vecs, idf: idf(recs), vocab: vocab}
}

// search ranks the records matching q and returns only what it actually
// matched. A record it could not score at all is not a weak answer to the
// question, it is not an answer, so it is left out rather than returned with a
// zero that a caller's floor is then forbidden to drop.
//
// The floor the caller did not name comes from the model that scored the
// record, because the score is that model's number: a table whose cosines run
// low answers nothing at a floor measured against another one, and a store
// that has just swapped models would go quiet without a single record having
// changed. It is per record and not per search because the two are not the
// same set while a model is landing — the query has the new model's vector and
// the records have not been brought up to it yet — and a record scored on
// words alone was not scored by that model at all. Judging it at the model's
// floor would let a weak word match through on the strength of a measurement
// that never touched it.
func (k ranker) search(q Query, qvec *vector) []Record {
	limit := q.Limit
	if limit <= 0 {
		limit = 10
	}
	model := ""
	if qvec != nil {
		model = qvec.Model
	}
	embedded, words := minScoreFor(model), defaultMinScore
	qtokens := weigh(terms(q.Text), k.idf)

	scored := make([]Record, 0, len(k.recs))
	for _, r := range k.recs {
		if !matches(r, q) {
			continue
		}
		score, byModel := k.score(r, qvec, qtokens)
		floor := q.MinScore
		if floor <= 0 {
			floor = words
			if byModel {
				floor = embedded
			}
		}
		if score <= 0 || score < floor {
			continue
		}
		out := readable(r, q)
		out.Score = score
		scored = append(scored, out)
	}
	sort.SliceStable(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })
	if len(scored) > limit {
		scored = scored[:limit]
	}
	return scored
}

// score prefers the embedding when both sides have one from the same model,
// and falls back to words in common otherwise, saying which it did. The
// fallback is not a degraded mode to apologise for: it is the only scoring a
// daemon with no embedding model has, and it is what makes the store work out
// of the box.
func (k ranker) score(r Record, qvec *vector, qtokens map[string]float64) (score float64, embedded bool) {
	lexical := phrased(qtokens, weigh(terms(r.Content), k.idf))
	if qvec != nil {
		if v, ok := k.vecs[r.ID]; ok && v.Model == qvec.Model {
			// Both, weighted towards meaning. The embedding answers the
			// question the words cannot — a fact worded differently — and the
			// words answer the one the embedding cannot: which of eight
			// sentences about services and ports is the one about this
			// service. Measured over a corpus with both kinds in it, the pair
			// beats either alone.
			return denseWeight*dense(qvec.Values, v.Values) + (1-denseWeight)*lexical, true
		}
	}
	return lexical, false
}

// similar reports the record a write would be a restatement of, if there is
// one. Only the scope and kind the write is landing in are considered, and
// both measures get a say: the embedding catches the same claim in different
// words, and words in common catch a sentence about identifiers and paths that
// no embedding table has ever seen.
func (k ranker) similar(r Record, vec *vector) string {
	tokens := weigh(tokenise(r.Content), k.idf)
	model := ""
	if vec != nil {
		model = vec.Model
	}
	limit := restatementFor(model)
	// Read once rather than per record: the write is one text and the answer
	// does not depend on what it is being compared against.
	unknown := unknownWords(k.vocab, r.Content)
	contradicted := ""
	for _, existing := range k.recs {
		if !samePlace(existing, r) || existing.Kind != r.Kind {
			continue
		}
		// A sentence that differs only in a number is not a restatement of it.
		// Ports, versions, namespaces, table numbers: the model cannot see any
		// of them — a numeric token is not in its vocabulary and contributes
		// nothing to the vector — so a family of facts about eight services
		// reads as one fact repeated, and the second write is refused as a
		// restatement of the first. The caller's answer to a refusal is to
		// supersede what it collided with, so the family collapses to whichever
		// member was written last and every question about the others is
		// answered confidently and wrongly. Measured, not imagined: eight facts
		// in, five kept.
		if !sameNumbers(r.Content, existing.Content) {
			continue
		}
		lexical := sparse(tokens, weigh(tokenise(existing.Content), k.idf))
		// Whether the two agree on polarity decides two things, and the first
		// of them is how close they have to be to count as saying the same
		// thing at all. A write that denies what the record claims is not
		// worded like it — the negator is a word the claim does not have, and
		// the model puts the pair further apart than it puts the same claim
		// twice — so judging a contradiction at the threshold measured on
		// restatements refuses hardly any of them, and a contradiction that is
		// not refused is never superseded: the store keeps the fact and its
		// denial and answers with whichever ranks higher.
		agrees := samePolarity(r.Content, existing.Content)
		restated := lexical >= sparseRestatement
		if vec != nil && !restated {
			if v, ok := k.vecs[existing.ID]; ok && v.Model == vec.Model {
				// Both measures, because at this similarity a restatement is
				// close in words as well: an embedding alone at the threshold
				// is sometimes two different facts wearing the same sentence.
				// Which pair of numbers that is comes from the model that
				// produced both vectors, for the same reason the search floor
				// does — a threshold measured against one space says nothing
				// about another — and it is safe to read it off the query's
				// model here because the record's is checked to be the same
				// one on the line above.
				//
				// And a third opinion, from the model itself, about whether
				// it read the pair at all. A word outside its vocabulary
				// contributes nothing to either vector, so two facts that
				// differ only in such a word are handed to the cosine as the
				// same sentence: "the frontend is deployed to Cloudflare"
				// against "the backend is deployed to Cloudflare" scores
				// exactly 1.0000 against the shipping table, and the lexical
				// floor cannot save it, because one content word apart is
				// nearly all the words in common. The words the model could
				// not see have to match before its number is allowed to
				// decide anything, for the same reason the numbers do.
				restated = sameWords(unknown, unknownWords(k.vocab, existing.Content)) &&
					dense(vec.Values, v.Values) >= limit.denseFor(agrees) && lexical >= limit.lexicalFloor
			}
		}
		if !restated {
			continue
		}
		// The second thing polarity decides is which collision the caller
		// is told about. A restatement of the opposite polarity is a
		// contradiction, and colliding with it is what lets the write
		// supersede the fact it contradicts. It is the collision of last
		// resort, though: when the store also holds the claim with this
		// write's own polarity, that is the record being restated, and naming
		// the contradiction instead would have the caller overwrite the fact
		// that disagrees and keep two records that agree.
		if agrees {
			return existing.ID
		}
		if contradicted == "" {
			contradicted = existing.ID
		}
	}
	return contradicted
}

// idf weighs a word by how rare it is in this set, so that a term every record
// shares — the name of the project, the vocabulary of the work — stops
// counting as evidence of anything.
func idf(recs []Record) map[string]float64 {
	df := map[string]int{}
	for _, r := range recs {
		for token := range set(terms(r.Content)) {
			df[token]++
		}
	}
	n := float64(len(recs))
	idf := make(map[string]float64, len(df))
	for token, count := range df {
		// 1 + n/df rather than n/df: with the latter a term present in every
		// record weighs nothing, and in a store holding one record that is
		// every term, so the only fact there is could never be recalled.
		idf[token] = math.Log(1 + n/float64(count))
	}
	return idf
}
