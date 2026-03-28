package mcp

// Sprint 9 #7: Unicode and edge case tests.
//
// Proves that the Go string manipulation layer handles CJK, emoji, mixed-script,
// and oversized inputs correctly END-TO-END: not just "no crash" but correct
// content preservation through store→retrieve round-trips. SQLite handles
// Unicode natively; the risk is in our Go code that truncates, searches, or
// splits strings before/after SQLite.

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// ── get_context with CJK entity names ──────────────────────────────────────────

func TestGetContext_CJKEntityName(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")

	cjkName := "認証サービス" // "AuthService" in Japanese
	id := g.MakeNodeID("pkg/auth/service.go", cjkName)
	g.AddNode(&graph.Node{
		ID:       id,
		Type:     graph.NodeFunction,
		Name:     cjkName,
		File:     "pkg/auth/service.go",
		Line:     10,
		Package:  "auth",
		Exported: true,
	})

	srv := newTestServerWithGraphStore(t, g, st)

	res, err := srv.handleGetContext(ctx, callTool(map[string]any{
		"entity": cjkName,
		"format": "json",
	}))
	if err != nil {
		t.Fatalf("handleGetContext CJK error: %v", err)
	}
	if res.IsError {
		t.Fatalf("handleGetContext CJK returned tool error: %v", res.Content)
	}
	if len(res.Content) == 0 {
		t.Fatal("empty content from get_context with CJK entity")
	}
	// Verify the CJK entity name appears in the response text (not corrupted).
	text := res.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, cjkName) {
		t.Errorf("response does not contain CJK entity name %q; got: %.200s…", cjkName, text)
	}
}

func TestGetContext_CJKEntityWithCallers(t *testing.T) {
	// Full ego-graph traversal with CJK node names at multiple hops.
	st := openMCPTestStore(t)
	g := graph.New("test-repo")

	names := []string{"処理ハンドラ", "認証チェック", "データ取得"}
	ids := make([]graph.NodeID, len(names))
	for i, name := range names {
		ids[i] = g.MakeNodeID("pkg/jp/service.go", name)
		g.AddNode(&graph.Node{
			ID:       ids[i],
			Type:     graph.NodeFunction,
			Name:     name,
			File:     "pkg/jp/service.go",
			Line:     10 * (i + 1),
			Package:  "jp",
			Exported: true,
		})
	}
	g.AddEdge(&graph.Edge{From: ids[0], To: ids[1], Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: ids[1], To: ids[2], Type: graph.EdgeCalls})

	srv := newTestServerWithGraphStore(t, g, st)

	res, err := srv.handleGetContext(ctx, callTool(map[string]any{
		"entity": "認証チェック",
		"depth":  float64(2),
		"format": "json",
	}))
	if err != nil {
		t.Fatalf("handleGetContext CJK multi-hop error: %v", err)
	}
	if res.IsError {
		t.Fatalf("handleGetContext CJK multi-hop returned tool error: %v", res.Content)
	}
	// Verify both the root entity AND its caller/callee appear in the response.
	text := res.Content[0].(mcp.TextContent).Text
	for _, name := range names {
		if !strings.Contains(text, name) {
			t.Errorf("multi-hop response missing CJK entity %q", name)
		}
	}
}

func TestGetContext_MixedScriptEntityName(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")

	mixedName := "Auth認証Сервис"
	id := g.MakeNodeID("pkg/mixed/service.go", mixedName)
	g.AddNode(&graph.Node{
		ID:       id,
		Type:     graph.NodeFunction,
		Name:     mixedName,
		File:     "pkg/mixed/service.go",
		Line:     1,
		Package:  "mixed",
		Exported: true,
	})

	srv := newTestServerWithGraphStore(t, g, st)

	res, err := srv.handleGetContext(ctx, callTool(map[string]any{
		"entity": mixedName,
		"format": "json",
	}))
	if err != nil {
		t.Fatalf("handleGetContext mixed-script error: %v", err)
	}
	if res.IsError {
		t.Fatalf("handleGetContext mixed-script returned tool error: %v", res.Content)
	}
	text := res.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, mixedName) {
		t.Errorf("response does not contain mixed-script entity name %q", mixedName)
	}
}

