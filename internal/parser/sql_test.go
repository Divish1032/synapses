package parser_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ─── SQL parser tests ────────────────────────────────────────────────────────

func parseSQL(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewSQLParser()
	if err := p.Parse(g, "/tmp/test.sql", []byte(src)); err != nil {
		t.Fatalf("SQLParser.Parse() error: %v", err)
	}
	return g
}

func TestSQLParser_Extensions(t *testing.T) {
	p := parser.NewSQLParser()
	exts := p.Extensions()
	if len(exts) != 1 || exts[0] != ".sql" {
		t.Errorf("Extensions() = %v, want [\".sql\"]", exts)
	}
}

func TestSQLParser_FileNode(t *testing.T) {
	g := parseSQL(t, "SELECT 1;")
	assertFileNode(t, g, "test.sql")
}

func TestSQLParser_EmptyFile(t *testing.T) {
	g := parseSQL(t, "")
	// Should produce at least a file node without crashing.
	if g.NodeCount() == 0 {
		t.Error("expected at least a file node for empty input")
	}
}

// ─── CREATE TABLE ────────────────────────────────────────────────────────────

func TestSQLParser_CreateTable(t *testing.T) {
	g := parseSQL(t, `CREATE TABLE users (
    id INT PRIMARY KEY,
    name VARCHAR(255),
    email VARCHAR(255) NOT NULL
);`)
	n := assertNode(t, g, "users", graph.NodeStruct)
	if n.Metadata["kind"] != "table" {
		t.Errorf("kind = %q, want \"table\"", n.Metadata["kind"])
	}
	if !n.Exported {
		t.Error("expected Exported = true")
	}
}

func TestSQLParser_CreateTableIfNotExists(t *testing.T) {
	g := parseSQL(t, `CREATE TABLE IF NOT EXISTS sessions (
    id UUID PRIMARY KEY,
    user_id INT REFERENCES users(id)
);`)
	assertNode(t, g, "sessions", graph.NodeStruct)
}

func TestSQLParser_CreateTableSchemaQualified(t *testing.T) {
	g := parseSQL(t, `CREATE TABLE myschema.users (
    id INT PRIMARY KEY,
    name VARCHAR(255)
);`)
	assertNode(t, g, "myschema.users", graph.NodeStruct)
}

func TestSQLParser_CreateTempTable(t *testing.T) {
	g := parseSQL(t, `CREATE TEMPORARY TABLE tmp_results (
    id INT,
    value TEXT
);`)
	n := assertNode(t, g, "tmp_results", graph.NodeStruct)
	if n.Metadata["kind"] != "table" {
		t.Errorf("kind = %q, want \"table\"", n.Metadata["kind"])
	}
}

// ─── CREATE VIEW ─────────────────────────────────────────────────────────────

func TestSQLParser_CreateView(t *testing.T) {
	g := parseSQL(t, `CREATE VIEW active_users AS
SELECT * FROM users WHERE active = true;`)
	n := assertNode(t, g, "active_users", graph.NodeStruct)
	if n.Metadata["kind"] != "view" {
		t.Errorf("kind = %q, want \"view\"", n.Metadata["kind"])
	}
}

func TestSQLParser_CreateOrReplaceView(t *testing.T) {
	g := parseSQL(t, `CREATE OR REPLACE VIEW user_summary AS
SELECT id, name, COUNT(*) as order_count
FROM users
JOIN orders ON users.id = orders.user_id
GROUP BY id, name;`)
	assertNode(t, g, "user_summary", graph.NodeStruct)
}

func TestSQLParser_CreateMaterializedView(t *testing.T) {
	g := parseSQL(t, `CREATE MATERIALIZED VIEW monthly_stats AS
SELECT date_trunc('month', created_at) as month, COUNT(*)
FROM events GROUP BY 1;`)
	n := assertNode(t, g, "monthly_stats", graph.NodeStruct)
	if n.Metadata["kind"] != "view" {
		t.Errorf("kind = %q, want \"view\"", n.Metadata["kind"])
	}
}

