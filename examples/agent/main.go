// Command agent runs the full sandbox flow: register a throwaway app with
// the free synthetic-data sandbox, connect as Epic with a fresh RS384 key,
// and let a model answer a question about a synthetic patient.
//
//	OPENROUTER_API_KEY=... go run ./examples/agent
//	SANDBOX_URL=http://localhost:8190 OPENROUTER_API_KEY=... go run ./examples/agent
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	acyclic "github.com/GrahamGoudeau/acyclic-sdk-go"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	sandbox := env("SANDBOX_URL", "https://sandbox.acyclic.sh")
	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		log.Fatal("set OPENROUTER_API_KEY")
	}

	// 1. Register an app. Epic uses private_key_jwt, so send a public key.
	key, err := acyclic.GenerateKeyPair("RS384")
	if err != nil {
		log.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"vendor": "epic", "name": "go example", "jwks": map[string]any{"keys": []any{key.PublicJWK}}})
	res, err := http.Post(sandbox+"/register", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Fatal(err)
	}
	var reg struct {
		ClientID    string `json:"client_id"`
		FHIRBaseURL string `json:"fhir_base_url"`
		TokenURL    string `json:"token_url"`
		Error       string `json:"error"`
	}
	_ = json.NewDecoder(res.Body).Decode(&reg)
	res.Body.Close()
	if res.StatusCode != 201 {
		log.Fatalf("register: %d %s", res.StatusCode, reg.Error)
	}

	// 2. Connect. Telemetry goes to stderr as JSON lines, with no patient data.
	ehr, err := acyclic.Connect(ctx, acyclic.Config{
		Vendor: "epic", BaseURL: reg.FHIRBaseURL, TokenURL: reg.TokenURL, ClientID: reg.ClientID,
		PrivateKey: key.PrivateKey, KeyID: key.KeyID,
		Telemetry: acyclic.JSONTelemetry(os.Stderr),
	})
	if err != nil {
		log.Fatal(err)
	}

	// 3. An unbound session: the model finds and selects the patient itself.
	sess := acyclic.NewSession(ehr, acyclic.SessionOptions{})
	model := acyclic.OpenAICompatible(acyclic.OpenAIConfig{
		BaseURL: "https://openrouter.ai/api/v1",
		APIKey:  apiKey,
		Model:   env("MODEL", "deepseek/deepseek-v4.1-flash-1017"),
	})
	out, err := acyclic.RunAgent(ctx, acyclic.AgentOptions{
		EHR: ehr, Session: sess, Model: model,
		DataPolicy: acyclic.Synthetic, // sandbox hosts only; use acyclic.BAA for real data
		Prompt:     "Find Samuel Okafor, born 1948-11-19. Summarize his active problems, medications and latest HbA1c.",
		OnEvent: func(e acyclic.Event) {
			if e.Type == "tool_call" {
				fmt.Fprintf(os.Stderr, "-> %s\n", e.Name)
			}
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(out.Text)
	fmt.Fprintf(os.Stderr, "stop=%s steps=%d tool_calls=%d tokens=%d+%d\n", out.Stop, out.Steps, out.ToolCalls, out.Usage.Input, out.Usage.Output)
}
