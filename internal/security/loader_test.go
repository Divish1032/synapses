package security

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// ── loadPatternFile ───────────────────────────────────────────────────────────

func TestLoadPatternFile_SingleObject(t *testing.T) {
	p := validPattern()
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	patterns, err := loadPatternFile(data, "test.json")
	if err != nil {
		t.Fatalf("loadPatternFile: %v", err)
	}
	if len(patterns) != 1 || patterns[0].ID != p.ID {
		t.Errorf("got %v, want [%s]", patterns, p.ID)
	}
}

func TestLoadPatternFile_ArrayFormat(t *testing.T) {
	p1 := validPattern()
	p2 := validPattern()
	p2.ID = "second-pattern"

	data, err := json.Marshal([]SecurityPattern{p1, p2})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	patterns, err := loadPatternFile(data, "test.json")
	if err != nil {
		t.Fatalf("loadPatternFile: %v", err)
	}
	if len(patterns) != 2 {
		t.Errorf("got %d patterns, want 2", len(patterns))
	}
}

func TestLoadPatternFile_EnvelopeFormat(t *testing.T) {
	p1 := validPattern()
	p2 := validPattern()
	p2.ID = "envelope-second"

	envelope := patternFileMulti{Patterns: []SecurityPattern{p1, p2}}
	data, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	patterns, err := loadPatternFile(data, "test.json")
	if err != nil {
		t.Fatalf("loadPatternFile: %v", err)
	}
	if len(patterns) != 2 {
		t.Errorf("got %d patterns, want 2", len(patterns))
	}
}

func TestLoadPatternFile_Empty(t *testing.T) {
	patterns, err := loadPatternFile([]byte(""), "empty.json")
	if err != nil {
		t.Errorf("empty input should not error, got: %v", err)
	}
	if len(patterns) != 0 {
		t.Errorf("empty input should return 0 patterns, got %d", len(patterns))
	}
}

func TestLoadPatternFile_Whitespace(t *testing.T) {
	patterns, err := loadPatternFile([]byte("   \n\t  "), "ws.json")
	if err != nil {
		t.Errorf("whitespace input should not error, got: %v", err)
	}
	if len(patterns) != 0 {
		t.Errorf("whitespace input should return 0 patterns, got %d", len(patterns))
	}
}

func TestLoadPatternFile_MalformedJSON(t *testing.T) {
	_, err := loadPatternFile([]byte("{invalid json}"), "bad.json")
	if err == nil {
		t.Error("malformed JSON should return error")
	}
}

func TestLoadPatternFile_ObjectMissingID(t *testing.T) {
	// A JSON object with no "id" and no "patterns" key is invalid.
	_, err := loadPatternFile([]byte(`{"foo": "bar"}`), "noid.json")
	if err == nil {
		t.Error("object missing id field should return error")
	}
}

func TestLoadPatternFile_UnexpectedStartChar(t *testing.T) {
	_, err := loadPatternFile([]byte(`"just a string"`), "str.json")
	if err == nil {
		t.Error("unexpected start character should return error")
	}
}

// ── validateAndFilter ─────────────────────────────────────────────────────────

