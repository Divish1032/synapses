package parser

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ── ExtractRouteRegistrations ─────────────────────────────────────────────────

func TestExtractGoRoutes_HandleFunc(t *testing.T) {
	src := []byte(`package main
import "net/http"
func setupRoutes() {
	http.HandleFunc("/api/users", GetUsers)
	router.HandleFunc("/api/orders", listOrders)
}`)
	regs := ExtractRouteRegistrations("server.go", src)
	if len(regs) != 2 {
		t.Fatalf("expected 2 registrations, got %d", len(regs))
	}
	assertReg(t, regs[0], "*", "/api/users", "GetUsers", "setupRoutes")
	assertReg(t, regs[1], "*", "/api/orders", "listOrders", "setupRoutes")
}

func TestExtractGoRoutes_GinMethodRoutes(t *testing.T) {
	src := []byte(`package main
func registerRoutes(r *gin.RouterGroup) {
	r.GET("/users", listUsers)
	r.POST("/users", createUser)
	r.DELETE("/users/:id", deleteUser)
}`)
	regs := ExtractRouteRegistrations("routes.go", src)
	if len(regs) != 3 {
		t.Fatalf("expected 3, got %d: %+v", len(regs), regs)
	}
	assertReg(t, regs[0], "GET", "/users", "listUsers", "registerRoutes")
	assertReg(t, regs[1], "POST", "/users", "createUser", "registerRoutes")
	assertReg(t, regs[2], "DELETE", "/users/:id", "deleteUser", "registerRoutes")
}

func TestExtractGoRoutes_EchoViaPrefixedVar(t *testing.T) {
	// e is in the reGoMethodRoute prefix list; should be detected.
	src := []byte(`func main() {
	e := echo.New()
	e.GET("/health", healthCheck)
}`)
	regs := ExtractRouteRegistrations("main.go", src)
	if len(regs) != 1 {
		t.Fatalf("expected 1, got %d", len(regs))
	}
	assertReg(t, regs[0], "GET", "/health", "healthCheck", "main")
}

func TestExtractGoRoutes_MultiLine(t *testing.T) {
	// Handler on the next line — mergeGoMultilineRoutes should join them.
	src := []byte(`func setup(r *chi.Mux) {
	r.GET("/profile",
		getProfile)
	r.POST("/profile",
		updateProfile)
}`)
	regs := ExtractRouteRegistrations("setup.go", src)
	if len(regs) != 2 {
		t.Fatalf("expected 2 multi-line routes, got %d: %+v", len(regs), regs)
	}
	assertReg(t, regs[0], "GET", "/profile", "getProfile", "setup")
	assertReg(t, regs[1], "POST", "/profile", "updateProfile", "setup")
}

func TestExtractGoRoutes_NoFalsePositiveOnErrDot(t *testing.T) {
	// err.GET(...) must NOT match — err is a common non-router variable.
	// The reGoMethodRoute prefix list does not include "err".
	src := []byte(`func bad() {
	if err := db.Get(&result, query); err != nil {
		log.Println(err)
	}
}`)
	regs := ExtractRouteRegistrations("bad.go", src)
	if len(regs) != 0 {
		t.Errorf("expected 0 false-positive registrations, got %d: %+v", len(regs), regs)
	}
}

func TestExtractTSRoutes_Express(t *testing.T) {
	src := []byte(`
app.get('/users', listUsers)
router.post('/orders', createOrder)
app.delete('/items/:id', removeItem)
`)
	regs := ExtractRouteRegistrations("routes.ts", src)
	if len(regs) != 3 {
		t.Fatalf("expected 3, got %d: %+v", len(regs), regs)
	}
	assertReg(t, regs[0], "GET", "/users", "listUsers", "")
	assertReg(t, regs[1], "POST", "/orders", "createOrder", "")
	assertReg(t, regs[2], "DELETE", "/items/:id", "removeItem", "")
}

