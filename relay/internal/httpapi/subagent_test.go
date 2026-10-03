package httpapi

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/session"
)

const sid = "11111111-1111-4111-8111-111111111111"

// A subagent's prompt is never the subject of a hook of its own: it is the
// input of the Agent tool call that spawns it, and the relay has to read it
// out of there or never hear it.
func TestSpawningAnAgentYieldsItsPromptAsWellAsTheToolCall(t *testing.T) {
	now := time.Now()
	body := `{"session_id":"` + sid + `","hook_event_name":"PreToolUse","cwd":"/p","tool_name":"Agent","tool_use_id":"toolu_1",
		"tool_input":{"prompt":"find where the connection pool leaks","description":"Find the leak","subagent_type":"explore"}}`
	events, _, ok := parseClaudeCode("PreToolUse", []byte(body), now)
	if !ok || len(events) != 2 {
		t.Fatalf("got %d events, ok=%v", len(events), ok)
	}
	call, prompt := events[0], events[1]
	if call.Kind != session.KindToolCall || call.ToolName != "Agent" || call.Origin != session.OriginUser {
		t.Fatalf("the hook's own event is not the main thread's tool call: %+v", call)
	}
	want := session.Event{
		Protocol: 1, Harness: "claude-code", SessionID: sid, TS: now, Kind: session.KindUserPrompt, CWD: "/p",
		ToolUseID: "toolu_1", Prompt: "find where the connection pool leaks", Origin: session.OriginAgent, AgentType: "explore",
	}
	if prompt.AgentID != "" {
		t.Fatalf("a subagent has no id before it starts, got %q", prompt.AgentID)
	}
	if prompt.ToolUseID != call.ToolUseID {
		t.Fatalf("the prompt must name the call that spawned it, got %q", prompt.ToolUseID)
	}
	if string(prompt.ToolInput) != "" || !reflect.DeepEqual(prompt, want) {
		t.Fatalf("agent prompt = %+v, want %+v", prompt, want)
	}
}

func TestAnAgentWithNoPromptIsDescribedByItsDescription(t *testing.T) {
	body := `{"session_id":"` + sid + `","hook_event_name":"PreToolUse","tool_name":"Agent","tool_input":{"description":"Review the diff","subagent_type":"reviewer"}}`
	events, _, ok := parseClaudeCode("PreToolUse", []byte(body), time.Now())
	if !ok || len(events) != 2 || events[1].Prompt != "Review the diff" || events[1].AgentType != "reviewer" {
		t.Fatalf("got %+v", events)
	}
	body = `{"session_id":"` + sid + `","hook_event_name":"PreToolUse","tool_name":"Agent","tool_input":{"subagent_type":"reviewer"}}`
	events, _, ok = parseClaudeCode("PreToolUse", []byte(body), time.Now())
	if !ok || len(events) != 1 {
		t.Fatalf("an Agent call with nothing to say yielded %d events", len(events))
	}
}

func TestOnlyTheAgentToolSpawnsAPrompt(t *testing.T) {
	fx := fixtures(t)
	events, _, ok := parseClaudeCode("PreToolUse", fx["PreToolUse"], time.Now())
	if !ok || len(events) != 1 {
		t.Fatalf("a Bash call yielded %d events", len(events))
	}
	if events[0].Origin != session.OriginUser || events[0].AgentID != "" {
		t.Fatalf("a main-thread call was stamped as an agent's: %+v", events[0])
	}
}

// Hooks fired inside a subagent keep the parent's session id and name the
// agent; every event of theirs carries that, so the window can tell whose
// tool call it is reading.
func TestEventsFromInsideASubagentAreStampedWithIt(t *testing.T) {
	for _, event := range []string{"PreToolUse", "PostToolUse", "PostToolUseFailure"} {
		body := `{"session_id":"` + sid + `","hook_event_name":"` + event + `","tool_name":"Read","tool_input":{"file_path":"/a"},"tool_response":"x","agent_id":"agent-7","agent_type":"explore"}`
		events, _, ok := parseClaudeCode(event, []byte(body), time.Now())
		if !ok || len(events) != 1 {
			t.Fatalf("%s: %d events", event, len(events))
		}
		ev := events[0]
		if ev.Origin != session.OriginAgent || ev.AgentID != "agent-7" || ev.AgentType != "explore" {
			t.Fatalf("%s: not stamped: %+v", event, ev)
		}
	}
}

