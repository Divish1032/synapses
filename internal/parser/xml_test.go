package parser_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ─── XML test helpers ───────────────────────────────────────────────────────

const pomXmlSource = `<?xml version="1.0" encoding="UTF-8"?>
<project>
  <modelVersion>4.0.0</modelVersion>
  <groupId>com.example</groupId>
  <artifactId>my-app</artifactId>
  <version>1.0.0</version>

  <dependencies>
    <dependency>
      <groupId>junit</groupId>
      <artifactId>junit</artifactId>
      <version>4.13.2</version>
    </dependency>
    <dependency>
      <groupId>com.google.guava</groupId>
      <artifactId>guava</artifactId>
      <version>31.1-jre</version>
    </dependency>
  </dependencies>

  <build>
    <plugins>
      <plugin>
        <groupId>org.apache.maven.plugins</groupId>
        <artifactId>maven-compiler-plugin</artifactId>
        <version>3.8.1</version>
      </plugin>
    </plugins>
  </build>
</project>
`

const pomWithModulesSource = `<?xml version="1.0" encoding="UTF-8"?>
<project>
  <groupId>com.example</groupId>
  <artifactId>parent-project</artifactId>
  <version>1.0.0</version>
  <packaging>pom</packaging>

  <modules>
    <module>module1</module>
    <module>module2</module>
    <module>common</module>
  </modules>
</project>
`

const androidManifestSource = `<?xml version="1.0" encoding="utf-8"?>
<manifest xmlns:android="http://schemas.android.com/apk/res/android"
    package="com.example.myapp">

    <uses-permission android:name="android.permission.INTERNET" />

    <application
        android:label="@string/app_name"
        android:icon="@mipmap/ic_launcher">

        <activity
            android:name=".MainActivity"
            android:label="@string/app_name">
            <intent-filter>
                <action android:name="android.intent.action.MAIN" />
                <category android:name="android.intent.category.LAUNCHER" />
            </intent-filter>
        </activity>

        <service
            android:name=".MyService"
            android:enabled="true">
        </service>

        <receiver
            android:name=".MyReceiver"
            android:enabled="true">
        </receiver>
    </application>
</manifest>
`

const springContextSource = `<?xml version="1.0" encoding="UTF-8"?>
<beans xmlns="http://www.springframework.org/schema/beans"
       xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
       xsi:schemaLocation="http://www.springframework.org/schema/beans
       http://www.springframework.org/schema/beans/spring-beans.xsd">

    <bean id="userService" class="com.example.UserService">
        <property name="userRepository" ref="userRepository"/>
    </bean>

    <bean id="userRepository" class="com.example.UserRepository">
        <constructor-arg name="datasource" ref="datasource"/>
    </bean>

    <bean id="datasource" class="javax.sql.DataSource">
    </bean>

    <import resource="classpath:other-beans.xml"/>
</beans>
`

const genericXmlSource = `<?xml version="1.0" encoding="UTF-8"?>
<root>
    <person id="p1">
        <name>John Doe</name>
        <age>30</age>
    </person>
    <person id="p2">
        <name>Jane Smith</name>
        <age>28</age>
    </person>
    <company name="TechCorp">
        <department>Engineering</department>
        <department>Sales</department>
    </company>
</root>
`

func parseXml(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewXMLParser()
	if err := p.Parse(g, "/tmp/test.xml", []byte(src)); err != nil {
		t.Fatalf("XMLParser.Parse() error: %v", err)
	}
	return g
}

func parseXmlWithFilename(t *testing.T, filename string, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewXMLParser()
	if err := p.Parse(g, "/tmp/"+filename, []byte(src)); err != nil {
		t.Fatalf("XMLParser.Parse() error: %v", err)
	}
	return g
}

// ─── Extensions ──────────────────────────────────────────────────────────────

func TestXMLParser_Extensions(t *testing.T) {
	exts := parser.NewXMLParser().Extensions()
	if len(exts) != 1 || exts[0] != ".xml" {
		t.Errorf("Extensions() = %v, want [.xml]", exts)
	}
}

// ─── File node ───────────────────────────────────────────────────────────────

func TestXMLParser_FileNode(t *testing.T) {
	g := parseXml(t, pomXmlSource)
	nodes := g.FindByName("test.xml")
	if len(nodes) == 0 {
		t.Fatal("file node test.xml not found")
	}
	if nodes[0].Type != graph.NodeFile {
		t.Errorf("file node type = %q, want NodeFile", nodes[0].Type)
	}
}

// ─── pom.xml parsing ──────────────────────────────────────────────────────────

