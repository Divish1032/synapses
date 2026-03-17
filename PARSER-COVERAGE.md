# Parser Coverage Plan — IMP-EVAL-1

*Last updated: 2026-03-17*

Goal: expand from 20 deep AST parsers to 70+ parsers, covering 95%+ of all code written worldwide.

---

## Current State

### Completed — Tier 0 (all 10/10 production-ready as of 2026-03-17)

| Parser | Extensions | Grammar | Status |
|--------|-----------|---------|--------|
| Bash/Shell | .sh .bash .zsh | smacker/bash | ✅ Done |
| SQL | .sql .psql .pgsql .mysql | smacker/sql | ✅ Done |
| CSS | .css | smacker/css | ✅ Done |
| SCSS/Sass | .scss .sass | regex | ✅ Done |
| OCaml | .ml .mli | smacker/ocaml | ✅ Done |
| Elm | .elm | smacker/elm | ✅ Done |
| HCL/Terraform | .tf .tfvars .hcl | smacker/hcl | ✅ Done |
| Svelte | .svelte | smacker/svelte | ✅ Done |
| Dockerfile | Dockerfile .dockerfile | smacker/dockerfile | ✅ Done |
| CUE | .cue | smacker/cue | ✅ Done |

### Completed — Pre-existing (20 parsers)

Go, TypeScript, JavaScript, Python, Java, Kotlin, Scala, Groovy, Rust, C, C++, C#, Swift, Ruby, RBS, PHP, Lua, Elixir, Protobuf, Generic (file-node fallback).

### In Progress — Tier 1

Dart, Perl, R, Haskell, Erlang, F#, Clojure, Objective-C, Zig, Julia, PowerShell, GDScript, D, Nim, Crystal, Common Lisp/Scheme/Emacs Lisp.

---

## Grammar Library

- **smacker/go-tree-sitter** (`github.com/smacker/go-tree-sitter`) — current dep, 31 bundled grammars. Used by all existing and Tier 0 parsers.
- **go-sitter-forest** (`github.com/alexaandru/go-sitter-forest/<lang>`) — 490+ standalone grammar modules, zero shared deps. Bridge via `sitter.NewLanguage(grammarpkg.GetLanguage())`. Used by Tier 1+ parsers.

**Rule:** never import the same language from both libraries — causes duplicate C symbol linker errors.

---

## Tier 2 — Enterprise & Domain-Specific

**Target: 20 parsers. Planned batches:**

- **Batch 1** (TIOBE top 25): Fortran, Pascal/Delphi, VB.NET, Prolog
- **Batch 2** (domain-specific): Solidity, GraphQL, GLSL, HLSL, WGSL
- **Batch 3** (hardware): VHDL, Verilog, SystemVerilog
- **Batch 4** (build + misc): Makefile, CMake, Starlark/Bazel, COBOL, Ada, Haxe, Racket, Nix

| # | Language | Extensions | What to Extract | Complexity |
|---|----------|-----------|-----------------|------------|
| 1 | **Fortran** | .f90 .f95 .f03 .f08 .f .for | functions, subroutines, modules, types | Medium |
| 2 | **COBOL** | .cob .cbl .cpy | paragraphs, sections, programs, copybooks | Hard |
| 3 | **Ada** | .adb .ads | functions, procedures, packages, types, generics, tasks | Hard |
| 4 | **Pascal/Delphi** | .pas .pp .dpr .lpr | functions, procedures, classes, records, units | Medium |
| 5 | **Visual Basic .NET** | .vb | functions, subs, classes, modules | Medium |
| 6 | **Prolog** | .pl .pro .prolog | predicates, rules, facts, modules | Medium |
| 7 | **Solidity** | .sol | contracts, functions, events, structs, interfaces, modifiers | Medium |
| 8 | **GLSL** | .glsl .vert .frag .geom .comp | functions, structs, uniforms | Low |
| 9 | **HLSL** | .hlsl .fx .fxh | functions, structs, cbuffers | Low |
| 10 | **WGSL** | .wgsl | functions, structs | Low |
| 11 | **VHDL** | .vhd .vhdl | entities, architectures, processes, functions, packages | Hard |
| 12 | **Verilog** | .v .vh | modules, functions, tasks | Medium |
| 13 | **SystemVerilog** | .sv .svh | modules, classes, interfaces, packages, tasks, functions | Hard |
| 14 | **GraphQL** | .graphql .gql | types, queries, mutations, subscriptions, inputs, enums | Medium |
| 15 | **Haxe** | .hx | classes, functions, interfaces, enums, abstracts | Medium |
| 16 | **Racket** | .rkt | functions, structs, classes, interfaces | Medium |
| 17 | **Starlark/Bazel** | .bzl .star BUILD | functions, rules, macros | Low |
| 18 | **Makefile** | Makefile .mk | targets, variables, functions | Low |
| 19 | **CMake** | CMakeLists.txt .cmake | functions, macros, variables | Low |
| 20 | **Nix** | .nix | functions (let), attrsets, derivations | Medium |

