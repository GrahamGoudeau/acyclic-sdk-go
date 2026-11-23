package acyclic

// FHIR R4 resource -> small flat object. Rules: spec/projections.md.
// Inputs are resources decoded with encoding/json into map[string]any.

import (
	"regexp"
	"strings"
)

var codeSystems = map[string]string{
	"http://snomed.info/sct":                      "SNOMED",
	"http://loinc.org":                            "LOINC",
	"http://hl7.org/fhir/sid/icd-10-cm":           "ICD-10-CM",
	"http://www.nlm.nih.gov/research/umls/rxnorm": "RXNORM",
	"http://hl7.org/fhir/sid/cvx":                 "CVX",
}

type kv struct {
	k string
	v any
}

// obj builds a projection, dropping absent values: nil, "" and empty arrays.
func obj(pairs ...kv) map[string]any {
	out := map[string]any{}
	for _, p := range pairs {
		switch x := p.v.(type) {
		case nil:
			continue
		case string:
			if x == "" {
				continue
			}
		case []string:
			if len(x) == 0 {
				continue
			}
		case []any:
			if len(x) == 0 {
				continue
			}
		}
		out[p.k] = p.v
	}
	return out
}

// Display is display(cc): text, else the first coding display, else the first coding code.
func Display(cc any) any {
	m := asMap(cc)
	if m == nil {
		return nil
	}
	if truthy(m["text"]) {
		return m["text"]
	}
	for _, c := range asArr(m["coding"]) {
		if d := get(c, "display"); truthy(d) {
			return d
		}
	}
	for _, c := range asArr(m["coding"]) {
		if d := get(c, "code"); truthy(d) {
			return d
		}
	}
	return nil
}

// Codes is codes(cc): every coding with a code as "SYSTEM|code".
func Codes(cc any) []string {
	out := []string{}
	for _, c := range asArr(get(cc, "coding")) {
		code := get(c, "code")
		if !truthy(code) {
			continue
		}
		sys := get(c, "system")
		short, ok := codeSystems[str(sys)]
		if !ok {
			if sys == nil {
				short = ""
			} else {
				short = jsString(sys)
			}
		}
		out = append(out, short+"|"+jsString(code))
	}
	return out
}

func ccStatus(cc any) any {
	for _, c := range asArr(get(cc, "coding")) {
		if code := get(c, "code"); truthy(code) {
			return code
		}
	}
	return nil
}

func ref(r any) any {
	m := asMap(r)
	if m == nil {
		return nil
	}
	if truthy(m["display"]) {
		return m["display"]
	}
	return m["reference"]
}

func hasCode(cc any, code string) bool {
	for _, c := range asArr(get(cc, "coding")) {
		if get(c, "code") == code {
			return true
		}
	}
	return false
}

func qtyText(q any) any {
	m := asMap(q)
	if m == nil || m["value"] == nil {
		return nil
	}
	unit := first(m["unit"], m["code"])
	if truthy(unit) {
		return jsString(m["value"]) + " " + jsString(unit)
	}
	return jsString(m["value"])
}

func index0(v any) any {
	a := asArr(v)
	if len(a) == 0 {
		return nil
	}
	return a[0]
}

// ProjectPatient projects a Patient.
func ProjectPatient(r map[string]any) map[string]any {
	names := asArr(r["name"])
	var n any
	for _, x := range names {
		if get(x, "use") == "official" {
			n = x
			break
		}
	}
	if n == nil && len(names) > 0 {
		n = names[0]
	}
	var name any
	if n != nil {
		if t := get(n, "text"); truthy(t) {
			name = t
		} else {
			var parts []string
			for _, g := range asArr(get(n, "given")) {
				if truthy(g) {
					parts = append(parts, jsString(g))
				}
			}
			if f := get(n, "family"); truthy(f) {
				parts = append(parts, jsString(f))
			}
			name = strings.Join(parts, " ")
		}
	}
	var mrn any
	for _, id := range asArr(r["identifier"]) {
		if hasCode(get(id, "type"), "MR") {
			mrn = get(id, "value")
			break
		}
	}
	var deceased any
	if r["deceasedBoolean"] == true || r["deceasedDateTime"] != nil {
		deceased = true
	}
	return obj(kv{"id", r["id"]}, kv{"name", name}, kv{"birth_date", r["birthDate"]}, kv{"gender", r["gender"]}, kv{"mrn", mrn}, kv{"deceased", deceased})
}

