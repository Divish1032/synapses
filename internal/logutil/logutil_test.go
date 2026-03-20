package logutil

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// captureStderr redirects os.Stderr to capture output from writeLog.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	origStderr := os.Stderr
	os.Stderr = w

	fn()

	w.Close()
	os.Stderr = origStderr

	var buf bytes.Buffer
	buf.ReadFrom(r)
	r.Close()
	return buf.String()
}

// rfc3339Pattern matches an RFC 3339 timestamp prefix.
var rfc3339Pattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}[+-]\d{2}:\d{2} `)

func TestError(t *testing.T) {
	out := captureStderr(t, func() {
		Error("synapses: something failed: %v\n", fmt.Errorf("disk full"))
	})
	if !rfc3339Pattern.MatchString(out) {
		t.Errorf("missing RFC3339 timestamp: %q", out)
	}
	if !strings.Contains(out, "ERROR: ") {
		t.Errorf("missing ERROR prefix: %q", out)
	}
	if !strings.Contains(out, "synapses: something failed: disk full") {
		t.Errorf("missing message body: %q", out)
	}
}

func TestWarn(t *testing.T) {
	out := captureStderr(t, func() {
		Warn("synapses: degraded mode\n")
	})
	if !strings.Contains(out, "WARN: synapses: degraded mode") {
		t.Errorf("unexpected output: %q", out)
	}
}

func TestInfo(t *testing.T) {
	out := captureStderr(t, func() {
		Info("synapses: ready\n")
	})
	if !strings.Contains(out, "INFO: synapses: ready") {
		t.Errorf("unexpected output: %q", out)
	}
}

func TestDebug(t *testing.T) {
	out := captureStderr(t, func() {
		Debug("synapses: unmarshal: %v\n", fmt.Errorf("bad json"))
	})
	if !strings.Contains(out, "DEBUG: synapses: unmarshal: bad json") {
		t.Errorf("unexpected output: %q", out)
	}
}

func TestErrorP_WithProject(t *testing.T) {
	out := captureStderr(t, func() {
		ErrorP("abc123", "synapses: store failed\n")
	})
	if !strings.Contains(out, "ERROR: [abc123] synapses: store failed") {
		t.Errorf("missing project identifier: %q", out)
	}
}

func TestInfoP_WithProject(t *testing.T) {
	out := captureStderr(t, func() {
		InfoP("myproj", "synapses: project ready (%d nodes)\n", 42)
	})
	if !strings.Contains(out, "INFO: [myproj] synapses: project ready (42 nodes)") {
		t.Errorf("unexpected output: %q", out)
	}
}

func TestEmptyProject_NoSquareBrackets(t *testing.T) {
	out := captureStderr(t, func() {
		ErrorP("", "synapses: oops\n")
	})
	// Empty project should not insert brackets.
	if strings.Contains(out, "[]") {
		t.Errorf("empty project should not produce brackets: %q", out)
	}
	if !strings.Contains(out, "ERROR: synapses: oops") {
		t.Errorf("unexpected output: %q", out)
	}
}

func TestNewlinePreservation(t *testing.T) {
	// Messages with trailing newline should not get a double newline.
	out := captureStderr(t, func() {
		Info("hello\n")
	})
	if strings.HasSuffix(out, "\n\n") {
		t.Errorf("double newline: %q", out)
	}

	// Messages without trailing newline should still work.
	out = captureStderr(t, func() {
		Info("no newline")
	})
	if !strings.Contains(out, "INFO: no newline") {
		t.Errorf("unexpected output: %q", out)
	}
}

func TestFormatTimestamp(t *testing.T) {
	out := captureStderr(t, func() {
		Info("test\n")
	})
	// Extract timestamp portion — everything before the first space after the timezone offset.
	parts := strings.SplitN(out, " ", 3)
	if len(parts) < 3 {
		t.Fatalf("unexpected format: %q", out)
	}
	ts := parts[0]
	// Must be a valid RFC3339 timestamp.
	if !rfc3339Pattern.MatchString(ts + " ") {
		t.Errorf("timestamp is not RFC3339: %q", ts)
	}
}

func TestGrepFriendly(t *testing.T) {
	// Verify the output is grep-friendly: level appears as a fixed prefix.
	levels := map[string]func(string, ...interface{}){
		"ERROR": Error,
		"WARN":  Warn,
		"INFO":  Info,
		"DEBUG": Debug,
	}
	for level, fn := range levels {
		out := captureStderr(t, func() {
			fn("test message\n")
		})
		pattern := level + ": test message"
		if !strings.Contains(out, pattern) {
			t.Errorf("level %s: output not grep-friendly: %q (wanted %q)", level, out, pattern)
		}
	}
}
