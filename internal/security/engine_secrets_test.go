package security

import (
	"strings"
	"testing"
)

// ──────────────────────────────────────────────────────────────────────────────
// Helper: single hardcoded-secret pattern with optional detect_fallback
// ──────────────────────────────────────────────────────────────────────────────

func makeSecretPattern(secretPatterns []string, detectFallback bool, lang string) SecurityPattern {
	b := true
	return SecurityPattern{
		ID:          "test-secret",
		Name:        "Test Hardcoded Secret",
		Language:    lang,
		Framework:   "*",
		PatternType: PatternTypeHardcodedSecret,
		Severity:    SeverityCritical,
		Description: "test",
		Message:     "Variable '{target}' in {file} contains a hardcoded credential",
		Enabled:     &b,
		Detection: Detection{
			CheckType:      CheckTypeHardcodedSecret,
			SecretPatterns: secretPatterns,
			DetectFallback: detectFallback,
		},
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// isPlaceholderValue — unit tests
// ──────────────────────────────────────────────────────────────────────────────

func TestIsPlaceholderValue_ShortValue(t *testing.T) {
	if !isPlaceholderValue("abc") {
		t.Error("values shorter than 6 chars should be placeholders")
	}
}

func TestIsPlaceholderValue_PlaceholderWords(t *testing.T) {
	cases := []string{
		"test", "example", "placeholder", "dummy", "fake", "changeme",
		"change-me", "your-secret", "enter-your-key", "replace-with-real",
		"sample", "default", "none", "null", "todo",
	}
	for _, c := range cases {
		if !isPlaceholderValue(c) {
			t.Errorf("isPlaceholderValue(%q) = false, want true", c)
		}
	}
}

func TestIsPlaceholderValue_RepeatedChars(t *testing.T) {
	if !isPlaceholderValue("aaaaaaa") {
		t.Error("all-same-char string should be placeholder")
	}
	if !isPlaceholderValue("xxxxxxxx") {
		t.Error("all-same-char string should be placeholder")
	}
}

func TestIsPlaceholderValue_RealSecretNotPlaceholder(t *testing.T) {
	realSecrets := []string{
		"sk-abcdefghijklmnopqrstuvwxyzABCDEF",
		"AKIAIOSFODNN7EXAMPLE",
		"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
		"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	}
	for _, s := range realSecrets {
		if isPlaceholderValue(s) {
			t.Errorf("isPlaceholderValue(%q) = true, want false (real secret)", s)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// checkHardcodedSecret — extended value patterns
// ──────────────────────────────────────────────────────────────────────────────

func TestCheckHardcodedSecret_AWSAccessKey(t *testing.T) {
	p := makeSecretPattern([]string{`^AKIA[0-9A-Z]{16}$`}, false, "go")
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/config/aws.go")

	content := []byte(`package config

var accessKey = "AKIAIOSFODNN7EXAMPLE"
`)
	violations := e.CheckFile(g, "/project/config/aws.go", content)
	if len(violations) == 0 {
		t.Error("expected violation: AWS access key assigned to accessKey variable")
	}
	if len(violations) > 0 && violations[0].Severity != SeverityCritical {
		t.Errorf("expected CRITICAL severity, got %s", violations[0].Severity)
	}
}

func TestCheckHardcodedSecret_StripeKey(t *testing.T) {
	p := makeSecretPattern([]string{`^sk_live_[a-zA-Z0-9]{24,}$`}, false, "go")
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/payments/stripe.go")

	content := []byte(`package payments

var apiKey = "sk_live_aBcDeFgHiJkLmNoPqRsTuVwXyZ123456"
`)
	violations := e.CheckFile(g, "/project/payments/stripe.go", content)
	if len(violations) == 0 {
		t.Error("expected violation: Stripe live key assigned to apiKey variable")
	}
}

func TestCheckHardcodedSecret_DBConnectionString_Postgres(t *testing.T) {
	p := makeSecretPattern([]string{`(?i)^postgres(?:ql)?://[^:/]+:[^@/]{4,}@`}, false, "go")
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/db/connect.go")

	content := []byte(`package db

var dsn = "postgres://myuser:s3cr3tPass@localhost:5432/mydb"
`)
	violations := e.CheckFile(g, "/project/db/connect.go", content)
	if len(violations) == 0 {
		t.Error("expected violation: postgres DSN with embedded credentials assigned to dsn variable")
	}
	if len(violations) > 0 && violations[0].Target != "dsn" {
		t.Errorf("expected target 'dsn', got %q", violations[0].Target)
	}
}

func TestCheckHardcodedSecret_DBConnectionString_MySQL(t *testing.T) {
	p := makeSecretPattern([]string{`(?i)^mysql://[^:/]+:[^@/]{4,}@`}, false, "go")
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/db/mysql.go")

	content := []byte(`package db

var connectionString = "mysql://root:myRealPassword@tcp(localhost:3306)/mydb"
`)
	violations := e.CheckFile(g, "/project/db/mysql.go", content)
	if len(violations) == 0 {
		t.Error("expected violation: MySQL DSN with embedded credentials assigned to connectionString")
	}
}

func TestCheckHardcodedSecret_DBCredentialVariable(t *testing.T) {
	// dbUrl and db_password are now in credentialVarRE — ensure they fire.
	p := makeSecretPattern([]string{`(?i)^[a-zA-Z0-9+/]{32,}={0,2}$`}, false, "go")
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/config/config.go")

	content := []byte(`package config

var dbPassword = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
`)
	violations := e.CheckFile(g, "/project/config/config.go", content)
	if len(violations) == 0 {
		t.Error("expected violation: dbPassword is a credential variable with base64-like value")
	}
}

func TestCheckHardcodedSecret_SlackToken(t *testing.T) {
	p := makeSecretPattern([]string{`^xox[bprs]-[0-9A-Za-z-]{20,}$`}, false, "go")
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/notify/slack.go")

	content := []byte(`package notify

var token = "xoxb-1234567890-abcdefghijklmnopqrstuvwx"
`)
	violations := e.CheckFile(g, "/project/notify/slack.go", content)
	if len(violations) == 0 {
		t.Error("expected violation: Slack token assigned to token variable")
	}
}

func TestCheckHardcodedSecret_PlaceholderFiltered(t *testing.T) {
	// "changeme" and "placeholder" should not fire even if variable name matches.
	p := makeSecretPattern([]string{`(?i)^[a-zA-Z0-9+/]{32,}={0,2}$`}, false, "go")
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/config/config.go")

	content := []byte(`package config

var jwtSecret = "changeme"
var apiKey = "example-key-not-real"
var password = "placeholder"
`)
	violations := e.CheckFile(g, "/project/config/config.go", content)
	if violations != nil {
		t.Errorf("expected no violations for placeholder values, got %v", violations)
	}
}

func TestCheckHardcodedSecret_SingleQuoteString_Python(t *testing.T) {
	// Python uses single-quoted strings — the updated stringLiteralRE must catch them.
	p := makeSecretPattern([]string{`^sk-[a-zA-Z0-9]{32,}$`}, false, "python")
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/app/config.py")

	content := []byte(`import os

api_key = 'sk-abcdefghijklmnopqrstuvwxyzABCDEFGH'
`)
	violations := e.CheckFile(g, "/project/app/config.py", content)
	if len(violations) == 0 {
		t.Error("expected violation: single-quoted OpenAI key in Python file")
	}
}

func TestCheckHardcodedSecret_SingleQuoteString_TypeScript(t *testing.T) {
	p := makeSecretPattern([]string{`(?i)^[a-zA-Z0-9+/]{32,}={0,2}$`}, false, "typescript")
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/src/config.ts")

	content := []byte(`const jwtSecret = 'wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY'
`)
	violations := e.CheckFile(g, "/project/src/config.ts", content)
	if len(violations) == 0 {
		t.Error("expected violation: single-quoted base64 secret in TypeScript file")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// checkFallbackEnvPattern — Go multi-line
// ──────────────────────────────────────────────────────────────────────────────

func TestFallbackSecret_Go_MultiLine(t *testing.T) {
	p := makeSecretPattern(nil, true, "go")
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/auth/jwt.go")

	content := []byte(`package auth

func getSecret() string {
	jwtSecret := os.Getenv("JWT_SECRET")
	if jwtSecret == "" {
		jwtSecret = "super-real-production-secret-key"
	}
	return jwtSecret
}
`)
	violations := e.CheckFile(g, "/project/auth/jwt.go", content)
	if len(violations) == 0 {
		t.Error("expected violation: Go fallback credential in if-empty block")
	}
	if len(violations) > 0 && violations[0].Target != "jwtSecret" {
		t.Errorf("expected target 'jwtSecret', got %q", violations[0].Target)
	}
}

func TestFallbackSecret_Go_MultiLine_LookupEnv(t *testing.T) {
	p := makeSecretPattern(nil, true, "go")
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/auth/keys.go")

	content := []byte(`package auth

var apiSecret string

func init() {
	apiSecret, _ = os.LookupEnv("API_SECRET")
	if apiSecret == "" {
		apiSecret = "fallback-api-secret-value-xyz"
	}
}
`)
	violations := e.CheckFile(g, "/project/auth/keys.go", content)
	if len(violations) == 0 {
		t.Error("expected violation: fallback assignment to credential variable in Go")
	}
}

func TestFallbackSecret_Go_PlaceholderFallback_NoViolation(t *testing.T) {
	// Fallback value is a placeholder — should not fire.
	p := makeSecretPattern(nil, true, "go")
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/auth/jwt.go")

	content := []byte(`package auth

func getSecret() string {
	jwtSecret := os.Getenv("JWT_SECRET")
	if jwtSecret == "" {
		jwtSecret = "changeme"
	}
	return jwtSecret
}
`)
	violations := e.CheckFile(g, "/project/auth/jwt.go", content)
	if violations != nil {
		t.Errorf("expected no violations: fallback is a placeholder word, got %v", violations)
	}
}

func TestFallbackSecret_Go_NonCredentialVar_NoViolation(t *testing.T) {
	// Variable name doesn't match credentialVarRE — should not fire.
	p := makeSecretPattern(nil, true, "go")
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/config/env.go")

	content := []byte(`package config

func getRegion() string {
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-1"
	}
	return region
}
`)
	violations := e.CheckFile(g, "/project/config/env.go", content)
	if violations != nil {
		t.Errorf("expected no violations: 'region' is not a credential variable, got %v", violations)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// checkFallbackEnvPattern — TypeScript/JavaScript
// ──────────────────────────────────────────────────────────────────────────────

func TestFallbackSecret_TypeScript_OR_Fallback(t *testing.T) {
	p := makeSecretPattern(nil, true, "typescript")
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/src/config.ts")

	content := []byte(`const jwtSecret = process.env.JWT_SECRET || "super-secret-fallback-value"
`)
	violations := e.CheckFile(g, "/project/src/config.ts", content)
	if len(violations) == 0 {
		t.Error("expected violation: process.env fallback with hardcoded value in TypeScript")
	}
	if len(violations) > 0 && !strings.Contains(violations[0].Target, "process.env.JWT_SECRET") {
		t.Errorf("expected target to contain 'process.env.JWT_SECRET', got %q", violations[0].Target)
	}
}

func TestFallbackSecret_TypeScript_NullCoalesce_Fallback(t *testing.T) {
	p := makeSecretPattern(nil, true, "typescript")
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/src/auth.ts")

	content := []byte(`const apiKey = process.env.API_KEY ?? "hardcoded-api-key-value-xyz"
`)
	violations := e.CheckFile(g, "/project/src/auth.ts", content)
	if len(violations) == 0 {
		t.Error("expected violation: process.env null-coalesce fallback in TypeScript")
	}
}

func TestFallbackSecret_TypeScript_PlaceholderFallback_NoViolation(t *testing.T) {
	p := makeSecretPattern(nil, true, "typescript")
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/src/config.ts")

	content := []byte(`const jwtSecret = process.env.JWT_SECRET || "changeme"
`)
	violations := e.CheckFile(g, "/project/src/config.ts", content)
	if violations != nil {
		t.Errorf("expected no violations: fallback is placeholder, got %v", violations)
	}
}

func TestFallbackSecret_JavaScript_OR_Fallback(t *testing.T) {
	p := makeSecretPattern(nil, true, "javascript")
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/server.js")

	content := []byte(`const secret = process.env.SESSION_SECRET || "my-prod-session-secret-12345"
`)
	violations := e.CheckFile(g, "/project/server.js", content)
	if len(violations) == 0 {
		t.Error("expected violation: process.env fallback with hardcoded value in JavaScript")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// checkFallbackEnvPattern — Python
// ──────────────────────────────────────────────────────────────────────────────

func TestFallbackSecret_Python_GetenvDefault(t *testing.T) {
	p := makeSecretPattern(nil, true, "python")
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/app/settings.py")

	content := []byte(`import os

SECRET_KEY = os.environ.get("SECRET_KEY", "django-insecure-fallback-key-xyz")
`)
	violations := e.CheckFile(g, "/project/app/settings.py", content)
	if len(violations) == 0 {
		t.Error("expected violation: os.environ.get with hardcoded default in Python")
	}
}

func TestFallbackSecret_Python_GetenvOr(t *testing.T) {
	p := makeSecretPattern(nil, true, "python")
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/app/config.py")

	content := []byte(`import os

jwt_secret = os.getenv("JWT_SECRET") or "hardcoded-jwt-secret-production"
`)
	violations := e.CheckFile(g, "/project/app/config.py", content)
	if len(violations) == 0 {
		t.Error("expected violation: os.getenv() or fallback in Python")
	}
}

func TestFallbackSecret_Python_PlaceholderDefault_NoViolation(t *testing.T) {
	p := makeSecretPattern(nil, true, "python")
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/app/settings.py")

	content := []byte(`import os

SECRET_KEY = os.environ.get("SECRET_KEY", "changeme")
`)
	violations := e.CheckFile(g, "/project/app/settings.py", content)
	if violations != nil {
		t.Errorf("expected no violations: fallback is a placeholder, got %v", violations)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// checkFallbackEnvPattern — Java
// ──────────────────────────────────────────────────────────────────────────────

func TestFallbackSecret_Java_TernaryFallback(t *testing.T) {
	p := makeSecretPattern(nil, true, "java")
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/src/Config.java")

	content := []byte(`public class Config {
    private String jwtSecret = System.getenv("JWT_SECRET") != null ? System.getenv("JWT_SECRET") : "production-jwt-secret-key";
}
`)
	violations := e.CheckFile(g, "/project/src/Config.java", content)
	if len(violations) == 0 {
		t.Error("expected violation: Java System.getenv ternary fallback")
	}
}

func TestFallbackSecret_Java_OptionalFallback(t *testing.T) {
	p := makeSecretPattern(nil, true, "java")
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/src/Auth.java")

	content := []byte(`import java.util.Optional;

public class Auth {
    private static final String SECRET = Optional.ofNullable(System.getenv("JWT_SECRET")).orElse("real-jwt-production-secret");
}
`)
	violations := e.CheckFile(g, "/project/src/Auth.java", content)
	if len(violations) == 0 {
		t.Error("expected violation: Java Optional.ofNullable(...).orElse fallback")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// checkFallbackEnvPattern — Rust
// ──────────────────────────────────────────────────────────────────────────────

func TestFallbackSecret_Rust_UnwrapOr(t *testing.T) {
	p := makeSecretPattern(nil, true, "rust")
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/src/config.rs")

	content := []byte(`use std::env;

fn get_secret() -> String {
    env::var("JWT_SECRET").unwrap_or("production-jwt-secret-value".to_string())
}
`)
	violations := e.CheckFile(g, "/project/src/config.rs", content)
	if len(violations) == 0 {
		t.Error("expected violation: Rust env::var().unwrap_or fallback")
	}
}

func TestFallbackSecret_Rust_UnwrapOrElse_Closure(t *testing.T) {
	p := makeSecretPattern(nil, true, "rust")
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/src/auth.rs")

	content := []byte(`use std::env;

fn jwt_secret() -> String {
    env::var("JWT_SECRET").unwrap_or_else(|_| "production-jwt-secret-value")
}
`)
	violations := e.CheckFile(g, "/project/src/auth.rs", content)
	if len(violations) == 0 {
		t.Error("expected violation: Rust env::var().unwrap_or_else closure fallback")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Test file severity downgrade still works with new patterns
// ──────────────────────────────────────────────────────────────────────────────

func TestCheckHardcodedSecret_TestFile_DowngradesWithNewPatterns(t *testing.T) {
	p := makeSecretPattern([]string{`^AKIA[0-9A-Z]{16}$`}, false, "go")
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/config/aws_test.go")

	content := []byte(`package config

var testAccessKey = "AKIAIOSFODNN7EXAMPLE"
`)
	violations := e.CheckFile(g, "/project/config/aws_test.go", content)
	if len(violations) == 0 {
		t.Error("expected violation even in test file (downgraded, not suppressed)")
	}
	if len(violations) > 0 && violations[0].Severity != SeverityMedium {
		t.Errorf("expected MEDIUM severity for test file, got %s", violations[0].Severity)
	}
}

func TestFallbackSecret_TestFile_DowngradesWithFallback(t *testing.T) {
	p := makeSecretPattern(nil, true, "go")
	e := makeEngine(p)
	g := buildTestGraph(t)
	// Test file — should get MEDIUM not CRITICAL/HIGH
	addFileWithImports(g, "/project/auth/jwt_test.go")

	content := []byte(`package auth

func TestGetSecret(t *testing.T) {
	jwtSecret := os.Getenv("JWT_SECRET")
	if jwtSecret == "" {
		jwtSecret = "super-real-production-secret-key"
	}
}
`)
	violations := e.CheckFile(g, "/project/auth/jwt_test.go", content)
	// Violation should fire but severity is MEDIUM for test files
	if len(violations) > 0 && violations[0].Severity != SeverityMedium {
		t.Errorf("expected MEDIUM severity in test file, got %s", violations[0].Severity)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Multi-language patterns load from built-ins
// ──────────────────────────────────────────────────────────────────────────────

func TestBuiltinPatterns_MultiLanguageLoaded(t *testing.T) {
	e := DefaultEngine()
	// Count patterns per language to verify all language files were loaded.
	byLang := map[string]int{}
	for _, p := range e.patterns.All() {
		byLang[p.Language]++
	}
	wantLangs := []string{"go", "typescript", "javascript", "python", "java", "rust"}
	for _, lang := range wantLangs {
		if byLang[lang] == 0 {
			// "*" (wildcard) covers some, so check combined.
			if byLang["*"] == 0 && byLang[lang] == 0 {
				t.Errorf("no built-in patterns loaded for language %q", lang)
			}
		}
	}
}

func TestBuiltinPatterns_HardcodedSecretPatternExistsForAllLanguages(t *testing.T) {
	e := DefaultEngine()
	secretPatterns := e.patterns.ForCheckType(CheckTypeHardcodedSecret)
	if len(secretPatterns) == 0 {
		t.Fatal("no hardcoded_secret patterns loaded from built-ins")
	}

	// Must have at least one pattern per target language (or wildcard).
	targetIDs := []string{
		"go-generic-hardcoded-secret",
		"go-generic-fallback-secret",
		"ts-generic-hardcoded-secret",
		"ts-generic-fallback-secret",
		"js-generic-hardcoded-secret",
		"js-generic-fallback-secret",
		"python-generic-hardcoded-secret",
		"python-generic-fallback-secret",
		"java-generic-hardcoded-secret",
		"java-generic-fallback-secret",
		"rust-generic-hardcoded-secret",
		"rust-generic-fallback-secret",
	}
	for _, id := range targetIDs {
		p, ok := e.patterns.ByID(id)
		if !ok {
			t.Errorf("expected built-in pattern %q not found", id)
			continue
		}
		if !p.IsEnabled() {
			t.Errorf("built-in pattern %q is disabled, expected enabled", id)
		}
	}
}

func TestBuiltinPattern_FallbackSecretDetectsGoPattern(t *testing.T) {
	e := DefaultEngine()
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/auth/jwt.go")

	content := []byte(`package auth

func secret() string {
	jwtSecret := os.Getenv("JWT_SECRET")
	if jwtSecret == "" {
		jwtSecret = "real-production-secret-value"
	}
	return jwtSecret
}
`)
	violations := e.CheckFile(g, "/project/auth/jwt.go", content)
	found := false
	for _, v := range violations {
		if v.PatternID == "go-generic-fallback-secret" {
			found = true
			if v.Severity != SeverityHigh {
				t.Errorf("fallback pattern severity: want HIGH, got %s", v.Severity)
			}
		}
	}
	if !found {
		t.Error("expected go-generic-fallback-secret violation to fire")
	}
}

func TestBuiltinPattern_AWSKeyDetected(t *testing.T) {
	e := DefaultEngine()
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/infra/aws.go")

	content := []byte(`package infra

var accessKey = "AKIAIOSFODNN7EXAMPLE"
`)
	violations := e.CheckFile(g, "/project/infra/aws.go", content)
	found := false
	for _, v := range violations {
		if v.PatternID == "go-generic-hardcoded-secret" {
			found = true
		}
	}
	if !found {
		t.Error("expected go-generic-hardcoded-secret to catch AWS access key")
	}
}

func TestBuiltinPattern_DBConnectionStringDetected(t *testing.T) {
	e := DefaultEngine()
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/db/setup.go")

	content := []byte(`package db

var dsn = "postgres://admin:real_password_here@prod-db.example.com:5432/myapp"
`)
	violations := e.CheckFile(g, "/project/db/setup.go", content)
	if len(violations) == 0 {
		t.Error("expected violation: postgres DSN with embedded credentials")
	}
}

func TestBuiltinPattern_TypeScriptFallbackDetected(t *testing.T) {
	e := DefaultEngine()
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/src/config.ts")

	content := []byte(`const jwtSecret = process.env.JWT_SECRET || "real-fallback-secret-value"
`)
	violations := e.CheckFile(g, "/project/src/config.ts", content)
	found := false
	for _, v := range violations {
		if v.PatternID == "ts-generic-fallback-secret" {
			found = true
		}
	}
	if !found {
		t.Error("expected ts-generic-fallback-secret to detect TypeScript process.env fallback")
	}
}

func TestBuiltinPattern_PythonFallbackDetected(t *testing.T) {
	e := DefaultEngine()
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/app/settings.py")

	content := []byte(`import os
SECRET_KEY = os.environ.get('SECRET_KEY', 'django-insecure-hardcoded-production-key')
`)
	violations := e.CheckFile(g, "/project/app/settings.py", content)
	found := false
	for _, v := range violations {
		if v.PatternID == "python-generic-fallback-secret" {
			found = true
		}
	}
	if !found {
		t.Error("expected python-generic-fallback-secret to detect Python fallback credential")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Nil content guard still works
// ──────────────────────────────────────────────────────────────────────────────

func TestCheckHardcodedSecret_NilContent_NoFallback(t *testing.T) {
	p := makeSecretPattern([]string{`(?i)^[a-zA-Z0-9+/]{32,}={0,2}$`}, false, "go")
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/auth/jwt.go")

	violations := e.CheckFile(g, "/project/auth/jwt.go", nil)
	if violations != nil {
		t.Errorf("nil content should produce no violations, got %v", violations)
	}
}

func TestCheckHardcodedSecret_NilContent_WithFallback_NoViolation(t *testing.T) {
	// detect_fallback with nil content should also produce no violations.
	p := makeSecretPattern(nil, true, "go")
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/auth/jwt.go")

	violations := e.CheckFile(g, "/project/auth/jwt.go", nil)
	if violations != nil {
		t.Errorf("nil content with detect_fallback should produce no violations, got %v", violations)
	}
}