func TestValidateAndFilter_Valid(t *testing.T) {
	p := validPattern()
	out, err := validateAndFilter([]SecurityPattern{p}, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 {
		t.Errorf("got %d patterns, want 1", len(out))
	}
}

func TestValidateAndFilter_Invalid(t *testing.T) {
	bad := validPattern()
	bad.ID = "" // missing required field
	_, err := validateAndFilter([]SecurityPattern{bad}, "test")
	if err == nil {
		t.Error("invalid pattern should return error")
	}
}

func TestValidateAndFilter_KeepsDisabled(t *testing.T) {
	// validateAndFilter should keep disabled patterns (IsEnabled check is in PatternSet methods).
	p := validPattern()
	f := false
	p.Enabled = &f
	out, err := validateAndFilter([]SecurityPattern{p}, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 {
		t.Errorf("got %d patterns, want 1 (disabled but valid)", len(out))
	}
}

// ── LoadBuiltin ───────────────────────────────────────────────────────────────

func TestLoadBuiltin_ReturnsPatterns(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	if ps == nil {
		t.Fatal("LoadBuiltin returned nil PatternSet")
	}
	if ps.Len() == 0 {
		t.Error("LoadBuiltin returned empty PatternSet — expected at least one built-in pattern")
	}
}

func TestLoadBuiltin_AllPatternsValid(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	// All patterns must have passed Validate() during loading.
	// Verify we can query them without panic.
	_ = ps.All()
	_ = ps.ForLanguage("go")
	_ = ps.ForLanguage("*")
}

func TestLoadBuiltin_ContainsGoGenericPatterns(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	_, ok := ps.ByID("go-generic-hardcoded-secret")
	if !ok {
		t.Error("expected go-generic-hardcoded-secret in built-in patterns")
	}
	_, ok = ps.ByID("go-generic-direct-db-import")
	if !ok {
		t.Error("expected go-generic-direct-db-import in built-in patterns")
	}
}

func TestLoadBuiltin_ContainsGenericPatterns(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	_, ok := ps.ByID("generic-admin-elevation")
	if !ok {
		t.Error("expected generic-admin-elevation in built-in patterns")
	}
}

func TestLoadBuiltin_SeveritiesAreValid(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	for _, p := range ps.All() {
		if err := p.Severity.Validate(); err != nil {
			t.Errorf("pattern %q has invalid severity: %v", p.ID, err)
		}
	}
}

func TestLoadBuiltin_PatternTypesAreValid(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	for _, p := range ps.All() {
		if err := p.PatternType.Validate(); err != nil {
			t.Errorf("pattern %q has invalid pattern_type: %v", p.ID, err)
		}
	}
}

// ── LoadDir ───────────────────────────────────────────────────────────────────

func TestLoadDir_NonexistentDir(t *testing.T) {
	ps, err := LoadDir("/tmp/synapses-test-nonexistent-999999")
	if err != nil {
		t.Fatalf("LoadDir with nonexistent dir should not error, got: %v", err)
	}
	if ps == nil || ps.Len() != 0 {
		t.Error("LoadDir with nonexistent dir should return empty PatternSet")
	}
}

func TestLoadDir_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	ps, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir empty dir: %v", err)
	}
	if ps.Len() != 0 {
		t.Errorf("empty dir should return 0 patterns, got %d", ps.Len())
	}
}

func TestLoadDir_EmptyString(t *testing.T) {
	ps, err := LoadDir("")
	if err != nil {
		t.Fatalf("LoadDir empty string: %v", err)
	}
	if ps.Len() != 0 {
		t.Error("empty string dir should return empty PatternSet")
	}
}

func TestLoadDir_SinglePatternFile(t *testing.T) {
	dir := t.TempDir()
	p := validPattern()
	data, _ := json.Marshal(p)
	if err := os.WriteFile(filepath.Join(dir, "test.json"), data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	ps, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if ps.Len() != 1 {
		t.Errorf("got %d patterns, want 1", ps.Len())
	}
	if _, ok := ps.ByID(p.ID); !ok {
		t.Errorf("pattern %q not found", p.ID)
	}
}

func TestLoadDir_NonJSONFilesIgnored(t *testing.T) {
	dir := t.TempDir()
	// Write a YAML file — should be ignored.
	if err := os.WriteFile(filepath.Join(dir, "patterns.yaml"), []byte("id: test\n"), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	// Write a README — should be ignored.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# patterns"), 0o644); err != nil {
		t.Fatalf("write readme: %v", err)
	}

	ps, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if ps.Len() != 0 {
		t.Errorf("non-JSON files should be ignored, got %d patterns", ps.Len())
	}
}

func TestLoadDir_MalformedJSONErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{invalid}"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := LoadDir(dir)
	if err == nil {
		t.Error("malformed JSON file should return error")
	}
}

