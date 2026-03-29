package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/SynapsesOS/synapses/internal/config"
)

func cmdConfig(args []string) error {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	global := fs.Bool("global", false, "Read/write the global config (~/.synapses/config.json)")
	show := fs.Bool("show", false, "Show merged config with source annotations")
	repoPath := fs.String("path", ".", "Path to the project root")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: synapses config [flags] [key] [value]

Read or write Synapses configuration.

FLAGS:
  --global       Target the global config (~/.synapses/config.json)
  --show         Show merged config or a single key with source annotation
  --path <dir>   Path to the project root (default: .)

EXAMPLES:
  synapses config --show                          Show full merged config
  synapses config --show brain.enabled            Show single value with source
  synapses config --global brain.enabled true     Set a global default
  synapses config brain.enabled false             Set a project-level override
  synapses config --global brain.enabled --show   Show where a global key comes from
`)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	remaining := fs.Args()

	if *show {
		return configShow(remaining, *repoPath, *global)
	}

	if len(remaining) < 1 {
		fs.Usage()
		return nil
	}

	key := remaining[0]

	// If no value provided, read the key
	if len(remaining) < 2 {
		return configGet(key, *repoPath, *global)
	}

	value := remaining[1]
	return configSet(key, value, *repoPath, *global)
}

// configShow shows the full merged config or a single key with source annotations.
func configShow(args []string, repoPath string, globalOnly bool) error {
	absPath, err := filepath.Abs(repoPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	// Load global config
	gc, _ := config.LoadGlobalConfig()

	// Load project config
	cfgDir, found := config.FindConfigDir(absPath)
	projCfg, projErr := config.Load(cfgDir)
	if projErr != nil {
		return fmt.Errorf("load project config: %w", projErr)
	}

	// If showing a single key
	if len(args) > 0 {
		key := args[0]
		return showKeyWithSource(key, projCfg, gc, found, cfgDir)
	}

	// Show full merged config
	data, err := json.MarshalIndent(projCfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	fmt.Println(string(data))

	// Print source summary
	fmt.Println()
	globalPath, _ := config.GlobalConfigPath()
	if gc != nil {
		fmt.Printf("Global: %s\n", globalPath)
	} else {
		fmt.Printf("Global: (not found)\n")
	}
	if found {
		fmt.Printf("Project: %s\n", filepath.Join(cfgDir, "synapses.json"))
	} else {
		fmt.Printf("Project: (no synapses.json found)\n")
	}
	return nil
}

// configGet reads a single key from the appropriate config.
func configGet(key, repoPath string, global bool) error {
	if global {
		gc, err := config.LoadGlobalConfig()
		if err != nil {
			return err
		}
		if gc == nil {
			return fmt.Errorf("no global config found — create one with: synapses config --global %s <value>", key)
		}
		val, err := getJSONField(gc, key)
		if err != nil {
			return err
		}
		fmt.Println(val)
		return nil
	}

	absPath, err := filepath.Abs(repoPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	cfgDir, _ := config.FindConfigDir(absPath)
	cfg, err := config.Load(cfgDir)
	if err != nil {
		return err
	}
	val, err := getJSONField(cfg, key)
	if err != nil {
		return err
	}
	fmt.Println(val)
	return nil
}

// configSet writes a key-value pair to the appropriate config.
func configSet(key, value, repoPath string, global bool) error {
	if global {
		return configSetGlobal(key, value)
	}
	return configSetProject(key, value, repoPath)
}

func configSetGlobal(key, value string) error {
	gc, err := config.LoadGlobalConfig()
	if err != nil {
		return err
	}
	if gc == nil {
		gc = &config.GlobalConfig{Version: "1"}
	}

	if err := setJSONField(gc, key, value); err != nil {
		return fmt.Errorf("set %s: %w", key, err)
	}

	if err := config.SaveGlobalConfig(gc); err != nil {
		return err
	}
	path, _ := config.GlobalConfigPath()
	fmt.Printf("Set %s in %s\n", key, path)
	return nil
}

func configSetProject(key, value, repoPath string) error {
	absPath, err := filepath.Abs(repoPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	cfgDir, found := config.FindConfigDir(absPath)
	cfgPath := filepath.Join(cfgDir, "synapses.json")

	// Read existing or start fresh
	var raw map[string]json.RawMessage
	if found {
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			return fmt.Errorf("read %s: %w", cfgPath, err)
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("parse %s: %w", cfgPath, err)
		}
	} else {
		raw = map[string]json.RawMessage{
			"version": json.RawMessage(`"1"`),
		}
		cfgPath = filepath.Join(absPath, "synapses.json")
	}

	if err := setRawJSONField(raw, key, value); err != nil {
		return fmt.Errorf("set %s: %w", key, err)
	}

	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", cfgPath, err)
	}
	fmt.Printf("Set %s in %s\n", key, cfgPath)
	return nil
}

// showKeyWithSource shows a key value with where it came from.
func showKeyWithSource(key string, merged *config.Config, gc *config.GlobalConfig, projFound bool, cfgDir string) error {
	val, err := getJSONField(merged, key)
	if err != nil {
		return err
	}

	source := "default"
	if projFound {
		// Check if project explicitly sets this key
		projPath := filepath.Join(cfgDir, "synapses.json")
		if data, err := os.ReadFile(projPath); err == nil {
			topKey := strings.SplitN(key, ".", 2)[0]
			keys := config.ExtractRawKeys(data)
			if keys[topKey] {
				source = projPath
			}
		}
	}
	if source == "default" && gc != nil {
		// Check if global sets this top-level key
		globalPath, _ := config.GlobalConfigPath()
		if data, err := os.ReadFile(globalPath); err == nil {
			topKey := strings.SplitN(key, ".", 2)[0]
			keys := config.ExtractRawKeys(data)
			if keys[topKey] {
				source = globalPath
			}
		}
	}

	fmt.Printf("%s = %s (from: %s)\n", key, val, source)
	return nil
}

// getJSONField gets a dotted key from a struct by marshaling to JSON.
func getJSONField(v interface{}, key string) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return "", err
	}

	parts := strings.SplitN(key, ".", 2)
	topKey := parts[0]
	raw, ok := m[topKey]
	if !ok {
		return "", fmt.Errorf("unknown key %q — run 'synapses config --show' to see available keys", key)
	}

	if len(parts) == 1 {
		return formatJSON(raw), nil
	}

	// Nested key
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(raw, &nested); err != nil {
		return "", fmt.Errorf("key %q is not an object — cannot access %q", topKey, parts[1])
	}
	subRaw, ok := nested[parts[1]]
	if !ok {
		return "", fmt.Errorf("unknown key %q under %q", parts[1], topKey)
	}
	return formatJSON(subRaw), nil
}

// setJSONField sets a dotted key on a struct by marshaling/unmarshaling JSON.
func setJSONField(v interface{}, key, value string) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}

	if err := setRawJSONField(m, key, value); err != nil {
		return err
	}

	result, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return json.Unmarshal(result, v)
}

// setRawJSONField sets a dotted key in a raw JSON map.
func setRawJSONField(m map[string]json.RawMessage, key, value string) error {
	parts := strings.SplitN(key, ".", 2)
	topKey := parts[0]
	jsonVal := coerceToJSON(value)

	if len(parts) == 1 {
		m[topKey] = json.RawMessage(jsonVal)
		return nil
	}

	// Nested key — get or create the sub-object
	var nested map[string]json.RawMessage
	if existing, ok := m[topKey]; ok {
		if err := json.Unmarshal(existing, &nested); err != nil {
			nested = make(map[string]json.RawMessage)
		}
	} else {
		nested = make(map[string]json.RawMessage)
	}
	nested[parts[1]] = json.RawMessage(jsonVal)
	nestedData, err := json.Marshal(nested)
	if err != nil {
		return err
	}
	m[topKey] = json.RawMessage(nestedData)
	return nil
}

// coerceToJSON converts a CLI string value to appropriate JSON.
func coerceToJSON(s string) string {
	// Booleans
	if s == "true" || s == "false" {
		return s
	}
	// Numbers
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return s
	}
	if _, err := strconv.ParseInt(s, 10, 64); err == nil {
		return s
	}
	// null
	if s == "null" {
		return s
	}
	// Already valid JSON (object or array)
	if (strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}")) ||
		(strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]")) {
		if json.Valid([]byte(s)) {
			return s
		}
	}
	// String
	b, _ := json.Marshal(s)
	return string(b)
}

// formatJSON formats raw JSON for display.
func formatJSON(raw json.RawMessage) string {
	// Try simple value first
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return strconv.FormatBool(b)
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil {
		if n == float64(int64(n)) {
			return strconv.FormatInt(int64(n), 10)
		}
		return strconv.FormatFloat(n, 'f', -1, 64)
	}
	// Complex value — pretty print
	var v interface{}
	json.Unmarshal(raw, &v)
	out, _ := json.MarshalIndent(v, "", "  ")
	return string(out)
}
