// Package vectors is the embedding model shoulder-daemon ships with. It exists
// so that recall works the moment the daemon is installed: no service to start,
// no model to pull, no key, no network, and nothing for anybody to configure.
//
// The weights are pre-trained GloVe word vectors, trimmed to the words that
// actually occur in what people tell a coding agent, quantised to one byte per
// dimension and compiled into the binary. A sentence is embedded as the
// rarity-weighted mean of its words, which is a real semantic measure — it puts
// "the deploy target is staging" next to "we ship to the staging cluster",
// which no amount of counting words in common will do — and it costs a few
// microseconds and no allocations beyond the result.
//
// It is not a transformer. Word order is lost, negation is invisible, and two
// sentences made of the same vocabulary look alike whatever they claim. That is
// the trade for something that runs everywhere with nothing installed, and the
// decision model above it is what reads the shortlist this produces.
package vectors

import (
	"context"
	_ "embed"
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"sync"
	"unicode"
)

//go:embed vectors.bin
var blob []byte

// magic identifies the format and its version. A daemon that meets a table it
// does not understand has to say so rather than read numbers out of the middle
// of somebody else's file.
const magic = "SDVEC01\n"

// Model names the weights. It is stored beside every vector this package
// produces, so a later table cannot be compared against an earlier one's
// numbers: the two are not in the same space and the cosine between them is a
// plausible-looking value that means nothing.
const Model = "glove-6b-100d-int8-v1"

// sifA is the smoothing constant of the rarity weighting, at the value the
// paper that introduced it settled on. It decides how much a common word is
// allowed to drag a short sentence towards the centre of the space: every fact
// contains "the", and without this every fact is a little bit like every other.
const sifA = 1e-3

type table struct {
	dims   int
	words  map[string]int32
	scales []float32
	// weights is the rarity weight per word, derived from its rank.
	weights []float32
	// data is the quantised rows, one signed byte per dimension, read out of
	// the embedded blob rather than copied out of it.
	data []byte
	// centre is the direction every sentence in this space points in a little,
	// as a unit vector. See Embed for why it is subtracted back out.
	centre []float32
	err    error
}

var (
	once   sync.Once
	loaded table
)

func load() *table {
	once.Do(func() { loaded = parse(blob) })
	return &loaded
}

func parse(raw []byte) table {
	var t table
	if len(raw) < len(magic)+8 || string(raw[:len(magic)]) != magic {
		t.err = errors.New("vectors: not a shoulder-daemon vector table")
		return t
	}
	at := len(magic)
	dims := int(binary.LittleEndian.Uint32(raw[at:]))
	count := int(binary.LittleEndian.Uint32(raw[at+4:]))
	at += 8
	if dims <= 0 || count <= 0 {
		t.err = errors.New("vectors: empty vector table")
		return t
	}

	t.dims = dims
	t.words = make(map[string]int32, count)
	for i := range count {
		if at+2 > len(raw) {
			t.err = errors.New("vectors: vocabulary is truncated")
			return t
		}
		n := int(binary.LittleEndian.Uint16(raw[at:]))
		at += 2
		if at+n > len(raw) {
			t.err = errors.New("vectors: vocabulary is truncated")
			return t
		}
		t.words[string(raw[at:at+n])] = int32(i)
		at += n
	}

	if at+4*count > len(raw) {
		t.err = errors.New("vectors: scales are truncated")
		return t
	}
	t.scales = make([]float32, count)
	for i := range count {
		t.scales[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[at+4*i:]))
	}
	at += 4 * count

	if at+dims*count > len(raw) {
		t.err = errors.New("vectors: weights are truncated")
		return t
	}
	// Read in place. The rows are the bulk of the binary, and copying them
	// would hold two of everything for the life of the process.
	t.data = raw[at : at+dims*count]

	// The table is written in descending frequency, so a word's position is
	// what is known about how common it is. Deriving the weight here rather
	// than storing it keeps the file to the numbers that cannot be recomputed.
	t.weights = make([]float32, count)
	harmonic := math.Log(float64(count)) + 0.5772156649
	for i := range count {
		p := 1 / (float64(i+1) * harmonic)
		t.weights[i] = float32(sifA / (sifA + p))
	}

	// The common direction of the whole vocabulary, weighted as a sentence
	// would be. Averaging word vectors puts every sentence near this line, so
	// two sentences with nothing to do with each other still score 0.6 against
	// one another; taking it back out is what makes the number mean something.
	centre := make([]float64, dims)
	for i := range count {
		row := t.data[i*dims : (i+1)*dims]
		w := float64(t.weights[i] * t.scales[i])
		for d, v := range row {
			centre[d] += w * float64(int8(v)) //nolint:gosec // G115: the rows are signed bytes; the conversion is the format, not an overflow
		}
	}
	var norm float64
	for _, v := range centre {
		norm += v * v
	}
	if norm > 0 {
		norm = math.Sqrt(norm)
		t.centre = make([]float32, dims)
		for d, v := range centre {
			t.centre[d] = float32(v / norm)
		}
	}
	return t
}

// Embedder is the model, as the store wants it.
type Embedder struct{}

func (Embedder) ID() string { return Model }

// Embed returns the unit vector of a piece of text, or nothing when it holds no
// word the table knows. Nothing is a legitimate answer: an identifier nobody
// has ever written in English is better scored on the characters it shares with
// the query than on a vector assembled out of the two words around it.
func (Embedder) Embed(_ context.Context, text string) ([]float32, error) {
	t := load()
	if t.err != nil {
		return nil, t.err
	}

	sum := make([]float32, t.dims)
	seen := 0
	for _, token := range tokenise(text) {
		i, ok := t.words[token]
		if !ok {
			continue
		}
		seen++
		row := t.data[int(i)*t.dims : (int(i)+1)*t.dims]
		w := t.weights[i] * t.scales[i]
		for d, v := range row {
			sum[d] += w * float32(int8(v)) //nolint:gosec // G115: the rows are signed bytes; the conversion is the format, not an overflow
		}
	}
	if seen == 0 {
		return nil, nil
	}

	// Subtract the common direction. Without it the scores of a search are all
	// crowded into the top of the range — an unrelated fact scores 0.6, a
	// relevant one 0.85 — and a caller cannot set a threshold that separates
	// them. It is the first component of the standard smooth-inverse-frequency
	// recipe, computed over the vocabulary rather than over the store, because
	// the store on a new install holds nothing to compute it from.
	if t.centre != nil {
		var along float32
		for d, v := range sum {
			along += v * t.centre[d]
		}
		for d := range sum {
			sum[d] -= along * t.centre[d]
		}
	}

	var norm float64
	for _, v := range sum {
		norm += float64(v) * float64(v)
	}
	if norm == 0 {
		return nil, nil
	}
	norm = math.Sqrt(norm)
	for i := range sum {
		sum[i] = float32(float64(sum[i]) / norm)
	}
	return sum, nil
}

// Dims is the width of a vector from this table, and Words its vocabulary. They
// are here for the daemon's startup line and for tests; nothing else asks.
func Dims() (int, error) {
	t := load()
	return t.dims, t.err
}

func Words() (int, error) {
	t := load()
	return len(t.words), t.err
}

// tokenise matches the store's own splitting: lower case, anything that is not
// a letter or a digit separates, single characters are dropped. The two have to
// agree or a word counted as rare on one side is missing on the other.
func tokenise(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := fields[:0]
	for _, f := range fields {
		if len(f) > 1 {
			out = append(out, f)
		}
	}
	return out
}
