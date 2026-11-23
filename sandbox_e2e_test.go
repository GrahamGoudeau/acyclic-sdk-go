package acyclic

// End-to-end against a running acyclic sandbox:
//   SANDBOX_URL=http://localhost:8190 go test ./...
// Mirrors acyclic-sdk-js test/sandbox.e2e.test.mjs.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

type registration struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	FHIRBaseURL  string `json:"fhir_base_url"`
	TokenURL     string `json:"token_url"`
	PracticeID   string `json:"practice_id"`
}

func register(t *testing.T, base, vendor string) (registration, *KeyPair) {
	t.Helper()
	body := map[string]any{"vendor": vendor, "name": "sdk e2e"}
	var key *KeyPair
	if Vendors[vendor].Auth == "private_key_jwt" {
		var err error
		if key, err = GenerateKeyPair("RS384"); err != nil {
			t.Fatal(err)
		}
		body["jwks"] = map[string]any{"keys": []any{key.PublicJWK}}
	}
	b, _ := json.Marshal(body)
	res, err := http.Post(base+"/register", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != 201 {
		t.Fatalf("register %s: %d %s", vendor, res.StatusCode, raw)
	}
	var reg registration
	if err := json.Unmarshal(raw, &reg); err != nil {
		t.Fatal(err)
	}
	return reg, key
}

func TestSandboxE2E(t *testing.T) {
	base := strings.TrimRight(os.Getenv("SANDBOX_URL"), "/")
	if base == "" {
		t.Skip("set SANDBOX_URL to run against a sandbox")
	}
	for _, vendor := range []string{"epic", "oracle", "athena", "meditech", "ecw"} {
		t.Run(vendor, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			reg, key := register(t, base, vendor)
			rec := &recorder{}
			var jsonLines bytes.Buffer
			cfg := Config{
				Vendor: vendor, BaseURL: reg.FHIRBaseURL, TokenURL: reg.TokenURL, ClientID: reg.ClientID, ClientSecret: reg.ClientSecret,
				PracticeID: reg.PracticeID, Write: true, Telemetry: MultiTelemetry(rec, JSONTelemetry(&jsonLines)),
			}
			if key != nil {
				cfg.PrivateKeyPEM, cfg.KeyID = key.PrivateKeyPEM, key.KeyID
			}
			ehr, err := Connect(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			s := NewSession(ehr, SessionOptions{AllowWrites: true, Approve: func(context.Context, ApprovalRequest) bool { return true }})
			found := s.Call(ctx, "find_patient", map[string]any{"family": "Okafor", "given": "Samuel", "birthdate": "1948-11-19"})
			if !found.OK {
				t.Fatal(found.Error)
			}
			items := found.Result.(map[string]any)["items"].([]map[string]any)
			if len(items) != 1 {
				t.Fatalf("find_patient returned %d", len(items))
			}
			pid := items[0]["id"].(string)
			if r := s.Call(ctx, "select_patient", map[string]any{"patient_id": pid}); !r.OK {
				t.Fatal(r.Error)
			}
			has := func(name string) bool {
				for _, d := range s.Definitions() {
					if d.Name == name {
						return true
					}
				}
				return false
			}
			for _, c := range []struct {
				name string
				args map[string]any
			}{
				{"get_patient", nil}, {"list_conditions", nil}, {"list_medications", map[string]any{"status": "all"}}, {"list_allergies", nil},
				{"get_labs_vitals", map[string]any{"category": "laboratory", "limit": 40}}, {"get_labs_vitals", map[string]any{"category": "vital-signs", "code": "8867-4"}},
				{"list_encounters", map[string]any{"limit": 5}}, {"list_appointments", nil}, {"list_documents", nil}, {"get_coverage", nil},
			} {
				if !has(c.name) {
					continue // vendor does not offer it
				}
				r := s.Call(ctx, c.name, c.args)
				if !r.OK {
					t.Fatalf("%s: %s", c.name, r.Error)
				}
				if m, ok := r.Result.(map[string]any); ok && c.name != "list_allergies" {
					if its, ok := m["items"].([]map[string]any); ok && len(its) == 0 {
						t.Fatalf("%s returned nothing", c.name)
					}
				}
			}
			labs := s.Call(ctx, "get_labs_vitals", map[string]any{"limit": 40})
			if n := len(labs.Result.(map[string]any)["items"].([]map[string]any)); n != 40 {
				t.Fatalf("paging past the first page: got %d labs", n)
			}
			docs := s.Call(ctx, "list_documents", nil)
			first := docs.Result.(map[string]any)["items"].([]map[string]any)[0]
			text := s.Call(ctx, "read_document", map[string]any{"document_id": first["id"]})
			if !text.OK {
				t.Fatal(text.Error)
			}
			if !strings.Contains(text.Result.(map[string]any)["text"].(string), "SYNTHETIC RECORD") {
				t.Fatalf("document text: %v", text.Result)
			}
			if has("create_note") {
				note := s.Call(ctx, "create_note", map[string]any{"title": "E2E note", "text": "Reviewed chart."})
				if !note.OK {
					t.Fatal(note.Error)
				}
				after := s.Call(ctx, "list_documents", nil)
				found := false
				for _, d := range after.Result.(map[string]any)["items"].([]map[string]any) {
					if d["title"] == "E2E note" {
						found = true
					}
				}
				if !found {
					t.Fatal("created note not listed")
				}
			}
			// The agent loop on an unbound session, with a scripted model.
			model := &fakeModel{replies: []ChatResponse{
				{ToolCalls: []ToolCall{{ID: "a", Name: "find_patient", Args: map[string]any{"family": "Okafor", "given": "Samuel", "birthdate": "1948-11-19"}}}},
				{ToolCalls: []ToolCall{{ID: "b", Name: "select_patient", Args: map[string]any{"patient_id": pid}}}},
				{ToolCalls: []ToolCall{{ID: "c", Name: "list_conditions", Args: map[string]any{}}}},
				{Content: "done", FinishReason: "stop"},
			}}
			out, err := RunAgent(ctx, AgentOptions{EHR: ehr, Session: NewSession(ehr, SessionOptions{}), Model: model, DataPolicy: Synthetic, Prompt: "Summarize Samuel Okafor's problems."})
			if err != nil || out.Stop != StopDone || out.ToolCalls != 3 {
				t.Fatalf("agent: %v %+v", err, out)
			}
			for _, m := range out.Messages {
				if m.Role == "tool" && strings.Contains(m.Content, `"error"`) {
					t.Fatalf("agent tool error: %s", m.Content)
				}
			}
			for name, tel := range map[string]string{"recorder": rec.all(), "json": jsonLines.String()} {
				if regexp.MustCompile(`Okafor|Samuel|1948-11-19`).MatchString(tel) {
					t.Fatalf("patient data leaked into %s telemetry", name)
				}
				if strings.Contains(tel, pid) {
					t.Fatalf("raw patient id leaked into %s telemetry", name)
				}
				if !strings.Contains(tel, `"patient_ref":"p_`) {
					t.Fatalf("%s telemetry has no audit records", name)
				}
			}
		})
	}
}
