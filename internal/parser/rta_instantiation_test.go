package parser_test

// Tests for RTA instantiation tracking in Java and TypeScript parsers.
// Verifies that object_creation_expression (Java) and new_expression (TypeScript)
// are recorded as instantiated types in the graph.

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// TestJavaInstantiatedTypes_BasicNew verifies that Java "new Foo(...)" expressions
// are recorded as instantiated types.
func TestJavaInstantiatedTypes_BasicNew(t *testing.T) {
	src := `
public class OrderController {
    private final OrderRepository repo = new OrderRepository();
    private final PaymentService payment;

    public OrderController() {
        this.payment = new PaymentService();
    }

    public void process() {
        Validator v = new Validator();
        v.validate();
    }
}
`
	g := graph.New("testrepo")
	p := parser.NewJavaParser()
	if err := p.Parse(g, "OrderController.java", []byte(src)); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	types := g.GetInstantiatedTypes()
	if types == nil {
		t.Fatal("GetInstantiatedTypes returned nil — no instantiation recorded")
	}

	for _, want := range []string{"OrderRepository", "PaymentService", "Validator"} {
		if !types[want] {
			t.Errorf("expected %q in instantiated types, got %v", want, types)
		}
	}
}

// TestJavaInstantiatedTypes_SkipsBuiltins verifies that Java stdlib types are
// not recorded as instantiated types (they don't correspond to graph nodes).
func TestJavaInstantiatedTypes_SkipsBuiltins(t *testing.T) {
	src := `
public class Main {
    public void run() {
        ArrayList<String> list = new ArrayList<>();
        HashMap<String, Integer> map = new HashMap<>();
        StringBuilder sb = new StringBuilder();
        UserService svc = new UserService();
    }
}
`
	g := graph.New("testrepo")
	p := parser.NewJavaParser()
	if err := p.Parse(g, "Main.java", []byte(src)); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	types := g.GetInstantiatedTypes()
	if types == nil {
		t.Fatal("GetInstantiatedTypes returned nil")
	}

	// UserService should be recorded.
	if !types["UserService"] {
		t.Error("expected UserService in instantiated types")
	}

	// Java builtins should NOT be recorded.
	for _, builtin := range []string{"ArrayList", "HashMap", "StringBuilder"} {
		if types[builtin] {
			t.Errorf("unexpected builtin %q in instantiated types", builtin)
		}
	}
}

// TestJavaInstantiatedTypes_NestedNew verifies that nested new expressions
// (constructors inside arguments) are also tracked.
func TestJavaInstantiatedTypes_NestedNew(t *testing.T) {
	src := `
public class App {
    public void start() {
        Server server = new Server(new Config(), new Logger());
    }
}
`
	g := graph.New("testrepo")
	p := parser.NewJavaParser()
	if err := p.Parse(g, "App.java", []byte(src)); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	types := g.GetInstantiatedTypes()
	for _, want := range []string{"Server", "Config", "Logger"} {
		if !types[want] {
			t.Errorf("expected %q in instantiated types", want)
		}
	}
}

// TestTSInstantiatedTypes_BasicNew verifies that TypeScript "new Foo(...)"
// expressions are recorded as instantiated types.
func TestTSInstantiatedTypes_BasicNew(t *testing.T) {
	src := `
class Application {
  private repo: UserRepository;
  private auth: AuthService;

  constructor() {
    this.repo = new UserRepository();
    this.auth = new AuthService(new TokenProvider());
  }
}
`
	g := graph.New("testrepo")
	p := parser.NewTypeScriptParser()
	if err := p.Parse(g, "app.ts", []byte(src)); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	types := g.GetInstantiatedTypes()
	if types == nil {
		t.Fatal("GetInstantiatedTypes returned nil")
	}

	for _, want := range []string{"UserRepository", "AuthService", "TokenProvider"} {
		if !types[want] {
			t.Errorf("expected %q in instantiated types, got %v", want, types)
		}
	}
}