### Language-Specific Notes

**Fortran** — `function`/`subroutine`/`module`/`program`/`type` declarations. `use` for imports. Case-insensitive — normalize names to lowercase.

**COBOL** — Paragraphs are the main unit (like functions). Sections group paragraphs. `COPY` statements are imports. No classes. Tricky because COBOL has very irregular grammar.

**Ada** — Packages are compilation units (`.ads` = spec, `.adb` = body). Generics are like templates. Tasks are concurrent units — model as NodeFunction with `kind=task`.

**Pascal/Delphi** — `unit` = module, `interface`/`implementation` sections. Classes, records (structs), functions, procedures. Delphi adds properties and published sections.

**VB.NET** — `Module`, `Class`, `Interface`, `Enum` declarations. `Sub`/`Function` for callables. `Imports` for module-level imports.

**Prolog** — Predicates are the primary unit: `name/arity`. Facts and rules both define predicates. `:-` operator for rules. `use_module` for imports.

**Solidity** — `contract`/`interface`/`library` declarations (map to NodeStruct). `function`/`modifier`/`event`/`error`/`struct`/`enum` inside contracts. `import` for edges. Inheritance via `is ContractName`.

**GLSL/HLSL/WGSL** — C-like syntax. Functions and structs are main units. Uniforms/cbuffers are variables. No cross-file imports (single compilation unit per shader). Low complexity.

**VHDL** — `entity`/`architecture`/`package`/`component` as structural units. `process` blocks as functions. `use`/`library` for imports. Hard due to verbose syntax.

**Verilog** — `module`/`function`/`task` declarations. `\`include` for imports. Ports as parameters.

**SystemVerilog** — Extends Verilog with `class`/`interface`/`package`. Much richer type system. Hard.

**GraphQL** — `type`/`input`/`interface`/`union`/`enum`/`scalar` as NodeStruct. `query`/`mutation`/`subscription` fields as NodeFunction. `extend type` merges into existing node. No runtime imports.

**Makefile** — Targets as NodeFunction (with `kind=target`). Variables as NodeVariable. Pattern rules (%.o: %.c) as NodeFunction with `kind=pattern`. `include` for imports.

**CMake** — `function()`/`macro()` as NodeFunction. `set()` top-level calls as NodeVariable. `include()`/`find_package()` as imports. `add_subdirectory()` as imports.

**Starlark/Bazel** — `def` functions. Rule instantiations (`cc_library(name=...)`) as NodeStruct with rule type in metadata. `load()` as imports.

**Nix** — `let` bindings at top level as NodeVariable. `rec {}` attrsets as NodeStruct. `mkDerivation` calls as NodeFunction with `kind=derivation`. `import` for edges.

---

## Tier 3 — Emerging & Research

**Target: 20 parsers.**

