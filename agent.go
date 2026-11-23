package acyclic

// The agent loop: ask the model, run the tools it calls, repeat, with hard
// step, tool-call and token budgets.

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// DefaultSystem is the default system prompt.
const DefaultSystem = "You are a clinical data assistant working inside an EHR integration. " +
	"Use the tools to read the chart. Every tool acts on the one patient bound to this session. " +
	"Answer only from tool results. If the chart does not say, say so. Cite dates and values exactly as returned. " +
	"Never guess identifiers. Writes need a person's approval; if one is declined, do not retry it."

// SandboxHosts are the only FHIR hosts DataPolicy Synthetic allows.
var SandboxHosts = []*regexp.Regexp{
	regexp.MustCompile(`(^|\.)acyclic\.sh$`), regexp.MustCompile(`^fhir\.epic\.com$`), regexp.MustCompile(`^fhir-ehr-code\.cerner\.com$`),
	regexp.MustCompile(`^fhir-open\.cerner\.com$`), regexp.MustCompile(`^api\.preview\.platform\.athenahealth\.com$`),
	regexp.MustCompile(`^localhost$`), regexp.MustCompile(`^127\.0\.0\.1$`), regexp.MustCompile(`^\[::1\]$`),
}

// DataPolicy says what data a run may touch. It is required.
type DataPolicy string

const (
	// BAA is real patient data: the model must be created with CoveredByBAA.
	BAA DataPolicy = "baa"
	// Synthetic is sandbox data only: the FHIR host must be in SandboxHosts.
	Synthetic DataPolicy = "synthetic"
)

// Stop reasons.
const (
	StopDone            = "done"
	StopMaxSteps        = "max_steps"
	StopMaxTokens       = "max_tokens"
	StopMaxToolCalls    = "max_tool_calls"
	StopOutputTruncated = "output_truncated"
)

// Event is a UI hook event. Type is model_request, model_response,
// tool_call, tool_result or done; the other fields depend on it. Events
// carry raw data (prompts, results): your code, your rules.
type Event struct {
	Type      string
	Step      int
	Messages  []Message  // model_request
	Tools     []ToolDef  // model_request
	Content   string     // model_response
	ToolCalls []ToolCall // model_response
	Usage     Usage      // model_response (this call), done (total)
	ID        string     // tool_call, tool_result
	Name      string     // tool_call, tool_result
	Args      map[string]any
	Result    *ToolResult // tool_result
	Stop      string      // done
	Text      string      // done
	Steps     int         // done
	ToolCallN int         // done: tool calls made
}

// AgentOptions configure RunAgent.
type AgentOptions struct {
	EHR     *EHR
	Session *Session
	Model   Model
	// DataPolicy is required: BAA or Synthetic.
	DataPolicy DataPolicy
	// Prompt is the user's request, appended to Messages (or the stored conversation).
	Prompt   string
	Messages []Message
	// System defaults to DefaultSystem.
	System string
	// Budgets: MaxSteps 8, MaxToolCalls 20, MaxTokens 30000 in total,
	// MaxOutputTokens 1024 per model call.
	MaxSteps        int
	MaxToolCalls    int
	MaxTokens       int
	MaxOutputTokens int
	// Store saves the conversation after every step.
	Store ConversationStore
	// ConversationID resumes or names the conversation in Store.
	ConversationID string
	OnEvent        func(Event)
}

// AgentResult is the outcome of a run.
type AgentResult struct {
	Text           string
	Stop           string // done, max_steps, max_tokens, max_tool_calls, output_truncated
	Messages       []Message
	Usage          Usage
	Steps          int
	ToolCalls      int
	ConversationID string
}

func checkPolicy(policy DataPolicy, model Model, ehr *EHR) error {
	switch policy {
	case BAA:
		if !model.CoveredByBAA() {
			return errors.New(`dataPolicy "baa" needs a model created with coveredByBAA: true`)
		}
		return nil
	case Synthetic:
		host := jsHostname(ehr.FHIR.BaseURL)
		for _, re := range SandboxHosts {
			if re.MatchString(host) {
				return nil
			}
		}
		return errors.New(`dataPolicy "synthetic" only allows sandbox hosts, not ` + host)
	}
	return errors.New(`set dataPolicy: "baa" (real patient data, BAA-covered model) or "synthetic" (sandbox only)`)
}

// jsHostname is URL.hostname as browsers report it (IPv6 in brackets, lowercase).
func jsHostname(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	h := strings.ToLower(u.Hostname())
	if strings.Contains(h, ":") {
		h = "[" + h + "]"
	}
	return h
}

func def(v, d int) int {
	if v == 0 {
		return d
	}
	return v
}

