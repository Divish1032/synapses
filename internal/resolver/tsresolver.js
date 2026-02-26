// Synapses TypeScript type resolver.
// Spawned by ResolveTSTypesCallEdges in tstypes.go.
//
// Usage: node tsresolver.js <project_root>
//
// Output: newline-delimited JSON objects on stdout:
//   {"from_file":"<abs>","from_line":<1-based>,"to_file":"<abs>","to_line":<1-based>}
//
// Errors/warnings go to stderr. Exit 0 on success, non-zero on fatal error.
// The Go side treats non-zero exit as non-fatal and logs the message.

'use strict';

const fs = require('fs');
const path = require('path');

const root = process.argv[2];
if (!root) {
  process.stderr.write('synapses/ts-types: usage: node tsresolver.js <project_root>\n');
  process.exit(1);
}

// Try to load the TypeScript compiler from the project's node_modules first,
// then fall back to a globally installed copy.
let ts;
try {
  ts = require(path.join(root, 'node_modules', 'typescript'));
} catch (_) {
  try {
    ts = require('typescript');
  } catch (_) {
    process.stderr.write(
      'synapses/ts-types: "typescript" package not found in ' +
      root + '/node_modules or global — install it with: npm install typescript\n'
    );
    process.exit(2);
  }
}

// ─── Discover source files ────────────────────────────────────────────────────

let fileNames;
let compilerOptions = { noEmit: true, skipLibCheck: true };

const tsconfigPath = path.join(root, 'tsconfig.json');
if (fs.existsSync(tsconfigPath)) {
  const configFile = ts.readConfigFile(tsconfigPath, ts.sys.readFile);
  if (configFile.error) {
    process.stderr.write('synapses/ts-types: tsconfig.json error: ' +
      ts.flattenDiagnosticMessageText(configFile.error.messageText, '\n') + '\n');
    // Fall through to glob-based discovery.
    fileNames = findTsFiles(root);
  } else {
    const parsed = ts.parseJsonConfigFileContent(
      configFile.config,
      ts.sys,
      root
    );
    if (parsed.errors && parsed.errors.length > 0) {
      for (const e of parsed.errors) {
        process.stderr.write('synapses/ts-types: tsconfig parse warning: ' +
          ts.flattenDiagnosticMessageText(e.messageText, '\n') + '\n');
      }
    }
    fileNames = parsed.fileNames;
    compilerOptions = Object.assign(parsed.options, { noEmit: true, skipLibCheck: true });
  }
} else {
  fileNames = findTsFiles(root);
}

if (fileNames.length === 0) {
  process.stderr.write('synapses/ts-types: no TypeScript files found in ' + root + '\n');
  process.exit(0);
}

// ─── Type-checked pass ────────────────────────────────────────────────────────

const program = ts.createProgram(fileNames, compilerOptions);
const checker = program.getTypeChecker();

// Hard cap: prevents runaway output on giant monorepos.
const MAX_EDGES = 10000;
let edgeCount = 0;

for (const sourceFile of program.getSourceFiles()) {
  if (edgeCount >= MAX_EDGES) break;
  if (sourceFile.isDeclarationFile) continue;
  // Skip anything under node_modules or .d.ts files.
  if (sourceFile.fileName.includes('/node_modules/') ||
      sourceFile.fileName.includes('\\node_modules\\')) continue;

  visitFile(sourceFile);
}

if (edgeCount >= MAX_EDGES) {
  process.stderr.write(
    'synapses/ts-types: edge cap (' + MAX_EDGES + ') reached — some cross-file CALLS may be missing\n'
  );
}

process.exit(0);

// ─── Visitor ─────────────────────────────────────────────────────────────────

function visitFile(sourceFile) {
  visitNode(sourceFile);
}

function visitNode(node) {
  if (edgeCount >= MAX_EDGES) return;

  if (node.kind === ts.SyntaxKind.CallExpression) {
    processCall(node);
  }

  ts.forEachChild(node, visitNode);
}

function processCall(callExpr) {
  // Find the nearest enclosing function/method/arrow that contains this call.
  const enclosing = getEnclosingDeclaration(callExpr);
  if (!enclosing) return;

  const callerFile = enclosing.getSourceFile();
  const callerStart = enclosing.getStart(callerFile, /*includeJsDocComment*/ false);
  const callerLC = callerFile.getLineAndCharacterOfPosition(callerStart);
  const callerLine = callerLC.line + 1; // 1-based

  // Resolve the callee via the type checker.
  const expr = callExpr.expression;
  let sym;
  try {
    sym = checker.getSymbolAtLocation(expr);
  } catch (_) {
    return;
  }
  if (!sym) return;

  // Follow aliases (e.g. re-exports).
  const resolvedSym = checker.getAliasedSymbol ? checker.getAliasedSymbol(sym) : sym;
  const decl = (resolvedSym.valueDeclaration) ||
                (resolvedSym.declarations && resolvedSym.declarations[0]);
  if (!decl) return;

  const calleeFile = decl.getSourceFile();
  if (!calleeFile) return;
  if (calleeFile.isDeclarationFile) return;
  if (calleeFile.fileName.includes('/node_modules/') ||
      calleeFile.fileName.includes('\\node_modules\\')) return;

  const calleeStart = decl.getStart(calleeFile, false);
  const calleeLC = calleeFile.getLineAndCharacterOfPosition(calleeStart);
  const calleeLine = calleeLC.line + 1; // 1-based

  // Emit edge.
  const edge = {
    from_file: callerFile.fileName,
    from_line: callerLine,
    to_file: calleeFile.fileName,
    to_line: calleeLine,
  };
  process.stdout.write(JSON.stringify(edge) + '\n');
  edgeCount++;
}

// Returns the nearest ancestor that is a function/method/arrow/constructor declaration.
function getEnclosingDeclaration(node) {
  let cur = node.parent;
  while (cur) {
    switch (cur.kind) {
      case ts.SyntaxKind.FunctionDeclaration:
      case ts.SyntaxKind.MethodDeclaration:
      case ts.SyntaxKind.ArrowFunction:
      case ts.SyntaxKind.FunctionExpression:
      case ts.SyntaxKind.Constructor:
      case ts.SyntaxKind.GetAccessor:
      case ts.SyntaxKind.SetAccessor:
        return cur;
      // Stop at class/module boundaries without finding a function.
      case ts.SyntaxKind.ClassDeclaration:
      case ts.SyntaxKind.ModuleDeclaration:
      case ts.SyntaxKind.SourceFile:
        return null;
    }
    cur = cur.parent;
  }
  return null;
}

// ─── File discovery fallback ──────────────────────────────────────────────────

function findTsFiles(dir) {
  const results = [];
  function walk(d) {
    let entries;
    try {
      entries = fs.readdirSync(d, { withFileTypes: true });
    } catch (_) {
      return;
    }
    for (const entry of entries) {
      if (entry.name.startsWith('.')) continue;
      if (entry.name === 'node_modules' || entry.name === 'dist' ||
          entry.name === 'build' || entry.name === 'out') continue;
      const full = path.join(d, entry.name);
      if (entry.isDirectory()) {
        walk(full);
      } else if (entry.isFile()) {
        const n = entry.name;
        if ((n.endsWith('.ts') || n.endsWith('.tsx')) && !n.endsWith('.d.ts')) {
          results.push(full);
        }
      }
    }
  }
  walk(dir);
  return results;
}
