package acyclic

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// A minimal in-memory database/sql driver that understands SQLStore's three
// statements, so the store is tested without a third-party driver.
type fakeDB struct {
	mu   sync.Mutex
	rows map[string]string
}

var theFakeDB = &fakeDB{rows: map[string]string{}}

func init() { sql.Register("acyclicfake", fakeDriver{}) }

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) { return fakeConn{}, nil }

type fakeConn struct{}

func (fakeConn) Prepare(q string) (driver.Stmt, error) { return fakeStmt{q}, nil }
func (fakeConn) Close() error                          { return nil }
func (fakeConn) Begin() (driver.Tx, error)             { return nil, errors.New("no tx") }

type fakeStmt struct{ q string }

func (fakeStmt) Close() error  { return nil }
func (fakeStmt) NumInput() int { return -1 }
func (s fakeStmt) Exec(args []driver.Value) (driver.Result, error) {
	theFakeDB.mu.Lock()
	defer theFakeDB.mu.Unlock()
	switch {
	case strings.HasPrefix(s.q, "insert into acyclic_conversations") && strings.Contains(s.q, "on conflict (id) do update"):
		theFakeDB.rows[args[0].(string)] = args[1].(string)
	case strings.HasPrefix(s.q, "delete from acyclic_conversations where id = $1"):
		delete(theFakeDB.rows, args[0].(string))
	default:
		return nil, errors.New("unexpected exec: " + s.q)
	}
	return driver.RowsAffected(1), nil
}
func (s fakeStmt) Query(args []driver.Value) (driver.Rows, error) {
	if s.q != "select messages from acyclic_conversations where id = $1" {
		return nil, errors.New("unexpected query: " + s.q)
	}
	theFakeDB.mu.Lock()
	defer theFakeDB.mu.Unlock()
	v, ok := theFakeDB.rows[args[0].(string)]
	return &fakeRows{v: v, has: ok}, nil
}

type fakeRows struct {
	v   string
	has bool
}

func (r *fakeRows) Columns() []string { return []string{"messages"} }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if !r.has {
		return io.EOF
	}
	r.has = false
	dest[0] = []byte(r.v)
	return nil
}

func TestStores(t *testing.T) {
	db, err := sql.Open("acyclicfake", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	for name, st := range map[string]ConversationStore{"memory": MemoryStore(), "sql": SQLStore(db)} {
		if got, err := st.Load(ctx, "nope"); err != nil || got != nil {
			t.Fatalf("%s: missing load = %v, %v", name, got, err)
		}
		msgs := []Message{{Role: "system", Content: "s"}, {Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "get_patient", Args: map[string]any{}}}}, {Role: "tool", ToolCallID: "c1", Name: "get_patient", Content: "{}"}}
		if err := st.Save(ctx, "c", msgs); err != nil {
			t.Fatal(err)
		}
		msgs[0].Content = "mutated" // the store keeps its own copy
		got, err := st.Load(ctx, "c")
		if err != nil || len(got) != 3 || got[0].Content != "s" || got[1].ToolCalls[0].ID != "c1" || got[2].ToolCallID != "c1" {
			t.Fatalf("%s: %+v %v", name, got, err)
		}
		if err := st.Delete(ctx, "c"); err != nil {
			t.Fatal(err)
		}
		if got, _ := st.Load(ctx, "c"); got != nil {
			t.Fatalf("%s: not deleted", name)
		}
	}
	if !strings.Contains(PostgresSchema, "messages jsonb not null") {
		t.Fatal("schema")
	}
}

func TestMessageJSONShapeMatchesJS(t *testing.T) {
	got := marshalJSON([]Message{{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "c1", Name: "x", Args: map[string]any{"a": 1}}}}, {Role: "tool", ToolCallID: "c1", Name: "x", Content: "{}"}})
	want := `[{"role":"assistant","content":"","toolCalls":[{"id":"c1","name":"x","args":{"a":1}}]},{"role":"tool","content":"{}","toolCallId":"c1","name":"x"}]`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestTelemetryAllowListAndParenting(t *testing.T) {
	var buf bytes.Buffer
	var slogBuf bytes.Buffer
	tel := MultiTelemetry(JSONTelemetry(&buf), SlogTelemetry(slog.New(slog.NewJSONHandler(&slogBuf, nil))))
	g := guardOf(tel)
	ctx, parent := g.startSpan(context.Background(), "invoke_agent acyclic", map[string]any{"gen_ai.operation.name": "invoke_agent", "url.full": "https://x/Patient?name=Okafor", "patient.name": "Okafor"})
	_, child := g.startSpan(ctx, "GET Patient", map[string]any{"fhir.resource_type": "Patient"})
	child.setAttributes(map[string]any{"http.response.status_code": 200, "db.query": "Okafor"})
	child.end(errors.New("FHIR 404 on Patient: Okafor"))
	parent.end(nil)
	g.log(ctx, "info", "agent.step", map[string]any{"prompt": "Okafor", "gen_ai.conversation.id": "run_1"})
	for name, out := range map[string]string{"json": buf.String(), "slog": slogBuf.String()} {
		if strings.Contains(out, "Okafor") || strings.Contains(out, "url.full") {
			t.Fatalf("%s: non-allow-listed data reached a hook:\n%s", name, out)
		}
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("%d lines: %s", len(lines), buf.String())
	}
	var childRec, parentRec map[string]any
	_ = jsonUnmarshal(lines[0], &childRec)
	_ = jsonUnmarshal(lines[1], &parentRec)
	if childRec["parent_span_id"] != parentRec["span_id"] || childRec["trace_id"] != parentRec["trace_id"] || childRec["status"] != "error" {
		t.Fatalf("parenting: %v / %v", childRec, parentRec)
	}
	// a panicking hook never breaks the SDK
	guardOf(panicTelemetry{}).log(context.Background(), "info", "x", nil)
	_, s := guardOf(panicTelemetry{}).startSpan(context.Background(), "x", nil)
	s.end(nil)
	if PatientRef("p1", "salt") != "p_4f99b169fd026476" || PatientRef("", "salt") != "" {
		t.Fatal(PatientRef("p1", "salt"))
	}
}

type panicTelemetry struct{}

func (panicTelemetry) StartSpan(context.Context, string, map[string]any) (context.Context, Span) {
	panic("boom")
}
func (panicTelemetry) Log(context.Context, string, string, map[string]any) { panic("boom") }
func (panicTelemetry) Audit(context.Context, AuditRecord)                  { panic("boom") }

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }
