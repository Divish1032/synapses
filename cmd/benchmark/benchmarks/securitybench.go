// securitybench.go implements SecurityBench — measures precision, recall,
// and severity accuracy of Synapses' 70 builtin security patterns.
//
// 80 synthetic micro-codebases (40 TP, 40 TN) across 7 pattern categories.
// Calls security.Engine.CheckFile() directly — no MCP, no agent, no Docker.
//
// Metrics: precision, recall, Youden Index (OWASP standard), per-severity accuracy.
package benchmarks

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/SynapsesOS/synapses/cmd/benchmark/reporter"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/security"
)

// SecurityBenchCase is one test case in the SecurityBench JSONL file.
type SecurityBenchCase struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Category    string `json:"category"` // auth, credentials, layers, admin, rate_limit, cross_transport, data_flow
	Language    string `json:"language"` // go, python, java, typescript, rust

	// Graph setup: nodes and edges to build.
	File    string   `json:"file"`    // file path to check
	Imports []string `json:"imports"` // import paths (creates NodePackage + IMPORTS edges)

	// Functions with their callees (creates NodeFunction + CALLS edges).
	Functions []struct {
		Name    string   `json:"name"`
		Callees []string `json:"callees"`
	} `json:"functions,omitempty"`

	// Routes registered in the file.
	Routes []struct {
		Method string `json:"method"`
		Path   string `json:"path"`
	} `json:"routes,omitempty"`

	// File content for content-based checks (hardcoded secrets).
	Content string `json:"content,omitempty"`

	// Expected: empty for true negatives, non-empty for true positives.
	ExpectedViolations []struct {
		PatternID string `json:"pattern_id"`
		Severity  string `json:"severity"` // CRITICAL, HIGH, MEDIUM
		Action    string `json:"action"`   // block, warn, inform
	} `json:"expected_violations"`
}

// SecurityBenchResult holds results for one test case.
type SecurityBenchResult struct {
	CaseID    string `json:"case_id"`
	Category  string `json:"category"`
	Language  string `json:"language"`
	IsTP      bool   `json:"is_tp"`      // ground truth: should fire
	Fired     bool   `json:"fired"`      // did fire
	Correct   bool   `json:"correct"`    // fired == expected
	Violations int   `json:"violations"` // number of violations found

	// For TP cases: did we match the expected pattern + severity?
	PatternMatch  bool `json:"pattern_match,omitempty"`
	SeverityMatch bool `json:"severity_match,omitempty"`
}

// RunSecurityBench runs the SecurityBench and returns results.
func RunSecurityBench(dataFile string) (*reporter.SecurityBenchReport, error) {
	cases, err := loadSecurityBenchCases(dataFile)
	if err != nil {
		return nil, fmt.Errorf("load cases: %w", err)
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("no test cases in %s", dataFile)
	}

	engine := security.DefaultEngine()
	log.Printf("[securitybench] %d test cases, %d patterns loaded", len(cases), engine.PatternCount())

	var results []SecurityBenchResult
	var tp, fp, tn, fn int

	for _, tc := range cases {
		r := runSecurityCase(engine, tc)
		results = append(results, r)

		if r.IsTP && r.Fired {
			tp++
		} else if r.IsTP && !r.Fired {
			fn++
		} else if !r.IsTP && r.Fired {
			fp++
		} else {
			tn++
		}
	}

	// Compute metrics.
	precision := float64(0)
	if tp+fp > 0 {
		precision = float64(tp) / float64(tp+fp)
	}
	recall := float64(0)
	if tp+fn > 0 {
		recall = float64(tp) / float64(tp+fn)
	}
	f1 := float64(0)
	if precision+recall > 0 {
		f1 = 2 * precision * recall / (precision + recall)
	}

	tpr := float64(0)
	if tp+fn > 0 {
		tpr = float64(tp) / float64(tp+fn)
	}
	fpr := float64(0)
	if fp+tn > 0 {
		fpr = float64(fp) / float64(fp+tn)
	}
	youden := tpr - fpr

	// Per-category breakdown.
	catResults := map[string]*struct{ tp, fp, tn, fn int }{}
	for _, r := range results {
		cr, ok := catResults[r.Category]
		if !ok {
			cr = &struct{ tp, fp, tn, fn int }{}
			catResults[r.Category] = cr
		}
		if r.IsTP && r.Fired {
			cr.tp++
		} else if r.IsTP && !r.Fired {
			cr.fn++
		} else if !r.IsTP && r.Fired {
			cr.fp++
		} else {
			cr.tn++
		}
	}

	// Severity accuracy (for TP cases that fired).
	severityCorrect := 0
	severityTotal := 0
	for _, r := range results {
		if r.IsTP && r.Fired {
			severityTotal++
			if r.SeverityMatch {
				severityCorrect++
			}
		}
	}

	report := &reporter.SecurityBenchReport{
		Timestamp:       reporter.Timestamp(),
		TotalCases:      len(cases),
		TP:              tp,
		FP:              fp,
		TN:              tn,
		FN:              fn,
		Precision:       precision * 100,
		Recall:          recall * 100,
		F1:              f1 * 100,
		YoudenIndex:     youden * 100,
		SeverityAccuracy: float64(severityCorrect) / float64(max(severityTotal, 1)) * 100,
		PatternCount:    engine.PatternCount(),
	}

	// Per-category.
	for cat, cr := range catResults {
		p := float64(0)
		if cr.tp+cr.fp > 0 {
			p = float64(cr.tp) / float64(cr.tp+cr.fp)
		}
		r := float64(0)
		if cr.tp+cr.fn > 0 {
			r = float64(cr.tp) / float64(cr.tp+cr.fn)
		}
		report.Categories = append(report.Categories, reporter.SecurityBenchCategory{
			Name:      cat,
			TP:        cr.tp,
			FP:        cr.fp,
			TN:        cr.tn,
			FN:        cr.fn,
			Precision: p * 100,
			Recall:    r * 100,
		})
	}

	report.Cases = results

	log.Printf("[securitybench] TP=%d FP=%d TN=%d FN=%d", tp, fp, tn, fn)
	log.Printf("[securitybench] Precision=%.1f%% Recall=%.1f%% F1=%.1f%% Youden=%.1f%%",
		report.Precision, report.Recall, report.F1, report.YoudenIndex)

	return report, nil
}