| # | Language | Extensions | What to Extract |
|---|----------|-----------|-----------------|
| 1 | **Gleam** | .gleam | functions, types, modules |
| 2 | **Odin** | .odin | functions, structs, unions |
| 3 | **Roc** | .roc | functions, types |
| 4 | **Hare** | .ha | functions, types |
| 5 | **Carbon** | .carbon | functions, classes |
| 6 | **V (Vlang)** | .v .vsh | functions, structs, interfaces |
| 7 | **PureScript** | .purs | functions, types, classes |
| 8 | **ReScript** | .res .resi | functions, modules, types |
| 9 | **Lean** | .lean | definitions, theorems, structures |
| 10 | **Agda** | .agda | definitions, data types, records |
| 11 | **Idris** | .idr | functions, data types, interfaces |
| 12 | **TLA+** | .tla | operators, modules, theorems |
| 13 | **Cairo** | .cairo | functions, structs, traits |
| 14 | **Move** | .move | modules, functions, structs, resources |
| 15 | **Fennel** | .fnl | functions, macros |
| 16 | **Janet** | .janet | functions, macros |
| 17 | **Vyper** | .vy | functions, events, interfaces |
| 18 | **Typst** | .typ | functions, rules |
| 19 | **Mojo** | .mojo | functions, structs, traits |
| 20 | **GDShader** | .gdshader | functions, uniforms |

**Fast-track candidates within Tier 3:**
- **Gleam** — v1.0 launched 2024, growing fast on BEAM/Erlang VM, simple clean syntax
- **Mojo** — Python superset for AI/ML (Modular), significant traction in the AI tooling space
- **Cairo** — StarkNet/ZK-proof language, active blockchain developer community
- **Move** — Sui/Aptos blockchain, resources model is unique and worth modeling
- **V (Vlang)** — Systems language with Python-like syntax, active community

---

## Tier 4 — Long-Tail Niche

**Target: 15 parsers. One sweep session.**

| # | Language | Extensions | Notes |
|---|----------|-----------|-------|
| 1 | Tcl | .tcl .tk | procs, namespaces |
| 2 | Awk | .awk | functions, rules |
| 3 | Hack | .hack .hh | classes, functions (PHP derivative) |
| 4 | Apex | .cls .trigger | Salesforce — classes, triggers |
| 5 | ABAP | .abap | SAP — function modules, classes |
| 6 | Vala | .vala .vapi | classes, methods (GNOME) |
| 7 | Pony | .pony | classes, actors, interfaces |
| 8 | Smalltalk | .st | classes, methods |
| 9 | Eiffel | .e | classes, features |
| 10 | Standard ML | .sml .sig | functions, structures, signatures |
| 11 | Forth | .fth .4th .fs | words, vocabularies |
| 12 | Factor | .factor | words, vocabularies, classes |
| 13 | Raku | .raku .rakumod .p6 | classes, methods, roles, grammars |
| 14 | Wren | .wren | classes, methods |
| 15 | SAS | .sas | data steps, procs, macros (no tree-sitter grammar — regex only) |

---

## Implementation Recipe (all parsers)

```
File: synapses/internal/parser/<lang>.go

1. Import grammar
   smacker grammars:   import "<lang>" "github.com/smacker/go-tree-sitter/<lang>"
   forest grammars:    import <lang>grammar "github.com/alexaandru/go-sitter-forest/<lang>"
   Bridge:             language: sitter.NewLanguage(<lang>grammar.GetLanguage())

2. Define node types → graph node types
   function/method declaration  → graph.NodeFunction
   class/struct/type             → graph.NodeStruct
   interface/typeclass           → graph.NodeInterface
   module/namespace/package      → graph.NodePackage
   variable/constant/field       → graph.NodeVariable

3. Extract metadata per node
   - kind (function, method, class, mixin, etc.)
   - doc (doc comment above the declaration)
   - signature (type signature if language has one)
   - line_count (end_line - start_line)
   - exported (public/private visibility)

4. Emit DEFINES edges: file → each declared symbol

5. Emit IMPORTS edges: file → NodePackage for each import/use/require

6. Collect call sites via AddCallSite() for cross-file resolution
   - Same-file calls: emit EdgeCalls directly during parse
   - Cross-file calls: AddCallSite for resolver to handle later
```

---

## Total Impact

| Metric | Tier 0 complete | Tier 1 complete | Tier 2 complete | All tiers |
|--------|----------------|----------------|----------------|-----------|
| Deep parsers | 30 | 46 | 66 | 81 |
| Extensions (deep) | ~60 | ~100 | ~160 | ~220 |
| TIOBE top 50 coverage | 55% | 70% | 88% | 95% |
| SO language coverage | 60% | 75% | 88% | 96% |
