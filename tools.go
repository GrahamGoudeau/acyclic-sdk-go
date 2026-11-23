package acyclic

// The tool layer: JSON Schema definitions plus handlers bound to one session
// and one patient. Works without the agent loop: Definitions() for any model
// API, Call(ctx, name, args) to execute.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ToolError is a tool-level failure returned to the model as {error, code}.
type ToolError struct {
	Message string
	Type    string
}

func (e *ToolError) Error() string     { return e.Message }
func (e *ToolError) ErrorType() string { return e.Type }

func toolErr(msg, typ string) error { return &ToolError{Message: msg, Type: typ} }

// ToolResult is the outcome of one tool call.
type ToolResult struct {
	OK     bool
	Result any
	Error  string
	Code   string
}

// MarshalJSON renders {"ok":true,"result":...} or {"ok":false,"error":...,"code":...}.
func (r ToolResult) MarshalJSON() ([]byte, error) {
	if r.OK {
		return []byte(marshalJSON(struct {
			OK     bool `json:"ok"`
			Result any  `json:"result"`
		}{true, r.Result})), nil
	}
	return []byte(marshalJSON(struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Code  string `json:"code,omitempty"`
	}{false, r.Error, r.Code})), nil
}

// JSON is the result as a JSON string.
func (r ToolResult) JSON() string {
	b, _ := r.MarshalJSON()
	return string(b)
}

// modelContent is what the agent loop sends back to the model.
func (r ToolResult) modelContent() string {
	if r.OK {
		return marshalJSON(r.Result)
	}
	return marshalJSON(struct {
		Error string `json:"error"`
		Code  string `json:"code,omitempty"`
	}{r.Error, r.Code})
}

// ToolDef is a tool definition for a model API.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ApprovalRequest asks a person to approve a write.
type ApprovalRequest struct {
	Tool    string
	Args    map[string]any
	Summary string
}

// SessionOptions configure NewSession.
type SessionOptions struct {
	// Patient binds the session now. Empty: the model must call
	// find_patient then select_patient.
	Patient string
	// AllowWrites enables create_note (it also needs Approve).
	AllowWrites bool
	// Approve is asked before every write. Required for writes.
	Approve func(ctx context.Context, req ApprovalRequest) bool
	// PurposeOfUse is the audit field; default "TREAT".
	PurposeOfUse string
	// TelemetrySalt is the HMAC salt for acyclic.patient_ref.
	TelemetrySalt string
	// RunID is the audit agent_run. RunAgent sets it when empty.
	RunID string
	// OnToolResult is your own hook. It sees raw data; the SDK does not log it.
	OnToolResult func(name string, args map[string]any, result any)
}

// Session is one patient-bound tool session. Safe for concurrent calls.
type Session struct {
	ehr   *EHR
	opts  SessionOptions
	tel   guard
	mu    sync.Mutex
	pid   string
	pref  string
	runID string
}

// NewSession binds a tool session to an EHR connection.
func NewSession(ehr *EHR, opts SessionOptions) *Session {
	if opts.PurposeOfUse == "" {
		opts.PurposeOfUse = "TREAT"
	}
	s := &Session{ehr: ehr, opts: opts, tel: guardOf(ehr.Telemetry), pid: opts.Patient, runID: opts.RunID}
	if s.pid != "" {
		s.pref = PatientRef(s.pid, opts.TelemetrySalt)
	}
	return s
}

// PatientID is the bound patient, or "".
func (s *Session) PatientID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pid
}

func (s *Session) state() (pid, pref, runID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pid, s.pref, s.runID
}

func (s *Session) setRunIDIfEmpty(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runID == "" {
		s.runID = id
	}
}

func (s *Session) need() (string, error) {
	pid, _, _ := s.state()
	if pid == "" {
		return "", toolErr("no patient selected; call find_patient then select_patient", "no_patient")
	}
	return pid, nil
}

