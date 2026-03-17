package parser

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	gositter "github.com/smacker/go-tree-sitter/golang"
)

// LogicWarning represents a single heuristic logic warning found by AST analysis.
type LogicWarning struct {
	Check    string `json:"check"`    // check name, e.g. "zero_value_id"
	File     string `json:"file"`     // file path
	Line     int    `json:"line"`     // 1-based line number
	Message  string `json:"message"`  // human-readable description
	Severity string `json:"severity"` // "warning"
}

// identifierWords are exact camelCase word matches (case-insensitive) that
// indicate an identifier parameter where zero is almost certainly wrong.
// These are matched against individual words produced by splitting the function
// name on camelCase boundaries — NOT as substrings — to avoid false positives
// on common words like "validate" (contains "id"), "account" (contains "count").
var identifierWords = map[string]bool{
	"port": true, "pid": true, "id": true, "count": true,
	"index": true, "fd": true, "num": true,
}

// cleanupPairs maps resource-acquiring call names to their expected cleanup counterparts.
// NewReader is intentionally excluded: bufio.Reader has no Close method and requires
// no explicit cleanup — closing the underlying reader is sufficient.
var cleanupPairs = map[string][]string{
	"Open":      {"Close"},
	"OpenFile":  {"Close"},
	"Create":    {"Close"},
	"Listen":    {"Close"},
	"Dial":      {"Close"},
	"DialTCP":   {"Close"},
	"DialUDP":   {"Close"},
	"DialUnix":  {"Close"},
	"NewWriter": {"Close", "Flush"},
	"Lock":      {"Unlock"},
	"RLock":     {"RUnlock"},
	"NewTicker": {"Stop"},
	"NewTimer":  {"Stop"},
}

// pathFunctions are functions where a tilde path is almost certainly a bug.
var pathFunctions = map[string]bool{
	"Open": true, "OpenFile": true, "Create": true, "Stat": true,
	"Lstat": true, "ReadFile": true, "WriteFile": true, "MkdirAll": true,
	"Mkdir": true, "Remove": true, "RemoveAll": true,
	"Dial": true, "DialTimeout": true,
	"Abs": true, "Join": true,
}

// RunLogicChecks parses the given source file and returns heuristic logic warnings.
// Only Go files are currently supported; other extensions return nil.
func RunLogicChecks(filePath string, src []byte) []LogicWarning {
	ext := strings.ToLower(filepath.Ext(filePath))
	if ext != ".go" {
		return nil
	}
	return runGoLogicChecks(filePath, src)
}

func runGoLogicChecks(filePath string, src []byte) []LogicWarning {
	parser := sitter.NewParser()
	parser.SetLanguage(gositter.GetLanguage())

	tree, err := parser.ParseCtx(context.Background(), nil, src)
	if err != nil || tree == nil {
		return nil
	}
	root := tree.RootNode()
	if root == nil {
		return nil
	}

	var warnings []LogicWarning

	// Walk all function/method bodies for checks.
	for i := 0; i < int(root.ChildCount()); i++ {
		child := root.Child(i)
		if child == nil {
			continue
		}
		switch child.Type() {
		case "function_declaration", "method_declaration":
			body := child.ChildByFieldName("body")
			if body == nil {
				continue
			}
			w := checkFunctionBody(filePath, body, src)
			warnings = append(warnings, w...)
		}
	}

	return warnings
}

// checkFunctionBody runs all heuristic checks against a single function body.
func checkFunctionBody(filePath string, body *sitter.Node, src []byte) []LogicWarning {
	var warnings []LogicWarning
	warnings = append(warnings, checkZeroValueIdentifier(filePath, body, src)...)
	warnings = append(warnings, checkMissingCleanup(filePath, body, src)...)
	warnings = append(warnings, checkPathExpansion(filePath, body, src)...)
	warnings = append(warnings, checkNilMethodCall(filePath, body, src)...)
	warnings = append(warnings, checkConcurrentMapWrite(filePath, body, src)...)
	return warnings
}

