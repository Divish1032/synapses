package parser

import (
	"fmt"
	"path/filepath"
	"strings"

	sitter "github.com/alexaandru/go-tree-sitter-bare"
	gositter "github.com/alexaandru/go-sitter-forest/go"
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
// indicate a parameter where literal zero is almost certainly a bug. These are
// matched against EVERY word produced by camelCase-splitting the function name
// (not as substrings) to avoid false positives on "validate"->["validate"],
// "account"->["account","Balance"], etc.
//
// "index" is intentionally excluded: slice index 0 is the first valid element
// and is ubiquitous in Go code -- it would produce enormous false-positive noise.
var identifierWords = map[string]bool{
	"port": true, "pid": true, "id": true, "count": true, "fd": true, "num": true,
}

// cleanupPairs maps resource-acquiring call names to their expected cleanup counterparts.
//
// Intentional exclusions:
//   - NewReader: bufio.Reader has no Close method; closing the underlying reader suffices.
//   - NewWriter "Close": bufio.Writer has no Close method (only Flush); including "Close"
//     produced a false-negative when an unrelated Close() was present.
var cleanupPairs = map[string][]string{
	"Open":      {"Close"},
	"OpenFile":  {"Close"},
	"Create":    {"Close"},
	"Listen":    {"Close"},
	"Dial":      {"Close"},
	"DialTCP":   {"Close"},
	"DialUDP":   {"Close"},
	"DialUnix":  {"Close"},
	"NewWriter": {"Flush"},
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
	parser.SetLanguage(sitter.NewLanguage(gositter.GetLanguage()))

	parseCtx, parseCancel := parseContext()
	defer parseCancel()
	tree, err := parser.ParseString(parseCtx, nil, src)
	if err != nil || tree == nil {
		return nil
	}
	defer tree.Close()
	root := tree.RootNode()
	if root.IsNull() {
		return nil
	}

	var warnings []LogicWarning

	for i := uint32(0); i < root.ChildCount(); i++ {
		child := root.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "function_declaration", "method_declaration":
			body := child.ChildByFieldName("body")
			if body.IsNull() {
				continue
			}
			warnings = append(warnings, checkFunctionBody(filePath, body, src)...)
			// checkMissingCleanup uses walkASTNoFuncLit so closure-internal
			// resource leaks are invisible from the outer body. Analyse every
			// nested closure as its own independent scope to catch those leaks.
			warnings = append(warnings, checkClosureCleanup(filePath, body, src)...)
		}
	}

	return warnings
}

// checkClosureCleanup finds every func_literal nested within body and runs
// checkMissingCleanup on each as its own independent scope.
// walkASTNoFuncLit ensures each level only sees DIRECT child closures;
// deeper nesting is handled by the recursive call.
func checkClosureCleanup(filePath string, body sitter.Node, src []byte) []LogicWarning {
	var warnings []LogicWarning
	walkASTNoFuncLit(body, func(n sitter.Node) {
		if n.Type() != "func_literal" {
			return
		}
		innerBody := n.ChildByFieldName("body")
		if innerBody.IsNull() {
			return
		}
		warnings = append(warnings, checkMissingCleanup(filePath, innerBody, src)...)
		warnings = append(warnings, checkClosureCleanup(filePath, innerBody, src)...)
	})
	return warnings
}

