package acyclic

import (
	_ "embed"
	"encoding/json"
)

// The shared tool contract (spec/tools.json), embedded at build time.
//
//go:embed spec/tools.json
var toolsJSON []byte

// ToolSpec is one entry of spec/tools.json.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Kind        string          `json:"kind"` // "read" or "write"
	UnboundOnly bool            `json:"unbound_only,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
	Output      string          `json:"output"`
}

// ToolSpecs is the parsed tool contract, in spec order.
var ToolSpecs = mustParseSpec()

// SpecVersion is the version field of spec/tools.json.
var SpecVersion string

func mustParseSpec() []ToolSpec {
	var s struct {
		Version string     `json:"version"`
		Tools   []ToolSpec `json:"tools"`
	}
	if err := json.Unmarshal(toolsJSON, &s); err != nil {
		panic("acyclic: bad embedded spec/tools.json: " + err.Error())
	}
	SpecVersion = s.Version
	return s.Tools
}