func TestExtractTSRoutes_UseBecomesWildcard(t *testing.T) {
	src := []byte(`app.use('/middleware', myMiddleware)`)
	regs := ExtractRouteRegistrations("app.js", src)
	if len(regs) != 1 {
		t.Fatalf("expected 1, got %d", len(regs))
	}
	if regs[0].Method != "*" {
		t.Errorf("use should map to *, got %q", regs[0].Method)
	}
}

func TestExtractPyRoutes_FastAPI(t *testing.T) {
	src := []byte(`from fastapi import FastAPI
app = FastAPI()

@app.get("/users")
async def list_users():
    pass

@app.post("/users")
def create_user():
    pass
`)
	regs := ExtractRouteRegistrations("routes.py", src)
	if len(regs) != 2 {
		t.Fatalf("expected 2, got %d: %+v", len(regs), regs)
	}
	// Handler is the function AFTER the decorator.
	assertReg(t, regs[0], "GET", "/users", "list_users", "")
	assertReg(t, regs[1], "POST", "/users", "create_user", "")
}

func TestExtractPyRoutes_EnclosingFnAtModuleLevel(t *testing.T) {
	// At module level, EnclosingFn must be "" — not the previous sibling def.
	src := []byte(`def unrelated():
    pass

@app.get("/health")
def health_check():
    pass
`)
	regs := ExtractRouteRegistrations("app.py", src)
	if len(regs) != 1 {
		t.Fatalf("expected 1, got %d", len(regs))
	}
	if regs[0].EnclosingFn != "" {
		t.Errorf("module-level decorator EnclosingFn should be empty, got %q", regs[0].EnclosingFn)
	}
}

func TestExtractPyRoutes_EnclosingFnInsideFunction(t *testing.T) {
	// When a route is registered inside a setup function, EnclosingFn should be set.
	src := []byte(`def register_routes(app):
    @app.get("/items")
    def list_items():
        pass
`)
	regs := ExtractRouteRegistrations("routes.py", src)
	if len(regs) != 1 {
		t.Fatalf("expected 1, got %d", len(regs))
	}
	if regs[0].EnclosingFn != "register_routes" {
		t.Errorf("expected EnclosingFn=register_routes, got %q", regs[0].EnclosingFn)
	}
}

func TestExtractPyRoutes_Django(t *testing.T) {
	src := []byte(`urlpatterns = [
    path('/users/', list_users),
    re_path(r'/orders/(?P<id>\d+)/', get_order),
]`)
	regs := ExtractRouteRegistrations("urls.py", src)
	if len(regs) != 2 {
		t.Fatalf("expected 2, got %d: %+v", len(regs), regs)
	}
	assertReg(t, regs[0], "*", "/users/", "list_users", "")
}

// ── InjectHandlesEdges ────────────────────────────────────────────────────────

func TestInjectHandlesEdges_Basic(t *testing.T) {
	g := graph.New("test")
	handlerNode := &graph.Node{
		ID:   g.MakeNodeID("server.go", "GetUsers"),
		Type: graph.NodeFunction,
		Name: "GetUsers",
		File: "server.go",
	}
	g.AddNode(handlerNode)

	regs := []RouteRegistration{
		{File: "server.go", Line: 5, Method: "GET", Path: "/users", Handler: "GetUsers", Confidence: 0.95},
	}
	n := InjectHandlesEdges(g, regs)
	if n != 1 {
		t.Fatalf("expected 1 edge injected, got %d", n)
	}
	// Route node must exist.
	routeID := g.MakeNodeID("server.go", "route:GET /users")
	if g.GetNode(routeID) == nil {
		t.Error("route node not created")
	}
	// HANDLES edge must exist.
	edges := g.OutEdges(routeID)
	if len(edges) != 1 || edges[0].Type != graph.EdgeHandles {
		t.Errorf("expected 1 HANDLES edge, got %v", edges)
	}
}