// RunAgent runs the agent loop until the model answers or a budget runs out.
func RunAgent(ctx context.Context, o AgentOptions) (*AgentResult, error) {
	if o.EHR == nil || o.Session == nil || o.Model == nil {
		return nil, errors.New("EHR, Session and Model are required")
	}
	if err := checkPolicy(o.DataPolicy, o.Model, o.EHR); err != nil {
		return nil, err
	}
	t := guardOf(o.EHR.Telemetry)
	runID := o.ConversationID
	if runID == "" {
		runID = randomID("run_")
	}
	o.Session.setRunIDIfEmpty(runID)
	maxSteps, maxToolCalls, maxTokens := def(o.MaxSteps, 8), def(o.MaxToolCalls, 20), def(o.MaxTokens, 30000)
	maxOutput := def(o.MaxOutputTokens, 1024)
	emit := func(e Event) {
		if o.OnEvent != nil {
			func() { defer func() { _ = recover() }(); o.OnEvent(e) }()
		}
	}

	var messages []Message
	if o.Messages != nil {
		messages = append(messages, o.Messages...)
	} else if o.Store != nil {
		stored, err := o.Store.Load(ctx, runID)
		if err != nil {
			return nil, err
		}
		messages = stored
	}
	hasSystem := false
	for _, m := range messages {
		if m.Role == "system" {
			hasSystem = true
			break
		}
	}
	if !hasSystem {
		system := o.System
		if system == "" {
			system = DefaultSystem
		}
		messages = append([]Message{{Role: "system", Content: system}}, messages...)
	}
	if o.Prompt != "" {
		messages = append(messages, Message{Role: "user", Content: o.Prompt})
	}
	save := func() error {
		if o.Store == nil {
			return nil
		}
		return o.Store.Save(ctx, runID, messages)
	}

	ctx, span := t.startSpan(ctx, "invoke_agent acyclic", map[string]any{
		"gen_ai.operation.name": "invoke_agent", "gen_ai.provider.name": o.Model.Provider(), "gen_ai.request.model": o.Model.Name(),
		"gen_ai.conversation.id": runID, "acyclic.vendor": o.EHR.Vendor.Name,
	})
	used, toolCalls, steps, stop := 0, 0, 0, StopDone
	var usage Usage
	fail := func(err error) (*AgentResult, error) {
		span.setAttributes(map[string]any{"error.type": errorType(err)})
		span.end(err)
		return nil, err
	}
	for {
		if steps >= maxSteps {
			stop = StopMaxSteps
			break
		}
		if used >= maxTokens {
			stop = StopMaxTokens
			break
		}
		steps++
		tools := o.Session.Definitions()
		emit(Event{Type: "model_request", Step: steps, Messages: messages, Tools: tools})
		mctx, ms := t.startSpan(ctx, "chat "+o.Model.Name(), map[string]any{
			"gen_ai.operation.name": "chat", "gen_ai.provider.name": o.Model.Provider(), "gen_ai.request.model": o.Model.Name(),
		})
		reply, err := o.Model.Chat(mctx, ChatRequest{Messages: messages, Tools: tools, MaxTokens: maxOutput})
		if err != nil {
			et := errorType(err)
			if et == "error" {
				et = "model_error"
			}
			ms.setAttributes(map[string]any{"error.type": et})
			ms.end(err)
			return fail(err)
		}
		finish := reply.FinishReason
		if finish == "" {
			finish = "undefined"
		}
		ms.setAttributes(map[string]any{
			"gen_ai.usage.input_tokens": reply.Usage.Input, "gen_ai.usage.output_tokens": reply.Usage.Output,
			"gen_ai.response.finish_reasons": []string{finish},
		})
		ms.end(nil)
		usage.Input += reply.Usage.Input
		usage.Output += reply.Usage.Output
		used = usage.Input + usage.Output
		var calls []ToolCall
		if len(reply.ToolCalls) > 0 {
			calls = reply.ToolCalls
		}
		messages = append(messages, Message{Role: "assistant", Content: reply.Content, ToolCalls: calls})
		emit(Event{Type: "model_response", Step: steps, Content: reply.Content, ToolCalls: reply.ToolCalls, Usage: reply.Usage})
		t.log(ctx, "info", "agent.step", map[string]any{
			"gen_ai.conversation.id": runID, "gen_ai.usage.input_tokens": reply.Usage.Input, "gen_ai.usage.output_tokens": reply.Usage.Output,
		})

		if len(reply.ToolCalls) == 0 {
			if err := save(); err != nil {
				return fail(err)
			}
			// A reply cut off by the output cap is not an answer; say so.
			if reply.FinishReason == "length" || reply.FinishReason == "max_tokens" {
				stop = StopOutputTruncated
			}
			break
		}
		for _, call := range reply.ToolCalls {
			var out ToolResult
			if toolCalls >= maxToolCalls {
				out = ToolResult{OK: false, Error: "tool call budget exhausted for this run", Code: "budget"}
			} else {
				toolCalls++
				emit(Event{Type: "tool_call", Step: steps, ID: call.ID, Name: call.Name, Args: call.Args})
				out = o.Session.Call(WithToolCallID(ctx, call.ID), call.Name, call.Args)
			}
			res := out
			emit(Event{Type: "tool_result", Step: steps, ID: call.ID, Name: call.Name, Result: &res})
			messages = append(messages, Message{Role: "tool", ToolCallID: call.ID, Name: call.Name, Content: out.modelContent()})
		}
		if err := save(); err != nil {
			return fail(err)
		}
		if toolCalls >= maxToolCalls {
			stop = StopMaxToolCalls
			break
		}
	}
	stopAttrs := map[string]any{"gen_ai.conversation.id": runID}
	if stop != StopDone {
		stopAttrs["error.type"] = stop
	}
	t.log(ctx, "info", "agent.stop", stopAttrs)
	span.end(nil)
	final := ""
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" {
			final = messages[i].Content
			break
		}
	}
	emit(Event{Type: "done", Stop: stop, Text: final, Usage: usage, Steps: steps, ToolCallN: toolCalls})
	return &AgentResult{Text: final, Stop: stop, Messages: messages, Usage: usage, Steps: steps, ToolCalls: toolCalls, ConversationID: runID}, nil
}
