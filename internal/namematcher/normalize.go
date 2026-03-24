// Package namematcher implements cross-domain entity name matching for Synapses.
// It creates MENTIONS edges between entities that share the same logical name
// across different domains (code, infra, API, docs).
package namematcher

import "strings"

// normalizeEntityName converts an entity name to a canonical lowercase form
// by removing common separators (hyphens, underscores, dots) and lowercasing.
// This enables kebab-case, snake_case, camelCase, and PascalCase names to match:
//   - "PaymentService" → "paymentservice"
//   - "payment-service" → "paymentservice"
//   - "payment_service" → "paymentservice"
//   - "payment.service" → "paymentservice"
func normalizeEntityName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '-', '_', '.', '/':
			// strip separators
		default:
			// lowercase everything
			if r >= 'A' && r <= 'Z' {
				b.WriteRune(r + 32)
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// genericNames is a blocklist of names that are too common to produce
// meaningful cross-domain matches. These names appear in many domains
// with unrelated semantics — matching them produces false positives
// that erode trust in the knowledge graph.
var genericNames = map[string]bool{
	"main":     true,
	"handler":  true,
	"config":   true,
	"client":   true,
	"server":   true,
	"service":  true,
	"manager":  true,
	"utils":    true,
	"util":     true,
	"helpers":  true,
	"helper":   true,
	"types":    true,
	"type":     true,
	"model":    true,
	"models":   true,
	"errors":   true,
	"error":    true,
	"test":     true,
	"init":     true,
	"new":      true,
	"run":      true,
	"start":    true,
	"stop":     true,
	"close":    true,
	"open":     true,
	"get":      true,
	"set":      true,
	"create":   true,
	"delete":   true,
	"update":   true,
	"list":     true,
	"index":    true,
	"default":  true,
	"base":     true,
	"common":   true,
	"core":     true,
	"data":     true,
	"api":      true,
	"app":      true,
	"db":       true,
	"logger":   true,
	"log":      true,
	"context":  true,
	"ctx":      true,
	"request":  true,
	"response": true,
	"result":   true,
	"router":   true,
	"options":  true,
	"opts":     true,
	"params":   true,
}

// isGenericName returns true if the normalized name is too common to
// produce meaningful cross-domain matches.
func isGenericName(normalized string) bool {
	return genericNames[normalized]
}
