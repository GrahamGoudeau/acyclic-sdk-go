package acyclic

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeModel replays scripted replies and records every request.
type fakeModel struct {
	baa     bool
	replies []ChatResponse
	reqs    []ChatRequest
	err     error
}

func (m *fakeModel) Provider() string   { return "openai" }
func (m *fakeModel) Name() string       { return "fake-1" }
func (m *fakeModel) CoveredByBAA() bool { return m.baa }
func (m *fakeModel) Chat(_ context.Context, r ChatRequest) (*ChatResponse, error) {
	m.reqs = append(m.reqs, r)
	if m.err != nil {
		return nil, m.err
	}
	if len(m.reqs) > len(m.replies) {
		return &ChatResponse{Content: "loop", ToolCalls: []ToolCall{{ID: "again", Name: "get_patient", Args: map[string]any{}}}, Usage: Usage{10, 1}, FinishReason: "tool_calls"}, nil
	}
	rep := m.replies[len(m.reqs)-1]
	return &rep, nil
}

func TestAgentToolCallsAndDone(t *testing.T) {
	f := newFakeFHIR(t)
	var buf bytes.Buffer
	ehr := connectFake(t, f, "generic", JSONTelemetry(&buf))
	sess := NewSession(ehr, SessionOptions{Patient: "p1"})
	model := &fakeModel{replies: []ChatResponse{
		{ToolCalls: []ToolCall{{ID: "c1", Name: "get_patient", Args: map[string]any{}}, {ID: "c2", Name: "get_labs_vitals", Args: map[string]any{"limit": 2}}}, Usage: Usage{100, 20}, FinishReason: "tool_calls"},
		{Content: "HbA1c 7.5 % on 2026-06-01.", Usage: Usage{200, 10}, FinishReason: "stop"},
	}}
	store := MemoryStore()
	var events []string
	out, err := RunAgent(context.Background(), AgentOptions{
		EHR: ehr, Session: sess, Model: model, DataPolicy: Synthetic, Prompt: "Latest A1c?", Store: store, ConversationID: "conv-1",
		OnEvent: func(e Event) { events = append(events, e.Type) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop != StopDone || out.Text != "HbA1c 7.5 % on 2026-06-01." || out.Steps != 2 || out.ToolCalls != 2 || out.Usage != (Usage{300, 30}) || out.ConversationID != "conv-1" {
		t.Fatalf("%+v", out)
	}
	roles := []string{}
	for _, m := range out.Messages {
		roles = append(roles, m.Role)
	}
	if strings.Join(roles, ",") != "system,user,assistant,tool,tool,assistant" {
		t.Fatal(roles)
	}
	if out.Messages[0].Content != DefaultSystem {
		t.Fatal("system prompt")
	}
	if !strings.Contains(out.Messages[3].Content, `"name":"Samuel Okafor"`) || out.Messages[3].ToolCallID != "c1" {
		t.Fatal(out.Messages[3])
	}
	if !strings.Contains(out.Messages[4].Content, `"truncated":true`) {
		t.Fatal(out.Messages[4].Content)
	}
	if got := strings.Join(events, ","); got != "model_request,model_response,tool_call,tool_result,tool_call,tool_result,model_request,model_response,done" {
		t.Fatal(got)
	}
	if model.reqs[0].MaxTokens != 4096 || len(model.reqs[0].Tools) == 0 {
		t.Fatal("request shape")
	}
	saved, _ := store.Load(context.Background(), "conv-1")
	if len(saved) != len(out.Messages) {
		t.Fatalf("store has %d messages", len(saved))
	}
	tel := buf.String()
	for _, bad := range []string{"Okafor", "1948", "Latest A1c", "HbA1c 7.5", `"p1"`} {
		if strings.Contains(tel, bad) {
			t.Errorf("telemetry leaked %q", bad)
		}
	}
	for _, want := range []string{`"name":"invoke_agent acyclic"`, `"name":"chat fake-1"`, `"name":"execute_tool get_labs_vitals"`, `"gen_ai.tool.call.id":"c2"`, `"agent_run":"conv-1"`, `"kind":"audit"`, `"name":"GET Observation"`} {
		if !strings.Contains(tel, want) {
			t.Errorf("telemetry missing %s", want)
		}
	}

	// Resume from the store.
	model2 := &fakeModel{replies: []ChatResponse{{Content: "Still 7.5.", FinishReason: "stop"}}}
	out2, err := RunAgent(context.Background(), AgentOptions{EHR: ehr, Session: sess, Model: model2, DataPolicy: Synthetic, Prompt: "And now?", Store: store, ConversationID: "conv-1"})
	if err != nil || len(model2.reqs[0].Messages) != len(out.Messages)+1 || out2.Text != "Still 7.5." {
		t.Fatalf("resume: %v %d", err, len(model2.reqs[0].Messages))
	}
}

func TestAgentBudgets(t *testing.T) {
	f := newFakeFHIR(t)
	ehr := connectFake(t, f, "generic", nil)
	ctx := context.Background()

	// max_steps: the fake model never stops calling tools.
	out, err := RunAgent(ctx, AgentOptions{EHR: ehr, Session: NewSession(ehr, SessionOptions{Patient: "p1"}), Model: &fakeModel{}, DataPolicy: Synthetic, Prompt: "x", MaxSteps: 3})
	if err != nil || out.Stop != StopMaxSteps || out.Steps != 3 {
		t.Fatalf("max_steps: %v %+v", err, out)
	}
	// max_tokens: 11 tokens per step, budget 25 -> three steps.
	out, _ = RunAgent(ctx, AgentOptions{EHR: ehr, Session: NewSession(ehr, SessionOptions{Patient: "p1"}), Model: &fakeModel{}, DataPolicy: Synthetic, Prompt: "x", MaxTokens: 25})
	if out.Stop != StopMaxTokens || out.Steps != 3 {
		t.Fatalf("max_tokens: %+v", out)
	}
	// max_tool_calls: three calls in one reply, budget two.
	three := []ToolCall{{ID: "a", Name: "get_patient"}, {ID: "b", Name: "get_patient"}, {ID: "c", Name: "get_patient"}}
	out, _ = RunAgent(ctx, AgentOptions{EHR: ehr, Session: NewSession(ehr, SessionOptions{Patient: "p1"}), Model: &fakeModel{replies: []ChatResponse{{ToolCalls: three}}}, DataPolicy: Synthetic, Prompt: "x", MaxToolCalls: 2})
	if out.Stop != StopMaxToolCalls || out.ToolCalls != 2 {
		t.Fatalf("max_tool_calls: %+v", out)
	}
	last := out.Messages[len(out.Messages)-1]
	if last.Role != "tool" || last.Content != `{"error":"tool call budget exhausted for this run","code":"budget"}` {
		t.Fatalf("budget message: %+v", last)
	}
	// output_truncated: no tool calls but the reply hit the output cap.
	for _, reason := range []string{"length", "max_tokens"} {
		out, _ = RunAgent(ctx, AgentOptions{EHR: ehr, Session: NewSession(ehr, SessionOptions{Patient: "p1"}), Model: &fakeModel{replies: []ChatResponse{{Content: "The patient has", FinishReason: reason}}}, DataPolicy: Synthetic, Prompt: "x"})
		if out.Stop != StopOutputTruncated || out.Text != "The patient has" {
			t.Fatalf("output_truncated (%s): %+v", reason, out)
		}
	}
	// a tool error goes back to the model, not out of RunAgent
	out, err = RunAgent(ctx, AgentOptions{EHR: ehr, Session: NewSession(ehr, SessionOptions{}), Model: &fakeModel{replies: []ChatResponse{{ToolCalls: []ToolCall{{ID: "a", Name: "get_patient"}}}, {Content: "no patient"}}}, DataPolicy: Synthetic, Prompt: "x"})
	if err != nil || !strings.Contains(out.Messages[3].Content, `"code":"no_patient"`) {
		t.Fatalf("%v %+v", err, out.Messages)
	}
	// model errors propagate
	_, err = RunAgent(ctx, AgentOptions{EHR: ehr, Session: NewSession(ehr, SessionOptions{Patient: "p1"}), Model: &fakeModel{err: &ModelError{Status: 429}}, DataPolicy: Synthetic, Prompt: "x"})
	var me *ModelError
	if !errors.As(err, &me) {
		t.Fatalf("%v", err)
	}
}

func TestAgentDataPolicy(t *testing.T) {
	f := newFakeFHIR(t)
	local := connectFake(t, f, "generic", nil)
	ctx := context.Background()
	run := func(ehr *EHR, m Model, p DataPolicy) error {
		_, err := RunAgent(ctx, AgentOptions{EHR: ehr, Session: NewSession(ehr, SessionOptions{Patient: "p1"}), Model: m, DataPolicy: p, Prompt: "x"})
		return err
	}
	if err := run(local, &fakeModel{}, ""); err == nil || err.Error() != `set dataPolicy: "baa" (real patient data, BAA-covered model) or "synthetic" (sandbox only)` {
		t.Fatalf("missing policy: %v", err)
	}
	if err := run(local, &fakeModel{}, BAA); err == nil || err.Error() != `dataPolicy "baa" needs a model created with coveredByBAA: true` {
		t.Fatalf("baa: %v", err)
	}
	if err := run(local, &fakeModel{baa: true, replies: []ChatResponse{{Content: "ok"}}}, BAA); err != nil {
		t.Fatalf("baa ok: %v", err)
	}
	real, err := Connect(ctx, Config{Vendor: "epic", BaseURL: "https://ehr.example-hospital.org/api/FHIR/R4", TokenURL: "https://ehr.example-hospital.org/oauth2/token", ClientID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if err := run(real, &fakeModel{baa: true}, Synthetic); err == nil || err.Error() != `dataPolicy "synthetic" only allows sandbox hosts, not ehr.example-hospital.org` {
		t.Fatalf("synthetic: %v", err)
	}
	for host, ok := range map[string]bool{"sandbox.acyclic.sh": true, "acyclic.sh": true, "evilacyclic.sh": false, "fhir.epic.com": true, "[::1]:8080": true, "127.0.0.1": true, "fhir.epic.com.evil.io": false} {
		ehr, _ := Connect(ctx, Config{Vendor: "epic", BaseURL: "http://" + host + "/fhir", TokenURL: "http://" + host + "/token", ClientID: "c"})
		if got := checkPolicy(Synthetic, &fakeModel{}, ehr) == nil; got != ok {
			t.Errorf("%s: allowed=%v", host, got)
		}
	}
}
