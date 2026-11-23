package acyclic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeFHIR is a tiny FHIR server for unit tests: one patient, labs, a
// token endpoint, and hooks to inject failures.
type fakeFHIR struct {
	*httptest.Server
	tokens    atomic.Int32
	requests  atomic.Int32
	mu        sync.Mutex
	queries   []string
	handle    func(w http.ResponseWriter, r *http.Request) bool // return true if handled
	expiresIn any
}

func newFakeFHIR(t *testing.T) *fakeFHIR {
	f := &fakeFHIR{expiresIn: "3600"}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		f.tokens.Add(1)
		_ = r.ParseForm()
		if r.PostForm.Get("grant_type") != "client_credentials" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"unsupported_grant_type","error_description":"secret stuff"}`))
			return
		}
		writeJSONT(w, 200, map[string]any{"access_token": "tok" + string(rune('0'+f.tokens.Load())), "expires_in": f.expiresIn, "token_type": "bearer"})
	})
	mux.HandleFunc("/fhir/.well-known/smart-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSONT(w, 200, map[string]any{"token_endpoint": f.URL + "/token"})
	})
	mux.HandleFunc("/fhir/", func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		f.mu.Lock()
		f.queries = append(f.queries, r.URL.RawQuery)
		f.mu.Unlock()
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer tok") {
			w.WriteHeader(401)
			return
		}
		if f.handle != nil && f.handle(w, r) {
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/fhir/")
		switch {
		case path == "Patient/p1":
			writeJSONT(w, 200, map[string]any{"resourceType": "Patient", "id": "p1", "name": []any{map[string]any{"given": []any{"Samuel"}, "family": "Okafor"}}, "birthDate": "1948-11-19"})
		case path == "Patient":
			writeJSONT(w, 200, bundle(nil, map[string]any{"resourceType": "Patient", "id": "p1", "name": []any{map[string]any{"text": "Samuel Okafor"}}}))
		case path == "Observation":
			page := r.URL.Query().Get("page")
			var next string
			entries := []any{}
			start := 0
			if page == "2" {
				start = 3
			} else {
				next = f.URL + "/fhir/Observation?page=2"
			}
			for i := start; i < start+3; i++ {
				entries = append(entries, map[string]any{"resourceType": "Observation", "id": "o" + string(rune('0'+i)), "subject": map[string]any{"reference": "Patient/p1"},
					"code": map[string]any{"text": "HbA1c"}, "valueQuantity": map[string]any{"value": 7.0 + float64(i)/10, "unit": "%"}, "effectiveDateTime": "2026-0" + string(rune('1'+i)) + "-01"})
			}
			// a cross-patient entry the server should never have sent
			entries = append(entries, map[string]any{"resourceType": "Observation", "id": "x", "subject": map[string]any{"reference": "Patient/other"}})
			writeJSONT(w, 200, bundleNext(next, entries...))
		default:
			writeJSONT(w, 404, map[string]any{"resourceType": "OperationOutcome", "issue": []any{map[string]any{"code": "not-found", "diagnostics": "no Okafor here"}}})
		}
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func bundle(_ any, entries ...any) map[string]any { return bundleNext("", entries...) }

func bundleNext(next string, resources ...any) map[string]any {
	entries := make([]any, len(resources))
	for i, r := range resources {
		entries[i] = map[string]any{"resource": r, "search": map[string]any{"mode": "match"}}
	}
	b := map[string]any{"resourceType": "Bundle", "type": "searchset", "entry": entries}
	if next != "" {
		b["link"] = []any{map[string]any{"relation": "next", "url": next}}
	}
	return b
}

func writeJSONT(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/fhir+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// recorder is a Telemetry that keeps every line as JSON.
type recorder struct {
	mu    sync.Mutex
	lines []string
}

type recSpan struct {
	r     *recorder
	name  string
	attrs map[string]any
}

func (r *recorder) add(v any) {
	r.mu.Lock()
	r.lines = append(r.lines, marshalJSON(v))
	r.mu.Unlock()
}

func (r *recorder) StartSpan(ctx context.Context, name string, attrs map[string]any) (context.Context, Span) {
	return ctx, &recSpan{r: r, name: name, attrs: attrs}
}
func (s *recSpan) SetAttributes(a map[string]any) {
	for k, v := range a {
		s.attrs[k] = v
	}
}
func (s *recSpan) End(err error) {
	e := ""
	if err != nil {
		e = err.Error()
	}
	s.r.add(map[string]any{"span": s.name, "attrs": s.attrs, "err": e})
}
func (r *recorder) Log(_ context.Context, level, name string, attrs map[string]any) {
	r.add(map[string]any{"log": name, "level": level, "attrs": attrs})
}
func (r *recorder) Audit(_ context.Context, rec AuditRecord) { r.add(rec) }

func (r *recorder) all() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}

func connectFake(t *testing.T, f *fakeFHIR, vendor string, tel Telemetry) *EHR {
	t.Helper()
	ehr, err := Connect(context.Background(), Config{
		Vendor: vendor, BaseURL: f.URL + "/fhir", ClientID: "client-1", ClientSecret: "s3cret", Auth: "client_secret_basic",
		Telemetry: tel, HTTPClient: &http.Client{Timeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	ehr.FHIR.sleep = func(context.Context, time.Duration) error { return nil }
	return ehr
}
