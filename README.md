# acyclic-sdk-go

A Go SDK for building agents on top of EHRs. It handles SMART Backend Services auth, a FHIR R4 client with profiles for Epic, Oracle Health, athenahealth, MEDITECH and eClinicalWorks, 13 patient-bound tools that return small JSON projections, an agent loop with hard budgets, and telemetry that keeps patient data out of your logs. Bring your own EHR credentials and your own model. Standard library only, Go 1.22+. Ports of the same [spec](https://github.com/GrahamGoudeau/acyclic-sdk-spec) exist in [JavaScript](https://github.com/GrahamGoudeau/acyclic-sdk-js) and [Rust](https://github.com/GrahamGoudeau/acyclic-sdk-rs).

## Install

```sh
go get github.com/GrahamGoudeau/acyclic-sdk-go
```

```go
import acyclic "github.com/GrahamGoudeau/acyclic-sdk-go"
```

## 30-second quickstart (free sandbox)

The hosted sandbox at `https://sandbox.acyclic.sh` serves vendor-compatible mock APIs with synthetic patients. The playground at [agentsdk.acyclic.sh](https://agentsdk.acyclic.sh) shows every request and generates this code.

```go
ctx := context.Background()

// 1. Register a sandbox app with a fresh key. Real vendors: use their developer portal.
key, _ := acyclic.GenerateKeyPair("RS384")
body, _ := json.Marshal(map[string]any{"vendor": "epic", "jwks": map[string]any{"keys": []any{key.PublicJWK}}})
res, _ := http.Post("https://sandbox.acyclic.sh/register", "application/json", bytes.NewReader(body))
var reg struct {
	ClientID    string `json:"client_id"`
	FHIRBaseURL string `json:"fhir_base_url"`
	TokenURL    string `json:"token_url"`
}
_ = json.NewDecoder(res.Body).Decode(&reg)

// 2. Connect.
ehr, err := acyclic.Connect(ctx, acyclic.Config{
	Vendor: "epic", BaseURL: reg.FHIRBaseURL, TokenURL: reg.TokenURL, ClientID: reg.ClientID,
	PrivateKey: key.PrivateKey, KeyID: key.KeyID,
	Telemetry: acyclic.JSONTelemetry(os.Stderr),
})
if err != nil {
	log.Fatal(err)
}

// 3. One session = one patient. Leave Patient empty and the model can call
//    find_patient + select_patient itself. (Sandbox patients: GET
//    https://sandbox.acyclic.sh/api/patients?vendor=epic)
sess := acyclic.NewSession(ehr, acyclic.SessionOptions{})

// 4. Call tools directly...
r := sess.Call(ctx, "find_patient", map[string]any{"family": "Okafor", "given": "Samuel", "birthdate": "1948-11-19"})
fmt.Println(r.JSON()) // {"ok":true,"result":{"items":[{"id":"e364...","name":"Samuel Okafor",...}],"total":1,"truncated":false}}

// ...or let a model drive them.
out, err := acyclic.RunAgent(ctx, acyclic.AgentOptions{
	EHR: ehr, Session: sess,
	Model: acyclic.OpenAICompatible(acyclic.OpenAIConfig{
		BaseURL: "https://openrouter.ai/api/v1", APIKey: os.Getenv("OPENROUTER_API_KEY"),
		Model: "deepseek/deepseek-v4.1-flash-1017",
	}),
	DataPolicy: acyclic.Synthetic, // sandbox hosts only; real patient data needs acyclic.BAA
	Prompt:     "Select Samuel Okafor and summarize his active problems and latest HbA1c.",
})
if err != nil {
	log.Fatal(err)
}
fmt.Println(out.Text, out.Stop)
```

A runnable version is in [`examples/agent`](examples/agent/main.go): `OPENROUTER_API_KEY=... go run ./examples/agent`.

## Connect to a real EHR

| `Vendor` | Default auth | FHIR base URL shape | Notes |
|---|---|---|---|
| `epic` | signed JWT (RS384) | `https://{host}/{instance}/api/FHIR/R4` | Register a JWKS (URL or upload) in Epic's developer portal |
| `oracle` | client secret (`Auth: "private_key_jwt"` also works) | `https://fhir-ehr.cerner.com/r4/{tenant}` | System account in Oracle's code console |
| `athena` | client secret (JWT also works) | `https://api.platform.athenahealth.com/fhir/r4` | Set `PracticeID` |
| `meditech` | client secret (JWT also works) | `https://{hospital-host}/v1/uscore/R4` | One host per hospital |
| `ecw` | signed JWT | `https://fhir4.eclinicalworks.com/fhir/r4/{practiceCode}` | Server-side only |
| `generic` | signed JWT | any US Core server | Standard parameters, SMART v2 scopes |

```go
ehr, err := acyclic.Connect(ctx, acyclic.Config{
	Vendor:        "epic",
	BaseURL:       os.Getenv("EHR_BASE_URL"),
	TokenURL:      "",                           // empty: read from {BaseURL}/.well-known/smart-configuration
	ClientID:      os.Getenv("EHR_CLIENT_ID"),
	PrivateKeyPEM: os.Getenv("EHR_PRIVATE_KEY"), // PKCS#8; PKCS#1 RSA and SEC1 EC PEM also parse
	KeyID:         os.Getenv("EHR_KEY_ID"),
	Alg:           "RS384",                      // or ES384
	// ClientSecret: os.Getenv("EHR_CLIENT_SECRET"), // client_secret_basic vendors
	// PracticeID:   "195900",                     // athena
	Write:     false, // true adds the DocumentReference write scope
	Telemetry: acyclic.JSONTelemetry(os.Stderr),
})
```

Other `Config` fields: `Scopes` (overrides the vendor default), `Auth` (overrides the auth method), `PrivateKey crypto.Signer` (in-memory key), `HTTPClient`, `Headers`, `Profile *acyclic.Vendor` (your own vendor profile), and `Inspect func(acyclic.InspectEvent)`, a local debugging hook that sees raw URLs and bodies. Keep `Inspect` out of production.

Keys and secrets stay in memory. Tokens are cached in memory, refreshed when less than 60 s remain, with one refresh in flight at a time.

## Tools

Every tool acts on the session's one patient and returns a small flat object ([projections](spec/projections.md)). Lists are `{"items":[...],"total":n,"truncated":bool}`.

| Tool | Returns |
|---|---|
| `find_patient` | Up to 10 matches by `identifier`, or `family` + `given` / `birthdate`. Only while unbound |
| `select_patient` | Binds the session to `patient_id`. Only while unbound |
| `get_patient` | Name, birth date, gender, MRN, deceased |
| `list_conditions` | Problem list; `status` `active` (default), `resolved`, `all` |
| `list_medications` | Medication orders; `status` `active` (default), `stopped`, `all` |
| `list_allergies` | Allergies with deduplicated reactions |
| `get_labs_vitals` | `category` `laboratory` or `vital-signs`, LOINC `code`, `since`, `limit` (20); newest first |
| `list_encounters` | Visits, newest first; `since`, `limit` (10) |
| `list_appointments` | Appointments from `since` (today) to `until`. athena: proprietary API. eCW: unavailable |
| `list_documents` | Note metadata, newest first; `since`, `limit` (10) |
| `read_document` | Plain text of one note, `max_chars` (4000); HTML reduced to text |
| `get_coverage` | Payer, plan, member id, period |
| `create_note` | Saves a text note. Needs `AllowWrites` and `Approve`. Epic, Oracle and generic only |

```go
sess := acyclic.NewSession(ehr, acyclic.SessionOptions{
	Patient:     patientID,
	AllowWrites: true,
	Approve: func(ctx context.Context, req acyclic.ApprovalRequest) bool {
		return askAHuman(req.Summary, req.Args["text"]) // true allows the write
	},
	PurposeOfUse:  "TREAT",           // audit field
	TelemetrySalt: os.Getenv("SALT"), // stable acyclic.patient_ref across processes
	OnToolResult:  func(name string, args map[string]any, result any) {}, // sees raw data
})
defs := sess.Definitions() // []acyclic.ToolDef{Name, Description, InputSchema} for any model API
res := sess.Call(ctx, "create_note", map[string]any{"title": "Visit summary", "text": "..."})
// res.OK, res.Result, res.Error, res.Code; res.JSON() is {"ok":...}
```

`Call` never returns a Go error. Failures come back as `{"ok":false,"error":"...","code":"..."}` with codes such as `no_patient`, `unknown_tool`, `writes_disabled`, `no_approver`, `declined`, `wrong_patient`, `budget`, an OperationOutcome issue code (`not-found`) or an HTTP status.

Models: `acyclic.OpenAICompatible` (OpenAI, Azure OpenAI, OpenRouter, vLLM, Ollama; `ExtraBody` is merged into each request), `acyclic.Anthropic`, and `acyclic.Bedrock` (Converse API, SigV4 signed in-package). Implement `acyclic.Model` (`Provider`, `Name`, `CoveredByBAA`, `Chat`) for anything else.

## Data policy and HIPAA

`RunAgent` refuses to start without `DataPolicy`:

- `acyclic.BAA`: real patient data. The model must be created with `CoveredByBAA: true`, which is your statement that a BAA covers it (for example Bedrock under an AWS BAA).
- `acyclic.Synthetic`: the FHIR host must match `acyclic.SandboxHosts` (`*.acyclic.sh`, Epic and Oracle public sandboxes, athena preview, localhost). Anything else errors.

Other safeguards:

- A session is bound to one patient. Results belonging to another patient are dropped, and `read_document` refuses another patient's note.
- Writes are off by default and each one needs `Approve` to return true. The default prompt tells the model not to retry a declined write.
- Budgets: `MaxSteps` 8, `MaxToolCalls` 20, `MaxTokens` 60000 total, `MaxOutputTokens` 4096 per call. `out.Stop` is `done`, `max_steps`, `max_tokens`, `max_tool_calls` or `output_truncated` (the last reply hit the output cap).
- `OnEvent` receives `model_request`, `model_response`, `tool_call`, `tool_result` and `done` events for a UI. Events carry raw data.

## Telemetry and OpenTelemetry

The SDK depends on no telemetry library. It calls this interface; span parents travel in the `context.Context`:

```go
type Telemetry interface {
	StartSpan(ctx context.Context, name string, attrs map[string]any) (context.Context, Span)
	Log(ctx context.Context, level, name string, attrs map[string]any)
	Audit(ctx context.Context, rec AuditRecord)
}
type Span interface { SetAttributes(map[string]any); End(err error) }
```

Built in: `acyclic.JSONTelemetry(w)` (JSON lines; `.Level`, `.NoAudit`), `acyclic.SlogTelemetry(logger)`, `acyclic.MultiTelemetry(a, b)`, `acyclic.NoTelemetry`.

Spans: `invoke_agent acyclic`, `chat {model}`, `execute_tool {tool}`, `{METHOD} {ResourceType}` per FHIR request, `POST token`. Logs: `fhir.retry`, `token.refresh`, `agent.step`, `agent.stop`, `write.denied`, `write.approved`. `Audit` gets one AuditEvent-shaped record per FHIR request (`time`, `action` C/R, `outcome` 0/4/8, `vendor`, `resource_type`, `patient_ref`, `agent_run`, `tool`, `purpose_of_use`, `client_id`, `status`) for your retention store.

**Allow-listed attributes** (the SDK drops every other key before your hook sees it; `acyclic.TelemetryKeys`): `gen_ai.operation.name`, `gen_ai.provider.name`, `gen_ai.request.model`, `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`, `gen_ai.response.finish_reasons`, `gen_ai.tool.name`, `gen_ai.tool.call.id`, `gen_ai.conversation.id`, `http.request.method`, `http.response.status_code`, `server.address`, `url.template`, `fhir.resource_type`, `fhir.result_count`, `acyclic.vendor`, `acyclic.patient_ref`, `acyclic.retry_count`, `acyclic.debug.arg_keys`, `acyclic.debug.response_bytes`, `error.type`.

**Never logged:** query strings and search values, request and response bodies, resource ids (patients appear only as `acyclic.patient_ref` = `"p_"` + 16 hex of HMAC-SHA256(salt, id)), prompts, model output, tool arguments and results, tokens, client assertions and keys. `Span.End` receives an error whose message is only its type (`429`, `OperationOutcome:not-found`, `transport`).

OTel adapter (put it in your code; it is not a dependency of this module):

```go
type otelTel struct{ tr trace.Tracer; audit func(acyclic.AuditRecord) }
type otelSpan struct{ s trace.Span }

func (o otelTel) StartSpan(ctx context.Context, name string, a map[string]any) (context.Context, acyclic.Span) {
	ctx, s := o.tr.Start(ctx, name, trace.WithAttributes(kvs(a)...))
	return ctx, otelSpan{s}
}
func (o otelTel) Log(ctx context.Context, level, name string, a map[string]any) {
	trace.SpanFromContext(ctx).AddEvent(name, trace.WithAttributes(append(kvs(a), attribute.String("level", level))...))
}
func (o otelTel) Audit(_ context.Context, r acyclic.AuditRecord) { o.audit(r) }
func (s otelSpan) SetAttributes(a map[string]any)              { s.s.SetAttributes(kvs(a)...) }
func (s otelSpan) End(err error) {
	if err != nil { s.s.SetStatus(codes.Error, err.Error()) }
	s.s.End()
}
func kvs(m map[string]any) (out []attribute.KeyValue) {
	for k, v := range m {
		switch x := v.(type) {
		case string: out = append(out, attribute.String(k, x))
		case int: out = append(out, attribute.Int(k, x))
		case []string: out = append(out, attribute.StringSlice(k, x))
		}
	}
	return out
}
```

Use it as `Telemetry: acyclic.MultiTelemetry(otelTel{otel.Tracer("acyclic"), sink}, acyclic.JSONTelemetry(os.Stderr))`.

## Persistence

Pass `Store` and `ConversationID` to `RunAgent`; the conversation is saved after every step and resumed on the next run with the same id.

```go
type ConversationStore interface {
	Load(ctx context.Context, id string) ([]acyclic.Message, error) // nil, nil when unknown
	Save(ctx context.Context, id string, messages []acyclic.Message) error
	Delete(ctx context.Context, id string) error
}
```

`acyclic.MemoryStore()` is in-process. `acyclic.SQLStore(db)` works over `database/sql` with Postgres placeholders; bring your own driver (`lib/pq`, `pgx/stdlib`) and create the table from `acyclic.PostgresSchema`:

```sql
create table if not exists acyclic_conversations (
  id text primary key,
  messages jsonb not null,
  updated timestamptz not null default now()
);
```

Messages use the same JSON shape as the JavaScript SDK (`role`, `content`, `toolCalls`, `toolCallId`, `name`). Stored conversations contain patient data: keep the database inside your HIPAA boundary, encrypt it at rest, set a retention period and support deletion.

## Why it's built this way

Each line is a common EHR-integration pitfall and how the SDK handles it:

- **Following `next` links to another host** would send the bearer token there; every URL is checked against the FHIR base origin first.
- **Logging search URLs** leaks names and birth dates; telemetry carries `url.template` (`/Patient/{id}`) and never a query string.
- **Go's `*url.Error` text includes the full URL**; transport errors keep only the underlying cause.
- **Dumping raw Bundles into the model** wastes 10 to 50 times the tokens and breaks HIPAA minimum necessary; tools return projections.
- **Trusting the server to scope results to one patient** fails when a server over-returns; each resource is checked against the bound patient.
- **Unbounded agent loops** burn money and rate limits; steps, tool calls and tokens all have hard caps.
- **Retrying non-idempotent POSTs** creates duplicate notes; only GETs retry.
- **Ignoring `Retry-After`** gets apps throttled harder; 429 and 503 wait as asked (capped at 30 s), else jittered exponential backoff, at most 3 retries.
- **Wildcard scopes** are rejected by Oracle, athena, MEDITECH and eCW; each resource scope is listed.
- **Parsing `expires_in` as an int only** breaks on athena, which sends a string; numbers and numeric strings both work.
- **Caching keys or tokens on disk** widens the blast radius of a leak; both stay in memory.
- **Putting OperationOutcome diagnostics in error messages** can echo search values; they stay on `FHIRError.Outcome`.
- **Letting telemetry hooks see everything** relies on every hook author; the allow-list runs inside the SDK and cannot be bypassed by a hook.
- **Writes without a person in the loop** are risky for clinical records; writes need `AllowWrites` and an `Approve` callback.

## Vendor quirks handled

- **Epic:** Observation searches send `code` or `category`, never both; Condition defaults to `category=problem-list-item`; DocumentReference to `category=clinical-note`; Appointment to `service-category=appointment`; `_sort` dropped; `create_note` files under the given or most recent encounter.
- **Oracle Health:** `code` wins over `category`; Appointment dates get a time (`T00:00:00Z`, `le...T23:59:59Z`); notes use `text/plain;charset=utf-8` and `context.period.end`; creates with an empty body take the id from `Location`.
- **athenahealth:** every search carries `ah-practice=Organization/a-1.Practice-{PracticeID}`; MedicationRequest gets `intent=order`; appointments come from `/v1/{practiceid}/patients/{id}/appointments`; no Appointment system scope plus the `athena/service/Athenanet.MDP.*` scope; no `create_note`.
- **MEDITECH:** SMART v2 scopes only; medication status `stopped` maps to `completed`; no `create_note`.
- **eClinicalWorks:** MedicationRequest gets `intent=order`; no Appointment API or scope; no `create_note`.
- **generic:** standard US Core parameters and SMART v2 scopes.

## Develop

```sh
go vet ./... && go test ./...                         # unit tests and spec projection fixtures
SANDBOX_URL=https://sandbox.acyclic.sh go test ./...  # plus end-to-end against all five vendor mocks
```

## License

Apache-2.0. Copyright 2026 Acyclic LLC. Not affiliated with or endorsed by Epic Systems, Oracle, athenahealth, MEDITECH or eClinicalWorks.
