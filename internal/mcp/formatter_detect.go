package mcp

import (
	"os"
	"path/filepath"
	"strings"
)

// formatterConvention pairs a display name with the set of config filenames
// that indicate its presence in the project root.
type formatterConvention struct {
	name  string
	files []string
}

// knownFormatters lists the formatter configs Synapses detects, in priority
// order. The first matching file for each formatter triggers a convention.
var knownFormatters = []formatterConvention{
	{
		name: "Prettier",
		files: []string{
			".prettierrc",
			".prettierrc.json",
			".prettierrc.js",
			".prettierrc.cjs",
			".prettierrc.mjs",
			".prettierrc.yml",
			".prettierrc.yaml",
			".prettierrc.toml",
			"prettier.config.js",
			"prettier.config.cjs",
			"prettier.config.mjs",
		},
	},
	{
		name:  "Biome",
		files: []string{"biome.json", "biome.jsonc"},
	},
	{
		name:  "rustfmt",
		files: []string{"rustfmt.toml", ".rustfmt.toml"},
	},
	// go.mod signals a Go module project; gofmt is universally active in all
	// Go projects regardless of whether a linter config is present.
	// Previously detected via .golangci.yml but that missed the majority of
	// Go projects — go.mod is the canonical Go presence signal.
	{
		name:  "gofmt",
		files: []string{"go.mod"},
	},
	{
		name:  "EditorConfig",
		files: []string{".editorconfig"},
	},
}

// detectFormatterConventions inspects the project root for known formatter
// configuration files and returns one natural-language convention string per
// detected formatter. Called once per session_init; results are prepended to
// the conventions list so agents learn about auto-formatting from the start.
func detectFormatterConventions(projectRoot string) []string {
	if projectRoot == "" {
		return nil
	}

	var convs []string

	for _, fc := range knownFormatters {
		for _, filename := range fc.files {
			if _, err := os.Stat(filepath.Join(projectRoot, filename)); err == nil {
				convs = append(convs, formatConvention(fc.name))
				break // one match per formatter is enough
			}
		}
	}

	// pyproject.toml is present in many Python projects but only indicates
	// auto-formatting if it contains a [tool.black] or [tool.ruff.format] section.
	convs = append(convs, detectPyprojectFormatter(projectRoot)...)

	return convs
}

// formatConvention builds the natural-language convention string for a detected
// formatter. The message instructs the agent to re-read files after writing to
// avoid acting on stale content when the formatter modifies the file on save.
func formatConvention(name string) string {
	return "This project auto-formats with " + name + " on save. " +
		"File contents change after edits — re-read files after writing to avoid stale-content errors."
}

// detectPyprojectFormatter reads pyproject.toml (if present) and returns a
// convention string when [tool.black] or [tool.ruff] formatting config is found.
// Returns nil when the file is absent or contains neither tool section.
func detectPyprojectFormatter(projectRoot string) []string {
	data, err := os.ReadFile(filepath.Join(projectRoot, "pyproject.toml"))
	if err != nil {
		return nil
	}
	content := string(data)

	// Look for black or ruff configuration sections. A bare "[tool.ruff]"
	// section may only configure linting; "[tool.ruff.format]" is the formatter.
	// We accept either: if ruff is configured at all it likely handles formatting.
	switch {
	case strings.Contains(content, "[tool.black]"):
		return []string{formatConvention("black")}
	case strings.Contains(content, "[tool.ruff.format]"):
		return []string{formatConvention("ruff")}
	case strings.Contains(content, "[tool.ruff]"):
		return []string{formatConvention("ruff")}
	}
	return nil
}