func TestXMLParser_ParsePomBasic(t *testing.T) {
	g := parseXmlWithFilename(t, "pom.xml", pomXmlSource)

	// Check for project identity node
	projNodes := g.FindByName("my-app")
	if len(projNodes) == 0 {
		t.Fatal("expected project identity node")
	}
	projNode := projNodes[0]
	if projNode.Type != graph.NodeStruct {
		t.Errorf("project: type = %q, want NodeStruct", projNode.Type)
	}
	if projNode.Metadata["kind"] != "project" {
		t.Errorf("project: kind = %q, want project", projNode.Metadata["kind"])
	}
	if projNode.Metadata["group"] != "com.example" {
		t.Errorf("project: group = %q, want com.example", projNode.Metadata["group"])
	}
	if projNode.Metadata["artifact"] != "my-app" {
		t.Errorf("project: artifact = %q, want my-app", projNode.Metadata["artifact"])
	}
	if projNode.Metadata["version"] != "1.0.0" {
		t.Errorf("project: version = %q, want 1.0.0", projNode.Metadata["version"])
	}
}

func TestXMLParser_ParsePomDependencies(t *testing.T) {
	g := parseXmlWithFilename(t, "pom.xml", pomXmlSource)

	// Check for dependency nodes
	junitNodes := g.FindByName("junit")
	if len(junitNodes) == 0 {
		t.Fatal("expected junit dependency node")
	}
	junitNode := junitNodes[0]
	if junitNode.Type != graph.NodeVariable {
		t.Errorf("junit dependency: type = %q, want NodeVariable", junitNode.Type)
	}
	if junitNode.Metadata["kind"] != "dependency" {
		t.Errorf("junit dependency: kind = %q, want dependency", junitNode.Metadata["kind"])
	}

	// Check for another dependency
	guavaNodes := g.FindByName("guava")
	if len(guavaNodes) == 0 {
		t.Fatal("expected guava dependency node")
	}
}

func TestXMLParser_ParsePomPlugins(t *testing.T) {
	g := parseXmlWithFilename(t, "pom.xml", pomXmlSource)

	// Check for plugin node
	pluginNodes := g.FindByName("maven-compiler-plugin")
	if len(pluginNodes) == 0 {
		t.Fatal("expected maven-compiler-plugin node")
	}
	pluginNode := pluginNodes[0]
	if pluginNode.Type != graph.NodeVariable {
		t.Errorf("plugin: type = %q, want NodeVariable", pluginNode.Type)
	}
}

func TestXMLParser_ParsePomModules(t *testing.T) {
	g := parseXmlWithFilename(t, "pom.xml", pomWithModulesSource)

	// Check for module nodes
	module1Nodes := g.FindByName("module1")
	if len(module1Nodes) == 0 {
		t.Fatal("expected module1 node")
	}

	module2Nodes := g.FindByName("module2")
	if len(module2Nodes) == 0 {
		t.Fatal("expected module2 node")
	}

	commonNodes := g.FindByName("common")
	if len(commonNodes) == 0 {
		t.Fatal("expected common module node")
	}
}

// ─── AndroidManifest.xml parsing ──────────────────────────────────────────────

func TestXMLParser_ParseAndroidManifest(t *testing.T) {
	g := parseXmlWithFilename(t, "AndroidManifest.xml", androidManifestSource)

	// Check for activity node
	activityNodes := g.FindByName("MainActivity")
	if len(activityNodes) == 0 {
		t.Fatal("expected MainActivity activity node")
	}
	activityNode := activityNodes[0]
	if activityNode.Type != graph.NodeStruct {
		t.Errorf("MainActivity: type = %q, want NodeStruct", activityNode.Type)
	}
}

func TestXMLParser_ParseAndroidService(t *testing.T) {
	g := parseXmlWithFilename(t, "AndroidManifest.xml", androidManifestSource)

	// Check for service node
	serviceNodes := g.FindByName("MyService")
	if len(serviceNodes) == 0 {
		t.Fatal("expected MyService service node")
	}
	serviceNode := serviceNodes[0]
	if serviceNode.Type != graph.NodeStruct {
		t.Errorf("MyService: type = %q, want NodeStruct", serviceNode.Type)
	}
}

func TestXMLParser_ParseAndroidReceiver(t *testing.T) {
	g := parseXmlWithFilename(t, "AndroidManifest.xml", androidManifestSource)

	// Check for receiver node
	receiverNodes := g.FindByName("MyReceiver")
	if len(receiverNodes) == 0 {
		t.Fatal("expected MyReceiver receiver node")
	}
}

// ─── Spring context.xml parsing ──────────────────────────────────────────────

func TestXMLParser_ParseSpringContext(t *testing.T) {
	g := parseXmlWithFilename(t, "app-context.xml", springContextSource)

	// Check for bean nodes
	userServiceNodes := g.FindByName("userService")
	if len(userServiceNodes) == 0 {
		t.Fatal("expected userService bean node")
	}
	userServiceNode := userServiceNodes[0]
	if userServiceNode.Type != graph.NodeStruct {
		t.Errorf("userService: type = %q, want NodeStruct", userServiceNode.Type)
	}
}