func TestInjectHandlesEdges_Idempotent(t *testing.T) {
	// Calling InjectHandlesEdges twice (simulating incremental reindex) must
	// not create duplicate edges — AddEdge now deduplicates.
	g := graph.New("test")
	handlerNode := &graph.Node{
		ID:   g.MakeNodeID("api.go", "handleLogin"),
		Type: graph.NodeFunction,
		Name: "handleLogin",
		File: "api.go",
	}
	g.AddNode(handlerNode)

	regs := []RouteRegistration{
		{File: "api.go", Line: 10, Method: "POST", Path: "/login", Handler: "handleLogin", Confidence: 0.9},
	}
	InjectHandlesEdges(g, regs)
	InjectHandlesEdges(g, regs) // second call — must be idempotent

	routeID := g.MakeNodeID("api.go", "route:POST /login")
	edges := g.OutEdges(routeID)
	if len(edges) != 1 {
		t.Errorf("expected exactly 1 HANDLES edge after double injection, got %d", len(edges))
	}
}

func TestInjectHandlesEdges_UnknownHandlerSkipped(t *testing.T) {
	g := graph.New("test")
	regs := []RouteRegistration{
		{File: "routes.go", Line: 1, Method: "GET", Path: "/missing", Handler: "doesNotExist", Confidence: 0.9},
	}
	n := InjectHandlesEdges(g, regs)
	if n != 0 {
		t.Errorf("expected 0 edges for unknown handler, got %d", n)
	}
}

func TestInjectHandlesEdges_EnclosingFnWired(t *testing.T) {
	g := graph.New("test")
	setupNode := &graph.Node{
		ID:   g.MakeNodeID("routes.go", "setupRoutes"),
		Type: graph.NodeFunction,
		Name: "setupRoutes",
		File: "routes.go",
	}
	handlerNode := &graph.Node{
		ID:   g.MakeNodeID("routes.go", "listItems"),
		Type: graph.NodeFunction,
		Name: "listItems",
		File: "routes.go",
	}
	g.AddNode(setupNode)
	g.AddNode(handlerNode)

	regs := []RouteRegistration{
		{File: "routes.go", Line: 5, Method: "GET", Path: "/items", Handler: "listItems",
			EnclosingFn: "setupRoutes", Confidence: 0.95},
	}
	InjectHandlesEdges(g, regs)

	// setupRoutes --CALLS--> routeNode should exist.
	calls := g.OutEdges(setupNode.ID)
	var foundCall bool
	for _, e := range calls {
		if e.Type == graph.EdgeCalls {
			foundCall = true
		}
	}
	if !foundCall {
		t.Error("expected CALLS edge from enclosing function to route node")
	}
}

// ── mergeGoMultilineRoutes ────────────────────────────────────────────────────

func TestMergeGoMultilineRoutes_JoinsHandlerOnNextLine(t *testing.T) {
	lines := [][]byte{
		[]byte(`r.GET("/users",`),
		[]byte(`	listUsers)`),
		[]byte(`r.POST("/items",`),
		[]byte(`	createItem)`),
	}
	merged := mergeGoMultilineRoutes(lines)
	// Lines 0 and 2 should be merged; lines 1 and 3 become empty.
	got0 := string(merged[0])
	if got0 != `r.GET("/users", listUsers)` {
		t.Errorf("unexpected merged line 0: %q", got0)
	}
	got2 := string(merged[2])
	if got2 != `r.POST("/items", createItem)` {
		t.Errorf("unexpected merged line 2: %q", got2)
	}
	// Continuation lines should be empty (not nil) to preserve line count.
	if len(merged[1]) != 0 {
		t.Errorf("continuation line should be empty after merge, got %q", merged[1])
	}
}

func TestMergeGoMultilineRoutes_LeavesCompleteLinesUntouched(t *testing.T) {
	lines := [][]byte{
		[]byte(`r.GET("/users", listUsers)`),
		[]byte(`someOtherCode()`),
	}
	merged := mergeGoMultilineRoutes(lines)
	if string(merged[0]) != `r.GET("/users", listUsers)` {
		t.Errorf("complete line should not be modified: %q", merged[0])
	}
	if string(merged[1]) != "someOtherCode()" {
		t.Errorf("second line should not be modified: %q", merged[1])
	}
}

