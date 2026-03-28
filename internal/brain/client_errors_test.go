package brain_test

// Error path tests for brain.Client (in-process NullBrain delegation).
// HTTP error paths no longer apply — the brain is embedded, not HTTP.
// Failure modes are: Ollama unreachable (NullBrain degrades gracefully).