// ─── CREATE FUNCTION ─────────────────────────────────────────────────────────

func TestSQLParser_CreateFunction(t *testing.T) {
	g := parseSQL(t, `CREATE FUNCTION get_user_count() RETURNS INT AS $$
BEGIN
    RETURN (SELECT COUNT(*) FROM users);
END;
$$ LANGUAGE plpgsql;`)
	n := assertNode(t, g, "get_user_count", graph.NodeFunction)
	if n.Metadata["kind"] != "function" {
		t.Errorf("kind = %q, want \"function\"", n.Metadata["kind"])
	}
}

func TestSQLParser_CreateOrReplaceFunction(t *testing.T) {
	g := parseSQL(t, `CREATE OR REPLACE FUNCTION calculate_tax(amount DECIMAL)
RETURNS DECIMAL AS $$
BEGIN
    RETURN amount * 0.1;
END;
$$ LANGUAGE plpgsql;`)
	assertNode(t, g, "calculate_tax", graph.NodeFunction)
}

func TestSQLParser_CreateFunctionSchemaQualified(t *testing.T) {
	g := parseSQL(t, `CREATE FUNCTION billing.compute_total(order_id INT)
RETURNS DECIMAL AS $$
BEGIN
    RETURN 0;
END;
$$ LANGUAGE plpgsql;`)
	assertNode(t, g, "billing.compute_total", graph.NodeFunction)
}

// ─── CREATE PROCEDURE ────────────────────────────────────────────────────────

func TestSQLParser_CreateProcedure(t *testing.T) {
	g := parseSQL(t, `CREATE PROCEDURE cleanup_old_sessions()
LANGUAGE SQL
AS $$
    DELETE FROM sessions WHERE expires_at < NOW();
$$;`)
	n := assertNode(t, g, "cleanup_old_sessions", graph.NodeFunction)
	if n.Metadata["kind"] != "procedure" {
		t.Errorf("kind = %q, want \"procedure\"", n.Metadata["kind"])
	}
}

func TestSQLParser_CreateOrReplaceProcedure(t *testing.T) {
	g := parseSQL(t, `CREATE OR REPLACE PROCEDURE archive_orders(cutoff DATE)
LANGUAGE plpgsql
AS $$
BEGIN
    INSERT INTO archived_orders SELECT * FROM orders WHERE created_at < cutoff;
    DELETE FROM orders WHERE created_at < cutoff;
END;
$$;`)
	assertNode(t, g, "archive_orders", graph.NodeFunction)
}

// ─── CREATE TRIGGER ──────────────────────────────────────────────────────────

func TestSQLParser_CreateTrigger(t *testing.T) {
	g := parseSQL(t, `CREATE TRIGGER update_timestamp
BEFORE UPDATE ON users
FOR EACH ROW
EXECUTE FUNCTION set_updated_at();`)
	n := assertNode(t, g, "update_timestamp", graph.NodeFunction)
	if n.Metadata["kind"] != "trigger" {
		t.Errorf("kind = %q, want \"trigger\"", n.Metadata["kind"])
	}
}

func TestSQLParser_CreateOrReplaceTrigger(t *testing.T) {
	g := parseSQL(t, `CREATE OR REPLACE TRIGGER audit_log_trigger
AFTER INSERT OR UPDATE OR DELETE ON important_table
FOR EACH ROW
EXECUTE FUNCTION write_audit_log();`)
	assertNode(t, g, "audit_log_trigger", graph.NodeFunction)
}

// ─── CREATE INDEX ────────────────────────────────────────────────────────────

func TestSQLParser_CreateIndex(t *testing.T) {
	g := parseSQL(t, `CREATE INDEX idx_users_email ON users(email);`)
	n := assertNode(t, g, "idx_users_email", graph.NodeVariable)
	if n.Metadata["kind"] != "index" {
		t.Errorf("kind = %q, want \"index\"", n.Metadata["kind"])
	}
}

func TestSQLParser_CreateUniqueIndex(t *testing.T) {
	g := parseSQL(t, `CREATE UNIQUE INDEX idx_users_unique_email ON users(email);`)
	assertNode(t, g, "idx_users_unique_email", graph.NodeVariable)
}