// ── Go middleware chain ────────────────────────────────────────────────────────

func TestExtractGoRoutes_MiddlewareChain(t *testing.T) {
	// Handler is the LAST arg — middleware comes before it.
	src := []byte(`func setup(r *gin.RouterGroup) {
	r.GET("/users", authMiddleware, listUsers)
	r.POST("/users", authMiddleware, rateLimiter, createUser)
}`)
	regs := ExtractRouteRegistrations("routes.go", src)
	if len(regs) != 2 {
		t.Fatalf("expected 2 registrations, got %d: %+v", len(regs), regs)
	}
	assertReg(t, regs[0], "GET", "/users", "listUsers", "setup")
	assertReg(t, regs[1], "POST", "/users", "createUser", "setup")
}

// ── TypeScript multi-line routes ──────────────────────────────────────────────

func TestExtractTSRoutes_MultiLine(t *testing.T) {
	src := []byte(`
app.get('/users',
  listUsers)
router.post('/orders',
  createOrder)
`)
	regs := ExtractRouteRegistrations("routes.ts", src)
	if len(regs) != 2 {
		t.Fatalf("expected 2 multi-line TS routes, got %d: %+v", len(regs), regs)
	}
	assertReg(t, regs[0], "GET", "/users", "listUsers", "")
	assertReg(t, regs[1], "POST", "/orders", "createOrder", "")
}

func TestMergeMultilineRoutesTS_JoinsHandlerOnNextLine(t *testing.T) {
	lines := [][]byte{
		[]byte(`app.get('/users',`),
		[]byte(`  listUsers)`),
		[]byte(`router.post('/orders',`),
		[]byte(`  createOrder)`),
	}
	merged := mergeMultilineRoutesTS(lines)
	got0 := string(merged[0])
	if got0 != `app.get('/users', listUsers)` {
		t.Errorf("unexpected merged line 0: %q", got0)
	}
	got2 := string(merged[2])
	if got2 != `router.post('/orders', createOrder)` {
		t.Errorf("unexpected merged line 2: %q", got2)
	}
	if len(merged[1]) != 0 {
		t.Errorf("continuation line should be empty after merge, got %q", merged[1])
	}
}

// ── R1 GAP1: group/h/public/private/admin variable names ─────────────────────

// TestExtractGoRoutes_GroupVariable verifies that routes registered on a Gin
// sub-router variable named "group" are detected. This is the primary Gin usage
// pattern and was missing from the original reGoMethodRoute prefix list.
func TestExtractGoRoutes_GroupVariable(t *testing.T) {
	src := []byte(`func registerAdmin(group *gin.RouterGroup) {
	group.GET("/users", listAdminUsers)
	group.POST("/users", createAdminUser)
	h.GET("/health", healthCheck)
	public.GET("/ping", ping)
	private.POST("/secret", secretHandler)
	admin.DELETE("/nuke", nukeEverything)
}`)
	regs := ExtractRouteRegistrations("admin.go", src)
	if len(regs) != 6 {
		t.Fatalf("expected 6 registrations (group/h/public/private/admin), got %d: %+v", len(regs), regs)
	}
	assertReg(t, regs[0], "GET", "/users", "listAdminUsers", "registerAdmin")
	assertReg(t, regs[1], "POST", "/users", "createAdminUser", "registerAdmin")
	assertReg(t, regs[2], "GET", "/health", "healthCheck", "registerAdmin")
	assertReg(t, regs[3], "GET", "/ping", "ping", "registerAdmin")
	assertReg(t, regs[4], "POST", "/secret", "secretHandler", "registerAdmin")
	assertReg(t, regs[5], "DELETE", "/nuke", "nukeEverything", "registerAdmin")
}

