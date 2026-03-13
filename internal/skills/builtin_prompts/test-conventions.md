---
id: test-conventions
description: Go testing patterns used in this codebase
file_pattern: "**/*_test.go"
auto_load: false
---
Testing conventions:
- Use table-driven tests: `tests := []struct{name string; ...}{{...}}` + `for _, tt := range tests { t.Run(tt.name, ...) }`
- Prefer stdlib `testing` package; avoid heavy frameworks for unit tests
- Integration tests that require external services use `//go:build integration` build tag
- Mock external dependencies via interfaces — never patch global state
- Test file lives alongside the file it tests (same package or `package foo_test`)
