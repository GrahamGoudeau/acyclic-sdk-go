package acyclic

// Vendor profiles: how five FHIR R4 servers that implement the same standard
// differ on the wire. Mirrors acyclic-sdk-js src/vendors.js rule for rule.

import (
	"context"
	"encoding/base64"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ToolHandler replaces a tool for one vendor (athena appointments).
type ToolHandler func(ctx context.Context, args map[string]any, h ToolContext) (any, error)

// ToolContext is what a vendor tool override receives.
type ToolContext struct {
	EHR       *EHR
	PatientID string
}

// Vendor is a vendor profile. Every func field is optional.
type Vendor struct {
	Name string
	Auth string // "private_key_jwt" | "client_secret_basic"
	// Scopes returns the default scopes (write adds the note write scope).
	Scopes func(write bool) []string
	// SearchParams fixes up a search before it is sent. p is a copy.
	SearchParams func(resourceType string, p Params, ehr *EHR) Params
	// PrepareNote builds the DocumentReference for create_note.
	PrepareNote func(ctx context.Context, patientID string, args map[string]any, ehr *EHR) (map[string]any, error)
	// Tools replaces whole tools.
	Tools map[string]ToolHandler
	// Supports reports whether the vendor offers a tool to backend apps.
	Supports func(tool string) bool
	// BinaryAccept is the Accept header for Binary reads (default application/fhir+json).
	BinaryAccept string
}

func (v *Vendor) supports(tool string) bool {
	if v.Supports == nil {
		return true
	}
	return v.Supports(tool)
}

func (v *Vendor) searchParams(resourceType string, p Params, ehr *EHR) Params {
	if v.SearchParams == nil {
		return p
	}
	return v.SearchParams(resourceType, p, ehr)
}

// drop removes nil values and empty lists.
func drop(p Params) Params {
	out := Params{}
	for k, v := range p {
		switch x := v.(type) {
		case nil:
			continue
		case []string:
			if len(x) == 0 {
				continue
			}
		case []any:
			if len(x) == 0 {
				continue
			}
		}
		out[k] = v
	}
	return out
}

func setDefault(p Params, k string, v any) {
	if p[k] == nil {
		p[k] = v
	}
}

var reads = []string{"Patient", "Condition", "MedicationRequest", "AllergyIntolerance", "Observation", "Encounter", "Appointment", "DocumentReference", "Binary", "Coverage"}

func without(types []string, drop string) []string {
	var out []string
	for _, t := range types {
		if t != drop {
			out = append(out, t)
		}
	}
	return out
}

func scopesV1(types []string, write bool) []string {
	var out []string
	for _, t := range types {
		out = append(out, "system/"+t+".read")
	}
	if write {
		out = append(out, "system/DocumentReference.write")
	}
	return out
}

func scopesV2(types []string, write bool) []string {
	var out []string
	for _, t := range types {
		out = append(out, "system/"+t+".rs")
	}
	if write {
		out = append(out, "system/DocumentReference.c")
	}
	return out
}

type noteOpts struct {
	contentType string
	extra       map[string]any
}

func note(pid string, a map[string]any, o noteOpts) map[string]any {
	ct := o.contentType
	if ct == "" {
		ct = "text/plain"
	}
	title := a["title"]
	r := map[string]any{
		"resourceType": "DocumentReference", "status": "current", "docStatus": "final",
		"type":     map[string]any{"coding": []any{map[string]any{"system": "http://loinc.org", "code": "11506-3", "display": "Progress note"}}},
		"category": []any{map[string]any{"coding": []any{map[string]any{"system": "http://hl7.org/fhir/us/core/CodeSystem/us-core-documentreference-category", "code": "clinical-note"}}}},
		"subject":  map[string]any{"reference": "Patient/" + pid}, "date": isoNow(), "description": title,
		"content": []any{map[string]any{"attachment": map[string]any{
			"contentType": ct, "data": base64.StdEncoding.EncodeToString([]byte(str(a["text"]))), "title": title,
		}}},
	}
	for k, v := range o.extra {
		r[k] = v
	}
	return r
}

func encounterRef(id string) []any {
	return []any{map[string]any{"reference": "Encounter/" + id}}
}

// latestEncounter is the most recent encounter, for vendors that file notes against one.
func latestEncounter(ctx context.Context, pid string, ehr *EHR) (string, error) {
	out, err := ehr.FHIR.Search(ctx, "Encounter", ehr.Vendor.searchParams("Encounter", Params{"patient": pid}, ehr), 50)
	if err != nil {
		return "", err
	}
	type enc struct{ id, start string }
	list := make([]enc, 0, len(out.Resources))
	for _, e := range out.Resources {
		list = append(list, enc{id: str(e["id"]), start: str(get(e, "period", "start"))})
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].start > list[j].start })
	if len(list) == 0 {
		return "", nil
	}
	return list[0].id, nil
}