func TestXMLParser_ParseSpringBeans(t *testing.T) {
	g := parseXmlWithFilename(t, "context.xml", springContextSource)

	// Check for multiple beans
	beanNames := []string{"userService", "userRepository", "datasource"}
	for _, name := range beanNames {
		nodes := g.FindByName(name)
		if len(nodes) == 0 {
			t.Errorf("expected %s bean node", name)
		}
	}
}

// ─── Generic XML parsing ──────────────────────────────────────────────────────

func TestXMLParser_ParseGenericXml(t *testing.T) {
	g := parseXml(t, genericXmlSource)

	// Check for elements with id attributes
	p1Nodes := g.FindByName("p1")
	if len(p1Nodes) == 0 {
		t.Fatal("expected element with id p1")
	}

	p2Nodes := g.FindByName("p2")
	if len(p2Nodes) == 0 {
		t.Fatal("expected element with id p2")
	}
}

func TestXMLParser_ParseGenericXmlNameAttribute(t *testing.T) {
	g := parseXml(t, genericXmlSource)

	// Check for elements with name attributes
	companyNodes := g.FindByName("TechCorp")
	if len(companyNodes) == 0 {
		t.Fatal("expected company with name TechCorp")
	}
}

// ─── Empty XML ────────────────────────────────────────────────────────────────

func TestXMLParser_EmptyXml(t *testing.T) {
	src := `<?xml version="1.0" encoding="UTF-8"?>
<root></root>
`
	g := parseXml(t, src)
	nodes := g.FindByName("test.xml")
	if len(nodes) == 0 {
		t.Fatal("file node should exist even for empty root")
	}
}

// ─── Malformed XML ────────────────────────────────────────────────────────────

func TestXMLParser_InvalidXml(t *testing.T) {
	src := `not valid xml at all`
	g := parseXml(t, src)
	// Should handle gracefully without crashing
	nodes := g.FindByName("test.xml")
	if len(nodes) == 0 {
		t.Fatal("file node should exist even for invalid XML")
	}
}

// ─── Multiple namespaces ──────────────────────────────────────────────────────

func TestXMLParser_WithNamespaces(t *testing.T) {
	src := `<?xml version="1.0" encoding="UTF-8"?>
<root xmlns:custom="http://example.com/custom">
    <custom:element name="test1"/>
    <custom:element name="test2"/>
</root>
`
	g := parseXml(t, src)
	nodes := g.FindByName("test.xml")
	if len(nodes) == 0 {
		t.Fatal("file node should exist with namespaces")
	}
}

// ─── Nested structures ────────────────────────────────────────────────────────

func TestXMLParser_DeepNesting(t *testing.T) {
	src := `<?xml version="1.0" encoding="UTF-8"?>
<root>
    <level1>
        <level2>
            <level3 id="deep">
                <level4>
                    <value>nested content</value>
                </level4>
            </level3>
        </level2>
    </level1>
</root>
`
	g := parseXml(t, src)
	// The generic XML parser may extract direct children with id attributes
	// Just verify the file was parsed without error
	fileNodes := g.FindByName("test.xml")
	if len(fileNodes) == 0 {
		t.Fatal("expected file node")
	}
}

// ─── Config XML parsing (Spring config variant) ──────────────────────────────

func TestXMLParser_ConfigXml(t *testing.T) {
	src := `<?xml version="1.0" encoding="UTF-8"?>
<configuration>
    <property name="env" value="production"/>
    <property name="timeout" value="30000"/>
</configuration>
`
	g := parseXmlWithFilename(t, "app-config.xml", src)
	// Just check the file was parsed without error
	if g == nil {
		t.Fatal("graph should be created")
	}
}

// ─── Large XML with many elements ──────────────────────────────────────────────

func TestXMLParser_ManyElements(t *testing.T) {
	src := `<?xml version="1.0" encoding="UTF-8"?>
<project>
    <groupId>com.example</groupId>
    <artifactId>multi-module</artifactId>
    <dependencies>
        <dependency><artifactId>lib1</artifactId></dependency>
        <dependency><artifactId>lib2</artifactId></dependency>
        <dependency><artifactId>lib3</artifactId></dependency>
        <dependency><artifactId>lib4</artifactId></dependency>
        <dependency><artifactId>lib5</artifactId></dependency>
    </dependencies>
</project>
`
	g := parseXmlWithFilename(t, "pom.xml", src)

	// Should extract all dependencies
	for i := 1; i <= 5; i++ {
		name := "lib" + string(rune('0'+i))
		nodes := g.FindByName(name)
		if len(nodes) == 0 {
			t.Errorf("expected %s dependency", name)
		}
	}
}