func TestLoadDir_InvalidPatternErrors(t *testing.T) {
	dir := t.TempDir()
	bad := validPattern()
	bad.Severity = "INVALID_SEVERITY"
	data, _ := json.Marshal(bad)
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := LoadDir(dir)
	if err == nil {
		t.Error("invalid pattern field should return error")
	}
}

func TestLoadDir_MultipleFiles(t *testing.T) {
	dir := t.TempDir()
	p1 := validPattern()
	p1.ID = "pattern-file-1"
	p2 := validPattern()
	p2.ID = "pattern-file-2"

	data1, _ := json.Marshal(p1)
	data2, _ := json.Marshal(p2)
	_ = os.WriteFile(filepath.Join(dir, "a.json"), data1, 0o644)
	_ = os.WriteFile(filepath.Join(dir, "b.json"), data2, 0o644)

	ps, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if ps.Len() != 2 {
		t.Errorf("got %d patterns, want 2", ps.Len())
	}
}

// ── LoadAll ───────────────────────────────────────────────────────────────────

func TestLoadAll_NoExtraDirs(t *testing.T) {
	ps, err := LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	// Should contain all built-in patterns.
	builtin, _ := LoadBuiltin()
	if ps.Len() != builtin.Len() {
		t.Errorf("LoadAll() without extra dirs: got %d, want %d (same as built-in)", ps.Len(), builtin.Len())
	}
}

func TestLoadAll_EmptyExtraDir(t *testing.T) {
	ps, err := LoadAll("")
	if err != nil {
		t.Fatalf("LoadAll with empty string: %v", err)
	}
	builtin, _ := LoadBuiltin()
	if ps.Len() != builtin.Len() {
		t.Errorf("empty extra dir should not change pattern count: got %d, want %d", ps.Len(), builtin.Len())
	}
}

func TestLoadAll_UserPatternsMerged(t *testing.T) {
	dir := t.TempDir()
	userPattern := validPattern()
	userPattern.ID = "user-custom-pattern"
	data, _ := json.Marshal(userPattern)
	_ = os.WriteFile(filepath.Join(dir, "custom.json"), data, 0o644)

	ps, err := LoadAll(dir)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	_, ok := ps.ByID("user-custom-pattern")
	if !ok {
		t.Error("user custom pattern should be present after merge")
	}

	// Built-in patterns should also be present.
	_, ok = ps.ByID("go-generic-hardcoded-secret")
	if !ok {
		t.Error("built-in pattern should still be present after merge")
	}
}

func TestLoadAll_UserOverridesBuiltin(t *testing.T) {
	dir := t.TempDir()

	// Override the built-in go-generic-hardcoded-secret with a lower severity.
	override := validPattern()
	override.ID = "go-generic-hardcoded-secret"
	override.Severity = SeverityHigh // changed from CRITICAL to HIGH
	data, _ := json.Marshal(override)
	_ = os.WriteFile(filepath.Join(dir, "overrides.json"), data, 0o644)

	ps, err := LoadAll(dir)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	got, ok := ps.ByID("go-generic-hardcoded-secret")
	if !ok {
		t.Fatal("overridden pattern not found")
	}
	if got.Severity != SeverityHigh {
		t.Errorf("user override should take precedence: got severity %q, want HIGH", got.Severity)
	}
}

