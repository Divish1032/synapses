// Package git provides utilities for extracting intelligence from git history.
package git

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// CouplingPair represents two files that frequently co-change in git history.
type CouplingPair struct {
	FileA      string  `json:"file_a"`
	FileB      string  `json:"file_b"`
	CoChanges  int     `json:"co_changes"`  // number of commits where both changed
	Confidence float64 `json:"confidence"`  // co_changes / max(total_A, total_B)
	SupportA   int     `json:"support_a"`   // total commits touching file_a
	SupportB   int     `json:"support_b"`   // total commits touching file_b
}

// AnalyzeChangeCoupling parses git log for the given repo to find files that
// frequently change together. commitLimit caps how many recent commits are
// analysed (0 uses 500). Returns pairs sorted by confidence descending.
//
// Algorithm (same as axon's coupling phase, ported to Go):
//  1. Parse `git log --format=%H` to get commit SHAs
//  2. For each commit, get changed files via `git diff-tree --name-only`
//  3. Build a co-occurrence matrix: for each pair of files in a commit, increment counter
//  4. Filter: skip commits with >50 files (merges/bulk reformats)
//  5. Compute confidence = co_changes / max(total_A, total_B)
//  6. Keep pairs with co_changes >= 3 AND confidence >= minConfidence (default 0.3)
func AnalyzeChangeCoupling(repoRoot string, commitLimit int, minConfidence float64) ([]CouplingPair, error) {
	if commitLimit <= 0 {
		commitLimit = 500
	}
	if minConfidence <= 0 {
		minConfidence = 0.3
	}

	// Step 1: get recent commit SHAs.
	shaOut, err := exec.Command(
		"git", "-C", repoRoot,
		"log", "--format=%H", fmt.Sprintf("-n%d", commitLimit),
	).Output()
	if err != nil {
		return nil, fmt.Errorf("git log: %w", err)
	}

	shas := strings.Fields(string(shaOut))
	if len(shas) == 0 {
		return nil, nil
	}

	// Step 2 & 3: for each commit collect changed files and update counters.
	totalChanges := map[string]int{}   // file → total commits that touched it
	coChanges := map[[2]string]int{}   // sorted pair → co-change count

	for _, sha := range shas {
		filesOut, err := exec.Command(
			"git", "-C", repoRoot,
			"diff-tree", "--no-commit-id", "--name-only", "-r", sha,
		).Output()
		if err != nil {
			continue // skip commits that can't be diffed (initial commit, etc.)
		}

		files := strings.Fields(string(filesOut))

		// Step 4: skip large commits (merges / bulk reformats).
		if len(files) > 50 {
			continue
		}

		for _, f := range files {
			totalChanges[f]++
		}

		// Build pairs for this commit.
		for i := 0; i < len(files); i++ {
			for j := i + 1; j < len(files); j++ {
				a, b := files[i], files[j]
				if a > b {
					a, b = b, a // canonical order: lexicographically smaller first
				}
				coChanges[[2]string{a, b}]++
			}
		}
	}

	// Step 5 & 6: compute confidence, filter, and collect results.
	var pairs []CouplingPair
	for pair, count := range coChanges {
		if count < 3 {
			continue
		}
		supA := totalChanges[pair[0]]
		supB := totalChanges[pair[1]]
		maxSup := supA
		if supB > maxSup {
			maxSup = supB
		}
		if maxSup == 0 {
			continue
		}
		confidence := float64(count) / float64(maxSup)
		if confidence < minConfidence {
			continue
		}
		pairs = append(pairs, CouplingPair{
			FileA:      pair[0],
			FileB:      pair[1],
			CoChanges:  count,
			Confidence: confidence,
			SupportA:   supA,
			SupportB:   supB,
		})
	}

	// Sort by confidence descending, then by co_changes descending.
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].Confidence != pairs[j].Confidence {
			return pairs[i].Confidence > pairs[j].Confidence
		}
		return pairs[i].CoChanges > pairs[j].CoChanges
	})

	// Cap at 50 results.
	if len(pairs) > 50 {
		pairs = pairs[:50]
	}
	return pairs, nil
}