// checkZeroValueIdentifier finds function calls where a numeric argument is
// literal 0 but the called function name suggests an identifier parameter
// (port, pid, id, count, etc.).
func checkZeroValueIdentifier(filePath string, body *sitter.Node, src []byte) []LogicWarning {
	var warnings []LogicWarning
	walkAST(body, func(n *sitter.Node) {
		if n.Type() != "call_expression" {
			return
		}
		fnNode := n.ChildByFieldName("function")
		argsNode := n.ChildByFieldName("arguments")
		if fnNode == nil || argsNode == nil {
			return
		}
		funcName := nodeText(fnNode, src)
		// Split on camelCase boundaries and check each word independently.
		// Substring matching (e.g. "id" in "validate") produces too many false positives.
		if !funcNameMatchesIdentifier(funcName) {
			return
		}
		// Check if any argument is literal 0.
		for j := 0; j < int(argsNode.ChildCount()); j++ {
			arg := argsNode.Child(j)
			if arg != nil && arg.Type() == "int_literal" && nodeText(arg, src) == "0" {
				warnings = append(warnings, LogicWarning{
					Check:    "zero_value_id",
					File:     filePath,
					Line:     int(arg.StartPoint().Row) + 1,
					Message:  fmt.Sprintf("%s called with literal 0 — likely unintentional (port/pid/id should not be zero)", funcName),
					Severity: "warning",
				})
			}
		}
	})
	return warnings
}

// checkMissingCleanup detects resource-acquiring calls (Open, Create, Listen, Lock, etc.)
// without a corresponding cleanup call (Close, Unlock, etc.) in the same function body.
func checkMissingCleanup(filePath string, body *sitter.Node, src []byte) []LogicWarning {
	type acquireInfo struct {
		name string
		line int
	}
	var acquires []acquireInfo
	cleanupSeen := make(map[string]bool)

	walkAST(body, func(n *sitter.Node) {
		if n.Type() != "call_expression" {
			return
		}
		fnNode := n.ChildByFieldName("function")
		if fnNode == nil {
			return
		}
		callName := lastIdentifier(fnNode, src)
		if callName == "" {
			return
		}
		if _, ok := cleanupPairs[callName]; ok {
			acquires = append(acquires, acquireInfo{name: callName, line: int(n.StartPoint().Row) + 1})
		}
		// Track all function calls as potential cleanup.
		cleanupSeen[callName] = true
	})

	// Also check for defer statements containing cleanup calls.
	walkAST(body, func(n *sitter.Node) {
		if n.Type() != "defer_statement" {
			return
		}
		walkAST(n, func(inner *sitter.Node) {
			if inner.Type() == "call_expression" {
				fnNode := inner.ChildByFieldName("function")
				if fnNode != nil {
					callName := lastIdentifier(fnNode, src)
					if callName != "" {
						cleanupSeen[callName] = true
					}
				}
			}
		})
	})

	var warnings []LogicWarning
	for _, acq := range acquires {
		expectedCleanups := cleanupPairs[acq.name]
		found := false
		for _, cleanup := range expectedCleanups {
			if cleanupSeen[cleanup] {
				found = true
				break
			}
		}
		if !found {
			warnings = append(warnings, LogicWarning{
				Check:    "missing_cleanup",
				File:     filePath,
				Line:     acq.line,
				Message:  fmt.Sprintf("%s() called without corresponding %s in the same function — possible resource leak", acq.name, strings.Join(expectedCleanups, "/")),
				Severity: "warning",
			})
		}
	}
	return warnings
}

// checkPathExpansion detects string literals containing "~/" passed directly
// to filesystem or network functions without tilde expansion.
func checkPathExpansion(filePath string, body *sitter.Node, src []byte) []LogicWarning {
	var warnings []LogicWarning
	walkAST(body, func(n *sitter.Node) {
		if n.Type() != "call_expression" {
			return
		}
		fnNode := n.ChildByFieldName("function")
		argsNode := n.ChildByFieldName("arguments")
		if fnNode == nil || argsNode == nil {
			return
		}
		callName := lastIdentifier(fnNode, src)
		if !pathFunctions[callName] {
			return
		}
		for j := 0; j < int(argsNode.ChildCount()); j++ {
			arg := argsNode.Child(j)
			if arg == nil {
				continue
			}
			if arg.Type() == "interpreted_string_literal" || arg.Type() == "raw_string_literal" {
				text := nodeText(arg, src)
				if strings.Contains(text, "~/") || strings.Contains(text, "%USERPROFILE%") {
					warnings = append(warnings, LogicWarning{
						Check:    "path_no_expand",
						File:     filePath,
						Line:     int(arg.StartPoint().Row) + 1,
						Message:  fmt.Sprintf("path %s passed to %s() without tilde/env expansion — use os.UserHomeDir() or os.ExpandEnv()", text, callName),
						Severity: "warning",
					})
				}
			}
		}
	})
	return warnings
}