// checkFunctionBody runs all heuristic checks against a single function body.
func checkFunctionBody(filePath string, body sitter.Node, src []byte) []LogicWarning {
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
func checkZeroValueIdentifier(filePath string, body sitter.Node, src []byte) []LogicWarning {
	var warnings []LogicWarning
	walkAST(body, func(n sitter.Node) {
		if n.Type() != "call_expression" {
			return
		}
		fnNode := n.ChildByFieldName("function")
		argsNode := n.ChildByFieldName("arguments")
		if fnNode.IsNull() || argsNode.IsNull() {
			return
		}
		funcName := nodeText(fnNode, src)
		if !funcNameMatchesIdentifier(funcName) {
			return
		}
		for j := uint32(0); j < argsNode.ChildCount(); j++ {
			arg := argsNode.Child(j)
			if !arg.IsNull() && arg.Type() == "int_literal" && nodeText(arg, src) == "0" {
				warnings = append(warnings, LogicWarning{
					Check:    "zero_value_id",
					File:     filePath,
					Line:     int(arg.StartPoint().Row) + 1,
					Message:  fmt.Sprintf("%s called with literal 0 -- likely unintentional (port/pid/id should not be zero)", funcName),
					Severity: "warning",
				})
			}
		}
	})
	return warnings
}

// checkMissingCleanup detects resource-acquiring calls (Open, Create, Listen, Lock, etc.)
// without a corresponding cleanup call (Close, Unlock, etc.) in the same function body.
//
// Per-variable tracking: when the acquire result is captured in a variable
// (e.g. f, err := os.Open(...)), the check looks for f.Close() specifically
// rather than any Close() call. This prevents a different file's Close() from
// silently satisfying an unrelated leaked resource's requirement.
//
// Closure isolation: walkASTNoFuncLit is used for acquire detection so that
// resources acquired inside a nested closure are not attributed to the enclosing
// function. Cleanup detection uses plain walkAST so that defer func() { f.Close() }()
// patterns are still recognised.
func checkMissingCleanup(filePath string, body sitter.Node, src []byte) []LogicWarning {
	type acquireInfo struct {
		acquireName string
		varName     string // resource variable ("f" from f,err:=os.Open); "" if untracked
		line        int
	}
	var acquires []acquireInfo
	trackedLines := make(map[int]bool)

	// Walk 1: assignments whose RHS contains an acquire call.
	walkASTNoFuncLit(body, func(n sitter.Node) {
		if n.Type() != "short_var_declaration" && n.Type() != "assignment_statement" {
			return
		}
		right := n.ChildByFieldName("right")
		if right.IsNull() {
			return
		}
		var foundCall sitter.Node
		walkAST(right, func(rn sitter.Node) {
			if !foundCall.IsNull() {
				return
			}
			if rn.Type() != "call_expression" {
				return
			}
			fn := rn.ChildByFieldName("function")
			if fn.IsNull() {
				return
			}
			if _, ok := cleanupPairs[lastIdentifier(fn, src)]; ok {
				foundCall = rn
			}
		})
		if foundCall.IsNull() {
			return
		}
		fn := foundCall.ChildByFieldName("function")
		acquireName := lastIdentifier(fn, src)
		line := int(foundCall.StartPoint().Row) + 1
		trackedLines[line] = true
		left := n.ChildByFieldName("left")
		varName := ""
		if !left.IsNull() {
			varName = firstAssignVar(left, src)
		}
		acquires = append(acquires, acquireInfo{acquireName: acquireName, varName: varName, line: line})
	})

	// Walk 2: bare call_expressions -- method-style acquires (mu.Lock()) and
	// unassigned opens. Skips lines already captured in Walk 1.
	walkASTNoFuncLit(body, func(n sitter.Node) {
		if n.Type() != "call_expression" {
			return
		}
		fn := n.ChildByFieldName("function")
		if fn.IsNull() {
			return
		}
		acquireName := lastIdentifier(fn, src)
		if _, ok := cleanupPairs[acquireName]; !ok {
			return
		}
		line := int(n.StartPoint().Row) + 1
		if trackedLines[line] {
			return
		}
		varName := ""
		if fn.Type() == "selector_expression" {
			if operand := fn.ChildByFieldName("operand"); !operand.IsNull() {
				varName = nodeText(operand, src)
			}
		}
		acquires = append(acquires, acquireInfo{acquireName: acquireName, varName: varName, line: line})
	})

	if len(acquires) == 0 {
		return nil
	}

	varsCleaned := make(map[string]bool)
	anyCleanupSeen := make(map[string]bool)
	walkAST(body, func(n sitter.Node) {
		if n.Type() != "call_expression" {
			return
		}
		fn := n.ChildByFieldName("function")
		if fn.IsNull() {
			return
		}
		if fn.Type() == "selector_expression" {
			operand := fn.ChildByFieldName("operand")
			field := fn.ChildByFieldName("field")
			if !operand.IsNull() && !field.IsNull() {
				varsCleaned[nodeText(operand, src)+"."+nodeText(field, src)] = true
				anyCleanupSeen[nodeText(field, src)] = true
			}
		} else {
			anyCleanupSeen[lastIdentifier(fn, src)] = true
		}
	})

	var warnings []LogicWarning
	for _, acq := range acquires {
		expectedCleanups := cleanupPairs[acq.acquireName]
		found := false
		for _, cleanup := range expectedCleanups {
			if acq.varName != "" {
				if varsCleaned[acq.varName+"."+cleanup] {
					found = true
					break
				}
			} else {
				if anyCleanupSeen[cleanup] {
					found = true
					break
				}
			}
		}
		if !found {
			warnings = append(warnings, LogicWarning{
				Check:    "missing_cleanup",
				File:     filePath,
				Line:     acq.line,
				Message:  fmt.Sprintf("%s() called without corresponding %s in the same function -- possible resource leak", acq.acquireName, strings.Join(expectedCleanups, "/")),
				Severity: "warning",
			})
		}
	}
	return warnings
}

// checkPathExpansion detects string literals containing "~/" passed directly
// to filesystem or network functions without tilde expansion.
func checkPathExpansion(filePath string, body sitter.Node, src []byte) []LogicWarning {
	var warnings []LogicWarning
	walkAST(body, func(n sitter.Node) {
		if n.Type() != "call_expression" {
			return
		}
		fnNode := n.ChildByFieldName("function")
		argsNode := n.ChildByFieldName("arguments")
		if fnNode.IsNull() || argsNode.IsNull() {
			return
		}
		callName := lastIdentifier(fnNode, src)
		if !pathFunctions[callName] {
			return
		}
		for j := uint32(0); j < argsNode.ChildCount(); j++ {
			arg := argsNode.Child(j)
			if arg.IsNull() {
				continue
			}
			if arg.Type() == "interpreted_string_literal" || arg.Type() == "raw_string_literal" {
				text := nodeText(arg, src)
				if strings.Contains(text, "~/") || strings.Contains(text, "%USERPROFILE%") {
					warnings = append(warnings, LogicWarning{
						Check:    "path_no_expand",
						File:     filePath,
						Line:     int(arg.StartPoint().Row) + 1,
						Message:  fmt.Sprintf("path %s passed to %s() without tilde/env expansion -- use os.UserHomeDir() or os.ExpandEnv()", text, callName),
						Severity: "warning",
					})
				}
			}
		}
	})
	return warnings
}

// checkNilMethodCall detects method calls on variables explicitly assigned nil
// at the TOP LEVEL of the function body (not inside a conditional branch).
func checkNilMethodCall(filePath string, body sitter.Node, src []byte) []LogicWarning {
	nilVars := make(map[string]int)
	for i := uint32(0); i < body.NamedChildCount(); i++ {
		n := body.NamedChild(i)
		if n.IsNull() {
			continue
		}
		switch n.Type() {
		case "short_var_declaration", "assignment_statement":
			right := n.ChildByFieldName("right")
			if right.IsNull() {
				continue
			}
			if strings.TrimSpace(nodeText(right, src)) != "nil" {
				continue
			}
			left := n.ChildByFieldName("left")
			if left.IsNull() {
				continue
			}
			for _, name := range strings.Split(nodeText(left, src), ",") {
				name = strings.TrimSpace(name)
				if name != "" && name != "_" {
					nilVars[name] = int(n.StartPoint().Row) + 1
				}
			}
		case "if_statement":
			cond := n.ChildByFieldName("condition")
			if !cond.IsNull() {
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

	var warnings []LogicWarning
	walkAST(body, func(n sitter.Node) {
		if n.Type() != "call_expression" {
			return
		}
		fnNode := n.ChildByFieldName("function")
		if fnNode.IsNull() || fnNode.Type() != "selector_expression" {
			return
		}
		operand := fnNode.ChildByFieldName("operand")
		if operand.IsNull() || operand.Type() != "identifier" {
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
			if !method.IsNull() {
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
func checkConcurrentMapWrite(filePath string, body sitter.Node, src []byte) []LogicWarning {
	var warnings []LogicWarning
	walkAST(body, func(n sitter.Node) {
		if n.Type() != "go_statement" {
			return
		}
		hasSyncPrimitive := false
		walkAST(n, func(inner sitter.Node) {
			if inner.Type() == "call_expression" {
				fnNode := inner.ChildByFieldName("function")
				if !fnNode.IsNull() {
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

		walkAST(n, func(inner sitter.Node) {
			if inner.Type() != "assignment_statement" {
				return
			}
			left := inner.ChildByFieldName("left")
			if left.IsNull() {
				return
			}
			hasMapWrite := false
			walkAST(left, func(lhs sitter.Node) {
				if lhs.Type() == "index_expression" && !isSliceIndex(lhs, src) {
					hasMapWrite = true
				}
			})
			if hasMapWrite {
				warnings = append(warnings, LogicWarning{
					Check:    "concurrent_map_write",
					File:     filePath,
					Line:     int(inner.StartPoint().Row) + 1,
					Message:  "map write inside goroutine without sync primitive -- use sync.Mutex or sync.Map",
					Severity: "warning",
				})
			}
		})
	})
	return warnings
}

// funcNameMatchesIdentifier returns true if ANY camelCase word in the function
// name exactly matches one of the identifierWords (port, pid, id, count, etc.).
// Word-level matching (not substring) avoids false positives on names like
// "validateInput", "accountBalance", "consider".
// All-words is intentional: an identifier keyword anywhere in the name is
// semantically meaningful (connectToPort, SetPortNumber, bindPortAddress).
func funcNameMatchesIdentifier(funcName string) bool {
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
		if isUpper(curr) && (isLower(prev) || isDigit(prev)) {
			words = append(words, string(runes[start:i]))
			start = i
			continue
		}
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
func walkAST(n sitter.Node, visit func(sitter.Node)) {
	if n.IsNull() {
		return
	}
	visit(n)
	for i := uint32(0); i < n.ChildCount(); i++ {
		walkAST(n.Child(i), visit)
	}
}

// walkASTNoFuncLit is like walkAST but does not descend into func_literal nodes.
// Prevents nested closures from being analysed as part of the enclosing function.
func walkASTNoFuncLit(n sitter.Node, visit func(sitter.Node)) {
	if n.IsNull() {
		return
	}
	visit(n)
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		if child.Type() == "func_literal" {
			visit(child) // visit the closure node itself but do not recurse into it
			continue
		}
		walkASTNoFuncLit(child, visit)
	}
}

// firstAssignVar returns the first non-"_" identifier on the LHS of an
// assignment or short_var_declaration.
func firstAssignVar(left sitter.Node, src []byte) string {
	text := nodeText(left, src)
	parts := strings.SplitN(text, ",", 2)
	if len(parts) == 0 {
		return ""
	}
	name := strings.TrimSpace(parts[0])
	if name == "_" {
		return ""
	}
	return name
}

// isSliceIndex reports whether an index_expression's index looks like a numeric
// slice access rather than a map key, delegating to looksLikeIntegerExpr.
func isSliceIndex(indexExpr sitter.Node, src []byte) bool {
	idx := indexExpr.ChildByFieldName("index")
	if idx.IsNull() && int(indexExpr.ChildCount()) >= 3 {
		idx = indexExpr.Child(2)
	}
	return looksLikeIntegerExpr(idx, src)
}

// looksLikeIntegerExpr returns true when a node is almost certainly an integer
// expression rather than a map key. Handles: integer literals, common loop
// counter identifiers, integer type conversions, len/cap, binary arithmetic,
// unary negation, and parenthesised expressions.
func looksLikeIntegerExpr(n sitter.Node, src []byte) bool {
	if n.IsNull() {
		return false
	}
	switch n.Type() {
	case "int_literal":
		return true

	case "identifier":
		switch nodeText(n, src) {
		case "i", "j", "k", "l", "m", "n",
			"x", "y", "z",
			"idx", "index", "pos", "off", "offset",
			"row", "col", "r", "c",
			"start", "end", "cur", "head", "tail",
			"cnt", "count", "num", "step", "stride":
			return true
		}
		return false

	case "binary_expression":
		left := n.ChildByFieldName("left")
		right := n.ChildByFieldName("right")
		return looksLikeIntegerExpr(left, src) || looksLikeIntegerExpr(right, src)

	case "unary_expression":
		operand := n.ChildByFieldName("operand")
		return looksLikeIntegerExpr(operand, src)

	case "call_expression":
		fn := n.ChildByFieldName("function")
		if fn.IsNull() {
			return false
		}
		switch nodeText(fn, src) {
		case "int", "int8", "int16", "int32", "int64",
			"uint", "uint8", "uint16", "uint32", "uint64",
			"uintptr", "byte", "rune",
			"len", "cap":
			return true
		}
		return false

	case "parenthesized_expression":
		if int(n.ChildCount()) >= 2 {
			return looksLikeIntegerExpr(n.Child(1), src)
		}
		return false
	}
	return false
}

// nodeText returns the source text for a tree-sitter node.
func nodeText(n sitter.Node, src []byte) string {
	return string(src[n.StartByte():n.EndByte()])
}

// lastIdentifier extracts the rightmost identifier from a function expression.
func lastIdentifier(fnNode sitter.Node, src []byte) string {
	switch fnNode.Type() {
	case "selector_expression":
		field := fnNode.ChildByFieldName("field")
		if !field.IsNull() {
			return nodeText(field, src)
		}
	case "identifier":
		return nodeText(fnNode, src)
	}
	return ""
}