// codeWins: Epic rejects category with code; Oracle ignores code when a
// category is present. Send only the code when there is one.
func codeWins(resourceType string, p Params) {
	if resourceType == "Observation" && truthy(p["code"]) {
		delete(p, "category")
	}
}

func stringList(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		var out []string
		for _, e := range x {
			if truthy(e) {
				out = append(out, jsString(e))
			}
		}
		return out
	case nil:
		return nil
	}
	if truthy(v) {
		return []string{jsString(v)}
	}
	return nil
}

// Epic: private_key_jwt, SMART v1 scopes, no _sort, problem-list and
// clinical-note categories, notes filed against an encounter.
var Epic = &Vendor{
	Name: "epic", Auth: "private_key_jwt",
	Scopes: func(write bool) []string { return scopesV1(reads, write) },
	SearchParams: func(t string, p Params, _ *EHR) Params {
		codeWins(t, p)
		if t == "Condition" {
			setDefault(p, "category", "problem-list-item")
		}
		if t == "DocumentReference" && !truthy(p["type"]) {
			setDefault(p, "category", "clinical-note")
		}
		if t == "Appointment" {
			setDefault(p, "service-category", "appointment")
		}
		delete(p, "_sort") // unsupported; Epic would ignore it with a warning
		return drop(p)
	},
	PrepareNote: func(ctx context.Context, pid string, a map[string]any, ehr *EHR) (map[string]any, error) {
		// Epic files notes against an encounter of the patient, from text/plain.
		enc := str(a["encounter_id"])
		if a["encounter_id"] == nil {
			var err error
			if enc, err = latestEncounter(ctx, pid, ehr); err != nil {
				return nil, err
			}
		}
		if enc == "" {
			return nil, errors.New("Epic notes need an encounter and this patient has none")
		}
		return note(pid, a, noteOpts{extra: map[string]any{"context": map[string]any{"encounter": encounterRef(enc)}}}), nil
	},
}

// Oracle Health (Cerner): client_secret_basic, SMART v1 scopes listed per
// resource (no wildcards), Appointment dates need a prefix and a time.
var Oracle = &Vendor{
	Name: "oracle", Auth: "client_secret_basic",
	Scopes: func(write bool) []string { return scopesV1(reads, write) },
	SearchParams: func(t string, p Params, _ *EHR) Params {
		codeWins(t, p)
		if t == "Appointment" {
			var dates []string
			for _, d := range stringList(p["date"]) {
				switch {
				case strings.Contains(d, "T"):
				case strings.HasPrefix(d, "le"):
					d += "T23:59:59Z"
				default:
					d += "T00:00:00Z"
				}
				dates = append(dates, d)
			}
			if len(dates) == 0 {
				dates = []string{"ge" + today() + "T00:00:00Z"}
			}
			p["date"] = dates
		}
		delete(p, "_sort")
		return drop(p)
	},
	PrepareNote: func(_ context.Context, pid string, a map[string]any, _ *EHR) (map[string]any, error) {
		c := map[string]any{"period": map[string]any{"end": isoNow()}}
		if truthy(a["encounter_id"]) {
			c["encounter"] = encounterRef(jsString(a["encounter_id"]))
		}
		return note(pid, a, noteOpts{contentType: "text/plain;charset=utf-8", extra: map[string]any{"context": c}}), nil
	},
}

// athenaOne appointments come from the proprietary API:
// GET /v1/{practiceid}/patients/{patientid}/appointments
var athenaStatus = map[string]string{"f": "booked", "x": "cancelled", "o": "proposed", "2": "arrived", "3": "fulfilled", "4": "fulfilled"}

var reAthenaFHIR = regexp.MustCompile(`(?i)/fhir/r4$`)

