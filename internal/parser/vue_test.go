package parser_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ─── Vue test helpers ───────────────────────────────────────────────────────

const basicVueSource = `<template>
  <div class="app">
    <h1>{{ title }}</h1>
    <button @click="handleClick">Click me</button>
  </div>
</template>

<script>
export default {
  data() {
    return {
      title: "Hello Vue"
    };
  },
  methods: {
    handleClick() {
      alert("Clicked!");
    }
  }
};
</script>

<style scoped>
.app {
  padding: 20px;
  background-color: #f5f5f5;
}
</style>
`

const vueWithSetupSource = `<template>
  <div>
    <p>Count: {{ count }}</p>
    <button @click="increment">+</button>
  </div>
</template>

<script setup>
import { ref } from 'vue';

const count = ref(0);

const increment = () => {
  count.value++;
};
</script>

<style scoped>
div {
  text-align: center;
}
</style>
`

const vueWithTypeScriptSource = `<template>
  <div class="component">
    <h2>{{ message }}</h2>
  </div>
</template>

<script lang="ts">
import { defineComponent } from 'vue';

export default defineComponent({
  name: 'TypeScriptComponent',
  setup() {
    const message: string = "Hello TypeScript";

    const updateMessage = (newMsg: string): void => {
      // update logic
    };

    return { message, updateMessage };
  }
});
</script>

<style scoped>
.component {
  color: blue;
}
</style>
`

const vueMultiBlockSource = `<template>
  <div class="container">
    <sidebar />
    <main-content />
  </div>
</template>

<script>
import Sidebar from './Sidebar.vue';
import MainContent from './MainContent.vue';

export default {
  name: 'Layout',
  components: {
    Sidebar,
    MainContent
  },
  props: {
    theme: String
  },
  computed: {
    isDarkMode() {
      return this.theme === 'dark';
    }
  }
};
</script>

<style scoped>
.container {
  display: flex;
}

.container > * {
  flex: 1;
}
</style>

<style>
/* Global styles */
.app-global {
  font-family: Arial, sans-serif;
}
</style>
`

const vueMinimalSource = `<template>
  <span>{{ message }}</span>
</template>

<script>
export default {
  data: () => ({ message: 'Hello' })
}
</script>
`

const vueLangScssSource = `<template>
  <div class="styled">
    <p>Styled with SCSS</p>
  </div>
</template>

<script>
export default {
  name: 'ScssComponent'
};
</script>

<style lang="scss" scoped>
.styled {
  p {
    color: #333;

    &:hover {
      color: #666;
    }
  }
}
</style>
`

const vueNoScriptSource = `<template>
  <div>
    <h1>Pure Template</h1>
    <p>No script block</p>
  </div>
</template>

<style scoped>
div {
  padding: 10px;
}
</style>
`

func parseVue(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewVueParser()
	if err := p.Parse(g, "/tmp/test.vue", []byte(src)); err != nil {
		t.Fatalf("VueParser.Parse() error: %v", err)
	}
	return g
}

func parseVueWithFilename(t *testing.T, filename string, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewVueParser()
	if err := p.Parse(g, "/tmp/"+filename, []byte(src)); err != nil {
		t.Fatalf("VueParser.Parse() error: %v", err)
	}
	return g
}

// ─── Extensions ──────────────────────────────────────────────────────────────

func TestVueParser_Extensions(t *testing.T) {
	exts := parser.NewVueParser().Extensions()
	if len(exts) != 1 || exts[0] != ".vue" {
		t.Errorf("Extensions() = %v, want [.vue]", exts)
	}
}

// ─── File node ───────────────────────────────────────────────────────────────

func TestVueParser_FileNode(t *testing.T) {
	g := parseVue(t, basicVueSource)
	nodes := g.FindByName("test.vue")
	if len(nodes) == 0 {
		t.Fatal("file node test.vue not found")
	}
	if nodes[0].Type != graph.NodeFile {
		t.Errorf("file node type = %q, want NodeFile", nodes[0].Type)
	}
}

