package acyclic

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestSearchPagingAndPatientFilter(t *testing.T) {
	f := newFakeFHIR(t)
	rec := &recorder{}
	ehr := connectFake(t, f, "generic", rec)
	s := NewSession(ehr, SessionOptions{Patient: "p1"})
	res := s.Call(context.Background(), "get_labs_vitals", map[string]any{"limit": 40, "code": "4548-4"})
	if !res.OK {
		t.Fatal(res.Error)
	}
	m := res.Result.(map[string]any)
	items := m["items"].([]map[string]any)
	if len(items) != 6 || m["total"] != 6 || m["truncated"] != false {
		t.Fatalf("got %d items: %s", len(items), res.JSON())
	}
	if items[0]["effective"] != "2026-06-01" {
		t.Fatalf("not newest first: %v", items[0])
	}
	for _, it := range items {
		if it["id"] == "x" {
			t.Fatal("cross-patient entry leaked")
		}
	}
	// limit truncation
	res = s.Call(context.Background(), "get_labs_vitals", map[string]any{"limit": 2})
	m = res.Result.(map[string]any)
	if len(m["items"].([]map[string]any)) != 2 || m["truncated"] != true {
		t.Fatalf("truncation: %s", res.JSON())
	}
	tel := rec.all()
	for _, bad := range []string{"Okafor", "4548-4", "loinc", "page=2", "p1\"", "Patient/p1"} {
		if strings.Contains(tel, bad) {
			t.Errorf("telemetry contains %q:\n%s", bad, tel)
		}
	}
	if !strings.Contains(tel, `"url.template":"/fhir/Observation"`) {
		t.Errorf("no url.template: %s", tel)
	}
}

func TestOperationOutcomeErrorHidesDiagnostics(t *testing.T) {
	f := newFakeFHIR(t)
	ehr := connectFake(t, f, "generic", nil)
	_, err := ehr.FHIR.Read(context.Background(), "Patient", "nobody-1")
	var fe *FHIRError
	if !errors.As(err, &fe) || fe.Status != 404 || fe.Code != "not-found" || fe.Type != "OperationOutcome:not-found" {
		t.Fatalf("%#v", err)
	}
	if strings.Contains(err.Error(), "Okafor") || err.Error() != "FHIR 404 on Patient (not-found)" {
		t.Fatalf("message: %s", err)
	}
	s := NewSession(ehr, SessionOptions{})
	r := s.Call(context.Background(), "select_patient", map[string]any{"patient_id": "nobody-1"})
	if r.OK || r.Code != "not-found" {
		t.Fatalf("%s", r.JSON())
	}
}

func TestCrossOriginNextLinkRefused(t *testing.T) {
	f := newFakeFHIR(t)
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("credentials sent cross-origin: %s", r.Header.Get("Authorization"))
	}))
	defer evil.Close()
	f.handle = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/Condition") {
			writeJSONT(w, 200, bundleNext(evil.URL+"/fhir/Condition?page=2", map[string]any{"resourceType": "Condition", "id": "c1"}))
			return true
		}
		return false
	}
	ehr := connectFake(t, f, "generic", nil)
	s := NewSession(ehr, SessionOptions{Patient: "p1"})
	r := s.Call(context.Background(), "list_conditions", nil)
	if r.OK || !strings.Contains(r.Error, "refusing to send credentials") {
		t.Fatalf("%s", r.JSON())
	}
}

func TestRetryGETOnlyAnd401Refresh(t *testing.T) {
	f := newFakeFHIR(t)
	var mu sync.Mutex
	calls := map[string]int{}
	f.handle = func(w http.ResponseWriter, r *http.Request) bool {
		mu.Lock()
		defer mu.Unlock()
		key := r.Method + " " + r.URL.Path
		calls[key]++
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/Coverage") && calls[key] <= 2:
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			return true
		case r.Method == "POST":
			w.WriteHeader(503)
			return true
		case strings.HasSuffix(r.URL.Path, "/AllergyIntolerance") && calls[key] == 1:
			w.WriteHeader(401)
			return true
		case r.Method == "GET":
			writeJSONT(w, 200, bundleNext(""))
			return true
		}
		return false
	}
	ehr := connectFake(t, f, "generic", nil)
	ctx := context.Background()
	if _, err := ehr.FHIR.Search(ctx, "Coverage", Params{"patient": "p1"}, 10); err != nil {
		t.Fatal(err)
	}
	if calls["GET /fhir/Coverage"] != 3 {
		t.Fatalf("coverage calls %d", calls["GET /fhir/Coverage"])
	}
	_, err := ehr.FHIR.Create(ctx, "DocumentReference", map[string]any{"resourceType": "DocumentReference"})
	if err == nil || calls["POST /fhir/DocumentReference"] != 1 {
		t.Fatalf("POST retried: %d %v", calls["POST /fhir/DocumentReference"], err)
	}
	before := f.tokens.Load()
	if _, err := ehr.FHIR.Search(ctx, "AllergyIntolerance", Params{"patient": "p1"}, 10); err != nil {
		t.Fatal(err)
	}
	if f.tokens.Load() != before+1 {
		t.Fatalf("401 did not force one refresh: %d -> %d", before, f.tokens.Load())
	}
}

