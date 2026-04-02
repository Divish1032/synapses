package security

import (
	"testing"
)

// ── PatternType.Validate ──────────────────────────────────────────────────────

func TestPatternTypeValidate_KnownValues(t *testing.T) {
	valid := []PatternType{
		PatternTypeAuthMiddleware,
		PatternTypeRateLimiting,
		PatternTypeInputValidation,
		PatternTypeCSRFProtection,
		PatternTypeHardcodedSecret,
		PatternTypeLayerViolation,
		PatternTypeAdminElevation,
	}
	for _, pt := range valid {
		if err := pt.Validate(); err != nil {
			t.Errorf("PatternType(%q).Validate() unexpected error: %v", pt, err)
		}
	}
}

func TestPatternTypeValidate_UnknownValue(t *testing.T) {
	if err := PatternType("sql_injection").Validate(); err == nil {
		t.Error("expected error for unknown pattern_type, got nil")
	}
}

func TestPatternTypeValidate_Empty(t *testing.T) {
	if err := PatternType("").Validate(); err == nil {
		t.Error("expected error for empty pattern_type, got nil")
	}
}

// ── Severity.Validate ─────────────────────────────────────────────────────────

func TestSeverityValidate_KnownValues(t *testing.T) {
	for _, s := range []Severity{SeverityCritical, SeverityHigh, SeverityMedium} {
		if err := s.Validate(); err != nil {
			t.Errorf("Severity(%q).Validate() unexpected error: %v", s, err)
		}
	}
}

func TestSeverityValidate_LowercaseRejected(t *testing.T) {
	// Severity values are SCREAMING_CASE; lowercase should fail.
	if err := Severity("critical").Validate(); err == nil {
		t.Error("expected error for lowercase 'critical', got nil")
	}
}

func TestSeverityValidate_UnknownValue(t *testing.T) {
	if err := Severity("INFO").Validate(); err == nil {
		t.Error("expected error for unknown severity INFO, got nil")
	}
}

// ── CheckType.Validate ────────────────────────────────────────────────────────

func TestCheckTypeValidate_KnownValues(t *testing.T) {
	valid := []CheckType{
		CheckTypeMissingMiddleware,
		CheckTypeDirectImport,
		CheckTypeMissingAnnotation,
		CheckTypeHardcodedSecret,
		CheckTypeAdminElevation,
		CheckTypeCrossTransportAuth,
	}
	for _, ct := range valid {
		if err := ct.Validate(); err != nil {
			t.Errorf("CheckType(%q).Validate() unexpected error: %v", ct, err)
		}
	}
}

func TestCheckTypeValidate_UnknownValue(t *testing.T) {
	if err := CheckType("ast_scan").Validate(); err == nil {
		t.Error("expected error for unknown check_type, got nil")
	}
}

// ── SecurityPattern.Validate ──────────────────────────────────────────────────

func validPattern() SecurityPattern {
	return SecurityPattern{
		ID:          "go-chi-missing-auth",
		Name:        "Chi route missing auth",
		Language:    "go",
		Framework:   "chi",
		PatternType: PatternTypeAuthMiddleware,
		Severity:    SeverityCritical,
		Description: "A chi route lacks auth middleware.",
		Detection:   Detection{CheckType: CheckTypeMissingMiddleware},
		Message:     "Route {target} in {file} missing auth.",
	}
}

func TestSecurityPatternValidate_Valid(t *testing.T) {
	p := validPattern()
	if err := p.Validate(); err != nil {
		t.Errorf("Validate() unexpected error for valid pattern: %v", err)
	}
}

func TestSecurityPatternValidate_MissingID(t *testing.T) {
	p := validPattern()
	p.ID = ""
	if err := p.Validate(); err == nil {
		t.Error("expected error for missing ID")
	}
}

func TestSecurityPatternValidate_MissingName(t *testing.T) {
	p := validPattern()
	p.Name = ""
	if err := p.Validate(); err == nil {
		t.Error("expected error for missing Name")
	}
}

