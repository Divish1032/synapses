package secrets

import (
	"strings"
	"testing"
)

func TestLooksLikeSecret_SubstringPatterns(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{"API_KEY=sk-abc123", true},
		{"export OPENAI_API_KEY=sk-xxx", true},
		{"PASSWORD=hunter2", true},
		{"DB_PASSWORD=secret", true},
		{"AWS_SECRET_ACCESS_KEY=wJalr...", true},
		{"GITHUB_TOKEN=ghp_xxxx", true},
		{"REDIS_URL=redis://user:pass@host", true},
		{"AUTHORIZATION: Bearer tok", true},
		{"func main() {}", false},
		{"// This is a comment", false},
		{"var count int", false},
	}
	for _, tc := range cases {
		if got := LooksLikeSecret(tc.line); got != tc.want {
			t.Errorf("LooksLikeSecret(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

func TestLooksLikeSecret_RegexPatterns(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{`apiKey = "sk-abc123def456ghi789"`, true},                      // generic assignment
		{`postgres://user:pass@host:5432/db`, true},                     // connection string
		{`AKIAIOSFODNN7EXAMPLE`, true},                                  // AWS key
		{`ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefgh1234`, true},           // GitHub token
		{`sk_live_abcdefghij0123456789`, true},                          // Stripe key
		{`Authorization: "Bearer eyJhbGciOi..."`, true},                 // bearer
		{`const maxRetries = 3`, false},
		{`import "net/http"`, false},
	}
	for _, tc := range cases {
		if got := LooksLikeSecret(tc.line); got != tc.want {
			t.Errorf("LooksLikeSecret(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

func TestFilterLines_PEMBlocks(t *testing.T) {
	input := `some code
-----BEGIN RSA PRIVATE KEY-----
MIIEpAIBAAKCAQEA
base64data...
-----END RSA PRIVATE KEY-----
more code`

	got := FilterLines(input)
	if strings.Contains(got, "MIIEpAIBAAKCAQEA") {
		t.Error("PEM body should be stripped")
	}
	if strings.Contains(got, "BEGIN RSA") {
		t.Error("PEM BEGIN marker should be stripped")
	}
	if !strings.Contains(got, "some code") || !strings.Contains(got, "more code") {
		t.Error("non-secret lines should be preserved")
	}
}

func TestFilterLines_MixedContent(t *testing.T) {
	input := `package main
import "os"
var dbURL = os.Getenv("DATABASE_URL=postgres://u:p@h/d")
func handler() {}
export GITHUB_TOKEN=ghp_xxxxxxxxxxxx
const version = "1.0"`

	got := FilterLines(input)
	if strings.Contains(got, "DATABASE_URL") {
		t.Error("DATABASE_URL line should be stripped")
	}
	if strings.Contains(got, "GITHUB_TOKEN") {
		t.Error("GITHUB_TOKEN line should be stripped")
	}
	if !strings.Contains(got, "package main") {
		t.Error("package line should be preserved")
	}
	if !strings.Contains(got, "func handler") {
		t.Error("function line should be preserved")
	}
	if !strings.Contains(got, `version = "1.0"`) {
		t.Error("version line should be preserved")
	}
}

func TestFilterLines_UnterminatedPEM(t *testing.T) {
	input := `before
-----BEGIN PRIVATE KEY-----
key data that never ends`

	got := FilterLines(input)
	if strings.Contains(got, "key data") {
		t.Error("unterminated PEM content should be stripped")
	}
	if !strings.Contains(got, "before") {
		t.Error("content before PEM should be preserved")
	}
}
