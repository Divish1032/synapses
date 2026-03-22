// Package secrets provides secret-detection utilities shared across the codebase.
//
// Used by:
//   - brain/ingestor: scrub code before sending to the local LLM
//   - federation/tracker_brain: filter secrets from cross-project code analysis
//
// Keeping detection logic in one place ensures consistent coverage and prevents
// pattern drift between consumers (value 2: user data never leaks).
package secrets

import (
	"regexp"
	"strings"
)

// secretRegexps are precompiled regular expressions for detecting secret
// patterns across all common assignment styles: env vars, code assignments,
// YAML/TOML, JSON, connection strings, provider-specific tokens, and
// high-entropy values assigned to secret-looking variable names.
var secretRegexps = []*regexp.Regexp{
	// Generic assignment: secret-like variable name followed by assignment operator and value
	regexp.MustCompile(`(?i)(api[_-]?key|secret|password|passwd|token|credential|auth[_-]?token|access[_-]?key|private[_-]?key)[^a-zA-Z0-9].*[=:]`),

	// Connection strings with embedded credentials
	regexp.MustCompile(`(?i)(postgres|mysql|mongodb|redis|amqp|smtp)://[^:]+:[^@]+@`),

	// AWS access key pattern
	regexp.MustCompile(`(?:A3T[A-Z0-9]|AKIA|AGPA|AIDA|AROA|AIPA|ANPA|ANVA|ASIA)[A-Z0-9]{16}`),

	// GitHub tokens
	regexp.MustCompile(`(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9_]{36,}`),

	// Slack tokens
	regexp.MustCompile(`xox[bpas]-[0-9]+-[0-9]+-[a-zA-Z0-9]+`),

	// Stripe keys
	regexp.MustCompile(`(?:sk|pk)_(?:live|test)_[a-zA-Z0-9]{20,}`),

	// Generic high-entropy: variable with secret-like name assigned a 20+ char alphanumeric value
	regexp.MustCompile(`(?i)(secret|key|token|password|credential).*["'][A-Za-z0-9+/=_-]{20,}["']`),

	// Bearer tokens
	regexp.MustCompile(`(?i)bearer\s+[a-zA-Z0-9._\-]+`),

	// Authorization headers
	regexp.MustCompile(`(?i)authorization['"]*\s*[:=]\s*['"]`),
}

// secretPatterns are case-insensitive substrings that indicate a line contains
// or assigns a secret value. Covers environment variables, config files, code
// constants, and common provider-specific key names.
var secretPatterns = []string{
	// Generic assignment patterns
	"API_KEY=", "APIKEY=", "API_KEY:", "APIKEY:",
	"SECRET=", "SECRET:", "SECRET_KEY", "SECRETKEY",
	"PASSWORD=", "PASSWORD:", "PASSWD=", "PASSWD:",
	"TOKEN=", "TOKEN:", "_TOKEN=", "_TOKEN:",
	"PRIVATE_KEY", "PRIVATEKEY",
	"CREDENTIALS=", "CREDENTIALS:",

	// AWS
	"AWS_SECRET_ACCESS_KEY", "AWS_ACCESS_KEY_ID", "AWS_SESSION_TOKEN",

	// Database
	"DATABASE_URL=", "DATABASE_URL:", "DB_PASSWORD", "DB_PASS",
	"MONGO_URI=", "REDIS_URL=", "REDIS_PASSWORD",

	// OAuth / social
	"CLIENT_SECRET", "GITHUB_TOKEN", "SLACK_TOKEN", "SLACK_WEBHOOK",
	"DISCORD_TOKEN", "OPENAI_API_KEY",

	// PEM blocks (matched as substrings for lines within code blocks)
	"-----BEGIN RSA", "-----BEGIN EC", "-----BEGIN PRIVATE",
	"-----BEGIN OPENSSH", "-----BEGIN PGP",

	// Generic bearer/auth
	"BEARER ", "AUTHORIZATION:",

	// Common prefixes for API keys
	"SK_LIVE_", "SK_TEST_", "PK_LIVE_", "PK_TEST_",
	"GHPAT_", "GHP_", "GHO_", "GHU_", "GHS_", "GHR_",
	"XOXB-", "XOXP-", "XOXA-",
	"SG.", // SendGrid
}

// LooksLikeSecret returns true if the line likely contains a secret value.
// Two-phase approach: cheap substring scan (fast-path), then regex for nuanced
// detection across all assignment styles (code, YAML, JSON, connection strings).
func LooksLikeSecret(line string) bool {
	upper := strings.ToUpper(strings.TrimSpace(line))
	for _, pat := range secretPatterns {
		if strings.Contains(upper, pat) {
			return true
		}
	}
	for _, re := range secretRegexps {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

// FilterLines removes lines that look like they contain secrets from content.
// Also strips entire PEM blocks (BEGIN to END markers inclusive).
func FilterLines(content string) string {
	lines := strings.Split(content, "\n")
	filtered := make([]string, 0, len(lines))
	inPEMBlock := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "-----BEGIN ") {
			inPEMBlock = true
			continue
		}
		if inPEMBlock {
			if strings.HasPrefix(trimmed, "-----END ") {
				inPEMBlock = false
			}
			continue
		}
		if LooksLikeSecret(line) {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.Join(filtered, "\n")
}
