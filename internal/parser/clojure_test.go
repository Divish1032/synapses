package parser_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ─── Clojure test helpers ─────────────────────────────────────────────────────

func parseClojure(t *testing.T, src, filename string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewClojureParser()
	if err := p.Parse(g, "/tmp/"+filename, []byte(src)); err != nil {
		t.Fatalf("ClojureParser.Parse() error: %v", err)
	}
	return g
}

// clojureSource is a realistic Clojure file covering all extract-able constructs.
const clojureSource = `(ns data-pipeline.core
  (:require [clojure.string :as str]
            [clojure.set :as set]
            [data-pipeline.utils :as utils]))

(def ^:private secret-key "hunter2")
(def batch-size 100)

(defrecord DataPoint [timestamp value labels])

(defprotocol DataSource
  "Source of streaming data"
  (fetch-data [this query])
  (close! [this]))

(deftype FileDataSource [path opts]
  DataSource
  (fetch-data [this query] nil)
  (close! [this] nil))

(defmulti process-event :event-type)

(defmethod process-event :create
  [event]
  {:result :created})

(defn- build-query
  "Builds a query map from params"
  [params]
  {:where params})

(defn ^:private internal-helper [x]
  (* x 2))

(defn fetch-batch
  "Fetches a batch of records from the source"
  [source limit offset]
  (take limit (drop offset (fetch-data source {}))))

(defmacro with-retry
  "Retries body up to n times on exception"
  [n & body]
  ` + "`" + `(loop [remaining# ~n]
     (try ~@body
       (catch Exception e#
         (if (pos? remaining#)
           (recur (dec remaining#))
           (throw e#))))))` + "`" + `
`

// clojureScriptSource is a ClojureScript file for testing .cljs extension.
const clojureScriptSource = `(ns my-app.core
  (:require [reagent.core :as r]))

(defn render-component [props]
  [:div (:title props)])

(def app-state (atom {}))
`

// ─── Extensions ──────────────────────────────────────────────────────────────

func TestClojureParser_Extensions_CLJ(t *testing.T) {
	exts := parser.NewClojureParser().Extensions()
	if !hasExtension(exts, ".clj") {
		t.Errorf("Extensions() = %v, missing .clj", exts)
	}
}

func TestClojureParser_Extensions_CLJS(t *testing.T) {
	exts := parser.NewClojureParser().Extensions()
	if !hasExtension(exts, ".cljs") {
		t.Errorf("Extensions() = %v, missing .cljs", exts)
	}
}

func TestClojureParser_Extensions_CLJC(t *testing.T) {
	exts := parser.NewClojureParser().Extensions()
	if !hasExtension(exts, ".cljc") {
		t.Errorf("Extensions() = %v, missing .cljc", exts)
	}
}

// ─── File node ───────────────────────────────────────────────────────────────

func TestClojureParser_FileNode(t *testing.T) {
	g := parseClojure(t, clojureSource, "core.clj")
	assertFileNode(t, g, "core.clj")
}

// ─── defn (public function) ──────────────────────────────────────────────────

func TestClojureParser_DefnPublic_Exported(t *testing.T) {
	g := parseClojure(t, clojureSource, "core.clj")
	n := assertNode(t, g, "fetch-batch", graph.NodeFunction)
	if !n.Exported {
		t.Error("fetch-batch (defn) should be Exported: true")
	}
}

// ─── defn- (private function) ────────────────────────────────────────────────

func TestClojureParser_DefnMinus_NotExported(t *testing.T) {
	g := parseClojure(t, clojureSource, "core.clj")
	n := assertNode(t, g, "build-query", graph.NodeFunction)
	if n.Exported {
		t.Error("build-query (defn-) should be Exported: false")
	}
}

// ─── ^:private metadata on defn ──────────────────────────────────────────────