// checkNilMethodCall detects method calls on variables that were explicitly
// assigned nil at the TOP LEVEL of the function body (not inside a conditional
// branch) without an intervening nil check.
//
// Intentionally conservative: nil assignments inside if/for/select blocks are
// skipped because they are almost always inside an error-handling branch that
// returns early (e.g. `if err != nil { x = nil; return err }`). Tracking those
// would produce false positives on the standard Go error-handling pattern.
func checkNilMethodCall(filePath string, body *sitter.Node, src []byte) []LogicWarning {
	// Only collect nil assignments that are DIRECT children of the function body
	// block — not assignments nested inside if/for/select/switch blocks.
	// This avoids false positives for the common pattern:
	//   if err != nil { svc = nil; return err }
	//   svc.Start()  ← safe: unreachable when svc is nil
	nilVars := make(map[string]int) // var name → line of nil assignment
	for i := 0; i < int(body.NamedChildCount()); i++ {
		n := body.NamedChild(i)
		if n == nil {
			continue
		}
		switch n.Type() {
		case "short_var_declaration", "assignment_statement":
			right := n.ChildByFieldName("right")
			if right == nil {
				continue
			}
			if strings.TrimSpace(nodeText(right, src)) != "nil" {
				continue
			}
			left := n.ChildByFieldName("left")
			if left == nil {
				continue
			}
			for _, name := range strings.Split(nodeText(left, src), ",") {
				name = strings.TrimSpace(name)
				if name != "" && name != "_" {
					nilVars[name] = int(n.StartPoint().Row) + 1
				}
			}
		case "if_statement":
			// A top-level nil check removes the variable from suspicion.
			cond := n.ChildByFieldName("condition")
			if cond != nil {
				condText := nodeText(cond, src)
				if strings.Contains(condText, "!= nil") || strings.Contains(condText, "== nil") {
					parts := strings.Fields(condText)
					if len(parts) >= 1 {
						delete(nilVars, parts[0])
					}
				}
			}
		}
	}

	if len(nilVars) == 0 {
		return nil
	}

	// Find method calls on nil-assigned variables.
	var warnings []LogicWarning
	walkAST(body, func(n *sitter.Node) {
		if n.Type() != "call_expression" {
			return
		}
		fnNode := n.ChildByFieldName("function")
		if fnNode == nil || fnNode.Type() != "selector_expression" {
			return
		}
		operand := fnNode.ChildByFieldName("operand")
		if operand == nil || operand.Type() != "identifier" {
			return
		}
		varName := nodeText(operand, src)
		nilLine, isNil := nilVars[varName]
		if !isNil {
			return
		}
		callLine := int(n.StartPoint().Row) + 1
		if callLine > nilLine {
			method := fnNode.ChildByFieldName("field")
			methodName := ""
			if method != nil {
				methodName = nodeText(method, src)
			}
			warnings = append(warnings, LogicWarning{
				Check:    "nil_method_call",
				File:     filePath,
				Line:     callLine,
				Message:  fmt.Sprintf("%s.%s() called but %s was assigned nil at line %d without nil check", varName, methodName, varName, nilLine),
				Severity: "warning",
			})
		}
	})
	return warnings
}

