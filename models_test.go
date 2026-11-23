package acyclic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var convo = []Message{
	{Role: "system", Content: "sys"},
	{Role: "user", Content: "hi"},
	{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "c1", Name: "get_patient", Args: map[string]any{}}, {ID: "c2", Name: "list_allergies", Args: map[string]any{}}}},
	{Role: "tool", ToolCallID: "c1", Name: "get_patient", Content: `{"id":"p"}`},
	{Role: "tool", ToolCallID: "c2", Name: "list_allergies", Content: `{"items":[]}`},
}

func capture(t *testing.T, reply string, check func(r *http.Request, body map[string]any)) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Error(err)
		}
		check(r, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOpenAICompatible(t *testing.T) {
	srv := capture(t, `{"choices":[{"message":{"content":null,"tool_calls":[{"id":"x1","function":{"name":"get_labs_vitals","arguments":"{\"code\":\"4548-4\"}"}},{"function":{"name":"get_patient","arguments":""}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`,
		func(r *http.Request, b map[string]any) {
			if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
				t.Errorf("%s %s", r.URL.Path, r.Header.Get("Authorization"))
			}
			if b["max_tokens"] != 50.0 || get(b, "provider", "order") == nil || len(asArr(b["tools"])) != 1 {
				t.Errorf("body %v", b)
			}
			msgs := asArr(b["messages"])
			if len(msgs) != 5 || get(msgs[2], "content") != nil || get(msgs[3], "tool_call_id") != "c1" || get(index0(get(msgs[2], "tool_calls")), "function", "arguments") != "{}" {
				t.Errorf("messages %s", marshalJSON(msgs))
			}
		})
	m := OpenAICompatible(OpenAIConfig{BaseURL: srv.URL + "/v1/", APIKey: "k", Model: "m", ExtraBody: map[string]any{"provider": map[string]any{"order": []string{"a"}}}})
	out, err := m.Chat(context.Background(), ChatRequest{Messages: convo, Tools: []ToolDef{{Name: "get_patient", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}}, MaxTokens: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.ToolCalls) != 2 || out.ToolCalls[0].Args["code"] != "4548-4" || out.ToolCalls[1].ID != "call_1" || out.Usage != (Usage{12, 3}) || out.FinishReason != "tool_calls" {
		t.Fatalf("%+v", out)
	}
}

func TestAnthropicMergesToolResults(t *testing.T) {
	srv := capture(t, `{"content":[{"type":"text","text":"ok "},{"type":"tool_use","id":"t1","name":"get_patient","input":{}}],"usage":{"input_tokens":5,"output_tokens":2},"stop_reason":"tool_use"}`,
		func(r *http.Request, b map[string]any) {
			if r.Header.Get("x-api-key") != "k" || r.Header.Get("anthropic-version") != "2023-06-01" || b["system"] != "sys" {
				t.Errorf("headers/system: %v", b["system"])
			}
			msgs := asArr(b["messages"])
			if len(msgs) != 3 || get(msgs[2], "role") != "user" || len(asArr(get(msgs[2], "content"))) != 2 {
				t.Errorf("messages %s", marshalJSON(msgs))
			}
		})
	out, err := Anthropic(AnthropicConfig{APIKey: "k", Model: "claude", BaseURL: srv.URL}).Chat(context.Background(), ChatRequest{Messages: convo})
	if err != nil || out.Content != "ok " || out.ToolCalls[0].ID != "t1" || out.FinishReason != "tool_use" {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestBedrockConverse(t *testing.T) {
	srv := capture(t, `{"output":{"message":{"content":[{"text":"done"},{"toolUse":{"toolUseId":"u1","name":"get_patient","input":{"a":1}}}]}},"usage":{"inputTokens":7,"outputTokens":1},"stopReason":"tool_use"}`,
		func(r *http.Request, b map[string]any) {
			if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AKID/") || r.URL.EscapedPath() != "/model/us.anthropic.claude-v1%3A0/converse" {
				t.Errorf("%s %s", r.Header.Get("Authorization"), r.URL.EscapedPath())
			}
			if get(index0(b["system"]), "text") != "sys" || get(b, "inferenceConfig", "maxTokens") != 1024.0 {
				t.Errorf("body %v", b)
			}
			tr := get(index0(get(asArr(b["messages"])[2], "content")), "toolResult", "toolUseId")
			if tr != "c1" {
				t.Errorf("tool result %v", tr)
			}
		})
	m := Bedrock(BedrockConfig{Model: "us.anthropic.claude-v1:0", Credentials: AWSCredentials{AccessKeyID: "AKID", SecretAccessKey: "s"}, Endpoint: srv.URL})
	out, err := m.Chat(context.Background(), ChatRequest{Messages: convo})
	if err != nil || out.Content != "done" || out.ToolCalls[0].Args["a"] != 1.0 || out.Usage != (Usage{7, 1}) {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestModelErrorKeepsDetailOutOfMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"message":"prompt mentions Okafor"}}`))
	}))
	defer srv.Close()
	_, err := OpenAICompatible(OpenAIConfig{BaseURL: srv.URL, Model: "m"}).Chat(context.Background(), ChatRequest{Messages: convo})
	me, ok := err.(*ModelError)
	if !ok || err.Error() != "model request failed: 400" || me.Detail != "prompt mentions Okafor" {
		t.Fatalf("%v", err)
	}
}
