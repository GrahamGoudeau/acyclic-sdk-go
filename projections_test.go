package acyclic

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Every SDK must produce the same projection for the same FHIR input:
// spec/fixtures/projections/*.json is the contract.
func TestProjectionFixtures(t *testing.T) {
	files, err := filepath.Glob("spec/fixtures/projections/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	n := 0
	for _, file := range files {
		kind := strings.TrimSuffix(filepath.Base(file), ".json")
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var cases []struct {
			Name     string          `json:"name"`
			Input    map[string]any  `json:"input"`
			Expected json.RawMessage `json:"expected"`
			MaxChars int             `json:"max_chars"`
		}
		if err := json.Unmarshal(raw, &cases); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		for _, c := range cases {
			n++
			t.Run(kind+": "+c.Name, func(t *testing.T) {
				var got map[string]any
				if kind == "document_text" {
					a := asMap(get(index0(c.Input["content"]), "attachment"))
					text, err := base64ToText(str(a["data"]))
					if err != nil {
						t.Fatal(err)
					}
					got = ProjectDocumentText(c.Input, text, str(a["contentType"]), c.MaxChars)
				} else {
					f := Projectors[kind]
					if f == nil {
						t.Fatalf("no projector for %s", kind)
					}
					got = f(c.Input)
				}
				var g, want any
				if err := json.Unmarshal([]byte(marshalJSON(got)), &g); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(c.Expected, &want); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(g, want) {
					t.Errorf("got  %s\nwant %s", marshalJSON(g), string(c.Expected))
				}
			})
		}
	}
	if n < 10 {
		t.Fatalf("only %d fixture cases ran", n)
	}
}

func TestValueNumberStaysNumber(t *testing.T) {
	got := ProjectObservation(map[string]any{"id": "o", "valueQuantity": map[string]any{"value": 7.2, "unit": "%"}})
	if _, ok := got["value_number"].(float64); !ok {
		t.Fatalf("value_number is %T", got["value_number"])
	}
	if !strings.Contains(marshalJSON(got), `"value_number":7.2`) {
		t.Fatal(marshalJSON(got))
	}
}

func TestJSNumber(t *testing.T) {
	for in, want := range map[float64]string{7.2: "7.2", 128: "128", 0.30000000000000004: "0.30000000000000004", 1e21: "1e+21", 1e-7: "1e-7", -3: "-3", 123456789012: "123456789012"} {
		if got := jsNumber(in); got != want {
			t.Errorf("jsNumber(%v) = %s, want %s", in, got, want)
		}
	}
}

func TestHTMLToText(t *testing.T) {
	got := HTMLToText("<p>A &amp; B</p>\n<p>&lt;x&gt;&nbsp;&quot;q&quot; &#39;s&#39; end</p>")
	if want := `A & B <x> "q" 's' end`; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	// Truncation counts characters, never splits a codepoint.
	d := ProjectDocumentText(map[string]any{"id": "d"}, "héllo wörld", "text/plain", 7)
	if d["text"] != "héllo w" || d["truncated"] != true {
		t.Fatalf("%v", d)
	}
}