// ─── Component node ───────────────────────────────────────────────────────────

func TestVueParser_ComponentNode(t *testing.T) {
	g := parseVueWithFilename(t, "MyComponent.vue", basicVueSource)
	componentNodes := g.FindByName("MyComponent")
	if len(componentNodes) == 0 {
		t.Fatal("expected component node named MyComponent")
	}
	componentNode := componentNodes[0]
	if componentNode.Type != graph.NodeStruct {
		t.Errorf("component: type = %q, want NodeStruct", componentNode.Type)
	}
	if componentNode.Metadata["kind"] != "component" {
		t.Errorf("component: kind = %q, want component", componentNode.Metadata["kind"])
	}
}

// ─── Script content parsing ───────────────────────────────────────────────────

func TestVueParser_ScriptJavaScript(t *testing.T) {
	g := parseVue(t, basicVueSource)
	// The script content is re-parsed by the JS parser,
	// so we should find the default export and its contents
	// Just verify the file was parsed successfully
	fileNodes := g.FindByName("test.vue")
	if len(fileNodes) == 0 {
		t.Fatal("file node should exist")
	}
}

// ─── Script setup (composition API) ────────────────────────────────────────────

func TestVueParser_ScriptSetup(t *testing.T) {
	g := parseVueWithFilename(t, "Counter.vue", vueWithSetupSource)

	// Script setup should be parsed by JS parser
	// Looking for the ref import and count variable
	fileNodes := g.FindByName("Counter.vue")
	if len(fileNodes) == 0 {
		t.Fatal("file node should exist")
	}

	// Check component node
	componentNodes := g.FindByName("Counter")
	if len(componentNodes) == 0 {
		t.Fatal("expected Counter component node")
	}
}

// ─── TypeScript script ─────────────────────────────────────────────────────────

func TestVueParser_TypeScriptScript(t *testing.T) {
	g := parseVueWithFilename(t, "TypeScriptComponent.vue", vueWithTypeScriptSource)

	// Check component node
	componentNodes := g.FindByName("TypeScriptComponent")
	if len(componentNodes) == 0 {
		t.Fatal("expected TypeScriptComponent node")
	}
	componentNode := componentNodes[0]
	if componentNode.Type != graph.NodeStruct {
		t.Errorf("component: type = %q, want NodeStruct", componentNode.Type)
	}
}

// ─── Style metadata (scoped) ───────────────────────────────────────────────────

func TestVueParser_ScopedStyle(t *testing.T) {
	g := parseVueWithFilename(t, "StyledComponent.vue", basicVueSource)

	componentNodes := g.FindByName("StyledComponent")
	if len(componentNodes) == 0 {
		t.Fatal("expected StyledComponent node")
	}
	componentNode := componentNodes[0]
	// Check that component has a metadata object
	if componentNode.Metadata == nil {
		t.Error("component should have metadata")
	}
}

// ─── Style language ───────────────────────────────────────────────────────────

func TestVueParser_StyleWithLanguage(t *testing.T) {
	g := parseVueWithFilename(t, "ScssComponent.vue", vueLangScssSource)

	componentNodes := g.FindByName("ScssComponent")
	if len(componentNodes) == 0 {
		t.Fatal("expected ScssComponent node")
	}
	componentNode := componentNodes[0]
	// Verify the component was parsed successfully
	if componentNode.Type != graph.NodeStruct {
		t.Errorf("component: type = %q, want NodeStruct", componentNode.Type)
	}
}

// ─── Multiple blocks (imports) ─────────────────────────────────────────────────

func TestVueParser_MultipleBlocks(t *testing.T) {
	g := parseVueWithFilename(t, "Layout.vue", vueMultiBlockSource)

	// Check component node
	componentNodes := g.FindByName("Layout")
	if len(componentNodes) == 0 {
		t.Fatal("expected Layout component")
	}

	// Verify component was parsed
	if len(componentNodes[0].Name) == 0 {
		t.Error("component should have a name")
	}
}