// checkConcurrentMapWrite detects map index assignments inside go statements
// (goroutines) without a sync primitive (Mutex/RWMutex Lock/RLock) in scope.
func checkConcurrentMapWrite(filePath string, body *sitter.Node, src []byte) []LogicWarning {
	var warnings []LogicWarning
	walkAST(body, func(n *sitter.Node) {
		if n.Type() != "go_statement" {
			return
		}
		// Check if there's a sync primitive in the goroutine body.
		hasSyncPrimitive := false
		walkAST(n, func(inner *sitter.Node) {
			if inner.Type() == "call_expression" {
				fnNode := inner.ChildByFieldName("function")
				if fnNode != nil {
					callName := lastIdentifier(fnNode, src)
					if callName == "Lock" || callName == "RLock" || callName == "Store" || callName == "LoadOrStore" || callName == "CompareAndSwap" {
						hasSyncPrimitive = true
					}
				}
			}
		})

		if hasSyncPrimitive {
			return
		}

		// Look for map index assignments inside the goroutine.
		walkAST(n, func(inner *sitter.Node) {
			if inner.Type() != "assignment_statement" {
				return
			}
			left := inner.ChildByFieldName("left")
			if left == nil {
				return
			}
			// Check if left side contains an index_expression (map[key] = value).
			hasMapWrite := false
			walkAST(left, func(lhs *sitter.Node) {
				if lhs.Type() == "index_expression" {
					hasMapWrite = true
				}
			})
			if hasMapWrite {
				warnings = append(warnings, LogicWarning{
					Check:    "concurrent_map_write",
					File:     filePath,
					Line:     int(inner.StartPoint().Row) + 1,
					Message:  "map write inside goroutine without sync primitive — use sync.Mutex or sync.Map",
					Severity: "warning",
				})
			}
		})
	})
	return warnings
}

// funcNameMatchesIdentifier returns true if any camelCase word in the function
// name exactly matches one of the identifierWords (port, pid, id, count, etc.).
// Uses word-level matching to avoid substring false positives:
//   "validateInput"  → ["validate","Input"]  → none match  → false ✓
//   "killByPort"     → ["kill","By","Port"]   → "Port"→"port" match → true ✓
//   "accountBalance" → ["account","Balance"]  → none match  → false ✓
func funcNameMatchesIdentifier(funcName string) bool {
	// Strip package qualifier: "pkg.Func" → "Func"
	if dot := strings.LastIndex(funcName, "."); dot >= 0 {
		funcName = funcName[dot+1:]
	}
	for _, word := range splitCamelWords(funcName) {
		if identifierWords[strings.ToLower(word)] {
			return true
		}
	}
	return false
}

// splitCamelWords splits a camelCase or PascalCase identifier into its
// constituent words. Consecutive uppercase runs (acronyms) are kept together.
// Examples:
//   "killByPort"    → ["kill","By","Port"]
//   "setHTTPPort"   → ["set","HTTP","Port"]
//   "getID"         → ["get","ID"]
//   "validateInput" → ["validate","Input"]
func splitCamelWords(s string) []string {
	if s == "" {
		return nil
	}
	var words []string
	start := 0
	runes := []rune(s)
	n := len(runes)
	for i := 1; i < n; i++ {
		curr := runes[i]
		prev := runes[i-1]
		// Transition: lower→upper or digit→upper starts a new word.
		if isUpper(curr) && (isLower(prev) || isDigit(prev)) {
			words = append(words, string(runes[start:i]))
			start = i
			continue
		}
		// Transition: upper-run followed by upper+lower (e.g. "HTTPPort" → "HTTP","Port").
		if i+1 < n && isUpper(curr) && isUpper(prev) && isLower(runes[i+1]) {
			words = append(words, string(runes[start:i]))
			start = i
		}
	}
	words = append(words, string(runes[start:]))
	return words
}

func isUpper(r rune) bool { return r >= 'A' && r <= 'Z' }
func isLower(r rune) bool { return r >= 'a' && r <= 'z' }
func isDigit(r rune) bool { return r >= '0' && r <= '9' }

// walkAST recursively visits every node in the subtree rooted at n.
func walkAST(n *sitter.Node, visit func(*sitter.Node)) {
	if n == nil {
		return
	}
	visit(n)
	for i := 0; i < int(n.ChildCount()); i++ {
		walkAST(n.Child(i), visit)
	}
}

// nodeText returns the source text for a tree-sitter node.
func nodeText(n *sitter.Node, src []byte) string {
	return string(src[n.StartByte():n.EndByte()])
}

// lastIdentifier extracts the rightmost identifier from a function expression.
// For "pkg.Func" returns "Func", for "a.b.Method" returns "Method", for "Func" returns "Func".
func lastIdentifier(fnNode *sitter.Node, src []byte) string {
	switch fnNode.Type() {
	case "selector_expression":
		field := fnNode.ChildByFieldName("field")
		if field != nil {
			return nodeText(field, src)
		}
	case "identifier":
		return nodeText(fnNode, src)
	}
	return ""
}