func TestSecurityPatternValidate_MissingLanguage(t *testing.T) {
	p := validPattern()
	p.Language = ""
	if err := p.Validate(); err == nil {
		t.Error("expected error for missing Language")
	}
}

func TestSecurityPatternValidate_MissingFramework(t *testing.T) {
	p := validPattern()
	p.Framework = ""
	if err := p.Validate(); err == nil {
		t.Error("expected error for missing Framework")
	}
}

func TestSecurityPatternValidate_InvalidPatternType(t *testing.T) {
	p := validPattern()
	p.PatternType = "bad_type"
	if err := p.Validate(); err == nil {
		t.Error("expected error for invalid PatternType")
	}
}

func TestSecurityPatternValidate_InvalidSeverity(t *testing.T) {
	p := validPattern()
	p.Severity = "LOW"
	if err := p.Validate(); err == nil {
		t.Error("expected error for invalid Severity")
	}
}

func TestSecurityPatternValidate_MissingDescription(t *testing.T) {
	p := validPattern()
	p.Description = ""
	if err := p.Validate(); err == nil {
		t.Error("expected error for missing Description")
	}
}

func TestSecurityPatternValidate_MissingMessage(t *testing.T) {
	p := validPattern()
	p.Message = ""
	if err := p.Validate(); err == nil {
		t.Error("expected error for missing Message")
	}
}

func TestSecurityPatternValidate_MissingCheckType(t *testing.T) {
	p := validPattern()
	p.Detection.CheckType = ""
	if err := p.Validate(); err == nil {
		t.Error("expected error for missing Detection.CheckType")
	}
}

func TestSecurityPatternValidate_InvalidCheckType(t *testing.T) {
	p := validPattern()
	p.Detection.CheckType = "unknown_check"
	if err := p.Validate(); err == nil {
		t.Error("expected error for invalid Detection.CheckType")
	}
}

func TestSecurityPatternValidate_WildcardLanguageAllowed(t *testing.T) {
	p := validPattern()
	p.Language = "*"
	if err := p.Validate(); err != nil {
		t.Errorf("wildcard language should be valid, got error: %v", err)
	}
}

func TestSecurityPatternValidate_WildcardFrameworkAllowed(t *testing.T) {
	p := validPattern()
	p.Framework = "*"
	if err := p.Validate(); err != nil {
		t.Errorf("wildcard framework should be valid, got error: %v", err)
	}
}

// ── SecurityPattern.IsEnabled ─────────────────────────────────────────────────

func TestIsEnabled_NilDefaultsToTrue(t *testing.T) {
	p := validPattern()
	// p.Enabled is nil
	if !p.IsEnabled() {
		t.Error("nil Enabled should default to true")
	}
}

func TestIsEnabled_ExplicitTrue(t *testing.T) {
	p := validPattern()
	b := true
	p.Enabled = &b
	if !p.IsEnabled() {
		t.Error("explicit enabled:true should return true")
	}
}

func TestIsEnabled_ExplicitFalse(t *testing.T) {
	p := validPattern()
	b := false
	p.Enabled = &b
	if p.IsEnabled() {
		t.Error("explicit enabled:false should return false")
	}
}

// ── PatternSet construction ───────────────────────────────────────────────────

func TestPatternSet_NilSafe(t *testing.T) {
	var ps *PatternSet
	if ps.Len() != 0 {
		t.Error("nil PatternSet.Len() should return 0")
	}
	if ps.All() != nil {
		t.Error("nil PatternSet.All() should return nil")
	}
	if ps.ForLanguage("go") != nil {
		t.Error("nil PatternSet.ForLanguage() should return nil")
	}
	if ps.ForFramework("chi") != nil {
		t.Error("nil PatternSet.ForFramework() should return nil")
	}
	if ps.ForCheckType(CheckTypeMissingMiddleware) != nil {
		t.Error("nil PatternSet.ForCheckType() should return nil")
	}
	if _, found := ps.ByID("any"); found {
		t.Error("nil PatternSet.ByID() should return false")
	}
}