func TestClojureParser_PrivateMetadata_NotExported(t *testing.T) {
	src := `(ns test.core)
(defn ^:private internal-helper [x] (* x 2))
`
	g := parseClojure(t, src, "test.clj")
	n := assertNode(t, g, "internal-helper", graph.NodeFunction)
	if n.Exported {
		t.Error("internal-helper (defn ^:private) should be Exported: false")
	}
}

// ─── defmacro ────────────────────────────────────────────────────────────────

func TestClojureParser_Defmacro_KindMacro(t *testing.T) {
	g := parseClojure(t, clojureSource, "core.clj")
	n := assertNode(t, g, "with-retry", graph.NodeFunction)
	if n.Metadata == nil || n.Metadata["kind"] != "macro" {
		t.Errorf("with-retry kind = %q, want 'macro'", n.Metadata["kind"])
	}
}

func TestClojureParser_Defmacro_Exported(t *testing.T) {
	g := parseClojure(t, clojureSource, "core.clj")
	n := assertNode(t, g, "with-retry", graph.NodeFunction)
	if !n.Exported {
		t.Error("with-retry (defmacro) should be Exported: true")
	}
}

// ─── defmulti ────────────────────────────────────────────────────────────────

func TestClojureParser_Defmulti_KindMultimethod(t *testing.T) {
	g := parseClojure(t, clojureSource, "core.clj")
	n := assertNode(t, g, "process-event", graph.NodeFunction)
	if n.Metadata == nil || n.Metadata["kind"] != "defmulti" {
		t.Errorf("process-event kind = %q, want 'defmulti'", n.Metadata["kind"])
	}
}

// ─── defmethod ───────────────────────────────────────────────────────────────

func TestClojureParser_Defmethod_KindMultimethod(t *testing.T) {
	g := parseClojure(t, clojureSource, "core.clj")
	// defmethod creates "multifnName.dispatchVal" node (colon stripped).
	n := assertNode(t, g, "process-event.create", graph.NodeFunction)
	if n.Metadata == nil || n.Metadata["kind"] != "defmethod" {
		t.Errorf("process-event.create kind = %q, want 'defmethod'", n.Metadata["kind"])
	}
}

// ─── defrecord ───────────────────────────────────────────────────────────────

func TestClojureParser_Defrecord_NodeStruct(t *testing.T) {
	g := parseClojure(t, clojureSource, "core.clj")
	n := assertNode(t, g, "DataPoint", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "record" {
		t.Errorf("DataPoint kind = %q, want 'record'", n.Metadata["kind"])
	}
}

func TestClojureParser_Defrecord_Exported(t *testing.T) {
	g := parseClojure(t, clojureSource, "core.clj")
	n := assertNode(t, g, "DataPoint", graph.NodeStruct)
	if !n.Exported {
		t.Error("DataPoint (defrecord) should be Exported: true")
	}
}

// ─── defprotocol ─────────────────────────────────────────────────────────────

func TestClojureParser_Defprotocol_NodeStruct(t *testing.T) {
	g := parseClojure(t, clojureSource, "core.clj")
	n := assertNode(t, g, "DataSource", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "protocol" {
		t.Errorf("DataSource kind = %q, want 'protocol'", n.Metadata["kind"])
	}
}

// ─── deftype ─────────────────────────────────────────────────────────────────

func TestClojureParser_Deftype_NodeStruct(t *testing.T) {
	g := parseClojure(t, clojureSource, "core.clj")
	n := assertNode(t, g, "FileDataSource", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "type" {
		t.Errorf("FileDataSource kind = %q, want 'type'", n.Metadata["kind"])
	}
}

// ─── ns declaration ──────────────────────────────────────────────────────────

func TestClojureParser_NS_NodePackage(t *testing.T) {
	g := parseClojure(t, clojureSource, "core.clj")
	nodes := g.FindByName("data-pipeline.core")
	if len(nodes) == 0 {
		t.Fatal("ns node 'data-pipeline.core' not found")
	}
	if nodes[0].Type != graph.NodePackage {
		t.Errorf("ns node type = %q, want NodePackage", nodes[0].Type)
	}
}

// ─── :require in ns form ─────────────────────────────────────────────────────

func TestClojureParser_RequireInNS_EdgeImports(t *testing.T) {
	g := parseClojure(t, clojureSource, "core.clj")
	fileNodes := g.FindByName("core.clj")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID

	wantImports := map[string]bool{
		"clojure.string":      false,
		"clojure.set":         false,
		"data-pipeline.utils": false,
	}
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			n := g.GetNode(e.To)
			if n != nil {
				if _, ok := wantImports[n.Name]; ok {
					wantImports[n.Name] = true
				}
			}
		}
	}
	for name, found := range wantImports {
		if !found {
			t.Errorf("missing IMPORTS edge for required namespace %q", name)
		}
	}
}

// ─── standalone require ──────────────────────────────────────────────────────

func TestClojureParser_StandaloneRequire_EdgeImports(t *testing.T) {
	src := `(ns myapp.core)
(require '[clojure.string :as str])
(require '[clojure.walk])
`
	g := parseClojure(t, src, "myapp.clj")
	fileNodes := g.FindByName("myapp.clj")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID

	found := map[string]bool{}
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			n := g.GetNode(e.To)
			if n != nil {
				found[n.Name] = true
			}
		}
	}
	for _, want := range []string{"clojure.string", "clojure.walk"} {
		if !found[want] {
			t.Errorf("missing IMPORTS edge for standalone require %q", want)
		}
	}
}