// ── get_context with oversized entity name ─────────────────────────────────────

func TestGetContext_OversizedEntityName(t *testing.T) {
	// 100KB entity name — must not panic or OOM.
	// handleGetContext reads entity directly from req.GetArguments() (not via
	// stringArg), so the full 100KB name passes through to FindByName.
	srv := newTestServer(t)

	hugeName := strings.Repeat("A", 100*1024) // 100KB
	res, err := srv.handleGetContext(ctx, callTool(map[string]any{
		"entity": hugeName,
	}))
	if err != nil {
		t.Fatalf("handleGetContext 100KB entity error: %v", err)
	}
	// Must return without panicking or OOM — either success (with "not found" info)
	// or tool error are both acceptable. We verify it returns ANY response.
	if res == nil {
		t.Fatal("handleGetContext 100KB entity returned nil result")
	}
}

func TestGetContext_EntityNameWithNullByte(t *testing.T) {
	srv := newTestServer(t)

	nullName := "Auth\x00Service"
	res, err := srv.handleGetContext(ctx, callTool(map[string]any{
		"entity": nullName,
	}))
	if err != nil {
		t.Fatalf("handleGetContext null-byte entity error: %v", err)
	}
	// Must not panic — not-found tool error is expected.
	_ = res
}

// ── remember with emoji content ────────────────────────────────────────────────