func (s *Session) audit(ctx context.Context, action, resourceType string, status int, tool string) {
	pid, pref, runID := s.state()
	if pref == "" && pid != "" {
		pref = PatientRef(pid, s.opts.TelemetrySalt)
	}
	outcome := "8" // 5xx, and transport failures (status 0)
	switch {
	case status == 0:
	case status < 400:
		outcome = "0"
	case status < 500:
		outcome = "4"
	}
	s.tel.audit(ctx, AuditRecord{
		Time: isoNow(), Action: action, Outcome: outcome, Vendor: s.ehr.Vendor.Name, ResourceType: resourceType,
		PatientRef: pref, AgentRun: runID, Tool: tool, PurposeOfUse: s.opts.PurposeOfUse, ClientID: s.ehr.ClientID, Status: status,
	})
}

func statusOf(err error) int {
	var fe *FHIRError
	if errors.As(err, &fe) {
		return fe.Status
	}
	var ae *AuthError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

// belongsTo keeps one session on one patient even if a server returns more
// than it was asked for.
func belongsTo(r map[string]any, pid string) bool {
	ref := first(get(r, "subject", "reference"), get(r, "patient", "reference"), get(r, "beneficiary", "reference"))
	if ref == nil {
		for _, p := range asArr(r["participant"]) {
			if strings.HasPrefix(str(get(p, "actor", "reference")), "Patient/") {
				ref = get(p, "actor", "reference")
				break
			}
		}
	}
	rs := str(ref)
	return !truthy(ref) || rs == "Patient/"+pid || strings.HasSuffix(rs, "/Patient/"+pid)
}

func (s *Session) search(ctx context.Context, resourceType string, params Params, limit int, tool string) (*SearchResult, error) {
	pid, err := s.need()
	if err != nil {
		return nil, err
	}
	cp := Params{}
	for k, v := range params {
		cp[k] = v
	}
	p := s.ehr.Vendor.searchParams(resourceType, cp, s.ehr)
	out, err := s.ehr.FHIR.Search(ctx, resourceType, p, limit)
	if err != nil {
		s.audit(ctx, "R", resourceType, statusOf(err), tool)
		return nil, err
	}
	s.audit(ctx, "R", resourceType, 200, tool)
	kept := out.Resources[:0:0]
	for _, r := range out.Resources {
		if belongsTo(r, pid) {
			kept = append(kept, r)
		}
	}
	out.Resources = kept
	return out, nil
}

func list(items []map[string]any, truncated bool) map[string]any {
	if items == nil {
		items = []map[string]any{}
	}
	return map[string]any{"items": items, "total": len(items), "truncated": truncated}
}

func project(rs []map[string]any, f func(map[string]any) map[string]any) []map[string]any {
	out := make([]map[string]any, len(rs))
	for i, r := range rs {
		out[i] = f(r)
	}
	return out
}

// sortDesc sorts newest first by key; missing values sort last; stable.
func sortDesc(items []map[string]any, key string) {
	sort.SliceStable(items, func(i, j int) bool {
		x, xok := items[i][key].(string)
		y, yok := items[j][key].(string)
		if !xok {
			return false
		}
		if !yok {
			return true
		}
		return x > y
	})
}

func dateGe(since string) any {
	if since == "" {
		return nil
	}
	return "ge" + since
}

// argStr reads a string argument ("" when absent).
func argStr(a map[string]any, k string) string {
	v := a[k]
	if v == nil {
		return ""
	}
	return jsString(v)
}

// argOpt is a present argument as a string, or nil.
func argOpt(a map[string]any, k string) any {
	if a[k] == nil {
		return nil
	}
	return jsString(a[k])
}

func argInt(a map[string]any, k string, def int) int {
	if f, ok := toNumber(a[k]); ok && a[k] != nil {
		return int(f)
	}
	return def
}

func argDefault(a map[string]any, k, def string) string {
	if a[k] == nil {
		return def
	}
	return jsString(a[k])
}

type handler func(ctx context.Context, s *Session, a map[string]any) (any, error)

var handlers = map[string]handler{
	"find_patient": func(ctx context.Context, s *Session, a map[string]any) (any, error) {
		if !truthy(a["identifier"]) && !truthy(a["family"]) {
			return nil, toolErr("give identifier, or family name (with given and/or birthdate)", "bad_args")
		}
		params := s.ehr.Vendor.searchParams("Patient", Params{
			"identifier": argOpt(a, "identifier"), "family": argOpt(a, "family"), "given": argOpt(a, "given"), "birthdate": argOpt(a, "birthdate"),
		}, s.ehr)
		out, err := s.ehr.FHIR.Search(ctx, "Patient", params, 10)
		if err != nil {
			return nil, err
		}
		return list(project(out.Resources, ProjectPatient), out.Truncated), nil
	},
	"select_patient": func(ctx context.Context, s *Session, a map[string]any) (any, error) {
		if pid, _, _ := s.state(); pid != "" {
			return nil, toolErr("session already bound to a patient", "already_bound")
		}
		r, err := s.ehr.FHIR.Read(ctx, "Patient", argStr(a, "patient_id"))
		if err != nil {
			return nil, err
		}
		id := str(r["id"])
		s.mu.Lock()
		if s.pid != "" {
			s.mu.Unlock()
			return nil, toolErr("session already bound to a patient", "already_bound")
		}
		s.pid = id
		s.pref = PatientRef(id, s.opts.TelemetrySalt)
		s.mu.Unlock()
		s.audit(ctx, "R", "Patient", 200, "select_patient")
		return ProjectPatient(r), nil
	},
	"get_patient": func(ctx context.Context, s *Session, a map[string]any) (any, error) {
		pid, err := s.need()
		if err != nil {
			return nil, err
		}
		r, err := s.ehr.FHIR.Read(ctx, "Patient", pid)
		if err != nil {
			return nil, err
		}
		s.audit(ctx, "R", "Patient", 200, "get_patient")
		return ProjectPatient(r), nil
	},
	"list_conditions": func(ctx context.Context, s *Session, a map[string]any) (any, error) {
		pid, err := s.need()
		if err != nil {
			return nil, err
		}
		st := argDefault(a, "status", "active")
		var cs any = st
		if st == "all" {
			cs = nil
		}
		out, err := s.search(ctx, "Condition", Params{"patient": pid, "clinical-status": cs}, 100, "list_conditions")
		if err != nil {
			return nil, err
		}
		return list(project(out.Resources, ProjectCondition), out.Truncated), nil
	},
	"list_medications": func(ctx context.Context, s *Session, a map[string]any) (any, error) {
		pid, err := s.need()
		if err != nil {
			return nil, err
		}
		st := argDefault(a, "status", "active")
		var ms any = st
		if st == "all" {
			ms = nil
		}
		out, err := s.search(ctx, "MedicationRequest", Params{"patient": pid, "status": ms}, 100, "list_medications")
		if err != nil {
			return nil, err
		}
		return list(project(out.Resources, ProjectMedication), out.Truncated), nil
	},
	"list_allergies": func(ctx context.Context, s *Session, a map[string]any) (any, error) {
		pid, err := s.need()
		if err != nil {
			return nil, err
		}
		out, err := s.search(ctx, "AllergyIntolerance", Params{"patient": pid}, 100, "list_allergies")
		if err != nil {
			return nil, err
		}
		return list(project(out.Resources, ProjectAllergy), out.Truncated), nil
	},
	"get_labs_vitals": func(ctx context.Context, s *Session, a map[string]any) (any, error) {
		pid, err := s.need()
		if err != nil {
			return nil, err
		}
		var code any
		if truthy(a["code"]) {
			code = "http://loinc.org|" + argStr(a, "code")
		}
		out, err := s.search(ctx, "Observation", Params{
			"patient": pid, "category": argDefault(a, "category", "laboratory"), "code": code,
			"date": dateGe(argStr(a, "since")), "_sort": "-date",
		}, argInt(a, "limit", 20), "get_labs_vitals")
		if err != nil {
			return nil, err
		}
		items := project(out.Resources, ProjectObservation)
		sortDesc(items, "effective")
		return list(items, out.Truncated), nil
	},
	"list_encounters": func(ctx context.Context, s *Session, a map[string]any) (any, error) {
		pid, err := s.need()
		if err != nil {
			return nil, err
		}
		out, err := s.search(ctx, "Encounter", Params{"patient": pid, "date": dateGe(argStr(a, "since")), "_sort": "-date"}, argInt(a, "limit", 10), "list_encounters")
		if err != nil {
			return nil, err
		}
		items := project(out.Resources, ProjectEncounter)
		sortDesc(items, "start")
		return list(items, out.Truncated), nil
	},
	"list_appointments": func(ctx context.Context, s *Session, a map[string]any) (any, error) {
		pid, err := s.need()
		if err != nil {
			return nil, err
		}
		since := argDefault(a, "since", today())
		date := []string{"ge" + since}
		if until := argStr(a, "until"); until != "" {
			date = append(date, "le"+until)
		}
		out, err := s.search(ctx, "Appointment", Params{"patient": pid, "date": date}, 50, "list_appointments")
		if err != nil {
			return nil, err
		}
		items := project(out.Resources, ProjectAppointment)
		startOf := func(m map[string]any) string {
			if v, ok := m["start"].(string); ok {
				return v
			}
			return "~"
		}
		sort.SliceStable(items, func(i, j int) bool { return startOf(items[i]) < startOf(items[j]) })
		return list(items, out.Truncated), nil
	},
	"list_documents": func(ctx context.Context, s *Session, a map[string]any) (any, error) {
		pid, err := s.need()
		if err != nil {
			return nil, err
		}
		out, err := s.search(ctx, "DocumentReference", Params{"patient": pid, "date": dateGe(argStr(a, "since")), "_sort": "-date"}, argInt(a, "limit", 10), "list_documents")
		if err != nil {
			return nil, err
		}
		items := project(out.Resources, ProjectDocument)
		sortDesc(items, "date")
		return list(items, out.Truncated), nil
	},
	"read_document": readDocument,
	"get_coverage": func(ctx context.Context, s *Session, a map[string]any) (any, error) {
		pid, err := s.need()
		if err != nil {
			return nil, err
		}
		out, err := s.search(ctx, "Coverage", Params{"patient": pid}, 20, "get_coverage")
		if err != nil {
			return nil, err
		}
		return list(project(out.Resources, ProjectCoverage), out.Truncated), nil
	},
	"create_note": createNote,
}

var (
	reTextContent = regexp.MustCompile(`text/(plain|html)`)
	reJSONType    = regexp.MustCompile(`json`)
	reDocLocation = regexp.MustCompile(`DocumentReference/([^/]+)`)
)

func readDocument(ctx context.Context, s *Session, a map[string]any) (any, error) {
	pid, err := s.need()
	if err != nil {
		return nil, err
	}
	doc, err := s.ehr.FHIR.Read(ctx, "DocumentReference", argStr(a, "document_id"))
	if err != nil {
		return nil, err
	}
	if !belongsTo(doc, pid) {
		s.audit(ctx, "R", "DocumentReference", 403, "read_document")
		return nil, toolErr("document belongs to a different patient", "wrong_patient")
	}
	s.audit(ctx, "R", "DocumentReference", 200, "read_document")
	var att map[string]any
	for _, c := range asArr(doc["content"]) {
		x := asMap(get(c, "attachment"))
		if reTextContent.MatchString(str(get(x, "contentType"))) {
			att = x
			break
		}
	}
	if att == nil {
		att = asMap(get(index0(doc["content"]), "attachment"))
	}
	if att == nil {
		return nil, toolErr("document has no content", "no_content")
	}
	var text string
	ct := str(att["contentType"])
	switch {
	case truthy(att["data"]):
		if text, err = base64ToText(str(att["data"])); err != nil {
			return nil, toolErr("attachment data is not valid base64", "bad_content")
		}
	case truthy(att["url"]):
		accept := s.ehr.Vendor.BinaryAccept
		if accept == "" {
			accept = "application/fhir+json"
		}
		bin, err := s.ehr.FHIR.Do(ctx, "GET", str(att["url"]), RequestOptions{Accept: accept, Raw: true, ResourceType: "Binary"})
		if err != nil {
			return nil, err
		}
		s.audit(ctx, "R", "Binary", bin.Status, "read_document")
		if reJSONType.MatchString(bin.ContentType) {
			var b map[string]any
			if err := json.Unmarshal([]byte(bin.Text), &b); err != nil {
				return nil, toolErr("Binary response is not valid JSON", "bad_content")
			}
			if truthy(b["data"]) {
				if text, err = base64ToText(str(b["data"])); err != nil {
					return nil, toolErr("Binary data is not valid base64", "bad_content")
				}
			}
			if b["contentType"] != nil {
				ct = str(b["contentType"])
			}
		} else {
			text = bin.Text
			if bin.ContentType != "" {
				ct = bin.ContentType
			}
		}
	default:
		return nil, toolErr("attachment has neither data nor url", "no_content")
	}
	check := ct
	if check == "" {
		check = "text/plain"
	}
	if !strings.HasPrefix(check, "text/") {
		return nil, toolErr("unsupported content type "+ct, "unsupported_content")
	}
	return ProjectDocumentText(doc, text, ct, argInt(a, "max_chars", 4000)), nil
}

func createNote(ctx context.Context, s *Session, a map[string]any) (any, error) {
	pid, err := s.need()
	if err != nil {
		return nil, err
	}
	if !s.opts.AllowWrites {
		s.tel.log(ctx, "warn", "write.denied", map[string]any{"gen_ai.tool.name": "create_note"})
		return nil, toolErr("writes are disabled for this session", "writes_disabled")
	}
	if s.opts.Approve == nil {
		return nil, toolErr("writes need an approve callback", "no_approver")
	}
	title, tok := a["title"].(string)
	text, xok := a["text"].(string)
	if !tok || !xok {
		return nil, toolErr("create_note needs title and text strings", "bad_args")
	}
	summary := "Save note \"" + title + "\" (" + strconv.Itoa(utf16Len(text)) + " chars) to the chart"
	if !s.opts.Approve(ctx, ApprovalRequest{Tool: "create_note", Args: a, Summary: summary}) {
		s.tel.log(ctx, "info", "write.denied", map[string]any{"gen_ai.tool.name": "create_note"})
		return nil, toolErr("a person declined this write", "declined")
	}
	s.tel.log(ctx, "info", "write.approved", map[string]any{"gen_ai.tool.name": "create_note"})
	var resource map[string]any
	if s.ehr.Vendor.PrepareNote != nil {
		if resource, err = s.ehr.Vendor.PrepareNote(ctx, pid, a, s.ehr); err != nil {
			return nil, err
		}
	} else {
		resource = defaultNote(pid, a)
	}
	created, err := s.ehr.FHIR.Create(ctx, "DocumentReference", resource)
	if err != nil {
		s.audit(ctx, "C", "DocumentReference", statusOf(err), "create_note")
		return nil, err
	}
	s.audit(ctx, "C", "DocumentReference", 201, "create_note")
	merged := map[string]any{}
	for k, v := range resource {
		merged[k] = v
	}
	for k, v := range created.JSON {
		merged[k] = v
	}
	id := created.JSON["id"]
	if id == nil {
		if m := reDocLocation.FindStringSubmatch(created.Location); m != nil {
			id = m[1]
		}
	}
	merged["id"] = id
	return ProjectDocument(merged), nil
}

func defaultNote(pid string, a map[string]any) map[string]any {
	var extra map[string]any
	if truthy(a["encounter_id"]) {
		extra = map[string]any{"context": map[string]any{"encounter": encounterRef(jsString(a["encounter_id"]))}}
	}
	return note(pid, a, noteOpts{extra: extra})
}

// Definitions lists the tools for a model API. Unbound-only tools vanish
// once a patient is bound; write tools appear only when writes are allowed;
// tools the vendor does not offer are left out.
func (s *Session) Definitions() []ToolDef {
	pid, _, _ := s.state()
	var out []ToolDef
	for _, t := range ToolSpecs {
		if t.Kind == "write" && !s.opts.AllowWrites {
			continue
		}
		if t.UnboundOnly && pid != "" {
			continue
		}
		if !s.ehr.Vendor.supports(t.Name) {
			continue
		}
		out = append(out, ToolDef{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	return out
}

type callIDKey struct{}

// WithToolCallID tags a context so Call records gen_ai.tool.call.id.
func WithToolCallID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, callIDKey{}, id)
}

// Call runs one tool. It never returns a Go error: failures come back as
// ToolResult{OK: false, Error, Code} for the model to read.
func (s *Session) Call(ctx context.Context, name string, args map[string]any) ToolResult {
	if args == nil {
		args = map[string]any{}
	}
	_, pref, _ := s.state()
	attrs := map[string]any{
		"gen_ai.operation.name": "execute_tool", "gen_ai.tool.name": name, "acyclic.vendor": s.ehr.Vendor.Name,
	}
	if id, _ := ctx.Value(callIDKey{}).(string); id != "" {
		attrs["gen_ai.tool.call.id"] = id
	}
	if pref != "" {
		attrs["acyclic.patient_ref"] = pref
	}
	ctx, span := s.tel.startSpan(ctx, "execute_tool "+name, attrs)
	result, err := s.run(ctx, name, args)
	if err != nil {
		span.setAttributes(map[string]any{"error.type": errorType(err)})
		span.end(err)
		return ToolResult{OK: false, Error: err.Error(), Code: errorCode(err)}
	}
	if _, pref, _ := s.state(); pref != "" {
		span.setAttributes(map[string]any{"acyclic.patient_ref": pref})
	}
	if m, ok := result.(map[string]any); ok {
		if items, ok := m["items"].([]map[string]any); ok {
			span.setAttributes(map[string]any{"fhir.result_count": len(items)})
		}
	}
	if s.opts.OnToolResult != nil {
		func() { defer func() { _ = recover() }(); s.opts.OnToolResult(name, args, result) }()
	}
	span.end(nil)
	return ToolResult{OK: true, Result: result}
}

func (s *Session) run(ctx context.Context, name string, args map[string]any) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			result, err = nil, toolErr(fmt.Sprintf("tool %s failed", name), "tool_error")
		}
	}()
	var h handler
	if override := s.ehr.Vendor.Tools[name]; override != nil {
		h = func(ctx context.Context, s *Session, a map[string]any) (any, error) {
			pid, err := s.need()
			if err != nil {
				return nil, err
			}
			out, err := override(ctx, a, ToolContext{EHR: s.ehr, PatientID: pid})
			if err != nil {
				return nil, err
			}
			rt := "Unknown"
			if name == "list_appointments" {
				rt = "Appointment"
			}
			s.audit(ctx, "R", rt, 200, name)
			return out, nil
		}
	} else {
		h = handlers[name]
	}
	available := false
	for _, d := range s.Definitions() {
		if d.Name == name {
			available = true
			break
		}
	}
	if h == nil || !available {
		return nil, toolErr("tool "+name+" is not available", "unknown_tool")
	}
	return h(ctx, s, args)
}