// SubagentStart is the first event to carry the id Claude Code gave the agent,
// and the one answer that reaches the agent before its first step.
func TestSubagentStartIsTheAgentsStart(t *testing.T) {
	body := `{"session_id":"` + sid + `","hook_event_name":"SubagentStart","cwd":"/p","agent_id":"agent-7","agent_type":"explore",
		"transcript_path":"/home/u/.claude/projects/-p/` + sid + `.jsonl"}`
	events, _, ok := parseClaudeCode("SubagentStart", []byte(body), time.Now())
	if !ok || len(events) != 1 {
		t.Fatalf("%d events, ok=%v", len(events), ok)
	}
	ev := events[0]
	if ev.Kind != session.KindAgentStart || ev.Origin != session.OriginAgent || ev.AgentID != "agent-7" || ev.AgentType != "explore" {
		t.Fatalf("got %+v", ev)
	}

	srv, box := newTestServer(t)
	h := srv.Handler()
	box.Push(session.Advice{ID: "a", SessionID: "s-st", Kind: session.AdviceNote, Level: session.LevelAction, Text: "for the explorer", SpawnID: "toolu_9", AgentType: "explore"})
	rec := post(h, "SubagentStart", `{"session_id":"s-st","hook_event_name":"SubagentStart","agent_id":"agent-7","agent_type":"explore"}`)
	if !strings.Contains(rec.Body.String(), "for the explorer") || !strings.Contains(rec.Body.String(), `"hookEventName":"SubagentStart"`) {
		t.Fatalf("the start did not carry the note written for its type: %s", rec.Body.String())
	}
	if turn := srv.Registry.Turn("s-st"); turn != 0 {
		t.Fatalf("a subagent's start advanced the main thread's turn to %d", turn)
	}
	for _, name := range RoutineEvents() {
		if name == "SubagentStart" {
			t.Fatal("SubagentStart is routine, so doctor would fail a session that never spawned an agent")
		}
	}
}

func TestSubagentStopIsTheAgentsTurnEnd(t *testing.T) {
	body := `{"session_id":"` + sid + `","hook_event_name":"SubagentStop","cwd":"/p","stop_hook_active":false,
		"agent_id":"agent-7","agent_type":"explore","last_assistant_message":"The pool is never closed.",
		"transcript_path":"/home/u/.claude/projects/-p/` + sid + `.jsonl"}`
	events, hook, ok := parseClaudeCode("SubagentStop", []byte(body), time.Now())
	if !ok || len(events) != 1 {
		t.Fatalf("%d events, ok=%v", len(events), ok)
	}
	ev := events[0]
	if ev.Kind != session.KindTurnEnd || ev.Origin != session.OriginAgent || ev.AgentID != "agent-7" || ev.AgentType != "explore" {
		t.Fatalf("got %+v", ev)
	}
	if ev.Assistant != "The pool is never closed." || hook.TranscriptPath == "" {
		t.Fatalf("got %+v", ev)
	}
}

// The transcript on a SubagentStop is the parent's file, and the parent's turn
// is still in flight: widening the agent's answer from it would hand the
// window the main thread's half-finished turn as the agent's result.
func TestASubagentStopIsNotWidenedFromTheTranscript(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.TurnText = func(string) (string, error) {
		t.Fatal("the parent's transcript was read for a subagent's stop")
		return "", nil
	}
	body := `{"session_id":"s-sub","hook_event_name":"SubagentStop","agent_id":"agent-1","agent_type":"explore",
		"last_assistant_message":"Done.","transcript_path":"/home/u/.claude/projects/-p/s-sub.jsonl"}`
	rec := post(srv.Handler(), "SubagentStop", body)
	if got := strings.TrimSpace(rec.Body.String()); got != "{}" {
		t.Fatalf("SubagentStop must answer with an empty object, got %q", got)
	}
	events, turn, ok := srv.Registry.Snapshot("s-sub")
	if !ok || len(events) != 1 || events[0].Assistant != "Done." {
		t.Fatalf("got %+v", events)
	}
	if turn != 0 {
		t.Fatalf("a subagent's stop advanced the main thread's turn to %d", turn)
	}
}