// ─── def (top-level var) ─────────────────────────────────────────────────────

func TestClojureParser_Def_NodeVariable(t *testing.T) {
	g := parseClojure(t, clojureSource, "core.clj")
	n := assertNode(t, g, "batch-size", graph.NodeVariable)
	if !n.Exported {
		t.Error("batch-size (def) should be Exported: true")
	}
}

func TestClojureParser_Def_PrivateNotExported(t *testing.T) {
	g := parseClojure(t, clojureSource, "core.clj")
	n := assertNode(t, g, "secret-key", graph.NodeVariable)
	if n.Exported {
		t.Error("secret-key (def ^:private) should be Exported: false")
	}
}

// ─── Docstring extraction ─────────────────────────────────────────────────────

func TestClojureParser_Defn_DocstringExtracted(t *testing.T) {
	src := `(ns myapp.core)
(defn fetch-batch
  "Fetches a batch of records from the source"
  [source limit offset]
  (take limit (fetch-data source {})))
`
	g := parseClojure(t, src, "myapp.clj")
	n := assertNode(t, g, "fetch-batch", graph.NodeFunction)
	if n.Metadata == nil {
		t.Fatal("fetch-batch should have metadata")
	}
	doc := n.Metadata["doc"]
	if doc == "" {
		t.Error("fetch-batch should have a docstring extracted")
	}
	if doc != "Fetches a batch of records from the source" {
		t.Errorf("fetch-batch doc = %q, want 'Fetches a batch of records from the source'", doc)
	}
}

// ─── Empty file ───────────────────────────────────────────────────────────────

func TestClojureParser_EmptyFile_NoCrash(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewClojureParser()
	if err := p.Parse(g, "/tmp/empty.clj", []byte("")); err != nil {
		t.Fatalf("Parse() on empty .clj returned error: %v", err)
	}
	// Must have at least a file node.
	nodes := g.FindByName("empty.clj")
	if len(nodes) == 0 {
		t.Error("Parse() should create a file node even for empty files")
	}
}

// ─── ClojureScript (.cljs) ────────────────────────────────────────────────────

func TestClojureParser_CLJS_FileWorks(t *testing.T) {
	g := parseClojure(t, clojureScriptSource, "core.cljs")
	assertFileNode(t, g, "core.cljs")
	assertNode(t, g, "render-component", graph.NodeFunction)
}