func TestTokenCacheSingleFlightAndStringExpiry(t *testing.T) {
	f := newFakeFHIR(t)
	f.expiresIn = "3600" // a string, as some servers send
	ehr := connectFake(t, f, "generic", nil)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ehr.FHIR.Read(context.Background(), "Patient", "p1"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := f.tokens.Load(); n != 1 {
		t.Fatalf("%d token requests, want 1", n)
	}
	// 30 s left is inside the 60 s refresh window.
	f.expiresIn = 30
	ehr2 := connectFake(t, f, "generic", nil)
	_, _ = ehr2.FHIR.Read(context.Background(), "Patient", "p1")
	_, _ = ehr2.FHIR.Read(context.Background(), "Patient", "p1")
	if n := f.tokens.Load(); n != 3 {
		t.Fatalf("%d token requests, want 3", n)
	}
	for _, c := range []struct {
		in   any
		want float64
	}{{"3600", 3600}, {3600.0, 3600}, {"", 300}, {"abc", 300}, {nil, 300}, {0.0, 300}, {" 60 ", 60}} {
		if got := expiresIn(c.in); got != c.want {
			t.Errorf("expiresIn(%v) = %v", c.in, got)
		}
	}
}

func TestAuthErrorDropsDescription(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONT(w, 401, map[string]any{"error": "invalid_client", "error_description": "client Okafor unknown"})
	}))
	defer srv.Close()
	ehr, err := Connect(context.Background(), Config{Vendor: "oracle", BaseURL: srv.URL + "/fhir", TokenURL: srv.URL + "/token", ClientID: "c", ClientSecret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ehr.FHIR.Read(context.Background(), "Patient", "p1")
	if err == nil || err.Error() != "token request failed: 401 invalid_client" {
		t.Fatalf("%v", err)
	}
}

func TestTransportErrorHasNoQuery(t *testing.T) {
	f := newFakeFHIR(t)
	ehr := connectFake(t, f, "generic", nil)
	ehr.FHIR.MaxRetries = 0
	_, _ = ehr.FHIR.Read(context.Background(), "Patient", "p1") // warm the token
	ehr.FHIR.client = &http.Client{Transport: failTransport{}}
	_, err := ehr.FHIR.Search(context.Background(), "Patient", Params{"family": "Okafor"}, 10)
	if err == nil || strings.Contains(err.Error(), "Okafor") || errorType(err) != "transport" {
		t.Fatalf("%v", err)
	}
}

type failTransport struct{}

func (failTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return nil, errors.New("connection reset")
}

func TestURLTemplate(t *testing.T) {
	c, _ := NewFHIRClient(FHIRClientConfig{BaseURL: "https://fhir.epic.com/api/FHIR/R4/"})
	for in, want := range map[string]string{
		"https://fhir.epic.com/api/FHIR/R4/Patient/eAB3mDIBBcyUKviyzrxsnAw3":  "/api/FHIR/R4/Patient/{id}",
		"https://fhir.epic.com/api/FHIR/R4/Observation?patient=x&code=4548-4": "/api/FHIR/R4/Observation",
		"https://fhir.epic.com/api/FHIR/R4/Patient/$match":                    "/api/FHIR/R4/Patient/$match",
		"https://fhir.epic.com/api/FHIR/R4/Binary/a.b-c":                      "/api/FHIR/R4/Binary/{id}",
	} {
		u, _ := url.Parse(in)
		if got := c.template(u); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
}

func TestVendorSearchParams(t *testing.T) {
	ehr := &EHR{PracticeID: "195900"}
	p := Epic.SearchParams("Observation", Params{"patient": "p", "category": "laboratory", "code": "x", "_sort": "-date", "date": nil}, ehr)
	if p["category"] != nil || p["_sort"] != nil || p["code"] != "x" || len(p) != 2 {
		t.Fatalf("epic: %v", p)
	}
	p = Oracle.SearchParams("Appointment", Params{"date": []string{"ge2026-01-01", "le2026-02-01"}}, ehr)
	if d := p["date"].([]string); d[0] != "ge2026-01-01T00:00:00Z" || d[1] != "le2026-02-01T23:59:59Z" {
		t.Fatalf("oracle: %v", p)
	}
	p = Athena.SearchParams("MedicationRequest", Params{"patient": "a-1.E-5"}, ehr)
	if p["ah-practice"] != "Organization/a-1.Practice-195900" || p["intent"] != "order" {
		t.Fatalf("athena: %v", p)
	}
	p = Meditech.SearchParams("MedicationRequest", Params{"status": "stopped"}, ehr)
	if p["status"] != "completed" {
		t.Fatalf("meditech: %v", p)
	}
	if ECW.Supports("list_appointments") || Athena.Supports("create_note") || !Epic.supports("create_note") {
		t.Fatal("supports")
	}
	if s := Athena.Scopes(true); s[len(s)-1] != "athena/service/Athenanet.MDP.*" || strings.Contains(strings.Join(s, " "), "Appointment") {
		t.Fatalf("athena scopes %v", s)
	}
	if s := Meditech.Scopes(true); s[0] != "system/Patient.rs" || s[len(s)-1] != "system/DocumentReference.c" {
		t.Fatalf("meditech scopes %v", s)
	}
}

func TestDefinitionsGating(t *testing.T) {
	f := newFakeFHIR(t)
	ehr := connectFake(t, f, "generic", nil)
	names := func(s *Session) string {
		var n []string
		for _, d := range s.Definitions() {
			n = append(n, d.Name)
		}
		return strings.Join(n, ",")
	}
	unbound := NewSession(ehr, SessionOptions{})
	if !strings.HasPrefix(names(unbound), "find_patient,select_patient,") || strings.Contains(names(unbound), "create_note") {
		t.Fatal(names(unbound))
	}
	bound := NewSession(ehr, SessionOptions{Patient: "p1", AllowWrites: true})
	if strings.Contains(names(bound), "find_patient") || !strings.HasSuffix(names(bound), "create_note") {
		t.Fatal(names(bound))
	}
	if r := bound.Call(context.Background(), "find_patient", map[string]any{"family": "x"}); r.Code != "unknown_tool" {
		t.Fatal(r.JSON())
	}
	if r := bound.Call(context.Background(), "create_note", map[string]any{"title": "t", "text": "x"}); r.Code != "no_approver" {
		t.Fatal(r.JSON())
	}
	declined := NewSession(ehr, SessionOptions{Patient: "p1", AllowWrites: true, Approve: func(context.Context, ApprovalRequest) bool { return false }})
	if r := declined.Call(context.Background(), "create_note", map[string]any{"title": "t", "text": "x"}); r.Code != "declined" {
		t.Fatal(r.JSON())
	}
	if r := unbound.Call(context.Background(), "get_patient", nil); r.Code != "no_patient" || r.JSON() != `{"ok":false,"error":"no patient selected; call find_patient then select_patient","code":"no_patient"}` {
		t.Fatal(r.JSON())
	}
	if r := unbound.Call(context.Background(), "find_patient", map[string]any{"family": "Okafor"}); !r.OK {
		t.Fatal(r.JSON())
	}
	if r := unbound.Call(context.Background(), "select_patient", map[string]any{"patient_id": "p1"}); !r.OK || unbound.PatientID() != "p1" {
		t.Fatal(r.JSON())
	}
	if r := unbound.Call(context.Background(), "select_patient", map[string]any{"patient_id": "p1"}); r.Code != "unknown_tool" {
		t.Fatal(r.JSON())
	}
}

func TestBelongsTo(t *testing.T) {
	cases := []struct {
		r    map[string]any
		want bool
	}{
		{map[string]any{"subject": map[string]any{"reference": "Patient/p1"}}, true},
		{map[string]any{"subject": map[string]any{"reference": "https://x/fhir/Patient/p1"}}, true},
		{map[string]any{"subject": map[string]any{"reference": "Patient/p2"}}, false},
		{map[string]any{"beneficiary": map[string]any{"reference": "Patient/p2"}}, false},
		{map[string]any{"participant": []any{map[string]any{"actor": map[string]any{"reference": "Patient/p2"}}}}, false},
		{map[string]any{}, true},
	}
	for i, c := range cases {
		if got := belongsTo(c.r, "p1"); got != c.want {
			t.Errorf("case %d: %v", i, got)
		}
	}
}

func TestURLTemplateLowercaseIDsAndOutsideBase(t *testing.T) {
	c, _ := NewFHIRClient(FHIRClientConfig{BaseURL: "https://api.platform.athenahealth.com/fhir/r4"})
	for in, want := range map[string]string{
		"https://api.platform.athenahealth.com/fhir/r4/Patient/abcdef":                "/fhir/r4/Patient/{id}",
		"https://api.platform.athenahealth.com/fhir/r4/Patient/p1/_history/2":         "/fhir/r4/Patient/{id}/_history/{id}",
		"https://api.platform.athenahealth.com/fhir/r4/Observation/_search":           "/fhir/r4/Observation/_search",
		"https://api.platform.athenahealth.com/v1/195900/patients/60990/appointments": "/v1/{id}/patients/{id}/appointments",
		"https://api.platform.athenahealth.com/fhir/r4x/Patient":                      "/{id}/{id}/Patient",
	} {
		u, _ := url.Parse(in)
		if got := c.template(u); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
}

func TestSearchStopsAfter100Pages(t *testing.T) {
	f := newFakeFHIR(t)
	var pages int
	f.handle = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/Condition") {
			pages++
			// A server that loops its next link forever, with empty pages.
			writeJSONT(w, 200, bundleNext(f.URL+"/fhir/Condition?page=again"))
			return true
		}
		return false
	}
	ehr := connectFake(t, f, "generic", nil)
	out, err := ehr.FHIR.Search(context.Background(), "Condition", Params{"patient": "p1"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if pages != 100 || !out.Truncated || len(out.Resources) != 0 {
		t.Fatalf("pages=%d truncated=%v n=%d", pages, out.Truncated, len(out.Resources))
	}
}

func TestArgsValidatedAgainstSchema(t *testing.T) {
	f := newFakeFHIR(t)
	ehr := connectFake(t, f, "generic", nil)
	ctx := context.Background()
	approved := false
	s := NewSession(ehr, SessionOptions{Patient: "p1", AllowWrites: true, Approve: func(context.Context, ApprovalRequest) bool { approved = true; return true }})
	before := f.requests.Load()
	for _, c := range []struct {
		name string
		args map[string]any
		msg  string
	}{
		{"get_labs_vitals", map[string]any{"limit": 2.5}, "limit must be an integer"},
		{"get_labs_vitals", map[string]any{"limit": "20"}, "limit must be an integer"},
		{"get_labs_vitals", map[string]any{"code": 4548}, "code must be a string"},
		{"get_labs_vitals", map[string]any{"since": nil}, "since must be a string"},
		{"read_document", map[string]any{}, "document_id is required"},
		{"read_document", map[string]any{"document_id": ""}, "document_id is required"},
		{"read_document", map[string]any{"document_id": 7.0}, "document_id must be a string"},
		{"create_note", map[string]any{"title": "t"}, "text is required"},
		{"create_note", map[string]any{"title": "t", "text": "   "}, "create_note needs title and text strings"},
	} {
		r := s.Call(ctx, c.name, c.args)
		if r.OK || r.Code != "bad_args" || r.Error != c.msg {
			t.Errorf("%s %v: %s", c.name, c.args, r.JSON())
		}
	}
	if approved || f.requests.Load() != before {
		t.Fatal("invalid args reached approval or the server")
	}
	if r := s.Call(ctx, "get_labs_vitals", map[string]any{"limit": 2.0}); !r.OK {
		t.Fatalf("whole float should pass: %s", r.JSON())
	}
	if r := s.Call(ctx, "get_labs_vitals", map[string]any{"limit": 2}); !r.OK {
		t.Fatalf("Go int should pass: %s", r.JSON())
	}
}