// ── R1 GAP3: 3-line route merge ───────────────────────────────────────────────

// TestExtractGoRoutes_ThreeLineRoute verifies that a route call split across
// three lines (path + middleware + handler) produces a single HANDLES edge.
// The old implementation only merged 2-line splits; the third line was dropped.
func TestExtractGoRoutes_ThreeLineRoute(t *testing.T) {
	src := []byte(`func setup(r *gin.RouterGroup) {
	r.GET("/users",
		authMiddleware,
		GetUsers)
}`)
	regs := ExtractRouteRegistrations("routes.go", src)
	if len(regs) != 1 {
		t.Fatalf("expected 1 registration for 3-line route, got %d: %+v", len(regs), regs)
	}
	assertReg(t, regs[0], "GET", "/users", "GetUsers", "setup")
}

// ── R1 GAP4: Python multiline decorator ──────────────────────────────────────

// TestExtractPyRoutes_MultilineDecorator verifies that a FastAPI decorator
// where the path argument appears on the continuation line is detected.
// The old implementation had no merge step for Python decorators.
func TestExtractPyRoutes_MultilineDecorator(t *testing.T) {
	src := []byte(`from fastapi import FastAPI
app = FastAPI()

@app.get(
    "/users",
    response_model=list,
)
async def list_users():
    pass

@app.post(
    "/users",
)
async def create_user():
    pass
`)
	regs := ExtractRouteRegistrations("routes.py", src)
	if len(regs) != 2 {
		t.Fatalf("expected 2 registrations for multiline decorators, got %d: %+v", len(regs), regs)
	}
	assertReg(t, regs[0], "GET", "/users", "list_users", "")
	assertReg(t, regs[1], "POST", "/users", "create_user", "")
}

// ── helpers ───────────────────────────────────────────────────────────────────

func assertReg(t *testing.T, reg RouteRegistration, method, path, handler, enclosingFn string) {
	t.Helper()
	if reg.Method != method {
		t.Errorf("Method: want %q, got %q", method, reg.Method)
	}
	if reg.Path != path {
		t.Errorf("Path: want %q, got %q", path, reg.Path)
	}
	if reg.Handler != handler {
		t.Errorf("Handler: want %q, got %q", handler, reg.Handler)
	}
	if enclosingFn != "" && reg.EnclosingFn != enclosingFn {
		t.Errorf("EnclosingFn: want %q, got %q", enclosingFn, reg.EnclosingFn)
	}
}

// ── R1 GAP5: backtick path strings ────────────────────────────────────────────

// TestExtractGoRoutes_BacktickPath verifies that route registrations using raw
// (backtick) string literals for the path are detected. All four Go route regexes
// previously only matched double-quoted paths.
func TestExtractGoRoutes_BacktickPath(t *testing.T) {
	src := []byte("func setup(r *gin.Engine) {\n" +
		"\tr.GET(`/users`, listUsers)\n" +
		"\tr.POST(`/users`, createUser)\n" +
		"}\n")

	regs := extractGoRoutes("setup.go", src)
	if len(regs) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(regs))
	}
	assertReg(t, regs[0], "GET", "/users", "listUsers", "")
	assertReg(t, regs[1], "POST", "/users", "createUser", "")
}

// ── R1 GAP6: no fuzzy fallback in resolveByName ───────────────────────────────

// TestInjectHandlesEdges_NoFuzzyFallback verifies that resolveByName does NOT
// create a HANDLES edge when the handler name is only a substring match.
// The old FindByPattern fallback would wire "Get" to "GetUser" (false edge).
func TestInjectHandlesEdges_NoFuzzyFallback(t *testing.T) {
	g := graph.New("test")
	// Register a function whose name CONTAINS the handler name as substring.
	g.AddNode(&graph.Node{
		ID:   graph.NodeID("test::file.go::GetUser"),
		Name: "GetUser",
		File: "file.go",
		Type: graph.NodeFunction,
	})

	// Route that references a handler named "Get" — exact name does not exist.
	regs := []RouteRegistration{{
		File:    "router.go",
		Method:  "GET",
		Path:    "/items",
		Handler: "Get",
	}}
	injected := InjectHandlesEdges(g, regs)
	if injected != 0 {
		t.Errorf("expected 0 HANDLES edges (no exact match), got %d", injected)
	}
}

