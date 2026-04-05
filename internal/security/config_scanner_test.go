package security

import (
	"testing"
)

func TestCheckConfigFile_OpenAPIYAML_UnsecuredEndpoints(t *testing.T) {
	spec := `
openapi: "3.0.1"
info:
  title: Test API
components:
  securitySchemes:
    bearerAuth:
      type: http
      scheme: bearer
paths:
  /users:
    get:
      summary: Get users
    post:
      summary: Create user
      security:
        - bearerAuth: []
  /admin/delete:
    delete:
      summary: Delete everything
`
	violations := CheckConfigFile("api.yml", []byte(spec))
	if len(violations) == 0 {
		t.Fatal("expected violations for unsecured endpoints, got 0")
	}

	// GET /users should be flagged (no security)
	// POST /users should NOT be flagged (has security)
	// DELETE /admin/delete should be flagged (no security)
	found := map[string]bool{}
	for _, v := range violations {
		found[v.Target] = true
		if v.Severity != SeverityCritical {
			t.Errorf("violation %s severity = %s, want CRITICAL", v.Target, v.Severity)
		}
		if v.Confidence != ConfidenceHigh {
			t.Errorf("violation %s confidence = %s, want HIGH", v.Target, v.Confidence)
		}
	}

	if !found["GET /users"] {
		t.Error("expected violation for GET /users")
	}
	if found["POST /users"] {
		t.Error("POST /users should NOT be flagged (has security)")
	}
	if !found["DELETE /admin/delete"] {
		t.Error("expected violation for DELETE /admin/delete")
	}
}

func TestCheckConfigFile_OpenAPIYAML_GlobalSecurity(t *testing.T) {
	spec := `
openapi: "3.0.0"
security:
  - bearerAuth: []
paths:
  /users:
    get:
      summary: Get users
  /admin:
    delete:
      summary: Delete
`
	violations := CheckConfigFile("api.yml", []byte(spec))
	if len(violations) != 0 {
		t.Errorf("expected 0 violations with global security, got %d", len(violations))
	}
}

func TestCheckConfigFile_OpenAPIJSON(t *testing.T) {
	spec := `{
  "openapi": "3.0.0",
  "paths": {
    "/users": {
      "get": {
        "summary": "Get users"
      }
    }
  }
}`
	violations := CheckConfigFile("api.json", []byte(spec))
	if len(violations) == 0 {
		t.Fatal("expected violations for unsecured JSON endpoint")
	}
}

func TestCheckConfigFile_NotOpenAPI(t *testing.T) {
	// Regular YAML, not OpenAPI
	content := `
name: my-app
version: 1.0
`
	violations := CheckConfigFile("config.yml", []byte(content))
	if len(violations) != 0 {
		t.Errorf("non-OpenAPI YAML should produce 0 violations, got %d", len(violations))
	}
}

func TestCheckConfigFile_NonYAML(t *testing.T) {
	violations := CheckConfigFile("main.go", []byte("package main"))
	if violations != nil {
		t.Errorf("non-YAML file should return nil, got %d violations", len(violations))
	}
}

func TestCheckConfigFile_SkipsHealthEndpoints(t *testing.T) {
	spec := `
openapi: "3.0.0"
paths:
  /:
    get:
      summary: Home
  /health:
    get:
      summary: Health check
  /healthcheck:
    get:
      summary: Health
  /users:
    get:
      summary: Users
`
	violations := CheckConfigFile("api.yml", []byte(spec))
	// Only /users should be flagged, not /, /health, /healthcheck
	if len(violations) != 1 {
		t.Errorf("expected 1 violation (/users only), got %d", len(violations))
		for _, v := range violations {
			t.Logf("  %s", v.Target)
		}
	}
}

func TestCheckConfigFile_VAmPI_RealSpec(t *testing.T) {
	// Simplified version of VAmPI's actual OpenAPI spec
	spec := `
openapi: "3.0.1"
components:
  securitySchemes:
    bearerAuth:
      type: http
      scheme: bearer
paths:
  /createdb:
    get:
      summary: Create DB
  /users/v1:
    get:
      summary: List users
  /users/v1/register:
    post:
      summary: Register
  /users/v1/login:
    post:
      summary: Login
  /users/v1/{username}:
    get:
      summary: Get user
      security:
        - bearerAuth: []
  /books/v1:
    get:
      summary: List books
      security:
        - bearerAuth: []
`
	violations := CheckConfigFile("openapi3.yml", []byte(spec))
	// /createdb, /users/v1, /users/v1/register, /users/v1/login should be flagged
	// /users/v1/{username} and /books/v1 should NOT (have security)
	if len(violations) < 3 {
		t.Errorf("expected at least 3 violations on VAmPI-like spec, got %d", len(violations))
	}

	for _, v := range violations {
		t.Logf("  [%s] %s — %s", v.Severity, v.Target, v.Evidence[:80])
	}

	// Verify the secured endpoints are NOT flagged
	for _, v := range violations {
		if v.Target == "GET /users/v1/{username}" || v.Target == "GET /books/v1" {
			t.Errorf("secured endpoint %s should NOT be flagged", v.Target)
		}
	}
}
