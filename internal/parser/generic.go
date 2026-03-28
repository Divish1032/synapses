package parser

import (
	"path/filepath"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// genericParser is a fallback for languages that do not yet have a dedicated
// AST-level parser. It records a file node so the file appears in the graph,
// the watcher can track changes, and rule checks can reference it — without
// attempting to parse function/class declarations.
type genericParser struct {
	exts []string
}

// newGenericParser returns a parser covering all common source file extensions
// that are not handled by a dedicated language parser.
func newGenericParser() *genericParser {
	return &genericParser{
		exts: []string{

			// ── Frontend ──────────────────────────────────────────────────────
			".html", ".htm", // HTML
			// NOTE: .css → CSSParser; .scss .sass → SCSSParser
			".less", ".styl", // Stylesheets (no deep parser yet)
			".astro", // Astro framework
			".mdx",   // MDX (Markdown + JSX)
			".wasm",  // WebAssembly

			// ── Systems (no dedicated parser) ─────────────────────────────────
			// NOTE: .c .h .ino → CParser; .cpp .cc .cxx .hpp .hh .hxx .mm → CppParser
			//       .cs → CSharpParser; .rs → RustParser; .swift → SwiftParser
			".zig",       // Zig
			".nim",       // Nim
			".asm", ".s", // Assembly

			// ── Mobile ───────────────────────────────────────────────────────
			// NOTE: .swift → SwiftParser; .mm → CppParser; .dart → DartParser
			".m",        // Objective-C (C grammar is close but not identical)
			".xcconfig", // Xcode config

			// ── Scripting / Dynamic ──────────────────────────────────────────
			// NOTE: .rb → RubyParser; .php → PHPParser; .lua → LuaParser
			// NOTE: .r .R → RParser; .ps1 .psm1 .psd1 → PowerShellParser
			".jl",        // Julia
			".pl", ".pm", // Perl
			// NOTE: .sh .bash .zsh → BashParser
			".fish",        // Shell scripts (no deep parser yet)
			".bat", ".cmd", // Windows batch

			// ── Data Science / Notebooks ─────────────────────────────────────
			".ipynb",       // Jupyter notebooks (Python/R/Julia)
			".rmd", ".Rmd", // R Markdown
			".qmd", // Quarto

			// ── Functional ───────────────────────────────────────────────────
			// NOTE: .ex .exs → ElixirParser; .ml .mli → OCamlParser
			// NOTE: .erl .hrl → ErlangParser; .hs .lhs → HaskellParser
			// NOTE: .clj .cljs .cljc .edn → ClojureParser; .fs .fsi .fsx → FSharpParser
			".lisp", ".cl", ".el", // Lisp / Common Lisp / Emacs Lisp
			".scm", ".ss", // Scheme

			// ── Database / Query ─────────────────────────────────────────────
			// NOTE: .sql → SQLParser
			".psql", ".pgsql", // PostgreSQL
			".mysql",       // MySQL
			".sqlite",      // SQLite
			".hql",         // HiveQL
			".ddl", ".dml", // DDL / DML scripts

			// ── API / Schema definition ───────────────────────────────────────
			// NOTE: .proto → ProtobufParser
			".graphql", ".gql", // GraphQL
			".avsc",         // Avro schema
			".thrift",       // Apache Thrift
			".wsdl", ".xsd", // XML Schema / SOAP

			// ── DevOps / Infrastructure ──────────────────────────────────────
			// NOTE: .yaml .yml → YAMLParser; .dockerfile → DockerfileParser
			// NOTE: .tf .tfvars .hcl → HCLParser
			".containerfile", // Dockerfile variants
			".bicep",         // Azure Bicep
			".nix",           // Nix expressions

			// ── Config / Data ────────────────────────────────────────────────
			".json", ".jsonc", ".json5", // JSON / JSON with comments
			".toml",                 // TOML
			".xml",                  // XML (Maven, Android, Spring, etc.)
			".ini", ".cfg", ".conf", // INI-style config
			".properties", // Java .properties
			".plist",      // Apple plist
			".env",        // Environment files (.env.local etc.)
			".dotenv",     // Dotenv variants

			// ── Web templates ─────────────────────────────────────────────────
			// NOTE: .svelte → SvelteParser
			".vue",          // Vue SFC
			".erb",          // Ruby ERB templates
			".ejs",          // EJS templates
			".jinja", ".j2", // Jinja2 templates
			".mustache", ".hbs", // Handlebars / Mustache
			".njk",              // Nunjucks
			".liquid",           // Liquid (Shopify)
			".twig",             // Twig (PHP)
			".razor", ".cshtml", // ASP.NET Razor
			".blade", // Laravel Blade

			// ── Documentation ─────────────────────────────────────────────────
			// NOTE: .md .markdown .mdx → MarkdownParser
			// NOTE: .rst .txt → PlaintextParser
			".tex", ".latex", // LaTeX
			".adoc", ".asciidoc", // AsciiDoc

			// ── Build / Package manifests ────────────────────────────────────
			".bazel", ".bzl", // Bazel BUILD files
			".cmake", // CMake
			".mk",    // Makefile includes
		},
	}
}

func (p *genericParser) Extensions() []string { return p.exts }

// Parse creates a file node. No AST parsing is performed.
func (p *genericParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	fileNodeID := g.MakeNodeID(filePath, filePath)
	g.AddNode(&graph.Node{
		ID:   fileNodeID,
		Type: graph.NodeFile,
		Name: filepath.Base(filePath),
		File: filePath,
		Line: 1,
	})
	return nil
}