// ── MCP handler registrations (BUG-EVAL-20 / IMP-EVAL-11) ────────────────────

// TestExtractMCPHandlerRegistrations_Basic verifies that a typical
// addOrDefer(mcp.NewTool("name",...), s.handleXxx) block is detected.
func TestExtractMCPHandlerRegistrations_Basic(t *testing.T) {
	src := []byte(`package mcp

func (s *Server) registerTools() {
	s.addOrDefer(mcp.NewTool("get_context",
		mcp.WithDescription("Returns context for an entity."),
	), s.handleGetContext)
	s.addOrDefer(mcp.NewTool("get_impact",
		mcp.WithDescription("Returns impact analysis."),
	), s.handleGetImpact)
}
`)
	regs := ExtractRouteRegistrations("server.go", src)
	// Filter to MCP registrations only.
	var mcpRegs []RouteRegistration
	for _, r := range regs {
		if r.Method == "mcp" {
			mcpRegs = append(mcpRegs, r)
		}
	}
	if len(mcpRegs) != 2 {
		t.Fatalf("expected 2 MCP registrations, got %d: %+v", len(mcpRegs), mcpRegs)
	}
	assertReg(t, mcpRegs[0], "mcp", "get_context", "handleGetContext", "registerTools")
	assertReg(t, mcpRegs[1], "mcp", "get_impact", "handleGetImpact", "registerTools")
}

// TestExtractMCPHandlerRegistrations_DescriptionWithParens verifies that
// parentheses inside mcp.WithDescription() strings do not confuse block detection.
func TestExtractMCPHandlerRegistrations_DescriptionWithParens(t *testing.T) {
	src := []byte(`package mcp

func (s *Server) registerTools() {
	s.addOrDefer(mcp.NewTool("find_entity",
		mcp.WithDescription("Find an entity (function, struct, or interface) by name."),
		mcp.WithString("query", mcp.Required(), mcp.Description("The name (or partial name) to search for.")),
	), s.handleFindEntity)
}
`)
	regs := ExtractRouteRegistrations("server.go", src)
	var mcpRegs []RouteRegistration
	for _, r := range regs {
		if r.Method == "mcp" {
			mcpRegs = append(mcpRegs, r)
		}
	}
	if len(mcpRegs) != 1 {
		t.Fatalf("expected 1 MCP registration, got %d: %+v", len(mcpRegs), mcpRegs)
	}
	assertReg(t, mcpRegs[0], "mcp", "find_entity", "handleFindEntity", "registerTools")
}

// TestExtractMCPHandlerRegistrations_SingleLine verifies detection when the
// entire addOrDefer call fits on one line.
func TestExtractMCPHandlerRegistrations_SingleLine(t *testing.T) {
	src := []byte(`func (s *Server) registerTools() {
	s.addOrDefer(mcp.NewTool("search"), s.handleSearch)
}
`)
	regs := ExtractRouteRegistrations("server.go", src)
	var mcpRegs []RouteRegistration
	for _, r := range regs {
		if r.Method == "mcp" {
			mcpRegs = append(mcpRegs, r)
		}
	}
	if len(mcpRegs) != 1 {
		t.Fatalf("expected 1 MCP registration, got %d: %+v", len(mcpRegs), mcpRegs)
	}
	assertReg(t, mcpRegs[0], "mcp", "search", "handleSearch", "registerTools")
}

