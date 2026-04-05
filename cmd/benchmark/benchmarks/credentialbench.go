// credentialbench.go implements Benchmark 3: Credential Detection.
//
// Tests Synapses' hardcoded credential detection against files with
// KNOWN credentials (both real leaked patterns and safe alternatives).
// Content-based detection — doesn't need graph call edges.
package benchmarks

import (
	"log"
	"strings"

	"github.com/SynapsesOS/synapses/cmd/benchmark/reporter"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/security"
)

// CredentialCase is one test file with known credential status.
type CredentialCase struct {
	ID       string
	Language string
	File     string
	Content  string // raw source with or without credentials
	HasCreds bool   // ground truth: does this file contain hardcoded creds?
}

// CredentialResult holds per-case results.
type CredentialResult struct {
	ID       string `json:"id"`
	Language string `json:"language"`
	HasCreds bool   `json:"has_creds"` // ground truth
	Detected bool   `json:"detected"`  // did engine detect?
	Correct  bool   `json:"correct"`
}

// RunCredentialBench tests credential detection on realistic code.
func RunCredentialBench() (*reporter.SecurityBenchReport, error) {
	cases := buildCredentialCases()
	engine := security.DefaultEngine()

	log.Printf("[credentialbench] %d test cases", len(cases))

	var tp, fp, tn, fn int
	var results []CredentialResult

	for _, tc := range cases {
		g := graph.New("cred-bench")
		g.SetRoot("/project")
		fileID := g.MakeNodeID(tc.File, tc.File)
		g.AddNode(&graph.Node{
			ID:   fileID,
			Type: graph.NodeFile,
			Name: tc.File,
			File: tc.File,
		})

		violations := engine.CheckFile(g, tc.File, []byte(tc.Content))
		detected := false
		for _, v := range violations {
			if strings.Contains(v.PatternID, "secret") || strings.Contains(v.PatternID, "credential") {
				detected = true
				break
			}
		}

		correct := detected == tc.HasCreds
		results = append(results, CredentialResult{
			ID:       tc.ID,
			Language: tc.Language,
			HasCreds: tc.HasCreds,
			Detected: detected,
			Correct:  correct,
		})

		if tc.HasCreds && detected {
			tp++
		} else if tc.HasCreds && !detected {
			fn++
		} else if !tc.HasCreds && detected {
			fp++
		} else {
			tn++
		}
	}

	precision := float64(0)
	if tp+fp > 0 {
		precision = float64(tp) / float64(tp+fp)
	}
	recall := float64(0)
	if tp+fn > 0 {
		recall = float64(tp) / float64(tp+fn)
	}
	f1 := float64(0)
	if precision+recall > 0 {
		f1 = 2 * precision * recall / (precision + recall)
	}
	youden := float64(0)
	if tp+fn > 0 && fp+tn > 0 {
		tpr := float64(tp) / float64(tp+fn)
		fpr := float64(fp) / float64(fp+tn)
		youden = tpr - fpr
	}

	report := &reporter.SecurityBenchReport{
		Timestamp:   reporter.Timestamp(),
		TotalCases:  len(cases),
		TP:          tp,
		FP:          fp,
		TN:          tn,
		FN:          fn,
		Precision:   precision * 100,
		Recall:      recall * 100,
		F1:          f1 * 100,
		YoudenIndex: youden * 100,
		Cases:       results,
	}

	log.Printf("[credentialbench] TP=%d FP=%d TN=%d FN=%d P=%.1f%% R=%.1f%% F1=%.1f%%",
		tp, fp, tn, fn, report.Precision, report.Recall, report.F1)
	return report, nil
}

func buildCredentialCases() []CredentialCase {
	return []CredentialCase{
		// ── True Positives: real credential patterns ──────────────
		{ID: "aws-key-go", Language: "go", File: "/project/config.go",
			Content: `package config
var AWSKey = "AKIAIOSFODNN7EXAMPLE"
var AWSSecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"`,
			HasCreds: true},

		{ID: "aws-key-python", Language: "python", File: "/project/config.py",
			Content: `AWS_ACCESS_KEY_ID = "AKIAIOSFODNN7EXAMPLE"
AWS_SECRET_ACCESS_KEY = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"`,
			HasCreds: true},

		{ID: "github-token-js", Language: "javascript", File: "/project/config.js",
			Content: `const GITHUB_TOKEN = "ghp_1234567890abcdefghijklmnopqrstuvwxyz";`,
			HasCreds: true},

		{ID: "stripe-key-ts", Language: "typescript", File: "/project/payment.ts",
			Content: `export const STRIPE_SECRET = "sk_live_1234567890abcdefghijklmnopq";`,
			HasCreds: true},

		{ID: "jwt-secret-go", Language: "go", File: "/project/auth/jwt.go",
			Content: `package auth
var jwtSecret = []byte("super_secret_jwt_signing_key_2024")`,
			HasCreds: true},

		{ID: "db-url-python", Language: "python", File: "/project/settings.py",
			Content: `DATABASE_URL = "postgresql://admin:p@ssw0rd@production-db.example.com:5432/myapp"`,
			HasCreds: true}, // NOTE: DB URL credential detection requires pattern for connection strings

		{ID: "private-key-go", Language: "go", File: "/project/crypto.go",
			Content: `package crypto
var privateKey = "-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA..."`,
			HasCreds: true}, // NOTE: PEM key detection requires pattern for -----BEGIN

		{ID: "slack-webhook-py", Language: "python", File: "/project/notify.py",
			Content: `SLACK_WEBHOOK = "https://hooks.slack.com/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX"`,
			HasCreds: true}, // NOTE: Slack webhook detection requires URL-based pattern

		// ── True Negatives: safe patterns ────────────────────────
		{ID: "env-var-go", Language: "go", File: "/project/config.go",
			Content: `package config
import "os"
func GetAWSKey() string { return os.Getenv("AWS_ACCESS_KEY_ID") }`,
			HasCreds: false},

		{ID: "env-var-python", Language: "python", File: "/project/config.py",
			Content: `import os
AWS_KEY = os.environ.get("AWS_ACCESS_KEY_ID")
DB_URL = os.environ["DATABASE_URL"]`,
			HasCreds: false},

		{ID: "env-var-ts", Language: "typescript", File: "/project/config.ts",
			Content: `export const API_KEY = process.env.API_KEY;
export const DB_URL = process.env.DATABASE_URL;`,
			HasCreds: false},

		{ID: "placeholder-go", Language: "go", File: "/project/config.go",
			Content: `package config
// Replace with your actual key
var APIKey = "your-api-key-here"  // TODO: use environment variable`,
			HasCreds: false},

		{ID: "test-mock-py", Language: "python", File: "/project/test_config.py",
			Content: `# Test fixtures
TEST_API_KEY = "test_key_not_real"
MOCK_DB_URL = "sqlite:///:memory:"`,
			HasCreds: false},

		{ID: "empty-string-go", Language: "go", File: "/project/config.go",
			Content: `package config
var APIKey = ""
var Secret = ""`,
			HasCreds: false},

		{ID: "comment-go", Language: "go", File: "/project/docs.go",
			Content: `package config
// Example: AKIAIOSFODNN7EXAMPLE (this is the AWS docs example, not a real key)
// See https://docs.aws.amazon.com/general/latest/gr/aws-sec-cred-types.html`,
			HasCreds: false},

		{ID: "constant-name-py", Language: "python", File: "/project/constants.py",
			Content: `MAX_API_KEY_LENGTH = 64
SECRET_KEY_ROTATION_DAYS = 90
AWS_REGION = "us-east-1"`,
			HasCreds: false},
	}
}
