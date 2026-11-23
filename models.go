package acyclic

// Model adapters. Messages use one neutral shape (Message); each adapter
// converts to and from its wire format.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Message is one conversation turn: role system, user, assistant or tool.
// The JSON shape matches acyclic-sdk-js, so stored conversations move
// between SDKs.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"toolCalls,omitempty"`
	ToolCallID string     `json:"toolCallId,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// ToolCall is a model's request to run a tool.
type ToolCall struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

// Usage counts tokens.
type Usage struct {
	Input  int `json:"input"`
	Output int `json:"output"`
}

// ChatRequest is one model call.
type ChatRequest struct {
	Messages  []Message
	Tools     []ToolDef
	MaxTokens int
}

// ChatResponse is one model reply.
type ChatResponse struct {
	Content      string
	ToolCalls    []ToolCall
	Usage        Usage
	FinishReason string
}

// Model is a chat model with tool calling.
type Model interface {
	Provider() string // gen_ai.provider.name: openai, anthropic, aws.bedrock
	Name() string     // gen_ai.request.model
	CoveredByBAA() bool
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
}

// ModelError is a non-2xx model API response. Detail is the provider's
// message, kept for your own handling and never logged by the SDK.
type ModelError struct {
	Status int
	Detail string
}

func (e *ModelError) Error() string     { return fmt.Sprintf("model request failed: %d", e.Status) }
func (e *ModelError) ErrorType() string { return strconv.Itoa(e.Status) }

func defaultClient(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: 120 * time.Second}
}

func postJSON(ctx context.Context, hc *http.Client, u string, headers map[string]string, body any) (map[string]any, error) {
	payload := []byte(marshalJSON(body))
	req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return doJSON(hc, req)
}

func doJSON(hc *http.Client, req *http.Request) (map[string]any, error) {
	res, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	var j map[string]any
	_ = json.Unmarshal(raw, &j)
	if res.StatusCode < 200 || res.StatusCode > 299 {
		detail := str(get(j, "error", "message"))
		if detail == "" {
			detail = str(j["message"])
		}
		return nil, &ModelError{Status: res.StatusCode, Detail: detail}
	}
	return j, nil
}

func intOf(v any) int {
	f, _ := toNumber(v)
	return int(f)
}

func parseArgs(v any) map[string]any {
	switch x := v.(type) {
	case string:
		if x == "" {
			return map[string]any{}
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(x), &m); err != nil || m == nil {
			var anyv any
			if json.Unmarshal([]byte(x), &anyv) == nil && anyv == nil {
				return map[string]any{}
			}
			return map[string]any{"__unparsed": x}
		}
		return m
	case map[string]any:
		return x
	case nil:
		return map[string]any{}
	}
	return map[string]any{"__unparsed": v}
}

// --- OpenAI-compatible ---

// OpenAIConfig configures OpenAICompatible.
type OpenAIConfig struct {
	BaseURL      string // default https://api.openai.com/v1
	APIKey       string
	Model        string
	CoveredByBAA bool
	Headers      map[string]string
	// ExtraBody is merged into every request (e.g. OpenRouter provider routing).
	ExtraBody  map[string]any
	HTTPClient *http.Client
}

type openAIModel struct{ c OpenAIConfig }

// OpenAICompatible is any OpenAI Chat Completions compatible API: OpenAI,
// Azure OpenAI, OpenRouter, vLLM, Ollama, DeepSeek.
func OpenAICompatible(c OpenAIConfig) Model {
	if c.BaseURL == "" {
		c.BaseURL = "https://api.openai.com/v1"
	}
	c.HTTPClient = defaultClient(c.HTTPClient)
	return &openAIModel{c}
}

func (m *openAIModel) Provider() string   { return "openai" }
func (m *openAIModel) Name() string       { return m.c.Model }
func (m *openAIModel) CoveredByBAA() bool { return m.c.CoveredByBAA }

func (m *openAIModel) Chat(ctx context.Context, r ChatRequest) (*ChatResponse, error) {
	maxTokens := r.MaxTokens
	if maxTokens == 0 {
		maxTokens = 1024
	}
	msgs := make([]any, 0, len(r.Messages))
	for _, x := range r.Messages {
		switch {
		case x.Role == "tool":
			msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": x.ToolCallID, "content": x.Content})
		case x.Role == "assistant" && len(x.ToolCalls) > 0:
			calls := make([]any, len(x.ToolCalls))
			for i, c := range x.ToolCalls {
				args := c.Args
				if args == nil {
					args = map[string]any{}
				}
				calls[i] = map[string]any{"id": c.ID, "type": "function", "function": map[string]any{"name": c.Name, "arguments": marshalJSON(args)}}
			}
			var content any
			if x.Content != "" {
				content = x.Content
			}
			msgs = append(msgs, map[string]any{"role": "assistant", "content": content, "tool_calls": calls})
		default:
			msgs = append(msgs, map[string]any{"role": x.Role, "content": x.Content})
		}
	}
	body := map[string]any{"model": m.c.Model, "max_tokens": maxTokens}
	for k, v := range m.c.ExtraBody {
		body[k] = v
	}
	body["messages"] = msgs
	if len(r.Tools) > 0 {
		tools := make([]any, len(r.Tools))
		for i, t := range r.Tools {
			tools[i] = map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": t.InputSchema}}
		}
		body["tools"] = tools
	}
	headers := map[string]string{"Authorization": "Bearer " + m.c.APIKey}
	for k, v := range m.c.Headers {
		headers[k] = v
	}
	j, err := postJSON(ctx, m.c.HTTPClient, strings.TrimSuffix(m.c.BaseURL, "/")+"/chat/completions", headers, body)
	if err != nil {
		return nil, err
	}
	choice := asMap(index0(j["choices"]))
	msg := asMap(choice["message"])
	out := &ChatResponse{
		Content:      str(msg["content"]),
		Usage:        Usage{Input: intOf(get(j, "usage", "prompt_tokens")), Output: intOf(get(j, "usage", "completion_tokens"))},
		FinishReason: str(choice["finish_reason"]),
	}
	for i, c := range asArr(msg["tool_calls"]) {
		id := str(get(c, "id"))
		if id == "" {
			id = "call_" + strconv.Itoa(i)
		}
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: id, Name: str(get(c, "function", "name")), Args: parseArgs(get(c, "function", "arguments"))})
	}
	return out, nil
}

// --- Anthropic ---

// AnthropicConfig configures Anthropic.
type AnthropicConfig struct {
	APIKey       string
	Model        string
	CoveredByBAA bool
	BaseURL      string // default https://api.anthropic.com
	HTTPClient   *http.Client
}

type anthropicModel struct{ c AnthropicConfig }

// Anthropic is the Anthropic Messages API.
func Anthropic(c AnthropicConfig) Model {
	if c.BaseURL == "" {
		c.BaseURL = "https://api.anthropic.com"
	}
	c.HTTPClient = defaultClient(c.HTTPClient)
	return &anthropicModel{c}
}

func (m *anthropicModel) Provider() string   { return "anthropic" }
func (m *anthropicModel) Name() string       { return m.c.Model }
func (m *anthropicModel) CoveredByBAA() bool { return m.c.CoveredByBAA }

type block = map[string]any

type wireMessage struct {
	role    string
	content []block
}

// toAnthropic splits out the system prompt and merges consecutive turns of
// one role, as the Messages and Converse APIs require.
func toAnthropic(messages []Message) (string, []wireMessage) {
	var system []string
	var out []wireMessage
	for _, m := range messages {
		if m.Role == "system" {
			if m.Content != "" {
				system = append(system, m.Content)
			}
			continue
		}
		role := m.Role
		var blocks []block
		switch m.Role {
		case "tool":
			role = "user"
			blocks = []block{{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": m.Content}}
		case "assistant":
			if m.Content != "" {
				blocks = append(blocks, block{"type": "text", "text": m.Content})
			}
			for _, c := range m.ToolCalls {
				args := c.Args
				if args == nil {
					args = map[string]any{}
				}
				blocks = append(blocks, block{"type": "tool_use", "id": c.ID, "name": c.Name, "input": args})
			}
		default:
			blocks = []block{{"type": "text", "text": m.Content}}
		}
		if n := len(out); n > 0 && out[n-1].role == role {
			out[n-1].content = append(out[n-1].content, blocks...)
		} else {
			if blocks == nil {
				blocks = []block{}
			}
			out = append(out, wireMessage{role, blocks})
		}
	}
	return strings.Join(system, "\n\n"), out
}

func fromAnthropic(content []any, input, output int, stop string) *ChatResponse {
	out := &ChatResponse{Usage: Usage{Input: input, Output: output}, FinishReason: stop}
	var text strings.Builder
	for _, b := range content {
		switch get(b, "type") {
		case "text":
			text.WriteString(str(get(b, "text")))
		case "tool_use":
			args := asMap(get(b, "input"))
			if args == nil {
				args = map[string]any{}
			}
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: str(get(b, "id")), Name: str(get(b, "name")), Args: args})
		}
	}
	out.Content = text.String()
	return out
}

func (m *anthropicModel) Chat(ctx context.Context, r ChatRequest) (*ChatResponse, error) {
	maxTokens := r.MaxTokens
	if maxTokens == 0 {
		maxTokens = 1024
	}
	system, msgs := toAnthropic(r.Messages)
	wire := make([]any, len(msgs))
	for i, w := range msgs {
		wire[i] = map[string]any{"role": w.role, "content": w.content}
	}
	body := map[string]any{"model": m.c.Model, "max_tokens": maxTokens, "messages": wire}
	if system != "" {
		body["system"] = system
	}
	if len(r.Tools) > 0 {
		tools := make([]any, len(r.Tools))
		for i, t := range r.Tools {
			tools[i] = map[string]any{"name": t.Name, "description": t.Description, "input_schema": t.InputSchema}
		}
		body["tools"] = tools
	}
	j, err := postJSON(ctx, m.c.HTTPClient, strings.TrimSuffix(m.c.BaseURL, "/")+"/v1/messages",
		map[string]string{"x-api-key": m.c.APIKey, "anthropic-version": "2023-06-01"}, body)
	if err != nil {
		return nil, err
	}
	return fromAnthropic(asArr(j["content"]), intOf(get(j, "usage", "input_tokens")), intOf(get(j, "usage", "output_tokens")), str(j["stop_reason"])), nil
}

// --- Bedrock ---

// BedrockConfig configures Bedrock.
type BedrockConfig struct {
	Region       string // default us-east-1
	Model        string // model id or inference profile, e.g. us.anthropic.claude-...-v1:0
	Credentials  AWSCredentials
	CoveredByBAA bool
	HTTPClient   *http.Client
	// Endpoint overrides https://bedrock-runtime.{region}.amazonaws.com (tests, VPC endpoints).
	Endpoint string
}

type bedrockModel struct{ c BedrockConfig }

// Bedrock is the Amazon Bedrock Converse API, signed with SigV4. Bedrock is
// covered by the AWS BAA for eligible accounts; you still set CoveredByBAA.
func Bedrock(c BedrockConfig) Model {
	if c.Region == "" {
		c.Region = "us-east-1"
	}
	c.HTTPClient = defaultClient(c.HTTPClient)
	return &bedrockModel{c}
}

func (m *bedrockModel) Provider() string   { return "aws.bedrock" }
func (m *bedrockModel) Name() string       { return m.c.Model }
func (m *bedrockModel) CoveredByBAA() bool { return m.c.CoveredByBAA }

func toConverse(b block) map[string]any {
	switch b["type"] {
	case "text":
		return map[string]any{"text": b["text"]}
	case "tool_use":
		return map[string]any{"toolUse": map[string]any{"toolUseId": b["id"], "name": b["name"], "input": b["input"]}}
	}
	return map[string]any{"toolResult": map[string]any{"toolUseId": b["tool_use_id"], "content": []any{map[string]any{"text": b["content"]}}}}
}

func (m *bedrockModel) url() string {
	base := m.c.Endpoint
	if base == "" {
		base = "https://bedrock-runtime." + m.c.Region + ".amazonaws.com"
	}
	return strings.TrimSuffix(base, "/") + "/model/" + encodeURIComponent(m.c.Model) + "/converse"
}

func (m *bedrockModel) Chat(ctx context.Context, r ChatRequest) (*ChatResponse, error) {
	maxTokens := r.MaxTokens
	if maxTokens == 0 {
		maxTokens = 1024
	}
	system, msgs := toAnthropic(r.Messages)
	wire := make([]any, len(msgs))
	for i, w := range msgs {
		content := make([]any, len(w.content))
		for k, b := range w.content {
			content[k] = toConverse(b)
		}
		wire[i] = map[string]any{"role": w.role, "content": content}
	}
	body := map[string]any{"messages": wire, "inferenceConfig": map[string]any{"maxTokens": maxTokens}}
	if system != "" {
		body["system"] = []any{map[string]any{"text": system}}
	}
	if len(r.Tools) > 0 {
		tools := make([]any, len(r.Tools))
		for i, t := range r.Tools {
			tools[i] = map[string]any{"toolSpec": map[string]any{"name": t.Name, "description": t.Description, "inputSchema": map[string]any{"json": t.InputSchema}}}
		}
		body["toolConfig"] = map[string]any{"tools": tools}
	}
	payload := []byte(marshalJSON(body))
	req, err := http.NewRequestWithContext(ctx, "POST", m.url(), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := SignV4(req, payload, m.c.Region, "bedrock", m.c.Credentials, time.Now()); err != nil {
		return nil, err
	}
	j, err := doJSON(m.c.HTTPClient, req)
	if err != nil {
		return nil, err
	}
	var content []any
	for _, c := range asArr(get(j, "output", "message", "content")) {
		if tu := asMap(get(c, "toolUse")); tu != nil {
			content = append(content, map[string]any{"type": "tool_use", "id": tu["toolUseId"], "name": tu["name"], "input": tu["input"]})
		} else {
			content = append(content, map[string]any{"type": "text", "text": str(get(c, "text"))})
		}
	}
	return fromAnthropic(content, intOf(get(j, "usage", "inputTokens")), intOf(get(j, "usage", "outputTokens")), str(j["stopReason"])), nil
}