func makePatternSet(patterns ...SecurityPattern) *PatternSet {
	return newPatternSet(patterns)
}

func TestPatternSet_Len(t *testing.T) {
	ps := makePatternSet(validPattern(), validPattern())
	if ps.Len() != 2 {
		t.Errorf("Len() = %d, want 2", ps.Len())
	}
}

func TestPatternSet_All_ExcludesDisabled(t *testing.T) {
	enabled := validPattern()
	disabled := validPattern()
	disabled.ID = "disabled-pattern"
	f := false
	disabled.Enabled = &f

	ps := makePatternSet(enabled, disabled)
	all := ps.All()
	if len(all) != 1 {
		t.Errorf("All() returned %d patterns, want 1 (disabled excluded)", len(all))
	}
	if all[0].ID != enabled.ID {
		t.Errorf("All() returned %q, want %q", all[0].ID, enabled.ID)
	}
}

func TestPatternSet_ForLanguage_MatchesExact(t *testing.T) {
	p1 := validPattern()
	p1.Language = "go"
	p2 := validPattern()
	p2.ID = "ts-pattern"
	p2.Language = "typescript"

	ps := makePatternSet(p1, p2)
	goPatterns := ps.ForLanguage("go")
	if len(goPatterns) != 1 || goPatterns[0].ID != p1.ID {
		t.Errorf("ForLanguage(go) = %v, want [%s]", goPatterns, p1.ID)
	}
}

func TestPatternSet_ForLanguage_CaseInsensitive(t *testing.T) {
	p := validPattern()
	p.Language = "Go"

	ps := makePatternSet(p)
	// Query with lowercase "go" should still match "Go".
	if results := ps.ForLanguage("go"); len(results) != 1 {
		t.Errorf("ForLanguage(go) with Language=Go: got %d, want 1", len(results))
	}
}

func TestPatternSet_ForLanguage_WildcardMatches(t *testing.T) {
	p := validPattern()
	p.Language = "*"

	ps := makePatternSet(p)
	// Wildcard pattern should match any language query.
	for _, lang := range []string{"go", "python", "typescript", "java", "rust"} {
		if results := ps.ForLanguage(lang); len(results) != 1 {
			t.Errorf("ForLanguage(%s) should match wildcard pattern, got %d", lang, len(results))
		}
	}
}

func TestPatternSet_ForFramework_MatchesExact(t *testing.T) {
	p1 := validPattern()
	p1.Framework = "chi"
	p2 := validPattern()
	p2.ID = "gin-pattern"
	p2.Framework = "gin"

	ps := makePatternSet(p1, p2)
	chiPatterns := ps.ForFramework("chi")
	if len(chiPatterns) != 1 || chiPatterns[0].Framework != "chi" {
		t.Errorf("ForFramework(chi) = %v, want [chi]", chiPatterns)
	}
}

func TestPatternSet_ForFramework_WildcardMatches(t *testing.T) {
	p := validPattern()
	p.Framework = "*"
	ps := makePatternSet(p)

	for _, fw := range []string{"chi", "gin", "echo", "express", "fastapi", "spring"} {
		if results := ps.ForFramework(fw); len(results) != 1 {
			t.Errorf("ForFramework(%s) should match wildcard, got %d", fw, len(results))
		}
	}
}

