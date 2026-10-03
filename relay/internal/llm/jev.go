package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/config"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/textutil"
)

// Action is what a turn calls for with respect to what is already stored.
type Action string

const (
	Nothing Action = "nothing"
	Create  Action = "create"
	Update  Action = "update"
	Inject  Action = "inject"
)

// Verdict is Jev's answer about one turn. FactID is set for Update and Inject
// only, and is always one of the recalled records' ids.
type Verdict struct {
	Action     Action
	Confidence float64
	FactID     string
}

const (
	DefaultJevBaseURL       = "https://api.typesafe.ai"
	DefaultJevModel         = "jev-latest"
	DefaultJevMinConfidence = 0.6

	// maxJevStateBytes keeps the encoded state near half of Jev's 32k-token
	// window. JSON escaping and a tokenizer that does worse than four bytes a
	// token on code both eat into it, and the questions travel in the same
	// window.
	maxJevStateBytes = 48_000

	// maxJevFactBytes bounds one stored fact, so that a single long record
	// cannot push the turn itself out of the state.
	maxJevFactBytes = 2_000

	// maxJevCriterionBytes bounds a fact quoted as a target option. The option
	// only has to tell the facts apart; the full text is in the state.
	maxJevCriterionBytes = 200

	clippedMark = "…"
)

var actionCriteria = map[Action]string{
	Nothing: "The turn neither establishes anything durable, nor contradicts a stored fact, nor makes one urgently relevant. This is the usual answer.",
	Create:  "The turn establishes something durable that none of the stored facts records: a rule the user stated about how work is done here, a preference of theirs, a fact about the project or the machine that will still be true next week, or a finding the session made by looking.",
	Update:  "The turn contradicts, corrects or refines one of the stored facts, so that stored fact is now wrong or incomplete and should be replaced.",
	Inject:  "One of the stored facts bears directly on what the session is doing right now, and the session appears unaware of it or about to act against it, so it should be reminded of that fact.",
}

// Jev is TypeSafe's System One: it answers typed questions with calibrated
// probabilities and never writes text. That makes it a triage in front of the
// decision model rather than a replacement for it: it can say a turn needs
// nothing, or which stored fact to repeat, but a new fact has to be written by
// something that writes.
type Jev struct {
	BaseURL string
	APIKey  string
	Model   string
	// Threshold is the confidence below which a verdict is not acted on.
	Threshold float64
	HTTP      *http.Client
}

func (j *Jev) Name() string           { return "jev" }
func (j *Jev) ModelID() string        { return j.Model }
func (j *Jev) MinConfidence() float64 { return j.Threshold }

// JevFromEnv builds the triage SHOULDER_TRIAGE asks for, and nil when it asks
// for none.
func JevFromEnv() (*Jev, error) {
	switch v := strings.ToLower(strings.TrimSpace(config.Setting("SHOULDER_TRIAGE"))); v {
	case "":
		return nil, nil
	case "jev":
	default:
		return nil, fmt.Errorf("unknown triage %q; the only one is jev, or leave SHOULDER_TRIAGE empty for none", v)
	}
	key := strings.TrimSpace(config.Setting("TYPESAFE_API_KEY"))
	if key == "" {
		return nil, errors.New("triage jev needs TYPESAFE_API_KEY")
	}
	threshold := DefaultJevMinConfidence
	if v := strings.TrimSpace(config.Setting("SHOULDER_JEV_MIN_CONFIDENCE")); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(f) || f < 0 || f > 1 {
			return nil, fmt.Errorf("SHOULDER_JEV_MIN_CONFIDENCE is %q; want a number from 0 to 1", v)
		}
		threshold = f
	}
	return &Jev{
		BaseURL:   config.Env("TYPESAFE_BASE_URL", DefaultJevBaseURL),
		APIKey:    key,
		Model:     config.Env("SHOULDER_JEV_MODEL", DefaultJevModel),
		Threshold: threshold,
	}, nil
}

type jevFact struct {
	ID       string `json:"id"`
	Scope    string `json:"scope"`
	Category string `json:"category"`
	Content  string `json:"content"`
}

type jevState struct {
	Turn        string    `json:"turn"`
	StoredFacts []jevFact `json:"stored_facts"`
}

type jevQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}

type jevRequest struct {
	State     jevState               `json:"state"`
	Model     string                 `json:"model"`
	Questions map[string]jevQuestion `json:"questions"`
}

type jevAnswer struct {
	Type       string   `json:"type"`
	Choice     string   `json:"choice"`
	Confidence *float64 `json:"confidence"`
}

type jevResponse struct {
	Answers map[string]jevAnswer `json:"answers"`
}

