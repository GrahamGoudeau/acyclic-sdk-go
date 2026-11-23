package acyclic

// Telemetry hooks. Rules: spec/telemetry.md. No OTel dependency: implement
// Telemetry (about 20 lines for an OTel adapter; see the README). The SDK
// filters every attribute through the allow-list before it reaches a hook,
// and recovers from panics in hooks so telemetry never breaks a request.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// Telemetry receives spans, logs and audit records. Parent spans travel in
// the context: StartSpan returns a context carrying the new span, and the SDK
// passes that context to child operations.
type Telemetry interface {
	StartSpan(ctx context.Context, name string, attrs map[string]any) (context.Context, Span)
	Log(ctx context.Context, level, name string, attrs map[string]any)
	Audit(ctx context.Context, rec AuditRecord)
}

// Span is one timed operation. End's error is a sanitized error whose
// message is only its type (for example "429" or "OperationOutcome:not-found").
type Span interface {
	SetAttributes(attrs map[string]any)
	End(err error)
}

// AuditRecord is one compliance record per FHIR request (spec/telemetry.md).
type AuditRecord struct {
	Time         string `json:"time"`
	Action       string `json:"action"`  // C create, R read or search
	Outcome      string `json:"outcome"` // 0 success, 4 4xx, 8 5xx or transport
	Vendor       string `json:"vendor"`
	ResourceType string `json:"resource_type"`
	PatientRef   string `json:"patient_ref,omitempty"`
	AgentRun     string `json:"agent_run,omitempty"`
	Tool         string `json:"tool,omitempty"`
	PurposeOfUse string `json:"purpose_of_use"`
	ClientID     string `json:"client_id"`
	Status       int    `json:"status"`
}

// TelemetryKeys is the attribute allow-list. No other key reaches a hook.
var TelemetryKeys = []string{
	"gen_ai.operation.name", "gen_ai.provider.name", "gen_ai.request.model",
	"gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens", "gen_ai.response.finish_reasons",
	"gen_ai.tool.name", "gen_ai.tool.call.id", "gen_ai.conversation.id",
	"http.request.method", "http.response.status_code", "server.address", "url.template",
	"fhir.resource_type", "fhir.result_count", "acyclic.vendor", "acyclic.patient_ref",
	"acyclic.retry_count", "acyclic.debug.arg_keys", "acyclic.debug.response_bytes", "error.type",
}

var allowed = func() map[string]bool {
	m := map[string]bool{}
	for _, k := range TelemetryKeys {
		m[k] = true
	}
	return m
}()

// cleanAttrs drops every key that is not allow-listed and every nil value.
func cleanAttrs(attrs map[string]any) map[string]any {
	out := make(map[string]any, len(attrs))
	for k, v := range attrs {
		if allowed[k] && v != nil {
			out[k] = v
		}
	}
	return out
}

// TelemetryError is the only error a Span's End receives: the error's type,
// never its message.
type TelemetryError struct{ Type string }

func (e TelemetryError) Error() string { return e.Type }

// --- internal guard: allow-list enforcement and panic safety ---

type guard struct{ t Telemetry }

func guardOf(t Telemetry) guard {
	if t == nil {
		t = NoTelemetry
	}
	return guard{t}
}

type guardSpan struct{ s Span }

func safe(f func()) {
	defer func() { _ = recover() }()
	f()
}

func (g guard) startSpan(ctx context.Context, name string, attrs map[string]any) (context.Context, guardSpan) {
	var out context.Context
	var span Span
	safe(func() { out, span = g.t.StartSpan(ctx, name, cleanAttrs(attrs)) })
	if out == nil {
		out = ctx
	}
	return out, guardSpan{span}
}

func (s guardSpan) setAttributes(attrs map[string]any) {
	if s.s != nil {
		safe(func() { s.s.SetAttributes(cleanAttrs(attrs)) })
	}
}

func (s guardSpan) end(err error) {
	if s.s == nil {
		return
	}
	var e error
	if err != nil {
		e = TelemetryError{errorType(err)}
	}
	safe(func() { s.s.End(e) })
}

func (g guard) log(ctx context.Context, level, name string, attrs map[string]any) {
	safe(func() { g.t.Log(ctx, level, name, cleanAttrs(attrs)) })
}

func (g guard) audit(ctx context.Context, rec AuditRecord) {
	safe(func() { g.t.Audit(ctx, rec) })
}

// --- NoTelemetry ---

type noTelemetry struct{}

type noSpan struct{}

func (noSpan) SetAttributes(map[string]any) {}
func (noSpan) End(error)                    {}

