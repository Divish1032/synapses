package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCoerceToJSON(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"true", "true"},
		{"false", "false"},
		{"42", "42"},
		{"3.14", "3.14"},
		{"null", "null"},
		{"hello", `"hello"`},
		{"http://localhost:11434", `"http://localhost:11434"`},
		{`{"a":1}`, `{"a":1}`},
	}
	for _, tt := range tests {
		got := coerceToJSON(tt.in)
		if got != tt.want {
			t.Errorf("coerceToJSON(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSetRawJSONField_TopLevel(t *testing.T) {
	m := map[string]json.RawMessage{}
	if err := setRawJSONField(m, "embeddings", "builtin"); err != nil {
		t.Fatal(err)
	}
	if string(m["embeddings"]) != `"builtin"` {
		t.Errorf("embeddings = %s, want %q", m["embeddings"], `"builtin"`)
	}
}

func TestSetRawJSONField_Nested(t *testing.T) {
	m := map[string]json.RawMessage{}
	if err := setRawJSONField(m, "brain.enabled", "true"); err != nil {
		t.Fatal(err)
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(m["brain"], &nested); err != nil {
		t.Fatal(err)
	}
	if string(nested["enabled"]) != "true" {
		t.Errorf("brain.enabled = %s, want true", nested["enabled"])
	}
}

func TestGetJSONField(t *testing.T) {
	type testStruct struct {
		Version string `json:"version"`
		Brain   struct {
			Enabled bool   `json:"enabled"`
			Model   string `json:"model"`
		} `json:"brain"`
	}
	v := testStruct{Version: "1"}
	v.Brain.Enabled = true
	v.Brain.Model = "qwen3.5:2b"

	val, err := getJSONField(&v, "version")
	if err != nil {
		t.Fatal(err)
	}
	if val != "1" {
		t.Errorf("version = %q, want %q", val, "1")
	}

	val, err = getJSONField(&v, "brain.enabled")
	if err != nil {
		t.Fatal(err)
	}
	if val != "true" {
		t.Errorf("brain.enabled = %q, want %q", val, "true")
	}

	val, err = getJSONField(&v, "brain.model")
	if err != nil {
		t.Fatal(err)
	}
	if val != "qwen3.5:2b" {
		t.Errorf("brain.model = %q, want %q", val, "qwen3.5:2b")
	}
}

func TestGetJSONField_Unknown(t *testing.T) {
	v := struct{ A string `json:"a"` }{A: "x"}
	_, err := getJSONField(&v, "nonexistent")
	if err == nil {
		t.Error("expected error for unknown key")
	}
}

func TestConfigSetGlobal_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	if err := configSetGlobal("brain.enabled", "true"); err != nil {
		t.Fatal(err)
	}
	if err := configSetGlobal("embeddings", "builtin"); err != nil {
		t.Fatal(err)
	}

	// Read back
	data, err := os.ReadFile(filepath.Join(tmp, ".synapses", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if string(m["embeddings"]) != `"builtin"` {
		t.Errorf("embeddings = %s, want %q", m["embeddings"], `"builtin"`)
	}
}

func TestConfigSetProject(t *testing.T) {
	projDir := t.TempDir()
	projData := `{"version": "1"}`
	os.WriteFile(filepath.Join(projDir, "synapses.json"), []byte(projData), 0o644)

	if err := configSetProject("brain.enabled", "true", projDir); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(projDir, "synapses.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	json.Unmarshal(data, &m)
	var brain map[string]json.RawMessage
	json.Unmarshal(m["brain"], &brain)
	if string(brain["enabled"]) != "true" {
		t.Errorf("brain.enabled = %s, want true", brain["enabled"])
	}
}

func TestFormatJSON(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{`"hello"`, "hello"},
		{`true`, "true"},
		{`false`, "false"},
		{`42`, "42"},
		{`3.14`, "3.14"},
	}
	for _, tt := range tests {
		got := formatJSON(json.RawMessage(tt.in))
		if got != tt.want {
			t.Errorf("formatJSON(%s) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