// ─── Doc comments ────────────────────────────────────────────────────────────

func TestSQLParser_DocComment(t *testing.T) {
	g := parseSQL(t, `-- User accounts table
-- Stores core user profile data
CREATE TABLE users (
    id INT PRIMARY KEY,
    name VARCHAR(255)
);`)
	n := assertNode(t, g, "users", graph.NodeStruct)
	if n.Metadata["doc"] == "" {
		t.Error("expected doc comment to be extracted")
	}
}

// ─── Multiple statements ────────────────────────────────────────────────────

func TestSQLParser_MultipleStatements(t *testing.T) {
	src := `CREATE TABLE orders (
    id INT PRIMARY KEY,
    user_id INT,
    total DECIMAL(10,2)
);

CREATE INDEX idx_orders_user ON orders(user_id);

CREATE VIEW order_totals AS
SELECT user_id, SUM(total) as total_spent
FROM orders GROUP BY user_id;`
	g := parseSQL(t, src)

	assertNode(t, g, "orders", graph.NodeStruct)
	assertNode(t, g, "idx_orders_user", graph.NodeVariable)
	assertNode(t, g, "order_totals", graph.NodeStruct)
}

func TestSQLParser_MultipleStatements_WithFunction(t *testing.T) {
	g := parseSQL(t, `CREATE TABLE items (id INT PRIMARY KEY);

CREATE FUNCTION get_item(iid INT) RETURNS TEXT AS $$
BEGIN
    RETURN (SELECT name FROM items WHERE id = iid);
END;
$$ LANGUAGE plpgsql;`)

	assertNode(t, g, "items", graph.NodeStruct)
	assertNode(t, g, "get_item", graph.NodeFunction)
}

// ─── Case insensitivity ─────────────────────────────────────────────────────

func TestSQLParser_CaseInsensitiveKeywords(t *testing.T) {
	g := parseSQL(t, `create table Products (
    id int primary key,
    name varchar(100)
);`)
	assertNode(t, g, "Products", graph.NodeStruct)
}

func TestSQLParser_MixedCaseKeywords(t *testing.T) {
	g := parseSQL(t, `Create Table Categories (
    id INT PRIMARY KEY,
    name VARCHAR(100)
);`)
	assertNode(t, g, "Categories", graph.NodeStruct)
}

// ─── DEFINES edges ──────────────────────────────────────────────────────────

func TestSQLParser_DefinesEdge(t *testing.T) {
	g := parseSQL(t, `CREATE TABLE widgets (id INT PRIMARY KEY);`)
	fileNodeID := g.MakeNodeID("/tmp/test.sql", "/tmp/test.sql")
	assertDefinesEdge(t, g, fileNodeID, "widgets")
}

// ─── Quoted identifiers ─────────────────────────────────────────────────────

func TestSQLParser_BacktickQuotedName(t *testing.T) {
	src := "CREATE TABLE `user-profiles` (id INT PRIMARY KEY);"
	g := parseSQL(t, src)
	assertNode(t, g, "user-profiles", graph.NodeStruct)
}

func TestSQLParser_DoubleQuotedName(t *testing.T) {
	g := parseSQL(t, `CREATE TABLE "Order Items" (id INT PRIMARY KEY);`)
	assertNode(t, g, "Order Items", graph.NodeStruct)
}

// ─── No crash on degenerate input ───────────────────────────────────────────

func TestSQLParser_NoCrash_GarbageInput(t *testing.T) {
	p := parser.NewSQLParser()
	assertNoCrash(t, p, ".sql", "not valid sql at all }{}{")
}

func TestSQLParser_NoCrash_OnlyComments(t *testing.T) {
	p := parser.NewSQLParser()
	assertNoCrash(t, p, ".sql", "-- just a comment\n-- another comment\n")
}

func TestSQLParser_NoCrash_IncompleteCreate(t *testing.T) {
	p := parser.NewSQLParser()
	assertNoCrash(t, p, ".sql", "CREATE TABLE")
}