func (noTelemetry) StartSpan(ctx context.Context, _ string, _ map[string]any) (context.Context, Span) {
	return ctx, noSpan{}
}
func (noTelemetry) Log(context.Context, string, string, map[string]any) {}
func (noTelemetry) Audit(context.Context, AuditRecord)                  {}

// NoTelemetry discards everything. It is the default.
var NoTelemetry Telemetry = noTelemetry{}

var levels = map[string]int{"debug": 10, "info": 20, "warn": 30, "error": 40}

type spanIDs struct{ traceID, spanID string }

// --- JSON lines ---

// JSONSink writes one JSON object per line for each finished span, log event
// and audit record. Create it with JSONTelemetry.
type JSONSink struct {
	// Level is the minimum log level written: debug, info (default), warn, error.
	Level string
	// NoAudit drops audit records (send them to a separate sink instead).
	NoAudit bool

	mu sync.Mutex
	w  io.Writer
}

type jsonSpanKey struct{ s *JSONSink }

// JSONTelemetry writes JSON lines to w (os.Stderr, a file, a pipe).
func JSONTelemetry(w io.Writer) *JSONSink { return &JSONSink{w: w, Level: "info"} }

func (j *JSONSink) write(v any) {
	line := marshalJSON(v) + "\n"
	j.mu.Lock()
	defer j.mu.Unlock()
	_, _ = io.WriteString(j.w, line)
}

type jsonSpan struct {
	j      *JSONSink
	name   string
	ids    spanIDs
	parent string
	start  time.Time
	mu     sync.Mutex
	attrs  map[string]any
}

// StartSpan implements Telemetry.
func (j *JSONSink) StartSpan(ctx context.Context, name string, attrs map[string]any) (context.Context, Span) {
	parent, _ := ctx.Value(jsonSpanKey{j}).(spanIDs)
	ids := spanIDs{traceID: parent.traceID, spanID: randomID("")[:16]}
	if ids.traceID == "" {
		ids.traceID = randomID("")
	}
	a := map[string]any{}
	for k, v := range attrs {
		a[k] = v
	}
	s := &jsonSpan{j: j, name: name, ids: ids, parent: parent.spanID, start: time.Now(), attrs: a}
	return context.WithValue(ctx, jsonSpanKey{j}, ids), s
}

func (s *jsonSpan) SetAttributes(attrs map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range attrs {
		s.attrs[k] = v
	}
}

func (s *jsonSpan) End(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := "ok"
	if err != nil {
		status = "error"
	}
	rec := map[string]any{
		"kind": "span", "name": s.name, "trace_id": s.ids.traceID, "span_id": s.ids.spanID,
		"start": s.start.UTC().Format("2006-01-02T15:04:05.000Z"), "duration_ms": time.Since(s.start).Milliseconds(),
		"status": status, "attributes": s.attrs,
	}
	if s.parent != "" {
		rec["parent_span_id"] = s.parent
	}
	s.j.write(rec)
}

// Log implements Telemetry.
func (j *JSONSink) Log(_ context.Context, level, name string, attrs map[string]any) {
	min, ok := levels[j.Level]
	if !ok {
		min = 20
	}
	l, ok := levels[level]
	if !ok {
		l = 20
	}
	if l >= min {
		j.write(map[string]any{"kind": "log", "time": isoNow(), "level": level, "name": name, "attributes": attrs})
	}
}

// Audit implements Telemetry.
func (j *JSONSink) Audit(_ context.Context, rec AuditRecord) {
	if j.NoAudit {
		return
	}
	j.write(struct {
		Kind string `json:"kind"`
		AuditRecord
	}{"audit", rec})
}

// --- Multi ---

type multi struct{ list []Telemetry }

type multiKey struct{ m *multi }

// MultiTelemetry fans out to several sinks, for example an OTel adapter and
// JSONTelemetry. Each sink keeps its own span parenting.
func MultiTelemetry(list ...Telemetry) Telemetry { return &multi{list: list} }

type multiSpan []Span

func (ms multiSpan) SetAttributes(a map[string]any) {
	for _, s := range ms {
		if s != nil {
			s.SetAttributes(a)
		}
	}
}

func (ms multiSpan) End(err error) {
	for _, s := range ms {
		if s != nil {
			s.End(err)
		}
	}
}