func TestClojureParser_CLJS_NSExtracted(t *testing.T) {
	g := parseClojure(t, clojureScriptSource, "core.cljs")
	nodes := g.FindByName("my-app.core")
	if len(nodes) == 0 {
		t.Fatal("ns node 'my-app.core' not found in .cljs file")
	}
	if nodes[0].Type != graph.NodePackage {
		t.Errorf("ns node type = %q, want NodePackage", nodes[0].Type)
	}
}

// ─── DEFINES edges ───────────────────────────────────────────────────────────

func TestClojureParser_DefinesEdge_FileToDefn(t *testing.T) {
	g := parseClojure(t, clojureSource, "core.clj")
	fileNodes := g.FindByName("core.clj")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	assertDefinesEdge(t, g, fileNodes[0].ID, "fetch-batch")
}

func TestClojureParser_DefinesEdge_FileToRecord(t *testing.T) {
	g := parseClojure(t, clojureSource, "core.clj")
	fileNodes := g.FindByName("core.clj")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	assertDefinesEdge(t, g, fileNodes[0].ID, "DataPoint")
}

// ─── IMPORTS edges ───────────────────────────────────────────────────────────

func TestClojureParser_ImportsEdge_FileToRequiredNS(t *testing.T) {
	g := parseClojure(t, clojureSource, "core.clj")
	fileNodes := g.FindByName("core.clj")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID
	found := false
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			n := g.GetNode(e.To)
			if n != nil && n.Name == "clojure.string" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("expected IMPORTS edge from file to clojure.string")
	}
}

// ─── Re-frame events/subscriptions and clojure.spec ──────────────────────────

func TestClojureParserReframeAndSpec(t *testing.T) {
	src := `
(ns app.events
  (:require [re-frame.core :as rf]
            [clojure.spec.alpha :as s]))

;; clojure.spec definitions
(s/def ::email string?)
(s/def ::user-id pos-int?)
(s/def ::user (s/keys :req [::email ::user-id]))

;; Re-frame events
(rf/reg-event-db
 ::initialize-db
 (fn [_ _] {:users [] :loading false}))

(rf/reg-event-fx
 ::load-users
 (fn [{:keys [db]} _]
   {:http-xhrio {:method :get
                 :uri "/api/users"}}))

;; Re-frame subscriptions
(rf/reg-sub
 ::users
 (fn [db _] (:users db)))

(rf/reg-sub
 ::loading?
 :-> :loading)

;; Re-frame effects
(rf/reg-fx
 :local-storage/set
 (fn [[key value]] (.setItem js/localStorage key value)))

;; Re-frame coeffects
(rf/reg-cofx
 :local-storage/get
 (fn [coeffects key] (assoc coeffects key (.getItem js/localStorage key))))

;; Regular function — should also be extracted
(defn fetch-user [id]
  (rf/dispatch [::load-user id]))
`
	g := parseClojure(t, src, "events.cljs")

	// s/def specs
	specTests := []string{"::email", "::user-id", "::user"}
	for _, key := range specTests {
		nodes := g.FindByName(key)
		if len(nodes) == 0 {
			t.Errorf("expected s/def spec %q not found", key)
			continue
		}
		n := nodes[0]
		if n.Type != graph.NodeVariable {
			t.Errorf("%q: expected NodeVariable, got %v", key, n.Type)
		}
		if n.Metadata["kind"] != "spec" {
			t.Errorf("%q: expected kind=spec, got %q", key, n.Metadata["kind"])
		}
	}

	// Re-frame events
	eventTests := []struct {
		key  string
		kind string
	}{
		{"::initialize-db", "re-frame-event"},
		{"::load-users", "re-frame-event"},
	}
	for _, tc := range eventTests {
		nodes := g.FindByName(tc.key)
		if len(nodes) == 0 {
			t.Errorf("expected Re-frame event %q not found", tc.key)
			continue
		}
		n := nodes[0]
		if n.Type != graph.NodeFunction {
			t.Errorf("%q: expected NodeFunction, got %v", tc.key, n.Type)
		}
		if n.Metadata["kind"] != tc.kind {
			t.Errorf("%q: expected kind=%s, got %q", tc.key, tc.kind, n.Metadata["kind"])
		}
	}

	// Re-frame subscriptions
	subTests := []string{"::users", "::loading?"}
	for _, key := range subTests {
		nodes := g.FindByName(key)
		if len(nodes) == 0 {
			t.Errorf("expected Re-frame subscription %q not found", key)
			continue
		}
		if nodes[0].Metadata["kind"] != "re-frame-sub" {
			t.Errorf("%q: expected kind=re-frame-sub, got %q", key, nodes[0].Metadata["kind"])
		}
	}

	// Re-frame fx/cofx
	fxTests := []string{":local-storage/set", ":local-storage/get"}
	for _, key := range fxTests {
		nodes := g.FindByName(key)
		if len(nodes) == 0 {
			t.Errorf("expected Re-frame fx/cofx %q not found", key)
			continue
		}
		if nodes[0].Metadata["kind"] != "re-frame-fx" {
			t.Errorf("%q: expected kind=re-frame-fx, got %q", key, nodes[0].Metadata["kind"])
		}
	}

	// Regular function still extracted
	if len(g.FindByName("fetch-user")) == 0 {
		t.Error("expected regular function 'fetch-user' not found")
	}
}