func TestLoadAll_UserDisablesBuiltin(t *testing.T) {
	dir := t.TempDir()

	// Disable the built-in go-generic-hardcoded-secret pattern.
	// Must include all required fields since Validate() runs.
	override := validPattern()
	override.ID = "go-generic-hardcoded-secret"
	f := false
	override.Enabled = &f
	data, _ := json.Marshal(override)
	_ = os.WriteFile(filepath.Join(dir, "disable.json"), data, 0o644)

	ps, err := LoadAll(dir)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	// ByID includes disabled, but All() should exclude it.
	got, ok := ps.ByID("go-generic-hardcoded-secret")
	if !ok {
		t.Fatal("pattern should still be findable by ID even when disabled")
	}
	if got.IsEnabled() {
		t.Error("disabled pattern should not be enabled")
	}

	for _, p := range ps.All() {
		if p.ID == "go-generic-hardcoded-secret" {
			t.Error("disabled pattern should not appear in All()")
		}
	}
}

func TestLoadAll_MultipleExtraDirs_LastWins(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	// Both dirs override the same pattern ID with different severities.
	p1 := validPattern()
	p1.ID = "contested-pattern"
	p1.Severity = SeverityHigh
	data1, _ := json.Marshal(p1)
	_ = os.WriteFile(filepath.Join(dir1, "p1.json"), data1, 0o644)

	p2 := validPattern()
	p2.ID = "contested-pattern"
	p2.Severity = SeverityMedium
	data2, _ := json.Marshal(p2)
	_ = os.WriteFile(filepath.Join(dir2, "p2.json"), data2, 0o644)

	ps, err := LoadAll(dir1, dir2)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	got, ok := ps.ByID("contested-pattern")
	if !ok {
		t.Fatal("contested pattern not found")
	}
	// dir2 is applied second, so it wins.
	if got.Severity != SeverityMedium {
		t.Errorf("last extra dir should win: got %q, want MEDIUM", got.Severity)
	}
}

// ── JSON round-trip (format compatibility) ────────────────────────────────────

func TestSecurityPattern_JSONRoundTrip(t *testing.T) {
	original := SecurityPattern{
		ID:          "test-round-trip",
		Name:        "Test pattern",
		Language:    "go",
		Framework:   "chi",
		PatternType: PatternTypeAuthMiddleware,
		Severity:    SeverityCritical,
		Description: "Test description.",
		Detection: Detection{
			CheckType:            CheckTypeMissingMiddleware,
			FrameworkIdentifiers: []string{"github.com/go-chi/chi/v5"},
			RequiredCallPatterns: []string{"Auth*", "JWT*"},
			RouteNodeNames:       []string{"Get", "Post"},
			MiddlewareNodeNames:  []string{"Use"},
			Scope:                ScopeFile,
		},
		Message: "Route {target} missing auth.",
		Tags:    []string{"owasp-a01", "auth"},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded SecurityPattern
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.ID != original.ID {
		t.Errorf("ID: got %q, want %q", decoded.ID, original.ID)
	}
	if decoded.Severity != original.Severity {
		t.Errorf("Severity: got %q, want %q", decoded.Severity, original.Severity)
	}
	if decoded.Detection.CheckType != original.Detection.CheckType {
		t.Errorf("Detection.CheckType: got %q, want %q", decoded.Detection.CheckType, original.Detection.CheckType)
	}
	if len(decoded.Detection.RequiredCallPatterns) != len(original.Detection.RequiredCallPatterns) {
		t.Errorf("RequiredCallPatterns length: got %d, want %d",
			len(decoded.Detection.RequiredCallPatterns),
			len(original.Detection.RequiredCallPatterns))
	}
	if decoded.Detection.Scope != original.Detection.Scope {
		t.Errorf("Detection.Scope: got %q, want %q", decoded.Detection.Scope, original.Detection.Scope)
	}
}

func TestSecurityPattern_EnabledOmitEmpty(t *testing.T) {
	// enabled field should be absent when nil (omitempty).
	p := validPattern()
	// p.Enabled is nil
	data, _ := json.Marshal(p)
	var raw map[string]interface{}
	_ = json.Unmarshal(data, &raw)
	if _, ok := raw["enabled"]; ok {
		t.Error("nil Enabled should be omitted from JSON (omitempty)")
	}
}