// ProjectCondition projects a Condition.
func ProjectCondition(r map[string]any) map[string]any {
	return obj(
		kv{"id", r["id"]}, kv{"display", Display(r["code"])}, kv{"codes", Codes(r["code"])},
		kv{"clinical_status", ccStatus(r["clinicalStatus"])},
		kv{"onset", first(r["onsetDateTime"], get(r, "onsetPeriod", "start"))}, kv{"recorded", r["recordedDate"]},
	)
}

// ProjectMedication projects a MedicationRequest.
func ProjectMedication(r map[string]any) map[string]any {
	return obj(
		kv{"id", r["id"]}, kv{"display", first(Display(r["medicationCodeableConcept"]), get(r, "medicationReference", "display"))},
		kv{"codes", Codes(r["medicationCodeableConcept"])}, kv{"status", r["status"]},
		kv{"dosage", get(index0(r["dosageInstruction"]), "text")}, kv{"authored_on", r["authoredOn"]}, kv{"requester", ref(r["requester"])},
	)
}

// ProjectAllergy projects an AllergyIntolerance.
func ProjectAllergy(r map[string]any) map[string]any {
	reactions := []any{}
	seen := map[string]bool{}
	for _, rx := range asArr(r["reaction"]) {
		for _, m := range asArr(get(rx, "manifestation")) {
			d := Display(m)
			if !truthy(d) {
				continue
			}
			key := jsString(d)
			if !seen[key] {
				seen[key] = true
				reactions = append(reactions, d)
			}
		}
	}
	return obj(
		kv{"id", r["id"]}, kv{"display", Display(r["code"])}, kv{"clinical_status", ccStatus(r["clinicalStatus"])},
		kv{"criticality", r["criticality"]}, kv{"reactions", reactions}, kv{"recorded", r["recordedDate"]},
	)
}

// ProjectObservation projects an Observation.
func ProjectObservation(r map[string]any) map[string]any {
	var value any
	comps := asArr(r["component"])
	switch {
	case truthy(r["valueQuantity"]):
		value = qtyText(r["valueQuantity"])
	case truthy(r["valueCodeableConcept"]):
		value = Display(r["valueCodeableConcept"])
	case r["valueString"] != nil:
		value = r["valueString"]
	case len(comps) > 0 && allComponentsQuantity(comps):
		q0 := asMap(get(comps[0], "valueQuantity"))
		unit := first(q0["unit"], q0["code"])
		vals := make([]string, len(comps))
		for i, c := range comps {
			vals[i] = jsString(get(c, "valueQuantity", "value"))
		}
		s := strings.Join(vals, "/")
		if truthy(unit) {
			s += " " + jsString(unit)
		}
		value = s
	}
	q := asMap(r["valueQuantity"])
	var num, unit any
	if q != nil {
		num = q["value"]
		unit = first(q["unit"], q["code"])
	}
	var loinc any
	for _, c := range asArr(get(r, "code", "coding")) {
		if get(c, "system") == "http://loinc.org" {
			loinc = get(c, "code")
			break
		}
	}
	var category any
outer:
	for _, cat := range asArr(r["category"]) {
		for _, c := range asArr(get(cat, "coding")) {
			if code := get(c, "code"); truthy(code) {
				category = code
				break outer
			}
		}
	}
	return obj(
		kv{"id", r["id"]}, kv{"display", Display(r["code"])}, kv{"loinc", loinc},
		kv{"value", value}, kv{"value_number", num}, kv{"unit", unit},
		kv{"interpretation", Display(index0(r["interpretation"]))},
		kv{"effective", first(r["effectiveDateTime"], get(r, "effectivePeriod", "start"), r["issued"])},
		kv{"category", category},
	)
}

func allComponentsQuantity(comps []any) bool {
	for _, c := range comps {
		if get(c, "valueQuantity", "value") == nil {
			return false
		}
	}
	return true
}

// ProjectEncounter projects an Encounter.
func ProjectEncounter(r map[string]any) map[string]any {
	var provider any
	for _, p := range asArr(r["participant"]) {
		if ind := get(p, "individual"); truthy(ind) {
			provider = ref(ind)
			break
		}
	}
	return obj(
		kv{"id", r["id"]}, kv{"status", r["status"]}, kv{"class", get(r, "class", "code")}, kv{"type", Display(index0(r["type"]))},
		kv{"start", get(r, "period", "start")}, kv{"end", get(r, "period", "end")}, kv{"reason", Display(index0(r["reasonCode"]))},
		kv{"location", ref(get(index0(r["location"]), "location"))},
		kv{"provider", provider},
	)
}