func athenaAppointments(ctx context.Context, a map[string]any, h ToolContext) (any, error) {
	ehr := h.EHR
	practice := ehr.PracticeID
	if practice == "" {
		return nil, errors.New("athena needs practiceId for the appointments API")
	}
	_, enterprise, found := strings.Cut(h.PatientID, ".E-")
	if !found {
		return nil, &ToolError{Message: "athena patient id has no enterprise id (.E-)", Type: "bad_patient_id"}
	}
	if i := strings.Index(enterprise, ".E-"); i >= 0 { // JS split(".E-")[1]
		enterprise = enterprise[:i]
	}
	base := reAthenaFHIR.ReplaceAllLiteralString(ehr.FHIR.BaseURL, "/v1/"+practice)
	since := str(a["since"])
	params := Params{}
	if since != "" && since < today() {
		params["showpast"] = "true"
	}
	res, err := ehr.FHIR.Do(ctx, "GET", base+"/patients/"+encodeURIComponent(enterprise)+"/appointments", RequestOptions{
		Params: params, Accept: "application/json", ResourceType: "Appointment",
	})
	if err != nil {
		return nil, err
	}
	if a["since"] == nil {
		since = today()
	}
	until := str(a["until"])
	items := []map[string]any{}
	for _, xv := range asArr(res.JSON["appointments"]) {
		x := asMap(xv)
		parts := strings.Split(jsString(x["date"]), "/")
		for len(parts) < 3 {
			parts = append(parts, "undefined")
		}
		mm, dd, yyyy := parts[0], parts[1], parts[2]
		start := yyyy + "-" + mm + "-" + dd + "T" + jsString(x["starttime"])
		var end any
		if truthy(x["duration"]) {
			if t, perr := time.Parse("2006-01-02T15:04:05Z", start+":00Z"); perr == nil {
				if mins, ok := toNumber(x["duration"]); ok {
					end = t.Add(time.Duration(mins * float64(time.Minute))).UTC().Format("2006-01-02T15:04")
				}
			}
		}
		status := any(x["appointmentstatus"])
		if s, ok := athenaStatus[jsString(x["appointmentstatus"])]; ok {
			status = s
		}
		item := obj(kv{"id", jsString(x["appointmentid"])}, kv{"status", status}, kv{"start", start}, kv{"end", end},
			kv{"type", firstTruthy(x["appointmenttype"], x["patientappointmenttypename"])})
		for k, v := range item {
			if !truthy(v) {
				delete(item, k)
			}
		}
		day := start
		if len(day) > 10 {
			day = day[:10]
		}
		if day >= since && (until == "" || day <= until) {
			items = append(items, item)
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return str(items[i]["start"]) < str(items[j]["start"]) })
	return map[string]any{"items": items, "total": len(items), "truncated": false}, nil
}

// Athena (athenaOne): client_secret_basic, no system Appointment scope, the
// MDP scope for the proprietary API, ah-practice on every search.
var Athena = &Vendor{
	Name: "athena", Auth: "client_secret_basic",
	// No wildcards; no system Appointment scope; MDP scope for the proprietary API.
	Scopes: func(bool) []string {
		return append(scopesV1(without(reads, "Appointment"), false), "athena/service/Athenanet.MDP.*")
	},
	SearchParams: func(t string, p Params, ehr *EHR) Params {
		if ehr != nil && ehr.PracticeID != "" {
			setDefault(p, "ah-practice", "Organization/a-1.Practice-"+ehr.PracticeID)
		}
		if t == "MedicationRequest" {
			setDefault(p, "intent", "order")
		}
		delete(p, "_sort")
		return drop(p)
	},
	Tools:    map[string]ToolHandler{"list_appointments": athenaAppointments},
	Supports: func(tool string) bool { return tool != "create_note" },
}

// Meditech (Expanse): client_secret_basic, SMART v2 system scopes only.
var Meditech = &Vendor{
	Name: "meditech", Auth: "client_secret_basic",
	Scopes: func(write bool) []string { return scopesV2(reads, write) },
	SearchParams: func(t string, p Params, _ *EHR) Params {
		// MEDITECH's MedicationRequest statuses are active, cancelled, completed.
		if t == "MedicationRequest" && p["status"] == "stopped" {
			p["status"] = "completed"
		}
		delete(p, "_sort")
		return drop(p)
	},
	Supports: func(tool string) bool { return tool != "create_note" },
}

// ECW (eClinicalWorks): private_key_jwt, no Appointment API, intent=order.
var ECW = &Vendor{
	Name: "ecw", Auth: "private_key_jwt",
	Scopes: func(write bool) []string { return scopesV1(without(reads, "Appointment"), write) },
	SearchParams: func(t string, p Params, _ *EHR) Params {
		if t == "MedicationRequest" {
			setDefault(p, "intent", "order")
		}
		delete(p, "_sort")
		return drop(p)
	},
	// eCW offers no Appointment API; notes go through contract-only transaction bundles.
	Supports: func(tool string) bool { return tool != "list_appointments" && tool != "create_note" },
}

// Generic is any other US Core server: standard params, private_key_jwt, v2 scopes.
var Generic = &Vendor{
	Name: "generic", Auth: "private_key_jwt",
	Scopes:       func(write bool) []string { return scopesV2(reads, write) },
	SearchParams: func(_ string, p Params, _ *EHR) Params { return drop(p) },
}

// Vendors maps vendor ids to profiles.
var Vendors = map[string]*Vendor{"epic": Epic, "oracle": Oracle, "athena": Athena, "meditech": Meditech, "ecw": ECW, "generic": Generic}
