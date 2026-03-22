package parser

import (
	"context"
	"time"
)

// DefaultParseTimeout is the maximum time allowed for tree-sitter to parse a single file.
const DefaultParseTimeout = 30 * time.Second

// parseContext returns a context with the default parse timeout.
// The caller must call the returned cancel function.
func parseContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), DefaultParseTimeout)
}