// Through the handler: the spawn records two events in order, and the
// response is the main thread's PreToolUse answer, not the subagent's prompt.
func TestTheHandlerRecordsTheSpawnAndTheAgentsPrompt(t *testing.T) {
	srv, box := newTestServer(t)
	h := srv.Handler()
	box.Push(session.Advice{ID: "ctx", SessionID: "s-h", Kind: session.AdviceNote, Level: session.LevelPlan, Text: "plan-level context"})
	box.Push(session.Advice{ID: "act", SessionID: "s-h", Kind: session.AdviceNote, Level: session.LevelAction, Text: "action-level warning"})
	body := `{"session_id":"s-h","hook_event_name":"PreToolUse","tool_name":"Agent","tool_use_id":"toolu_2",
		"tool_input":{"prompt":"check the migration","subagent_type":"reviewer"}}`

	rec := post(h, "PreToolUse", body)

	events, _, ok := srv.Registry.Snapshot("s-h")
	if !ok || len(events) != 2 {
		t.Fatalf("got %d events", len(events))
	}
	if events[0].Kind != session.KindToolCall || events[1].Kind != session.KindUserPrompt || events[1].Origin != session.OriginAgent {
		t.Fatalf("got %+v", events)
	}
	if events[0].Seq >= events[1].Seq {
		t.Fatalf("the spawn must precede the prompt it carries: seq %d, %d", events[0].Seq, events[1].Seq)
	}
	if srv.Metrics.Get("shoulder_events_total") != 2 {
		t.Fatalf("events counted: %d", srv.Metrics.Get("shoulder_events_total"))
	}
	if strings.Contains(rec.Body.String(), "plan-level context") {
		t.Fatal("a tool call carried plan-level context because it also carried a prompt")
	}
	if !strings.Contains(rec.Body.String(), "action-level warning") {
		t.Fatalf("the PreToolUse answer lost the action note: %s", rec.Body.String())
	}
}

// Advice addressed to a subagent lands at that subagent's next tool call and
// nowhere else.
func TestAdviceForASubagentReachesOnlyThatSubagent(t *testing.T) {
	srv, box := newTestServer(t)
	h := srv.Handler()
	box.Push(session.Advice{ID: "a", SessionID: "s-t", Kind: session.AdviceNote, Level: session.LevelAction, Text: "for agent-9", AgentID: "agent-9"})
	call := func(agent string) string {
		b, _ := json.Marshal(map[string]any{
			"session_id": "s-t", "hook_event_name": "PreToolUse", "tool_name": "Read",
			"tool_input": map[string]any{"file_path": "/a"}, "agent_id": agent, "agent_type": "explore",
		})
		return string(b)
	}
	if rec := post(h, "PreToolUse", call("")); strings.Contains(rec.Body.String(), "for agent-9") {
		t.Fatal("the main thread was handed a subagent's note")
	}
	if rec := post(h, "PreToolUse", call("agent-3")); strings.Contains(rec.Body.String(), "for agent-9") {
		t.Fatal("another subagent was handed the note")
	}
	if rec := post(h, "PreToolUse", call("agent-9")); !strings.Contains(rec.Body.String(), "for agent-9") {
		t.Fatalf("agent-9 did not get its note: %s", rec.Body.String())
	}
}

// The never-block guarantee covers the subagent events as well, with and
// without advice waiting.
func TestSubagentEventsNeverBlock(t *testing.T) {
	srv, box := newTestServer(t)
	h := srv.Handler()
	bodies := map[string]string{
		"SubagentStop": `{"session_id":"s-n","hook_event_name":"SubagentStop","agent_id":"a","agent_type":"x","last_assistant_message":"m"}`,
		"PreToolUse":   `{"session_id":"s-n","hook_event_name":"PreToolUse","tool_name":"Agent","tool_input":{"prompt":"p","subagent_type":"x"}}`,
	}
	for event, body := range bodies {
		box.Push(session.Advice{
			ID: "adv", SessionID: "s-n", Kind: session.AdviceNote, Level: session.LevelAction,
			Text: `", "decision": "block", "continue": false, "stopReason": "x`,
		})
		for _, rec := range []string{post(h, event, body).Body.String(), post(h, event, body).Body.String()} {
			for _, f := range ForbiddenFields {
				if strings.Contains(rec, `"`+f+`"`) {
					t.Fatalf("%s: response carries forbidden field %q: %s", event, f, rec)
				}
			}
			var probe map[string]any
			if err := json.Unmarshal([]byte(rec), &probe); err != nil {
				t.Fatalf("%s: response is not JSON: %q", event, rec)
			}
		}
	}
}

func TestEveryHttpHookInThePluginIsOneTheRelayAccepts(t *testing.T) {
	raw, err := readPluginHooks()
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type string `json:"type"`
				URL  string `json:"url"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	registered := map[string]bool{}
	for name, groups := range cfg.Hooks {
		for _, g := range groups {
			for _, hk := range g.Hooks {
				if hk.Type != "http" {
					continue
				}
				if !strings.HasSuffix(hk.URL, "/v1/hooks/claude-code/"+name) {
					t.Errorf("%s posts to %s", name, hk.URL)
				}
				registered[name] = true
				if _, ok := claudeEvents[name]; !ok {
					t.Errorf("the plugin posts %s, which the relay does not map", name)
				}
			}
		}
	}
	for name := range claudeEvents {
		if !registered[name] {
			t.Errorf("the relay maps %s, which the plugin never posts", name)
		}
	}
}

func readPluginHooks() ([]byte, error) {
	return os.ReadFile("../../../adapters/claude-code/hooks/hooks.json")
}