// TestTSInstantiatedTypes_MemberExpression verifies that qualified new expressions
// (new ns.Foo()) record just the property name ("Foo"), not the full "ns.Foo".
func TestTSInstantiatedTypes_MemberExpression(t *testing.T) {
	src := `
import * as db from './db';
class Service {
  setup() {
    const conn = new db.Connection();
    const local = new LocalCache();
  }
}
`
	g := graph.New("testrepo")
	p := parser.NewTypeScriptParser()
	if err := p.Parse(g, "service.ts", []byte(src)); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	types := g.GetInstantiatedTypes()
	if !types["LocalCache"] {
		t.Error("expected LocalCache in instantiated types")
	}
	// Qualified new: either "Connection" (property) or nothing — but NOT "db.Connection".
	if types["db.Connection"] {
		t.Error("unexpected qualified name 'db.Connection' — should use property part only")
	}
}

// TestTSInstantiatedTypes_SkipsBuiltins verifies that TypeScript builtins
// (Map, Set, Promise, Error) are not recorded.
func TestTSInstantiatedTypes_SkipsBuiltins(t *testing.T) {
	src := `
class Handler {
  handle() {
    const m = new Map<string, number>();
    const s = new Set<string>();
    const e = new Error("oops");
    const svc = new OrderService();
  }
}
`
	g := graph.New("testrepo")
	p := parser.NewTypeScriptParser()
	if err := p.Parse(g, "handler.ts", []byte(src)); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	types := g.GetInstantiatedTypes()
	if !types["OrderService"] {
		t.Error("expected OrderService in instantiated types")
	}
	for _, builtin := range []string{"Map", "Set", "Error"} {
		if types[builtin] {
			t.Errorf("unexpected builtin %q in instantiated types", builtin)
		}
	}
}

// --- Java DI annotation tests ---

func TestJavaInstantiatedTypes_SpringServiceAnnotation(t *testing.T) {
	src := `
@Service
public class UserService {
    public void doWork() {}
}
`
	g := graph.New("testrepo")
	p := parser.NewJavaParser()
	if err := p.Parse(g, "UserService.java", []byte(src)); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	types := g.GetInstantiatedTypes()
	if !types["UserService"] {
		t.Errorf("expected UserService in instantiated types (via @Service), got %v", types)
	}
}

func TestJavaInstantiatedTypes_SpringComponentAnnotation(t *testing.T) {
	src := `
@Component
public class EventListener {
    public void onEvent() {}
}
`
	g := graph.New("testrepo")
	p := parser.NewJavaParser()
	if err := p.Parse(g, "EventListener.java", []byte(src)); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	types := g.GetInstantiatedTypes()
	if !types["EventListener"] {
		t.Errorf("expected EventListener in instantiated types (via @Component), got %v", types)
	}
}

func TestJavaInstantiatedTypes_SpringBeanMethod(t *testing.T) {
	src := `
@Configuration
public class AppConfig {
    @Bean
    public DataSource dataSource() {
        return new HikariDataSource();
    }
    @Bean
    public CacheManager cacheManager() {
        return new RedisCacheManager();
    }
}
`
	g := graph.New("testrepo")
	p := parser.NewJavaParser()
	if err := p.Parse(g, "AppConfig.java", []byte(src)); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	types := g.GetInstantiatedTypes()
	// AppConfig itself is @Configuration → instantiated
	if !types["AppConfig"] {
		t.Errorf("expected AppConfig in instantiated types (via @Configuration), got %v", types)
	}
	// @Bean return types should also be instantiated
	for _, want := range []string{"DataSource", "CacheManager"} {
		if !types[want] {
			t.Errorf("expected %q in instantiated types (via @Bean return type), got %v", want, types)
		}
	}
}

func TestJavaInstantiatedTypes_StaticFactory_SameClass(t *testing.T) {
	src := `
public class ConnectionPool {
    public static ConnectionPool getInstance() { return null; }
    public static ConnectionPool create(String url) { return null; }
}
`
	g := graph.New("testrepo")
	p := parser.NewJavaParser()
	if err := p.Parse(g, "ConnectionPool.java", []byte(src)); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	types := g.GetInstantiatedTypes()
	if !types["ConnectionPool"] {
		t.Errorf("expected ConnectionPool in instantiated types (via static factory), got %v", types)
	}
}

