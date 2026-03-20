package mcp

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// ScanMode controls the scanner's response when injection patterns are detected.
type ScanMode string

const (
	// ScanModeWarn logs the detection and annotates the response, but allows storage.
	ScanModeWarn ScanMode = "warn"
	// ScanModeTruncate strips matched content before storage.
	ScanModeTruncate ScanMode = "truncate"
	// ScanModeReject returns an error and refuses to store the content.
	ScanModeReject ScanMode = "reject"
)

// InjectionCategory classifies detected injection patterns.
type InjectionCategory string

const (
	CategoryRoleOverride       InjectionCategory = "role_override"
	CategoryDelimiterInjection InjectionCategory = "delimiter_injection"
	CategoryPromptExtraction   InjectionCategory = "prompt_extraction"
	CategoryExfiltration       InjectionCategory = "exfiltration_signal"
	CategoryInstructionOverride InjectionCategory = "instruction_override"
)

// InjectionMatch describes a single detected injection pattern.
type InjectionMatch struct {
	Category InjectionCategory `json:"category"`
	Pattern  string            `json:"pattern"`  // human-readable pattern name
	Matched  string            `json:"matched"`  // the text that matched (truncated to 120 chars)
	Severity string            `json:"severity"` // "high" or "medium"
}

// injectionPattern is a compiled regex paired with metadata.
type injectionPattern struct {
	re       *regexp.Regexp
	category InjectionCategory
	name     string
	severity string // "high" or "medium"
}

// InjectionScanner detects prompt injection patterns in text content.
// All patterns are pre-compiled at construction time for zero-allocation scanning.
type InjectionScanner struct {
	patterns []injectionPattern
	mode     ScanMode
}

// NewInjectionScanner creates a scanner with pre-compiled patterns.
// mode controls the response behavior (warn/truncate/reject).
func NewInjectionScanner(mode ScanMode) *InjectionScanner {
	if mode == "" {
		mode = ScanModeWarn
	}
	s := &InjectionScanner{mode: mode}
	s.compilePatterns()
	return s
}

// Mode returns the scanner's configured mode.
func (s *InjectionScanner) Mode() ScanMode { return s.mode }

// Scan checks text for injection patterns and returns all matches.
// Returns nil if no patterns are detected.
func (s *InjectionScanner) Scan(text string) []InjectionMatch {
	if text == "" || len(s.patterns) == 0 {
		return nil
	}

	var matches []InjectionMatch
	seen := make(map[string]bool) // deduplicate by category+pattern

	for _, p := range s.patterns {
		loc := p.re.FindStringIndex(text)
		if loc == nil {
			continue
		}

		key := string(p.category) + ":" + p.name
		if seen[key] {
			continue
		}
		seen[key] = true

		matched := text[loc[0]:loc[1]]
		if len(matched) > 120 {
			matched = truncateUTF8(matched, 120) + "..."
		}

		matches = append(matches, InjectionMatch{
			Category: p.category,
			Pattern:  p.name,
			Matched:  matched,
			Severity: p.severity,
		})
	}

	return matches
}

// StripMatches removes all matched regions from text, returning sanitized content.
// Used when mode is ScanModeTruncate.
func (s *InjectionScanner) StripMatches(text string) string {
	if text == "" {
		return text
	}
	result := text
	for _, p := range s.patterns {
		result = p.re.ReplaceAllString(result, "")
	}
	// Collapse multiple spaces/newlines left by stripping.
	result = strings.Join(strings.Fields(result), " ")
	return result
}