func (m *multi) StartSpan(ctx context.Context, name string, attrs map[string]any) (context.Context, Span) {
	parts, _ := ctx.Value(multiKey{m}).([]context.Context)
	ctxs := make([]context.Context, len(m.list))
	spans := make(multiSpan, len(m.list))
	for i, t := range m.list {
		c := ctx
		if i < len(parts) && parts[i] != nil {
			c = parts[i]
		}
		a := make(map[string]any, len(attrs))
		for k, v := range attrs {
			a[k] = v
		}
		ctxs[i], spans[i] = t.StartSpan(c, name, a)
		if ctxs[i] == nil {
			ctxs[i] = c
		}
	}
	return context.WithValue(ctx, multiKey{m}, ctxs), spans
}

func (m *multi) Log(ctx context.Context, level, name string, attrs map[string]any) {
	for _, t := range m.list {
		t.Log(ctx, level, name, attrs)
	}
}

func (m *multi) Audit(ctx context.Context, rec AuditRecord) {
	for _, t := range m.list {
		t.Audit(ctx, rec)
	}
}

// --- slog ---

type slogTelemetry struct{ l *slog.Logger }

type slogSpanKey struct{ l *slog.Logger }

// SlogTelemetry writes spans (when they end), logs and audit records to a
// log/slog logger. Audit records are logged at Info with message "audit".
func SlogTelemetry(l *slog.Logger) Telemetry { return &slogTelemetry{l} }

func slogAttrs(attrs map[string]any) []any {
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, slog.Any(k, attrs[k]))
	}
	return out
}

type slogSpan struct {
	t      *slogTelemetry
	ctx    context.Context
	name   string
	ids    spanIDs
	parent string
	start  time.Time
	mu     sync.Mutex
	attrs  map[string]any
}

func (s *slogTelemetry) StartSpan(ctx context.Context, name string, attrs map[string]any) (context.Context, Span) {
	parent, _ := ctx.Value(slogSpanKey{s.l}).(spanIDs)
	ids := spanIDs{traceID: parent.traceID, spanID: randomID("")[:16]}
	if ids.traceID == "" {
		ids.traceID = randomID("")
	}
	a := map[string]any{}
	for k, v := range attrs {
		a[k] = v
	}
	out := context.WithValue(ctx, slogSpanKey{s.l}, ids)
	return out, &slogSpan{t: s, ctx: ctx, name: name, ids: ids, parent: parent.spanID, start: time.Now(), attrs: a}
}

func (s *slogSpan) SetAttributes(attrs map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range attrs {
		s.attrs[k] = v
	}
}

func (s *slogSpan) End(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	level, status := slog.LevelInfo, "ok"
	if err != nil {
		level, status = slog.LevelWarn, "error"
	}
	args := []any{
		slog.String("span", s.name), slog.String("trace_id", s.ids.traceID), slog.String("span_id", s.ids.spanID),
		slog.String("parent_span_id", s.parent), slog.Int64("duration_ms", time.Since(s.start).Milliseconds()),
		slog.String("status", status), slog.Group("attributes", slogAttrs(s.attrs)...),
	}
	s.t.l.Log(s.ctx, level, "span", args...)
}

func (s *slogTelemetry) Log(ctx context.Context, level, name string, attrs map[string]any) {
	l := slog.LevelInfo
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	}
	s.l.Log(ctx, l, name, slog.Group("attributes", slogAttrs(attrs)...))
}

func (s *slogTelemetry) Audit(ctx context.Context, r AuditRecord) {
	s.l.LogAttrs(ctx, slog.LevelInfo, "audit",
		slog.String("time", r.Time), slog.String("action", r.Action), slog.String("outcome", r.Outcome),
		slog.String("vendor", r.Vendor), slog.String("resource_type", r.ResourceType), slog.String("patient_ref", r.PatientRef),
		slog.String("agent_run", r.AgentRun), slog.String("tool", r.Tool), slog.String("purpose_of_use", r.PurposeOfUse),
		slog.String("client_id", r.ClientID), slog.Int("status", r.Status))
}

// --- patient_ref ---

var (
	processSaltOnce sync.Once
	processSalt     string
)

// PatientRef is HMAC-SHA256(salt, id) as "p_" + 16 hex characters. With an
// empty salt it uses a random per-process salt, so refs then match within
// one process only. Returns "" for an empty id.
func PatientRef(id, salt string) string {
	if id == "" {
		return ""
	}
	if salt == "" {
		processSaltOnce.Do(func() { processSalt = randomID("") })
		salt = processSalt
	}
	m := hmac.New(sha256.New, []byte(salt))
	m.Write([]byte(id))
	return "p_" + hex.EncodeToString(m.Sum(nil)[:8])
}