// TestExtractMCPHandlerRegistrations_NoMatch verifies that ordinary Go code
// without addOrDefer produces no MCP registrations.
func TestExtractMCPHandlerRegistrations_NoMatch(t *testing.T) {
	src := []byte(`package main

func init() {
	http.HandleFunc("/users", listUsers)
	log.Println("started")
}
`)
	regs := ExtractRouteRegistrations("main.go", src)
	for _, r := range regs {
		if r.Method == "mcp" {
			t.Errorf("unexpected MCP registration: %+v", r)
		}
	}
}

// TestExtractMCPHandlerRegistrations_NoCrossBlockContamination is the critical
// correctness test: a block that has no handler reference (e.g. a non-MCP
// addOrDefer overload) must NOT steal the handler from the next block.
//
// Before the block-boundary sentinel was added, the sliding window would scan
// past the end of block A into block B and emit toolA→handleB — a false edge.
func TestExtractMCPHandlerRegistrations_NoCrossBlockContamination(t *testing.T) {
	// Block 1: addOrDefer with no s.handle* arg (non-MCP overload or typo).
	// Block 2: valid addOrDefer that immediately follows.
	src := []byte(`package mcp

func (s *Server) registerTools() {
	// Block 1: non-standard addOrDefer with no handler reference.
	s.addOrDefer(mcp.NewTool("orphan",
		mcp.WithDescription("This tool has no handler arg."),
	))
	// Block 2: well-formed registration that must NOT be claimed by block 1.
	s.addOrDefer(mcp.NewTool("search",
		mcp.WithDescription("Full-text search."),
	), s.handleSearch)
}
`)
	regs := ExtractRouteRegistrations("server.go", src)
	var mcpRegs []RouteRegistration
	for _, r := range regs {
		if r.Method == "mcp" {
			mcpRegs = append(mcpRegs, r)
		}
	}

	// Must detect exactly one registration: "search" → handleSearch.
	// "orphan" has no handler so it must be silently skipped.
	if len(mcpRegs) != 1 {
		t.Fatalf("expected 1 MCP registration, got %d: %+v", len(mcpRegs), mcpRegs)
	}
	if mcpRegs[0].Path != "search" {
		t.Errorf("Path: want %q, got %q", "search", mcpRegs[0].Path)
	}
	if mcpRegs[0].Handler != "handleSearch" {
		t.Errorf("Handler: want %q, got %q", "handleSearch", mcpRegs[0].Handler)
	}
}

// TestExtractMCPHandlerRegistrations_AdjacentBlocks verifies that two consecutive
// well-formed addOrDefer blocks each bind to their own handler when they sit
// within the same mcpBlockWindow of each other.
func TestExtractMCPHandlerRegistrations_AdjacentBlocks(t *testing.T) {
	src := []byte(`package mcp

func (s *Server) registerTools() {
	s.addOrDefer(mcp.NewTool("tool_a"), s.handleToolA)
	s.addOrDefer(mcp.NewTool("tool_b"), s.handleToolB)
	s.addOrDefer(mcp.NewTool("tool_c"), s.handleToolC)
}
`)
	regs := ExtractRouteRegistrations("server.go", src)
	var mcpRegs []RouteRegistration
	for _, r := range regs {
		if r.Method == "mcp" {
			mcpRegs = append(mcpRegs, r)
		}
	}
	if len(mcpRegs) != 3 {
		t.Fatalf("expected 3 MCP registrations, got %d: %+v", len(mcpRegs), mcpRegs)
	}
	// Each tool must be paired with its own handler.
	want := map[string]string{
		"tool_a": "handleToolA",
		"tool_b": "handleToolB",
		"tool_c": "handleToolC",
	}
	for _, r := range mcpRegs {
		wantHandler, ok := want[r.Path]
		if !ok {
			t.Errorf("unexpected registration path %q", r.Path)
			continue
		}
		if r.Handler != wantHandler {
			t.Errorf("tool %q: handler want %q, got %q", r.Path, wantHandler, r.Handler)
		}
		delete(want, r.Path)
	}
	if len(want) > 0 {
		t.Errorf("missing registrations: %v", want)
	}
}