// ─── Realistic data pipeline ──────────────────────────────────────────────────

func TestClojureParser_DataPipeline_AllEntitiesPresent(t *testing.T) {
	src := `(ns pipeline.transform
  (:require [clojure.string :as str]
            [pipeline.schema :as schema]))

(def default-batch-size 500)

(defrecord TransformResult [status data errors])

(defprotocol Transformer
  "Transforms data between formats"
  (transform [this data opts])
  (validate [this data]))

(defmulti apply-rule :rule-type)

(defmethod apply-rule :filter
  [{:keys [predicate]} data]
  (filter predicate data))

(defmethod apply-rule :map
  [{:keys [f]} data]
  (map f data))

(defn- validate-schema
  "Internal schema validation"
  [schema data]
  (schema/check schema data))

(defn transform-batch
  "Transforms a batch of records applying all rules"
  [rules records]
  (reduce (fn [acc rule] (apply-rule rule acc)) records rules))

(defmacro with-transform-context
  "Executes body within a transform context"
  [ctx & body]
  ` + "`" + `(binding [*ctx* ~ctx] ~@body)` + "`" + `)
`
	g := parseClojure(t, src, "transform.clj")

	// All expected entities should be present.
	checks := []struct {
		name     string
		nodeType graph.NodeType
	}{
		{"default-batch-size", graph.NodeVariable},
		{"TransformResult", graph.NodeStruct},
		{"Transformer", graph.NodeStruct},
		{"apply-rule", graph.NodeFunction},
		{"apply-rule/:filter", graph.NodeFunction},
		{"apply-rule/:map", graph.NodeFunction},
		{"validate-schema", graph.NodeFunction},
		{"transform-batch", graph.NodeFunction},
		{"with-transform-context", graph.NodeFunction},
	}

	for _, tc := range checks {
		nodes := g.FindByName(tc.name)
		if len(nodes) == 0 {
			t.Errorf("expected node %q (type %q) not found", tc.name, tc.nodeType)
			continue
		}
		if nodes[0].Type != tc.nodeType {
			t.Errorf("%s: type = %q, want %q", tc.name, nodes[0].Type, tc.nodeType)
		}
	}

	// Exported rules.
	validateSchema := g.FindByName("validate-schema")
	if len(validateSchema) > 0 && validateSchema[0].Exported {
		t.Error("validate-schema (defn-) should be Exported: false")
	}
	transformBatch := g.FindByName("transform-batch")
	if len(transformBatch) > 0 && !transformBatch[0].Exported {
		t.Error("transform-batch (defn) should be Exported: true")
	}
}
