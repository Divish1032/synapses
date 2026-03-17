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

// identifierParams contains parameter-name substrings that indicate an identifier
// where zero is almost certainly wrong.
var identifierParams = []string{"port", "pid", "id", "count", "index", "fd", "num"}

// cleanupPairs maps resource-acquiring call names to their expected cleanup counterparts.
var cleanupPairs = map[string][]string{
	"Open":      {"Close"},
	"OpenFile":  {"Close"},
	"Create":    {"Close"},
	"Listen":    {"Close"},
	"Dial":      {"Close"},
	"DialTCP":   {"Close"},
	"DialUDP":   {"Close"},
	"DialUnix":  {"Close"},
	"NewReader": {"Close"},
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
		// Check if the function name contains an identifier-like substring.
		funcLower := strings.ToLower(funcName)
		matchesIdentifier := false
		for _, param := range identifierParams {
			if strings.Contains(funcLower, param) {
				matchesIdentifier = true
				break
			}
		}
		if !matchesIdentifier {
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
// assigned nil in the same scope without an intervening nil check.
// This is a conservative heuristic — it only flags direct nil assignments
// followed by method calls on the same variable name.
func checkNilMethodCall(filePath string, body *sitter.Node, src []byte) []LogicWarning {
	// Collect variables assigned nil via short_var_declaration or assignment_statement.
	nilVars := make(map[string]int) // var name → line of nil assignment
	walkAST(body, func(n *sitter.Node) {
		if n.Type() == "short_var_declaration" || n.Type() == "assignment_statement" {
			right := n.ChildByFieldName("right")
			if right == nil {
				return
			}
			// Check if right side is nil.
			rightText := strings.TrimSpace(nodeText(right, src))
			if rightText != "nil" {
				return
			}
			left := n.ChildByFieldName("left")
			if left == nil {
				return
			}
			// Extract variable names from left side.
			leftText := nodeText(left, src)
			for _, name := range strings.Split(leftText, ",") {
				name = strings.TrimSpace(name)
				if name != "" && name != "_" {
					nilVars[name] = int(n.StartPoint().Row) + 1
				}
			}
		}
		// If we see a nil check (if x != nil / if x == nil), remove from nilVars.
		if n.Type() == "if_statement" {
			cond := n.ChildByFieldName("condition")
			if cond != nil {
				condText := nodeText(cond, src)
				if strings.Contains(condText, "!= nil") || strings.Contains(condText, "== nil") {
					// Extract the variable being checked.
					parts := strings.Fields(condText)
					if len(parts) >= 1 {
						delete(nilVars, parts[0])
					}
				}
			}
		}
	})

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