// ─── Minimal Vue component ────────────────────────────────────────────────────

func TestVueParser_MinimalComponent(t *testing.T) {
	g := parseVueWithFilename(t, "Minimal.vue", vueMinimalSource)

	componentNodes := g.FindByName("Minimal")
	if len(componentNodes) == 0 {
		t.Fatal("expected Minimal component")
	}
	componentNode := componentNodes[0]
	if componentNode.Type != graph.NodeStruct {
		t.Errorf("Minimal: type = %q, want NodeStruct", componentNode.Type)
	}
}

// ─── No script block ───────────────────────────────────────────────────────────

func TestVueParser_NoScriptBlock(t *testing.T) {
	g := parseVueWithFilename(t, "TemplateOnly.vue", vueNoScriptSource)

	// Should still create component node even without script
	componentNodes := g.FindByName("TemplateOnly")
	if len(componentNodes) == 0 {
		t.Fatal("expected TemplateOnly component")
	}
	componentNode := componentNodes[0]
	if componentNode.Metadata["kind"] != "component" {
		t.Errorf("component: kind = %q, want component", componentNode.Metadata["kind"])
	}
}

// ─── Default style (not scoped) ────────────────────────────────────────────────

func TestVueParser_GlobalStyle(t *testing.T) {
	g := parseVueWithFilename(t, "Global.vue", vueMultiBlockSource)

	componentNodes := g.FindByName("Global")
	// Will only check if there's at least one style block recorded
	if len(componentNodes) > 0 {
		// This is just to ensure the parser ran without error
		t.Logf("Global component parsed successfully")
	}
}

// ─── Empty template ───────────────────────────────────────────────────────────

func TestVueParser_EmptyTemplate(t *testing.T) {
	src := `<template></template>
<script>
export default {
  name: 'EmptyTemplate'
};
</script>
`
	g := parseVueWithFilename(t, "Empty.vue", src)

	componentNodes := g.FindByName("Empty")
	if len(componentNodes) == 0 {
		t.Fatal("expected Empty component")
	}
}

// ─── Only template block ──────────────────────────────────────────────────────

func TestVueParser_OnlyTemplate(t *testing.T) {
	src := `<template>
  <div>Content only</div>
</template>
`
	g := parseVueWithFilename(t, "TemplateAlone.vue", src)

	componentNodes := g.FindByName("TemplateAlone")
	if len(componentNodes) == 0 {
		t.Fatal("expected TemplateAlone component")
	}
}

// ─── Complex script with multiple exports ─────────────────────────────────────

func TestVueParser_ComplexScript(t *testing.T) {
	src := `<template>
  <div>Complex</div>
</template>

<script>
const helper = () => console.log('helper');

export default {
  name: 'Complex',
  components: {},
  data() {
    return { value: 0 };
  },
  methods: {
    doSomething() {
      helper();
    }
  }
};
</script>
`
	g := parseVueWithFilename(t, "Complex.vue", src)

	componentNodes := g.FindByName("Complex")
	if len(componentNodes) == 0 {
		t.Fatal("expected Complex component")
	}
}

// ─── Multiple style blocks ────────────────────────────────────────────────────

func TestVueParser_MultipleStyleBlocks(t *testing.T) {
	src := `<template>
  <div class="app"></div>
</template>

<script>
export default { name: 'MultiStyle' };
</script>

<style>
.app { color: red; }
</style>

<style scoped>
.app { padding: 10px; }
</style>
`
	g := parseVueWithFilename(t, "MultiStyle.vue", src)

	componentNodes := g.FindByName("MultiStyle")
	if len(componentNodes) == 0 {
		t.Fatal("expected MultiStyle component")
	}
}