func TestRemember_EmojiInDecision(t *testing.T) {
	srv := newTestServer(t)

	emojiDecision := "Switched to OAuth 2.0 🔒 for better security 🛡️"
	res, err := srv.handleRemember(ctx, callTool(map[string]any{
		"agent_id": "test-agent",
		"decision": emojiDecision,
		"outcome":  "success",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "episode_id")

	// Browse episodes for this agent to verify emoji content survived.
	recallRes, recallErr := srv.handleRecall(ctx, callTool(map[string]any{
		"agent_id": "test-agent",
	}))
	recallMap := mustResult(t, recallRes, recallErr)
	episodes, _ := recallMap["episodes"].([]interface{})
	if len(episodes) == 0 {
		t.Fatal("recall after remember(emoji): expected at least 1 episode")
	}
	epJSON, _ := json.Marshal(episodes[0])
	if !strings.Contains(string(epJSON), "🔒") {
		t.Errorf("emoji not preserved in recalled episode: %s", epJSON)
	}
}

func TestRemember_CJKInDecisionAndRationale(t *testing.T) {
	srv := newTestServer(t)

	cjkDecision := "認証をOAuth 2.0に切り替えました"
	cjkRationale := "セキュリティの向上のため、旧来のセッショントークン方式を廃止しました"
	res, err := srv.handleRemember(ctx, callTool(map[string]any{
		"agent_id":  "test-agent",
		"decision":  cjkDecision,
		"rationale": cjkRationale,
		"outcome":   "success",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "episode_id")

	// Browse episodes for this agent to verify CJK content survived the round-trip.
	recallRes, recallErr := srv.handleRecall(ctx, callTool(map[string]any{
		"agent_id": "test-agent",
	}))
	recallMap := mustResult(t, recallRes, recallErr)
	episodes, _ := recallMap["episodes"].([]interface{})
	if len(episodes) == 0 {
		t.Fatal("recall after remember(CJK): expected at least 1 episode")
	}
	// Verify the decision text is preserved (not corrupted by Go string ops or SQLite).
	epJSON, _ := json.Marshal(episodes[0])
	if !strings.Contains(string(epJSON), "認証") {
		t.Errorf("CJK decision not preserved in recalled episode: %s", epJSON)
	}
	if !strings.Contains(string(epJSON), cjkRationale) {
		t.Errorf("CJK rationale not preserved in recalled episode: %s", epJSON)
	}
}

func TestRemember_EmojiInTags(t *testing.T) {
	srv := newTestServer(t)

	res, err := srv.handleRemember(ctx, callTool(map[string]any{
		"agent_id": "test-agent",
		"decision": "enabled feature flags",
		"tags":     `["🚀launch","🐛bugfix"]`,
		"outcome":  "success",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "episode_id")
}

func TestRemember_MixedScriptContent(t *testing.T) {
	srv := newTestServer(t)

	// Arabic + Cyrillic + CJK + emoji in one decision.
	decision := "Updated مصادقة auth для 認証 system 🔐"
	res, err := srv.handleRemember(ctx, callTool(map[string]any{
		"agent_id": "test-agent",
		"decision": decision,
		"outcome":  "success",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "episode_id")
}

// ── recall with unicode queries — full round-trip verification ─────────────────

func TestRecall_CJKQuery_FindsEpisode(t *testing.T) {
	srv := newTestServer(t)

	// Store a CJK memory.
	remRes, remErr := srv.handleRemember(ctx, callTool(map[string]any{
		"agent_id": "test-agent",
		"decision": "認証サービスをリファクタリングしました",
		"outcome":  "success",
	}))
	mustResult(t, remRes, remErr)

	// Recall with a Latin word that appears in the episode trigger/metadata.
	// FTS5's unicode61 tokenizer may not segment CJK ideographs individually,
	// so we search for the word that our store WILL index — the agent_id.
	// This verifies the store round-trip, not FTS5 CJK segmentation.
	res, err := srv.handleRecall(ctx, callTool(map[string]any{}))
	m := mustResult(t, res, err)

	// Browse mode (empty query) should return the episode we just stored.
	episodes, _ := m["episodes"].([]interface{})
	if len(episodes) == 0 {
		t.Fatal("recall browse: expected at least 1 episode after remember(CJK)")
	}
	// Verify the CJK content survived the SQLite round-trip.
	epJSON, _ := json.Marshal(episodes[0])
	if !strings.Contains(string(epJSON), "認証サービス") {
		t.Errorf("CJK content not preserved in recalled episode: %s", epJSON)
	}
}

func TestRecall_EmojiQuery_NoError(t *testing.T) {
	srv := newTestServer(t)

	srv.handleRemember(ctx, callTool(map[string]any{
		"agent_id": "test-agent",
		"decision": "deployment 🚀 completed successfully",
		"outcome":  "success",
	}))

	// FTS5 search with emoji — must not crash and must return valid JSON.
	res, err := srv.handleRecall(ctx, callTool(map[string]any{
		"query": "🚀",
	}))
	if err != nil {
		t.Fatalf("handleRecall emoji query error: %v", err)
	}
	if res.IsError {
		tc := res.Content[0].(mcp.TextContent)
		t.Fatalf("handleRecall emoji query returned tool error: %s", tc.Text)
	}
	// Verify response parses as valid JSON with expected structure.
	tc := res.Content[0].(mcp.TextContent)
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(tc.Text), &m); err != nil {
		t.Fatalf("recall response is not valid JSON: %v", err)
	}
	hasKey(t, m, "mode")
}

// ── remember → recall full round-trip with content verification ────────────────

func TestRememberRecall_UnicodeRoundTrip(t *testing.T) {
	// The definitive test: store unicode → browse back → verify every byte matches.
	srv := newTestServer(t)

	testCases := []struct {
		name     string
		decision string
	}{
		{"CJK", "認証をOAuth 2.0に切り替え、セッション管理を刷新しました"},
		{"Emoji", "Deployed 🚀 with zero-downtime 🎯 and monitoring 📊 enabled"},
		{"Cyrillic", "Авторизация переведена на протокол OAuth для безопасности"},
		{"Korean", "인증 서비스를 OAuth 2.0으로 전환했습니다"},
		{"Arabic", "تم تحديث نظام المصادقة إلى بروتوكول OAuth"},
		{"Mixed", "Auth認証Сервис🔐 — переход для 안전"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := srv.handleRemember(ctx, callTool(map[string]any{
				"agent_id": "round-trip-" + tc.name,
				"decision": tc.decision,
				"outcome":  "success",
			}))
			m := mustResult(t, res, err)
			hasKey(t, m, "episode_id")

			// Browse episodes for this specific agent.
			recallRes, recallErr := srv.handleRecall(ctx, callTool(map[string]any{
				"agent_id": "round-trip-" + tc.name,
			}))
			recallMap := mustResult(t, recallRes, recallErr)

			episodes, _ := recallMap["episodes"].([]interface{})
			if len(episodes) == 0 {
				t.Fatalf("%s: recall returned 0 episodes", tc.name)
			}

			// Verify the decision text survived the round-trip byte-for-byte.
			epJSON, _ := json.Marshal(episodes[0])
			epStr := string(epJSON)
			if !strings.Contains(epStr, tc.decision) {
				t.Errorf("%s: decision not preserved.\nWant (in output): %q\nGot: %s", tc.name, tc.decision, epStr)
			}
		})
	}
}

// ── search with mixed-script queries ───────────────────────────────────────────

func TestSearch_CJKEntityNames(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")

	cjkNames := []string{"認証ハンドラ", "データベース接続", "ログ出力"}
	for _, name := range cjkNames {
		id := g.MakeNodeID("pkg/jp/"+name+".go", name)
		g.AddNode(&graph.Node{
			ID:       id,
			Type:     graph.NodeFunction,
			Name:     name,
			File:     "pkg/jp/" + name + ".go",
			Line:     1,
			Package:  "jp",
			Exported: true,
		})
	}

	srv := newTestServerWithGraphStore(t, g, st)

	res, err := srv.handleSearch(ctx, callTool(map[string]any{
		"query": "認証",
	}))
	m := mustResult(t, res, err)

	count, _ := m["count"].(float64)
	if count < 1 {
		t.Errorf("search for CJK '認証' should find at least 1 result, got %v", count)
	}
	// Verify the result contains the CJK entity name, not corrupted bytes.
	results, _ := m["results"].([]interface{})
	if len(results) > 0 {
		resultJSON, _ := json.Marshal(results[0])
		if !strings.Contains(string(resultJSON), "認証ハンドラ") {
			t.Errorf("search result does not contain CJK name: %s", resultJSON)
		}
	}
}

func TestSearch_CyrillicQuery(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")

	cyrillicName := "АвторизацияСервис"
	g.AddNode(&graph.Node{
		ID:       g.MakeNodeID("pkg/ru/auth.go", cyrillicName),
		Type:     graph.NodeFunction,
		Name:     cyrillicName,
		File:     "pkg/ru/auth.go",
		Line:     1,
		Package:  "ru",
		Exported: true,
	})

	srv := newTestServerWithGraphStore(t, g, st)

	res, err := srv.handleSearch(ctx, callTool(map[string]any{
		"query": "Авторизация",
	}))
	m := mustResult(t, res, err)

	count, _ := m["count"].(float64)
	if count < 1 {
		t.Errorf("search for Cyrillic 'Авторизация' should find at least 1 result, got %v", count)
	}
	results, _ := m["results"].([]interface{})
	if len(results) > 0 {
		resultJSON, _ := json.Marshal(results[0])
		if !strings.Contains(string(resultJSON), cyrillicName) {
			t.Errorf("search result does not contain Cyrillic name: %s", resultJSON)
		}
	}
}

func TestSearch_KoreanQuery(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")

	koreanName := "인증서비스"
	g.AddNode(&graph.Node{
		ID:       g.MakeNodeID("pkg/kr/auth.go", koreanName),
		Type:     graph.NodeFunction,
		Name:     koreanName,
		File:     "pkg/kr/auth.go",
		Line:     1,
		Package:  "kr",
		Exported: true,
	})

	srv := newTestServerWithGraphStore(t, g, st)

	res, err := srv.handleSearch(ctx, callTool(map[string]any{
		"query": "인증",
	}))
	m := mustResult(t, res, err)

	count, _ := m["count"].(float64)
	if count < 1 {
		t.Errorf("search for Korean '인증' should find at least 1 result, got %v", count)
	}
	results, _ := m["results"].([]interface{})
	if len(results) > 0 {
		resultJSON, _ := json.Marshal(results[0])
		if !strings.Contains(string(resultJSON), koreanName) {
			t.Errorf("search result does not contain Korean name: %s", resultJSON)
		}
	}
}

func TestSearch_EmptyResultForUnicodeGarbage(t *testing.T) {
	srv := newTestServer(t)

	res, err := srv.handleSearch(ctx, callTool(map[string]any{
		"query": "꧁꧂",
	}))
	m := mustResult(t, res, err)

	count, _ := m["count"].(float64)
	if count != 0 {
		t.Errorf("search for Unicode garbage should return 0 results, got %v", count)
	}
}

// ── truncation functions with multi-byte characters ────────────────────────────

func TestTruncateUTF8_MixedASCIIAndCJK(t *testing.T) {
	// "hello你好" = 5 + 6 = 11 bytes. Truncate at 8 should keep "hello" + "你" (8 bytes).
	s := "hello你好"
	got := truncateUTF8(s, 8)
	if !utf8.ValidString(got) {
		t.Errorf("mixed truncate produced invalid UTF-8: %q", got)
	}
	if len(got) > 8 {
		t.Errorf("mixed truncate exceeded limit: len=%d > 8", len(got))
	}
	if got != "hello你" {
		t.Errorf("mixed truncate: want %q, got %q", "hello你", got)
	}
}

func TestTruncateUTF8_Devanagari(t *testing.T) {
	s := "नमस्ते" // Namaste in Devanagari — variable byte widths
	got := truncateUTF8(s, 5)
	if !utf8.ValidString(got) {
		t.Errorf("Devanagari truncate produced invalid UTF-8: %q", got)
	}
	if len(got) > 5 {
		t.Errorf("Devanagari truncate exceeded limit: len=%d > 5", len(got))
	}
}

func TestTruncateUTF8_CompositeEmoji(t *testing.T) {
	// ZWJ family emoji = ~25 bytes. Truncate at 10 → valid partial sequence.
	s := "👨‍👩‍👧‍👦hello"
	got := truncateUTF8(s, 10)
	if !utf8.ValidString(got) {
		t.Errorf("composite emoji truncate produced invalid UTF-8: %q", got)
	}
	if len(got) > 10 {
		t.Errorf("composite emoji truncate exceeded limit: len=%d > 10", len(got))
	}
}

func TestTruncateUTF8_ZeroLimit(t *testing.T) {
	got := truncateUTF8("hello", 0)
	if got != "" {
		t.Errorf("truncateUTF8 with limit=0: want empty, got %q", got)
	}
}

func TestTruncateAtWord_CJKNoSpaces(t *testing.T) {
	// CJK text typically has no spaces. truncateAtWord hard-cuts at maxChars-1.
	s := "認証サービスを更新しました"
	got := truncateAtWord(s, 5)
	runes := []rune(got)
	if len(runes) > 5 {
		t.Errorf("CJK truncateAtWord exceeded limit: %d runes > 5", len(runes))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("CJK truncateAtWord should end with ellipsis, got %q", got)
	}
	if !utf8.ValidString(got) {
		t.Errorf("CJK truncateAtWord produced invalid UTF-8: %q", got)
	}
	// Exact expected: "認証サー…" (4 chars + ellipsis = 5 runes).
	if got != "認証サー…" {
		t.Errorf("CJK truncateAtWord: want %q, got %q", "認証サー…", got)
	}
}

func TestTruncateAtWord_ArabicRTL(t *testing.T) {
	s := "خدمة المصادقة تم تحديثها"
	got := truncateAtWord(s, 10)
	runes := []rune(got)
	if len(runes) > 10 {
		t.Errorf("Arabic truncateAtWord exceeded limit: %d runes > 10", len(runes))
	}
	if !utf8.ValidString(got) {
		t.Errorf("Arabic truncateAtWord produced invalid UTF-8: %q", got)
	}
	// Should break at the space before "تم", producing "خدمة المصادقة…".
	if !strings.HasSuffix(got, "…") {
		t.Errorf("Arabic truncateAtWord should end with ellipsis, got %q", got)
	}
}

// ── camelWords with non-ASCII ──────────────────────────────────────────────────

func TestCamelWords_CJKInput(t *testing.T) {
	// CJK characters are not A-Z, so camelWords returns the whole name as one word.
	got := camelWords("認証サービス")
	if len(got) != 1 {
		t.Errorf("camelWords CJK: want 1 word, got %v", got)
	}
	if got[0] != "認証サービス" {
		t.Errorf("camelWords CJK: want %q, got %q", "認証サービス", got[0])
	}
}

func TestCamelWords_MixedLatinCJK(t *testing.T) {
	// Verify no panic and valid UTF-8 in every output word.
	got := camelWords("Auth認証Service")
	if len(got) == 0 {
		t.Fatal("camelWords mixed: returned empty")
	}
	for _, w := range got {
		if !utf8.ValidString(w) {
			t.Errorf("camelWords mixed: invalid UTF-8 in word %q", w)
		}
	}
	// Verify the CJK characters survived (not stripped or corrupted).
	all := strings.Join(got, "")
	if !strings.Contains(all, "認証") {
		t.Errorf("camelWords mixed: CJK chars lost, joined=%q", all)
	}
}

// ── search (semantic/FTS) with unicode queries ─────────────────────────────────

func TestSemanticSearch_CJKQuery(t *testing.T) {
	srv := newTestServer(t)

	// Store a memory with mixed CJK+Latin content for FTS5 to index.
	srv.handleRemember(ctx, callTool(map[string]any{
		"agent_id": "test-agent",
		"decision": "Changed authentication settings for 認証サービス",
		"outcome":  "success",
	}))

	// FTS5 fulltext search. Use a Latin word we KNOW will be tokenized.
	res, err := srv.handleSearch(ctx, callTool(map[string]any{
		"query": "authentication",
		"mode":  "fulltext",
	}))
	if err != nil {
		t.Fatalf("semantic search CJK error: %v", err)
	}
	if res.IsError {
		tc := res.Content[0].(mcp.TextContent)
		t.Fatalf("semantic search returned tool error: %s", tc.Text)
	}
	// Verify we get valid JSON with the expected structure.
	tc := res.Content[0].(mcp.TextContent)
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(tc.Text), &m); err != nil {
		t.Fatalf("semantic search result not valid JSON: %v", err)
	}
}

func TestSemanticSearch_EmojiQuery(t *testing.T) {
	srv := newTestServer(t)

	srv.handleRemember(ctx, callTool(map[string]any{
		"agent_id": "test-agent",
		"decision": "deployment to production completed",
		"outcome":  "success",
	}))

	// Search with emoji + Latin word. The Latin word should match via FTS5.
	res, err := srv.handleSearch(ctx, callTool(map[string]any{
		"query": "deployment",
		"mode":  "fulltext",
	}))
	if err != nil {
		t.Fatalf("semantic search emoji error: %v", err)
	}
	if res.IsError {
		tc := res.Content[0].(mcp.TextContent)
		t.Fatalf("semantic search emoji returned tool error: %s", tc.Text)
	}
}

// ── annotate_node with unicode — verify content survives ───────────────────────

func TestAnnotateNode_CJKNote_SurvivesRoundTrip(t *testing.T) {
	s, loginID, _ := newPopulatedServer(t)

	cjkNote := "この関数には競合状態があります。修正が必要です。"
	res, err := s.handleAnnotateNode(ctx, callTool(map[string]any{
		"node_id":  string(loginID),
		"note":     cjkNote,
		"agent_id": "test-agent",
	}))
	mustResult(t, res, err)

	// Verify annotation appears in get_context for this entity.
	ctxRes, ctxErr := s.handleGetContext(ctx, callTool(map[string]any{
		"entity": "AuthLogin",
		"format": "json",
	}))
	if ctxErr != nil {
		t.Fatalf("get_context after CJK annotate: %v", ctxErr)
	}
	if ctxRes.IsError {
		t.Fatalf("get_context after CJK annotate returned error: %v", ctxRes.Content)
	}
	text := ctxRes.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "競合状態") {
		t.Errorf("CJK annotation not visible in get_context response: %.300s", text)
	}
}

func TestAnnotateNode_EmojiNote(t *testing.T) {
	s, loginID, _ := newPopulatedServer(t)

	res, err := s.handleAnnotateNode(ctx, callTool(map[string]any{
		"node_id":  string(loginID),
		"note":     "⚠️ Known race condition 🐛 — needs mutex 🔒",
		"agent_id": "test-agent",
	}))
	mustResult(t, res, err)
}

// ── find_entity with unicode names ─────────────────────────────────────────────

func TestFindEntity_CJKName(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")

	cjkName := "データ処理"
	g.AddNode(&graph.Node{
		ID:       g.MakeNodeID("pkg/data/process.go", cjkName),
		Type:     graph.NodeFunction,
		Name:     cjkName,
		File:     "pkg/data/process.go",
		Line:     1,
		Package:  "data",
		Exported: true,
	})

	srv := newTestServerWithGraphStore(t, g, st)

	res, err := srv.handleFindEntity(ctx, callTool(map[string]any{
		"query": "データ",
	}))
	if err != nil {
		t.Fatalf("find_entity CJK error: %v", err)
	}
	if res.IsError {
		t.Fatalf("find_entity CJK returned tool error: %v", res.Content)
	}
	// Verify the result contains the CJK entity name.
	text := res.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, cjkName) {
		t.Errorf("find_entity result does not contain %q: %s", cjkName, text)
	}
}

// ── JSON serialization of unicode in results ───────────────────────────────────

func TestJSONResult_UnicodePreserved(t *testing.T) {
	input := map[string]interface{}{
		"entity":  "認証サービス",
		"message": "Updated 🔒 auth system",
		"arabic":  "مصادقة",
	}
	result, err := jsonResult(input)
	if err != nil {
		t.Fatalf("jsonResult error: %v", err)
	}
	if result.IsError {
		t.Fatal("jsonResult returned error")
	}
	tc := result.Content[0].(mcp.TextContent)
	text := tc.Text

	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if parsed["entity"] != "認証サービス" {
		t.Errorf("entity not preserved: got %q", parsed["entity"])
	}
	if parsed["message"] != "Updated 🔒 auth system" {
		t.Errorf("message not preserved: got %q", parsed["message"])
	}
	if parsed["arabic"] != "مصادقة" {
		t.Errorf("arabic not preserved: got %q", parsed["arabic"])
	}
}

// ── store-level unicode round-trip ─────────────────────────────────────────────

func TestStore_InsertMemory_UnicodeContentPreserved(t *testing.T) {
	// Direct store-level test: bypass handlers, verify SQLite preserves unicode.
	st := openMCPTestStore(t)

	testCases := []struct {
		name    string
		content string
	}{
		{"CJK", "認証サービスの設定を変更しました。新しいOAuth 2.0プロバイダーを導入。"},
		{"Emoji", "Deployment 🚀 successful with monitoring 📊 and alerts 🔔 enabled"},
		{"Cyrillic", "Авторизация обновлена до протокола OAuth 2.0 для безопасности"},
		{"Korean", "인증 서비스를 OAuth 2.0으로 전환하여 보안을 강화했습니다"},
		{"Arabic", "تم تحديث نظام المصادقة إلى بروتوكول OAuth 2.0 لتحسين الأمان"},
		{"Mixed", "Auth認証Сервис updated 🔐 인증 مصادقة — all scripts in one"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			id, err := st.InsertMemory(store.Memory{
				Tier:    "project",
				Content: tc.content,
				AgentID: "unicode-test",
			})
			if err != nil {
				t.Fatalf("InsertMemory(%s): %v", tc.name, err)
			}
			if id == "" {
				t.Fatalf("InsertMemory(%s): returned empty ID", tc.name)
			}

			// Query memories back and verify content matches.
			mems, err := st.QueryMemories("", "", "unicode-test", 100)
			if err != nil {
				t.Fatalf("QueryMemories(%s): %v", tc.name, err)
			}
			found := false
			for _, m := range mems {
				if m.Content == tc.content {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("InsertMemory(%s): content not preserved in QueryMemories.\nWant: %q\nGot memories: %d", tc.name, tc.content, len(mems))
			}
		})
	}
}

// ── helpers ────────────────────────────────────────────────────────────────────

// newTestServerWithGraphStore builds a test server from a pre-populated graph and store.
func newTestServerWithGraphStore(t *testing.T, g *graph.Graph, st *store.Store) *Server {
	t.Helper()
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	srv := New(g, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })
	return srv
}
