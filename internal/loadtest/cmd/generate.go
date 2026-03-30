//go:build ignore

// generate.go creates synthetic repositories for load testing.
//
// Usage:
//
//	go run internal/loadtest/cmd/generate.go -files 10000 -out /tmp/synthetic_10k
//	go run internal/loadtest/cmd/generate.go -files 50000 -out /tmp/synthetic_50k
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
)

var (
	numFiles = flag.Int("files", 10000, "number of files to generate")
	langs    = flag.String("langs", "go,python,typescript", "comma-separated languages")
	depth    = flag.Int("depth", 5, "max directory nesting depth")
	edges    = flag.Int("edges", 3, "avg cross-file references per file")
	outDir   = flag.String("out", "/tmp/synthetic_10k", "output directory")
	seed     = flag.Int64("seed", 42, "random seed for reproducibility")
)

type langSpec struct {
	ext      string
	template func(pkg string, imports []string) string
}

var langSpecs = map[string]langSpec{
	"go": {
		ext: ".go",
		template: func(pkg string, imports []string) string {
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("package %s\n\n", pkg))
			if len(imports) > 0 {
				sb.WriteString("import (\n")
				for _, imp := range imports {
					sb.WriteString(fmt.Sprintf("\t%q\n", imp))
				}
				sb.WriteString(")\n\n")
			}
			sb.WriteString(fmt.Sprintf("type %sService struct {\n", strings.Title(pkg)))
			sb.WriteString("\tID   int\n")
			sb.WriteString("\tName string\n")
			sb.WriteString("}\n\n")
			sb.WriteString(fmt.Sprintf("func New%sService() *%sService {\n", strings.Title(pkg), strings.Title(pkg)))
			sb.WriteString(fmt.Sprintf("\treturn &%sService{}\n", strings.Title(pkg)))
			sb.WriteString("}\n\n")
			sb.WriteString(fmt.Sprintf("func (s *%sService) Process() error {\n", strings.Title(pkg)))
			for _, imp := range imports {
				parts := strings.Split(imp, "/")
				sb.WriteString(fmt.Sprintf("\t_ = %s.New%sService()\n", parts[len(parts)-1], strings.Title(parts[len(parts)-1])))
			}
			sb.WriteString("\treturn nil\n")
			sb.WriteString("}\n")
			return sb.String()
		},
	},
	"python": {
		ext: ".py",
		template: func(pkg string, imports []string) string {
			var sb strings.Builder
			for _, imp := range imports {
				parts := strings.Split(imp, "/")
				mod := parts[len(parts)-1]
				sb.WriteString(fmt.Sprintf("from %s import %sService\n", strings.ReplaceAll(imp, "/", "."), strings.Title(mod)))
			}
			sb.WriteString("\n\n")
			sb.WriteString(fmt.Sprintf("class %sService:\n", strings.Title(pkg)))
			sb.WriteString("    def __init__(self):\n")
			sb.WriteString("        self.id = 0\n")
			sb.WriteString("        self.name = \"\"\n\n")
			sb.WriteString("    def process(self):\n")
			if len(imports) > 0 {
				for _, imp := range imports {
					parts := strings.Split(imp, "/")
					mod := strings.Title(parts[len(parts)-1])
					sb.WriteString(fmt.Sprintf("        svc = %sService()\n", mod))
				}
			} else {
				sb.WriteString("        pass\n")
			}
			return sb.String()
		},
	},
	"typescript": {
		ext: ".ts",
		template: func(pkg string, imports []string) string {
			var sb strings.Builder
			for _, imp := range imports {
				parts := strings.Split(imp, "/")
				mod := parts[len(parts)-1]
				sb.WriteString(fmt.Sprintf("import { %sService } from './%s';\n", strings.Title(mod), imp))
			}
			sb.WriteString("\n")
			sb.WriteString(fmt.Sprintf("export class %sService {\n", strings.Title(pkg)))
			sb.WriteString("  private id: number;\n")
			sb.WriteString("  private name: string;\n\n")
			sb.WriteString(fmt.Sprintf("  constructor() {\n"))
			sb.WriteString("    this.id = 0;\n")
			sb.WriteString("    this.name = '';\n")
			sb.WriteString("  }\n\n")
			sb.WriteString("  process(): void {\n")
			for _, imp := range imports {
				parts := strings.Split(imp, "/")
				mod := strings.Title(parts[len(parts)-1])
				sb.WriteString(fmt.Sprintf("    const svc = new %sService();\n", mod))
			}
			sb.WriteString("  }\n")
			sb.WriteString("}\n")
			return sb.String()
		},
	},
}

