// Package logutil provides structured, grep-friendly logging to stderr.
//
// Every message is prefixed with an ISO 8601 timestamp and a level tag
// (ERROR, WARN, INFO, DEBUG).  When a project identifier is available,
// the "P" variants (ErrorP, WarnP, …) insert it as [project] after the level.
//
// Output format:
//
//	2026-03-20T10:32:00-07:00 ERROR: synapses: something broke: file not found
//	2026-03-20T10:32:00-07:00 INFO: [abc123] synapses: project ready
//
// This is NOT a full structured logging framework — it is a minimal,
// grep-friendly convention that enables `grep ERROR:` for incident triage.
package logutil

import (
	"fmt"
	"os"
	"time"
)

// Error logs an ERROR-level message to stderr with timestamp.
func Error(format string, args ...interface{}) {
	writeLog("ERROR", "", format, args...)
}

// ErrorP logs an ERROR-level message with project identifier.
func ErrorP(project string, format string, args ...interface{}) {
	writeLog("ERROR", project, format, args...)
}

// Warn logs a WARN-level message to stderr with timestamp.
func Warn(format string, args ...interface{}) {
	writeLog("WARN", "", format, args...)
}

// WarnP logs a WARN-level message with project identifier.
func WarnP(project string, format string, args ...interface{}) {
	writeLog("WARN", project, format, args...)
}

// Info logs an INFO-level message to stderr with timestamp.
func Info(format string, args ...interface{}) {
	writeLog("INFO", "", format, args...)
}

// InfoP logs an INFO-level message with project identifier.
func InfoP(project string, format string, args ...interface{}) {
	writeLog("INFO", project, format, args...)
}

// Debug logs a DEBUG-level message to stderr with timestamp.
func Debug(format string, args ...interface{}) {
	writeLog("DEBUG", "", format, args...)
}

// DebugP logs a DEBUG-level message with project identifier.
func DebugP(project string, format string, args ...interface{}) {
	writeLog("DEBUG", project, format, args...)
}

func writeLog(level, project, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	ts := time.Now().Format(time.RFC3339)
	if project != "" {
		fmt.Fprintf(os.Stderr, "%s %s: [%s] %s", ts, level, project, msg)
	} else {
		fmt.Fprintf(os.Stderr, "%s %s: %s", ts, level, msg)
	}
}
