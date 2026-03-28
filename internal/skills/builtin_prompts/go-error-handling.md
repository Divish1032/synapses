---
id: go-error-handling
description: Go error wrapping and handling conventions
file_pattern: "**/*.go"
auto_load: false
---
Go error-handling conventions in this codebase:
- Wrap errors with context: `fmt.Errorf("package.FuncName: %w", err)`
- Return errors as the last return value; never return a non-nil error with a non-zero value
- Check errors with `errors.Is()` / `errors.As()`, never string comparison
- Do not panic except in `init()` or truly unrecoverable states
- Log OR return an error — never both (avoids double-logging)
