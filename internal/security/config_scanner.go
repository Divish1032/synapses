// config_scanner.go scans configuration files (OpenAPI YAML/JSON) for
// security definition gaps. This is a separate signal from the graph-based
// CheckFile — it handles declarative routing where routes are defined in
// config, not in code.
//
// This fixes the architectural gap where frameworks like Connexion, FastAPI
// (with OpenAPI), and Spring (with XML config) define routes declaratively.
// The graph-based engine can't see these routes because they have no
// NodeRoute in the code graph.
package security

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// CheckConfigFile scans a configuration file for security gaps.
// Currently supports OpenAPI 3.x and Swagger 2.x specs (YAML/JSON).
// Returns nil if the file is not a recognized config format or has no issues.
func CheckConfigFile(filePath string, content []byte) []Violation {
	if len(content) == 0 {
		return nil
	}

	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".yml", ".yaml":
		return checkOpenAPIYAML(filePath, content)
	case ".json":
		return checkOpenAPIJSON(filePath, content)
	default:
		return nil
	}
}

// checkOpenAPIYAML parses a YAML file and checks for OpenAPI security gaps.
func checkOpenAPIYAML(filePath string, content []byte) []Violation {
	var spec map[string]interface{}
	if err := yaml.Unmarshal(content, &spec); err != nil {
		return nil // not valid YAML or not a map
	}
	return checkOpenAPISpec(filePath, spec)
}

// checkOpenAPIJSON parses a JSON file and checks for OpenAPI security gaps.
func checkOpenAPIJSON(filePath string, content []byte) []Violation {
	var spec map[string]interface{}
	if err := json.Unmarshal(content, &spec); err != nil {
		return nil
	}
	return checkOpenAPISpec(filePath, spec)
}

// checkOpenAPISpec checks an OpenAPI/Swagger spec for unsecured endpoints.
func checkOpenAPISpec(filePath string, spec map[string]interface{}) []Violation {
	// Must be an OpenAPI or Swagger spec.
	_, hasOpenAPI := spec["openapi"]
	_, hasSwagger := spec["swagger"]
	if !hasOpenAPI && !hasSwagger {
		return nil
	}

	// Check for global security definition.
	hasGlobalSecurity := false
	if globalSec, ok := spec["security"]; ok {
		if secList, ok := globalSec.([]interface{}); ok && len(secList) > 0 {
			hasGlobalSecurity = true
		}
	}

	// Check if security schemes are defined at all.
	hasSecuritySchemes := false
	if components, ok := spec["components"].(map[string]interface{}); ok {
		if _, ok := components["securitySchemes"]; ok {
			hasSecuritySchemes = true
		}
	}
	// Swagger 2.x uses securityDefinitions.
	if _, ok := spec["securityDefinitions"]; ok {
		hasSecuritySchemes = true
	}

	// Walk paths and find operations without security.
	paths, ok := spec["paths"].(map[string]interface{})
	if !ok {
		return nil
	}

	httpMethods := map[string]bool{
		"get": true, "post": true, "put": true, "delete": true,
		"patch": true, "head": true, "options": true,
	}

	var violations []Violation

	for path, pathItem := range paths {
		pathMap, ok := pathItem.(map[string]interface{})
		if !ok {
			continue
		}

		for method, operation := range pathMap {
			if !httpMethods[strings.ToLower(method)] {
				continue
			}

			opMap, ok := operation.(map[string]interface{})
			if !ok {
				continue
			}

			// Check if this operation has its own security definition.
			hasSecurity := false
			if opSec, ok := opMap["security"]; ok {
				if secList, ok := opSec.([]interface{}); ok && len(secList) > 0 {
					hasSecurity = true
				}
			}

			// Skip if global security covers this endpoint.
			if hasGlobalSecurity {
				continue
			}

			// Skip if this operation has its own security.
			if hasSecurity {
				continue
			}

			// Skip health checks and docs endpoints.
			lowerPath := strings.ToLower(path)
			if lowerPath == "/" || lowerPath == "/health" || lowerPath == "/healthcheck" ||
				lowerPath == "/docs" || lowerPath == "/swagger" || lowerPath == "/openapi" ||
				strings.HasPrefix(lowerPath, "/swagger") {
				continue
			}

			// This endpoint has no security definition.
			target := fmt.Sprintf("%s %s", strings.ToUpper(method), path)
			msg := fmt.Sprintf(
				"OpenAPI endpoint %s in %s has no security definition. "+
					"Add a 'security' field to the operation or a global 'security' field at the spec root.",
				target, filepath.Base(filePath))

			evidence := "No security field on this operation"
			if hasSecuritySchemes {
				evidence += " (security schemes are defined but not applied to this endpoint)"
			} else {
				evidence += " (no security schemes defined in the spec at all)"
			}

			violations = append(violations, Violation{
				PatternID:        "config-openapi-missing-security",
				PatternName:      "OpenAPI endpoint missing security definition",
				Severity:         SeverityCritical,
				Action:           "block",
				File:             filePath,
				Target:           target,
				Message:          msg,
				Evidence:         evidence,
				Tags:             []string{"owasp-a01", "auth", "openapi", "config"},
				Confidence:       ConfidenceHigh,
				ConfidenceReason: "openapi-spec-analysis",
			})
		}
	}

	return violations
}