func runSecurityCase(engine *security.Engine, tc SecurityBenchCase) SecurityBenchResult {
	g := graph.New("test-repo")
	g.SetRoot("/project")

	// Build graph from test case.
	fileID := g.MakeNodeID(tc.File, tc.File)
	g.AddNode(&graph.Node{
		ID:   fileID,
		Type: graph.NodeFile,
		Name: tc.File,
		File: tc.File,
	})

	for _, imp := range tc.Imports {
		impID := g.MakeNodeID(imp, imp)
		g.AddNode(&graph.Node{
			ID:      impID,
			Type:    graph.NodePackage,
			Name:    imp,
			Package: imp,
			File:    tc.File,
		})
		g.AddEdge(&graph.Edge{From: fileID, To: impID, Type: graph.EdgeImports})
	}

	for _, fn := range tc.Functions {
		fnID := g.MakeNodeID(tc.File, fn.Name)
		g.AddNode(&graph.Node{
			ID:   fnID,
			Type: graph.NodeFunction,
			Name: fn.Name,
			File: tc.File,
		})
		for _, callee := range fn.Callees {
			calleeID := g.MakeNodeID("callee", callee)
			g.AddNode(&graph.Node{
				ID:   calleeID,
				Type: graph.NodeFunction,
				Name: callee,
				File: "other.go",
			})
			g.AddEdge(&graph.Edge{From: fnID, To: calleeID, Type: graph.EdgeCalls})
		}
	}

	for _, route := range tc.Routes {
		routeName := route.Method + " " + route.Path
		routeID := g.MakeNodeID(tc.File, "route:"+routeName)
		g.UpsertRouteNode(&graph.Node{
			ID:   routeID,
			Type: graph.NodeRoute,
			Name: routeName,
			File: tc.File,
			Metadata: map[string]string{
				"method": route.Method,
				"path":   route.Path,
			},
		})
	}

	// Run check.
	var content []byte
	if tc.Content != "" {
		content = []byte(tc.Content)
	}
	violations := engine.CheckFile(g, tc.File, content)

	// Evaluate.
	isTP := len(tc.ExpectedViolations) > 0
	fired := len(violations) > 0

	result := SecurityBenchResult{
		CaseID:     tc.ID,
		Category:   tc.Category,
		Language:   tc.Language,
		IsTP:       isTP,
		Fired:      fired,
		Correct:    fired == isTP,
		Violations: len(violations),
	}

	// For TP cases, check pattern and severity match.
	if isTP && fired {
		expected := tc.ExpectedViolations[0] // primary expected violation
		for _, v := range violations {
			if v.PatternID == expected.PatternID {
				result.PatternMatch = true
				if strings.EqualFold(string(v.Severity), expected.Severity) {
					result.SeverityMatch = true
				}
				break
			}
		}
	}

	return result
}

func loadSecurityBenchCases(path string) ([]SecurityBenchCase, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cases []SecurityBenchCase
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var tc SecurityBenchCase
		if err := json.Unmarshal([]byte(line), &tc); err != nil {
			log.Printf("WARNING: skip malformed line: %v", err)
			continue
		}
		cases = append(cases, tc)
	}
	return cases, nil
}