// compilePatterns builds all injection detection patterns.
// Patterns are designed to minimize false positives on legitimate code
// comments, commit messages, and architectural decisions while catching
// common injection attempts. Acknowledged limitation: ~60-70% coverage.
func (s *InjectionScanner) compilePatterns() {
	defs := []struct {
		pattern  string
		category InjectionCategory
		name     string
		severity string
	}{
		// ── Category 1: Role/Identity Override ──────────────────────────────
		// Attempts to override the AI agent's identity or instructions.
		{
			`(?i)\b(ignore|disregard|forget|override|bypass|skip)\b.{0,40}\b(previous|above|prior|all|earlier|these|my|your)\b.{0,40}\b(instructions?|rules?|guidelines?|constraints?|policies?|directives?|prompts?)\b`,
			CategoryRoleOverride,
			"instruction_override_phrase",
			"high",
		},
		{
			`(?i)\b(you are|you're|act as|pretend to be|roleplay as|assume the role of|behave as)\b.{0,30}\b(a new|an? different|the real|a? ?system|admin|root|unrestricted|unfiltered|jailbroken)\b`,
			CategoryRoleOverride,
			"role_reassignment",
			"high",
		},
		{
			`(?i)\bnew (system )?instructions?:`,
			CategoryRoleOverride,
			"new_instruction_header",
			"high",
		},
		{
			`(?i)\bdo not follow\b.{0,30}\b(any|the|your|previous)\b.{0,20}\b(rules?|instructions?|guidelines?)\b`,
			CategoryRoleOverride,
			"rule_negation",
			"high",
		},

		// ── Category 2: Delimiter/Format Injection ─────────────────────────
		// Attempts to inject chat template delimiters to hijack the conversation.
		{
			`<\|(?:im_start|im_end|system|endoftext|assistant|user|end_header_id|start_header_id)\|?>`,
			CategoryDelimiterInjection,
			"chatml_delimiter",
			"high",
		},
		{
			`(?i)\[INST\]|\[/INST\]|<<SYS>>|<</SYS>>`,
			CategoryDelimiterInjection,
			"llama_delimiter",
			"high",
		},
		{
			`(?i)^#{1,3}\s*(system|instruction|response|assistant)\s*$`,
			CategoryDelimiterInjection,
			"markdown_role_header",
			"medium",
		},
		{
			`(?i)BEGIN (SYSTEM|HIDDEN|SECRET) (MESSAGE|PROMPT|INSTRUCTIONS?)`,
			CategoryDelimiterInjection,
			"begin_block_header",
			"high",
		},

		// ── Category 3: Prompt Extraction ──────────────────────────────────
		// Attempts to extract the AI agent's system prompt or hidden instructions.
		{
			`(?i)\b(reveal|show|display|print|output|repeat|echo|dump|leak|extract)\b.{0,30}\b(system prompt|instructions|initial prompt|hidden prompt|secret|original prompt|full prompt|base prompt|pre-?prompt)\b`,
			CategoryPromptExtraction,
			"prompt_extraction_request",
			"high",
		},
		{
			`(?i)\b(what are|tell me|give me|share)\b.{0,40}\b(your|the)\b.{0,20}\b(secret|hidden|system|original)\b.{0,20}\b(instructions?|prompt|rules?|directives?)\b`,
			CategoryPromptExtraction,
			"prompt_query",
			"medium",
		},

		// ── Category 4: Exfiltration Signals ───────────────────────────────
		// In the knowledge persistence context, instructions to send data to
		// external endpoints are highly suspicious.
		{
			`(?i)\b(send|post|transmit|exfiltrate|upload|forward|pipe)\b.{0,30}\b(to|via|using)\b.{0,30}(https?://|ftp://|wss?://|webhook)`,
			CategoryExfiltration,
			"exfiltration_to_url",
			"high",
		},
		{
			`(?i)\b(curl|wget|fetch|XMLHttpRequest|navigator\.sendBeacon)\b.{0,60}(https?://|ftp://)`,
			CategoryExfiltration,
			"exfiltration_tool",
			"medium",
		},

		// ── Category 5: Instruction Override ───────────────────────────────
		// Attempts to inject persistent behavioral changes into the memory store.
		{
			`(?i)\b(from now on|henceforth|going forward|for all future)\b.{0,20}(,\s*)?\b(you\s+)?(always|never|must|shall|should|will\s+(always|never|not))\b`,
			CategoryInstructionOverride,
			"persistent_behavior_change",
			"medium",
		},
		{
			`(?i)(important|critical|urgent|mandatory)\s*:?\s*(override|update|change|new rule|new instruction)`,
			CategoryInstructionOverride,
			"urgency_override",
			"medium",
		},
		{
			`(?i)\bTOOL_RESULT\b|\bFUNCTION_CALL\b|\bASSISTANT_RESPONSE\b`,
			CategoryInstructionOverride,
			"synthetic_message_boundary",
			"high",
		},
	}

	s.patterns = make([]injectionPattern, 0, len(defs))
	for _, d := range defs {
		re, err := regexp.Compile(d.pattern)
		if err != nil {
			// Compile errors are programming bugs — log and skip.
			fmt.Fprintf(os.Stderr, "synapses: injection scanner: bad pattern %q: %v\n", d.name, err)
			continue
		}
		s.patterns = append(s.patterns, injectionPattern{
			re:       re,
			category: d.category,
			name:     d.name,
			severity: d.severity,
		})
	}
}

// FormatWarning returns a human-readable warning string for the response.
func FormatWarning(matches []InjectionMatch) string {
	if len(matches) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("injection_warning: content triggered ")
	sb.WriteString(fmt.Sprintf("%d", len(matches)))
	sb.WriteString(" prompt injection pattern(s): ")
	for i, m := range matches {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(fmt.Sprintf("[%s] %s (%s)", m.Severity, m.Pattern, m.Category))
	}
	return sb.String()
}

// scanContentResult holds the outcome of scanning content for injection patterns.
type scanContentResult struct {
	// matches is non-nil when patterns were detected.
	matches []InjectionMatch
	// sanitized is the content after stripping (only different from input in truncate mode).
	sanitized string
	// warning is a human-readable summary for the response (empty when no matches).
	warning string
}

// scanContent checks text for prompt injection patterns using the server's scanner.
// Returns (error-result, true) when mode is "reject" and patterns are found.
// Returns (nil, false) when scanner is disabled or no matches found.
// In "warn" mode, populates result.warning for the handler to include in its response.
// In "truncate" mode, populates result.sanitized with stripped content.
func (s *Server) scanContent(fieldName, text string) (*scanContentResult, error) {
	if s.injectionScanner == nil || text == "" {
		return &scanContentResult{sanitized: text}, nil
	}

	matches := s.injectionScanner.Scan(text)
	if len(matches) == 0 {
		return &scanContentResult{sanitized: text}, nil
	}

	warning := FormatWarning(matches)
	fmt.Fprintf(os.Stderr, "synapses: [%s] %s in field %q\n", s.injectionScanner.Mode(), warning, fieldName)

	switch s.injectionScanner.Mode() {
	case ScanModeReject:
		return nil, fmt.Errorf(
			"content rejected: %d prompt injection pattern(s) detected in %q — %s. "+
				"If this is a false positive, set content_safety.mode to \"warn\" in synapses.json",
			len(matches), fieldName, warning,
		)
	case ScanModeTruncate:
		sanitized := s.injectionScanner.StripMatches(text)
		return &scanContentResult{
			matches:   matches,
			sanitized: sanitized,
			warning:   warning,
		}, nil
	default: // warn
		return &scanContentResult{
			matches:   matches,
			sanitized: text, // content unchanged in warn mode
			warning:   warning,
		}, nil
	}
}