func main() {
	flag.Parse()

	rng := rand.New(rand.NewSource(*seed))

	languages := strings.Split(*langs, ",")
	for _, l := range languages {
		if _, ok := langSpecs[l]; !ok {
			fmt.Fprintf(os.Stderr, "unknown language: %s\n", l)
			os.Exit(1)
		}
	}

	dirs := generateDirs(rng, *depth, *numFiles/20)

	type fileInfo struct {
		path string
		lang string
		pkg  string
	}

	files := make([]fileInfo, 0, *numFiles)
	for i := 0; i < *numFiles; i++ {
		lang := languages[i%len(languages)]
		spec := langSpecs[lang]
		dir := dirs[rng.Intn(len(dirs))]
		pkg := filepath.Base(dir)
		if pkg == "." || pkg == "" {
			pkg = "root"
		}
		name := fmt.Sprintf("%s_%04d%s", pkg, i, spec.ext)
		files = append(files, fileInfo{
			path: filepath.Join(dir, name),
			lang: lang,
			pkg:  sanitizeIdent(pkg),
		})
	}

	fmt.Printf("Generating %d files in %s (seed=%d, langs=%v, depth=%d, edges=%d)\n",
		*numFiles, *outDir, *seed, languages, *depth, *edges)

	for i, f := range files {
		spec := langSpecs[f.lang]

		var imports []string
		numImports := rng.Intn(*edges*2 + 1)
		for j := 0; j < numImports && j < len(files)-1; j++ {
			target := rng.Intn(len(files))
			if target == i || files[target].lang != f.lang {
				continue
			}
			imports = append(imports, filepath.Dir(files[target].path))
		}

		seen := make(map[string]bool)
		var unique []string
		for _, imp := range imports {
			if !seen[imp] {
				seen[imp] = true
				unique = append(unique, imp)
			}
		}

		content := spec.template(f.pkg, unique)
		fullPath := filepath.Join(*outDir, f.path)

		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "mkdir %s: %v\n", filepath.Dir(fullPath), err)
			os.Exit(1)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", fullPath, err)
			os.Exit(1)
		}
		if (i+1)%1000 == 0 {
			fmt.Printf("  %d/%d files written\n", i+1, *numFiles)
		}
	}
	fmt.Printf("Done: %d files in %s\n", *numFiles, *outDir)
}

func generateDirs(rng *rand.Rand, maxDepth, count int) []string {
	components := []string{
		"api", "auth", "cache", "config", "core", "data", "db",
		"handlers", "lib", "middleware", "models",
		"pkg", "routes", "service", "store", "types", "utils",
		"worker", "events", "queue", "search", "sync", "transform",
	}

	dirs := make([]string, 0, count)
	dirs = append(dirs, "src")
	for i := 0; i < count; i++ {
		d := rng.Intn(maxDepth) + 1
		parts := make([]string, d)
		parts[0] = "src"
		for j := 1; j < d; j++ {
			parts[j] = components[rng.Intn(len(components))]
		}
		dirs = append(dirs, filepath.Join(parts...))
	}
	return dirs
}

func sanitizeIdent(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "pkg"
	}
	result := b.String()
	if result[0] >= '0' && result[0] <= '9' {
		result = "p" + result
	}
	return result
}