func TestPatternSet_ForLanguageAndFramework(t *testing.T) {
	goGin := validPattern()
	goGin.ID = "go-gin-auth"
	goGin.Language = "go"
	goGin.Framework = "gin"

	goWild := validPattern()
	goWild.ID = "go-all-secret"
	goWild.Language = "go"
	goWild.Framework = "*"

	pyFast := validPattern()
	pyFast.ID = "py-fastapi-auth"
	pyFast.Language = "python"
	pyFast.Framework = "fastapi"

	ps := makePatternSet(goGin, goWild, pyFast)

	// go+gin should match go-gin-auth and go-all-secret.
	goGinResults := ps.ForLanguageAndFramework("go", "gin")
	if len(goGinResults) != 2 {
		t.Errorf("ForLanguageAndFramework(go,gin): got %d, want 2", len(goGinResults))
	}

	// python+fastapi should match only py-fastapi-auth.
	pyResults := ps.ForLanguageAndFramework("python", "fastapi")
	if len(pyResults) != 1 || pyResults[0].ID != "py-fastapi-auth" {
		t.Errorf("ForLanguageAndFramework(python,fastapi): got %v, want [py-fastapi-auth]", pyResults)
	}

	// go+echo should match only go-all-secret (wildcard framework).
	goEchoResults := ps.ForLanguageAndFramework("go", "echo")
	if len(goEchoResults) != 1 || goEchoResults[0].ID != "go-all-secret" {
		t.Errorf("ForLanguageAndFramework(go,echo): got %v, want [go-all-secret]", goEchoResults)
	}
}

func TestPatternSet_ForCheckType(t *testing.T) {
	p1 := validPattern()
	p1.Detection.CheckType = CheckTypeMissingMiddleware

	p2 := validPattern()
	p2.ID = "import-check"
	p2.Detection.CheckType = CheckTypeDirectImport

	ps := makePatternSet(p1, p2)
	results := ps.ForCheckType(CheckTypeMissingMiddleware)
	if len(results) != 1 || results[0].ID != p1.ID {
		t.Errorf("ForCheckType(missing_middleware) = %v, want [%s]", results, p1.ID)
	}
}

func TestPatternSet_ForPatternType(t *testing.T) {
	p1 := validPattern()
	p1.PatternType = PatternTypeAuthMiddleware

	p2 := validPattern()
	p2.ID = "secret-pattern"
	p2.PatternType = PatternTypeHardcodedSecret
	p2.Detection.CheckType = CheckTypeHardcodedSecret

	ps := makePatternSet(p1, p2)
	results := ps.ForPatternType(PatternTypeHardcodedSecret)
	if len(results) != 1 || results[0].ID != "secret-pattern" {
		t.Errorf("ForPatternType(hardcoded_secret) = %v, want [secret-pattern]", results)
	}
}

func TestPatternSet_ByID_Found(t *testing.T) {
	p := validPattern()
	ps := makePatternSet(p)

	got, ok := ps.ByID(p.ID)
	if !ok {
		t.Fatalf("ByID(%q) not found", p.ID)
	}
	if got.ID != p.ID {
		t.Errorf("ByID(%q) = %q, want %q", p.ID, got.ID, p.ID)
	}
}

func TestPatternSet_ByID_NotFound(t *testing.T) {
	ps := makePatternSet(validPattern())
	_, ok := ps.ByID("nonexistent-pattern")
	if ok {
		t.Error("ByID(nonexistent) should return false")
	}
}

func TestPatternSet_ByID_ReturnsDisabled(t *testing.T) {
	// ByID should return even disabled patterns — callers need to check IsEnabled().
	p := validPattern()
	f := false
	p.Enabled = &f

	ps := makePatternSet(p)
	got, ok := ps.ByID(p.ID)
	if !ok {
		t.Fatal("ByID should find disabled patterns")
	}
	if got.IsEnabled() {
		t.Error("found pattern should be disabled")
	}
}

// ── equalFold (internal helper) ───────────────────────────────────────────────

func TestEqualFold(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"go", "go", true},
		{"go", "Go", true},
		{"Go", "go", true},
		{"GO", "go", true},
		{"chi", "CHI", true},
		{"go", "python", false},
		{"go", "goo", false},
		{"", "", true},
		{"a", "", false},
	}
	for _, tc := range cases {
		if got := equalFold(tc.a, tc.b); got != tc.want {
			t.Errorf("equalFold(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