func TestJavaInstantiatedTypes_StaticFactory_DifferentReturn(t *testing.T) {
	src := `
public class Factory {
    public static String create() { return ""; }
    public static int count() { return 0; }
}
`
	g := graph.New("testrepo")
	p := parser.NewJavaParser()
	if err := p.Parse(g, "Factory.java", []byte(src)); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	types := g.GetInstantiatedTypes()
	// String is a builtin, Factory is not recorded (return type != enclosing class)
	if types["Factory"] {
		t.Errorf("unexpected Factory in instantiated types — static method returns String, not Factory")
	}
	if types["String"] {
		t.Errorf("unexpected String (builtin) in instantiated types")
	}
}

func TestJavaInstantiatedTypes_EnumConstants(t *testing.T) {
	src := `
public enum Status {
    ACTIVE, INACTIVE, PENDING
}
`
	g := graph.New("testrepo")
	p := parser.NewJavaParser()
	if err := p.Parse(g, "Status.java", []byte(src)); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	types := g.GetInstantiatedTypes()
	if !types["Status"] {
		t.Errorf("expected Status in instantiated types (enum with constants), got %v", types)
	}
}

func TestJavaInstantiatedTypes_EnumEmpty(t *testing.T) {
	src := `
public enum Empty {
}
`
	g := graph.New("testrepo")
	p := parser.NewJavaParser()
	if err := p.Parse(g, "Empty.java", []byte(src)); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	types := g.GetInstantiatedTypes()
	if types["Empty"] {
		t.Errorf("unexpected Empty in instantiated types — enum has no constants")
	}
}

func TestJavaInstantiatedTypes_MultipleAnnotations(t *testing.T) {
	src := `
@Repository
@Transactional
public class UserRepo {
    public void save() {}
}
`
	g := graph.New("testrepo")
	p := parser.NewJavaParser()
	if err := p.Parse(g, "UserRepo.java", []byte(src)); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	types := g.GetInstantiatedTypes()
	if !types["UserRepo"] {
		t.Errorf("expected UserRepo in instantiated types (via @Repository), got %v", types)
	}
}

// --- TypeScript DI decorator tests ---

func TestTSInstantiatedTypes_InjectableDecorator(t *testing.T) {
	src := `
@Injectable()
export class AuthService {
  login() {}
}
`
	g := graph.New("testrepo")
	p := parser.NewTypeScriptParser()
	if err := p.Parse(g, "auth.service.ts", []byte(src)); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	types := g.GetInstantiatedTypes()
	if !types["AuthService"] {
		t.Errorf("expected AuthService in instantiated types (via @Injectable), got %v", types)
	}
}

func TestTSInstantiatedTypes_ComponentDecorator(t *testing.T) {
	src := `
@Component({
  selector: 'app-root',
  template: '<h1>Hello</h1>'
})
export class AppComponent {
  title = 'app';
}
`
	g := graph.New("testrepo")
	p := parser.NewTypeScriptParser()
	if err := p.Parse(g, "app.component.ts", []byte(src)); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	types := g.GetInstantiatedTypes()
	if !types["AppComponent"] {
		t.Errorf("expected AppComponent in instantiated types (via @Component), got %v", types)
	}
}

func TestTSInstantiatedTypes_StaticFactory_SameClass(t *testing.T) {
	src := `
class DatabaseConnection {
  static getInstance(): DatabaseConnection {
    return new DatabaseConnection();
  }
}
`
	g := graph.New("testrepo")
	p := parser.NewTypeScriptParser()
	if err := p.Parse(g, "db.ts", []byte(src)); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	types := g.GetInstantiatedTypes()
	if !types["DatabaseConnection"] {
		t.Errorf("expected DatabaseConnection in instantiated types (via static factory), got %v", types)
	}
}

func TestTSInstantiatedTypes_StaticFactory_DifferentReturn(t *testing.T) {
	src := `
class Builder {
  static build(): string {
    return '';
  }
}
`
	g := graph.New("testrepo")
	p := parser.NewTypeScriptParser()
	if err := p.Parse(g, "builder.ts", []byte(src)); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	types := g.GetInstantiatedTypes()
	if types["Builder"] {
		t.Errorf("unexpected Builder in instantiated types — static method returns string, not Builder")
	}
}