// ProjectAppointment projects an Appointment.
func ProjectAppointment(r map[string]any) map[string]any {
	actor := func(prefix string) any {
		for _, p := range asArr(r["participant"]) {
			if strings.HasPrefix(str(get(p, "actor", "reference")), prefix) {
				return ref(get(p, "actor"))
			}
		}
		return nil
	}
	return obj(
		kv{"id", r["id"]}, kv{"status", r["status"]}, kv{"start", r["start"]}, kv{"end", r["end"]},
		kv{"type", first(Display(r["appointmentType"]), Display(index0(r["serviceType"])))}, kv{"description", r["description"]},
		kv{"practitioner", actor("Practitioner/")}, kv{"location", actor("Location/")},
	)
}

func docTitle(r map[string]any) any {
	return firstTruthy(r["description"], get(index0(r["content"]), "attachment", "title"), Display(r["type"]))
}

// ProjectDocument projects a DocumentReference (metadata only).
func ProjectDocument(r map[string]any) map[string]any {
	return obj(
		kv{"id", r["id"]}, kv{"title", docTitle(r)}, kv{"type", Display(r["type"])}, kv{"date", r["date"]},
		kv{"author", ref(index0(r["author"]))}, kv{"content_type", get(index0(r["content"]), "attachment", "contentType")}, kv{"status", r["status"]},
	)
}

// ProjectCoverage projects a Coverage.
func ProjectCoverage(r map[string]any) map[string]any {
	var plan any
	for _, c := range asArr(r["class"]) {
		if hasCode(get(c, "type"), "plan") {
			plan = first(get(c, "value"), get(c, "name"))
			break
		}
	}
	return obj(
		kv{"id", r["id"]}, kv{"status", r["status"]}, kv{"payer", ref(index0(r["payor"]))}, kv{"plan", plan},
		kv{"member_id", first(r["subscriberId"], get(index0(r["identifier"]), "value"))}, kv{"relationship", Display(r["relationship"])},
		kv{"period_start", get(r, "period", "start")}, kv{"period_end", get(r, "period", "end")},
	)
}

// JavaScript's \s and String.prototype.trim whitespace set.
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

var (
	reTag      = regexp.MustCompile(`<[^>]*>`)
	reEntity   = regexp.MustCompile(`&(amp|lt|gt|quot|#39|nbsp);`)
	reJSSpaces = regexp.MustCompile(`[\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]+`)
	entities   = map[string]string{"&amp;": "&", "&lt;": "<", "&gt;": ">", "&quot;": `"`, "&#39;": "'", "&nbsp;": " "}
)

func jsTrim(s string) string { return strings.TrimFunc(s, isJSSpace) }

// HTMLToText reduces HTML to text: tags become spaces, six entities are
// decoded, whitespace runs collapse to one space, and the result is trimmed.
func HTMLToText(html string) string {
	s := reTag.ReplaceAllString(html, " ")
	s = reEntity.ReplaceAllStringFunc(s, func(m string) string { return entities[m] })
	return jsTrim(reJSSpaces.ReplaceAllString(s, " "))
}

var reHTML = regexp.MustCompile(`(?i)html`)

// ProjectDocumentText projects a DocumentReference with its decoded text,
// cut to maxChars characters (never mid-codepoint).
func ProjectDocumentText(r map[string]any, text, contentType string, maxChars int) map[string]any {
	var t string
	if reHTML.MatchString(contentType) {
		t = HTMLToText(text)
	} else {
		t = jsTrim(text)
	}
	runes := []rune(t)
	truncated := len(runes) > maxChars
	if truncated {
		t = string(runes[:maxChars])
	}
	out := obj(kv{"id", r["id"]}, kv{"title", docTitle(r)}, kv{"text", t})
	out["truncated"] = truncated
	return out
}

// Projectors maps each projection name in spec/projections.md to its function.
var Projectors = map[string]func(map[string]any) map[string]any{
	"patient": ProjectPatient, "condition": ProjectCondition, "medication": ProjectMedication,
	"allergy": ProjectAllergy, "observation": ProjectObservation, "encounter": ProjectEncounter,
	"appointment": ProjectAppointment, "document": ProjectDocument, "coverage": ProjectCoverage,
}
