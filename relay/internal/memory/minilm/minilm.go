// Package minilm is the transformer embedder for the built-in store. It runs
// sentence-transformers/all-MiniLM-L6-v2 in pure Go through cybertron, which
// keeps the release build a static, cross-compiled binary, at the price of a
// 91 MB download the first time and a couple of hundred megabytes of resident
// memory while the daemon runs.
//
// The model is fetched, converted and loaded in the background. Until it is
// there Embed answers memory.ErrEmbedderNotReady, and the store scores on the
// vectors compiled into the binary, so a daemon on a machine that has never
// seen the network starts exactly as fast and works exactly as well as it did
// before this package existed.
package minilm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	bertconverter "github.com/nlpodyssey/cybertron/pkg/converter/bert"
	"github.com/nlpodyssey/cybertron/pkg/models/bert"
	bertencoder "github.com/nlpodyssey/cybertron/pkg/tasks/textencoding/bert"
	"github.com/rs/zerolog"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory"
)

// Model names the weights and their precision. It is stored beside every
// vector this package produces, and it is not the name of the GloVe table:
// the two spaces have nothing in common, and a vector under the wrong name
// would be compared as though they did.
const Model = "all-minilm-l6-v2-f32-v1"

// Hub is the model's name on huggingface.co, and its path under the model
// directory.
const Hub = "sentence-transformers/all-MiniLM-L6-v2"

// revision pins the files to one commit of the model repository, so what is
// downloaded is what the checksums below were taken from, whatever the branch
// points at later.
const revision = "1110a243fdf4706b3f48f1d95db1a4f5529b4d41"

// files is what the converter needs, with the SHA-256 of each at revision. A
// file that does not match is thrown away: half a download would otherwise be
// kept and mistaken for a whole one on every later start.
var files = map[string]string{ //nolint:gosec // G101: checksums of public files, not credentials
	"config.json":           "953f9c0d463486b10a6871cc2fd59f223b2c70184f49815e7efbcab5d8908b41",
	"tokenizer_config.json": "acb92769e8195aabd29b7b2137a9e6d6e25c476a4f15aa4355c233426c61576b",
	"vocab.txt":             "07eced375cec144d27c900241f3e339478dec958f92fddbc551f295c992038a3",
	"pytorch_model.bin":     "c3a85f238711653950f6a79ece63eb0ea93d76f6a6284be04019c53733baf256",
}

const (
	weights  = "pytorch_model.bin"
	loadable = "spago_model.bin"
	source   = "https://huggingface.co"
	// fetchTimeout bounds the whole download. There is no resume, so a link
	// slower than this is a link that will never finish.
	fetchTimeout = 15 * time.Minute
)

// Embedder is the model, as the store wants it. Construct it with New.
type Embedder struct {
	dir     string
	source  string
	log     *slog.Logger
	enc     atomic.Pointer[bertencoder.TextEncoding]
	settled chan struct{}
	// err says why the model is not ready, once settled is closed.
	err error
}

// New starts fetching and loading the model under dir and returns at once.
// One attempt is made per process: a download that fails is reported once and
// tried again when the daemon next starts, not every few seconds while it
// runs.
func New(dir string, log *slog.Logger) *Embedder {
	return start(dir, source, log)
}

func start(dir, source string, log *slog.Logger) *Embedder {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	// cybertron reports through zerolog's global logger at debug level, to
	// stderr, which nobody reading this daemon's log asked for.
	zerolog.SetGlobalLevel(zerolog.WarnLevel)
	e := &Embedder{dir: dir, source: source, log: log, settled: make(chan struct{})}
	go e.load()
	return e
}

func (e *Embedder) ID() string { return Model }

// Ready reports whether Embed will answer.
func (e *Embedder) Ready() bool { return e.enc.Load() != nil }

// Settled is closed once loading has finished, ready or not.
func (e *Embedder) Settled() <-chan struct{} { return e.settled }

// Err is why the model is not ready, and nil until loading has settled.
func (e *Embedder) Err() error {
	select {
	case <-e.settled:
		return e.err
	default:
		return nil
	}
}

// Dir is where the model lives.
func (e *Embedder) Dir() string { return filepath.Join(e.dir, filepath.FromSlash(Hub)) }

// maxTokens is the sequence length the model was trained and published with,
// which is half the positions it has. Beyond it the output is a number the
// model was never asked to make mean anything.
const maxTokens = 256