// Triage asks, in one call, what the turn calls for and which stored fact it
// is about.
func (j *Jev) Triage(ctx context.Context, turnWindow string, recalled []memory.Record) (Verdict, error) {
	// Only a record with an id can be named back, and an id offered twice
	// would be two options Jev cannot tell apart.
	var targets []memory.Record
	seen := map[string]bool{}
	for _, r := range recalled {
		if r.ID == "" || seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		targets = append(targets, r)
	}

	actions := map[string]string{
		string(Nothing): actionCriteria[Nothing],
		string(Create):  actionCriteria[Create],
	}
	if len(targets) > 0 {
		actions[string(Update)] = actionCriteria[Update]
		actions[string(Inject)] = actionCriteria[Inject]
	}
	questions := map[string]jevQuestion{"action": {
		Type:         "choice",
		Instructions: "A memory assistant is watching a coding session. `turn` is the latest exchange; `stored_facts` are facts it already remembers that matched the turn. Decide what the assistant should do after this turn.",
		Criteria:     actions,
	}}
	if len(targets) > 1 {
		options := make(map[string]string, len(targets))
		for _, r := range targets {
			options[r.ID] = clipBytes(r.Content, maxJevCriterionBytes)
		}
		questions["target"] = jevQuestion{
			Type:         "choice",
			Instructions: "Which stored fact, by id, the turn is about: the one it contradicts or corrects, or the one the session most needs reminding of.",
			Criteria:     options,
		}
	}

	state, err := fitState(turnWindow, recalled)
	if err != nil {
		return Verdict{}, err
	}
	body, err := encodeJSON(jevRequest{State: state, Model: j.Model, Questions: questions})
	if err != nil {
		return Verdict{}, err
	}
	out, err := j.post(ctx, body)
	if err != nil {
		return Verdict{}, err
	}

	action, err := answerTo(out, "action", questions["action"].Criteria)
	if err != nil {
		return Verdict{}, err
	}
	v := Verdict{Action: Action(action.Choice), Confidence: *action.Confidence}
	if v.Action != Update && v.Action != Inject {
		return v, nil
	}
	if len(targets) == 1 {
		v.FactID = targets[0].ID
		return v, nil
	}
	target, err := answerTo(out, "target", questions["target"].Criteria)
	if err != nil {
		return Verdict{}, err
	}
	// Acting on the right action against the wrong fact repeats or replaces
	// something the turn was not about, so the verdict is only as sure as the
	// less sure of its two halves.
	v.FactID = target.Choice
	v.Confidence = min(v.Confidence, *target.Confidence)
	return v, nil
}

// answerTo is the answer to one question, refused unless it is one of the options
// that question offered and carries a usable confidence.
func answerTo(out jevResponse, name string, options map[string]string) (jevAnswer, error) {
	a, ok := out.Answers[name]
	if !ok {
		return jevAnswer{}, fmt.Errorf("jev did not answer %q", name)
	}
	if a.Type != "choice" {
		return jevAnswer{}, fmt.Errorf("jev answered %q with type %q, want choice", name, a.Type)
	}
	if _, ok := options[a.Choice]; !ok {
		return jevAnswer{}, fmt.Errorf("jev answered %q with %q, which was not offered", name, a.Choice)
	}
	if a.Confidence == nil || math.IsNaN(*a.Confidence) || *a.Confidence < 0 || *a.Confidence > 1 {
		return jevAnswer{}, fmt.Errorf("jev answered %q with no confidence from 0 to 1", name)
	}
	return a, nil
}

// fitState is the turn and the stored facts, encoding to no more than
// maxJevStateBytes. What gives way is the oldest part of the turn: the end of
// it is what was just said, and the facts are already bounded one by one.
func fitState(turnWindow string, recalled []memory.Record) (jevState, error) {
	s := jevState{Turn: turnWindow, StoredFacts: make([]jevFact, 0, len(recalled))}
	for _, r := range recalled {
		s.StoredFacts = append(s.StoredFacts, jevFact{
			ID: r.ID, Scope: string(r.Scope), Category: r.Category,
			Content: clipBytes(r.Content, maxJevFactBytes),
		})
	}
	keep := len(turnWindow)
	for {
		b, err := encodeJSON(s)
		if err != nil {
			return jevState{}, err
		}
		over := len(b) - maxJevStateBytes
		if over <= 0 {
			return s, nil
		}
		if keep == 0 {
			return jevState{}, fmt.Errorf("the stored facts alone exceed the %d bytes jev is sent", maxJevStateBytes)
		}
		// Escaping makes the encoded turn longer than the raw one by a ratio
		// that depends on what is in it, so the raw tail shrinks in proportion
		// rather than by the excess.
		enc, err := encodeJSON(s.Turn)
		if err != nil {
			return jevState{}, err
		}
		keep = max(0, keep*(len(enc)-over)/len(enc)-len(clippedMark))
		s.Turn = clippedMark + lastBytes(turnWindow, keep)
	}
}

// encodeJSON leaves angle brackets and ampersands as they are. Turns are full
// of code, and json.Marshal would spend six bytes of Jev's window on each.
func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// clipBytes is the first n bytes of s, less any partial character at the cut,
// marked as cut. textutil.Clip splits a character, which the encoder then
// sends as a replacement character in front of Jev.
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := n
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i] + clippedMark
}

// lastBytes is the last n bytes of s, less any partial character at the cut.
func lastBytes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}

func (j *Jev) post(ctx context.Context, body []byte) (jevResponse, error) {
	var out jevResponse
	url := strings.TrimRight(j.BaseURL, "/") + "/v1/systemone"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+j.APIKey)

	client := j.HTTP
	if client == nil {
		client = defaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return out, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, fmt.Errorf("jev status %d: %s", resp.StatusCode, textutil.Clip(string(raw), 200))
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("jev returned unparseable JSON: %w", err)
	}
	return out, nil
}