// Embed returns the unit vector of text, mean-pooled over the tokens. A text
// longer than the model reads is cut on a token boundary rather than refused,
// because a fact is what its first sentences say and the longest facts are
// the ones written when something went wrong.
//
// This type deliberately does not implement memory.Vocabulary, and the store
// therefore treats every word as one the model saw. That is not a gap: the
// interface asks which words the embedder could not look up, and a WordPiece
// tokeniser looks nothing up as a word. "cloudflare" is not in vocab.txt and
// is not missing from it either — it is spelled "cloud", "##fl", "##are", the
// pieces are embedded, and the sentence vector moves, which is exactly what
// the compiled-in table cannot do and what the guard exists to compensate for
// there. Measured on this model, the two sentences that guard was built for
// score 0.9384 rather than 1.0000. There is no word here to report, so
// answering would mean inventing one.
func (e *Embedder) Embed(ctx context.Context, text string) ([]float32, error) {
	enc := e.enc.Load()
	if enc == nil {
		return nil, memory.ErrEmbedderNotReady
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	resp, err := enc.Encode(ctx, fit(enc, text), int(bert.MeanPooling))
	if err != nil {
		return nil, err
	}
	return unit(resp.Vector.Data().F32()), nil
}

// fit is text cut to what the model reads, counted the way the model counts:
// lower-cased first, because that is what its tokenizer sees, and in runes,
// because that is what the offsets are in. A text that fits is returned as it
// came.
func fit(enc *bertencoder.TextEncoding, text string) string {
	lowered := strings.ToLower(text)
	tokens := enc.Tokenizer.Tokenize(lowered)
	// Two for the class and separator tokens the encoder adds.
	if len(tokens)+2 <= maxTokens {
		return text
	}
	return string([]rune(lowered)[:tokens[maxTokens-2].Offsets.Start])
}

func unit(v []float32) []float32 {
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	if norm == 0 {
		return nil
	}
	norm = math.Sqrt(norm)
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) / norm)
	}
	return out
}

func (e *Embedder) load() {
	defer close(e.settled)
	dir := e.Dir()
	if _, err := os.Stat(filepath.Join(dir, loadable)); err != nil {
		started := time.Now()
		if err := e.fetch(dir); err != nil {
			e.err = err
			e.log.Warn("the embedding model could not be fetched; recall uses the built-in vectors until the next start",
				"model", Hub, "dir", e.dir, "error", err)
			return
		}
		e.log.Info("embedding model fetched and converted", "model", Hub, "dir", dir, "took", time.Since(started).Round(time.Millisecond))
	}
	started := time.Now()
	enc, err := bertencoder.LoadTextEncoding(dir)
	if err != nil {
		e.err = err
		e.log.Warn("the embedding model on disk could not be loaded; delete it to fetch it again",
			"dir", dir, "error", err)
		return
	}
	e.enc.Store(enc)
	e.log.Info("embedding model loaded", "model", Model, "dir", dir, "took", time.Since(started).Round(time.Millisecond))
}

// fetch downloads and converts into a staging directory and moves it into
// place in one rename. The model directory either exists whole or not at all;
// a crash or a lost connection leaves nothing that a later start could mistake
// for a model.
func (e *Embedder) fetch(final string) error {
	if err := os.MkdirAll(e.dir, 0o700); err != nil {
		return err
	}
	// Staging left by a process that died mid-way. The daemon is one per
	// machine, so nothing else is using it.
	if stale, _ := filepath.Glob(filepath.Join(e.dir, ".staging-*")); len(stale) > 0 {
		for _, s := range stale {
			_ = os.RemoveAll(s)
		}
	}
	staging, err := os.MkdirTemp(e.dir, ".staging-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }() // a no-op once the rename below has succeeded

	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	for name, sum := range files {
		if err := download(ctx, e.source, name, filepath.Join(staging, name), sum); err != nil {
			return err
		}
	}
	if err := bertconverter.Convert[float32](staging, false); err != nil {
		return fmt.Errorf("converting %s: %w", Hub, err)
	}
	// The converted file is the model from here on; the original is 91 MB
	// that nothing will read again.
	if err := os.Remove(filepath.Join(staging, weights)); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return err
	}
	if err := os.Rename(staging, final); err != nil {
		if _, serr := os.Stat(filepath.Join(final, loadable)); serr == nil {
			return nil
		}
		return err
	}
	return nil
}

// download fetches one file of the pinned revision and refuses it unless its
// checksum matches.
func download(ctx context.Context, source, name, dest, sum string) (err error) {
	url := source + path.Join("/", Hub, "resolve", revision, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetching %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching %s: %s answered %s", name, url, resp.Status)
	}
	f, err := os.Create(dest) //nolint:gosec // G304: dest is inside the staging directory this package just created
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		return fmt.Errorf("fetching %s: %w", name, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum {
		return fmt.Errorf("%s from %s does not match its checksum: got %s", name, url, got)
	}
	return nil
}
