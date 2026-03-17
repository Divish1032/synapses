package main

import (
	"fmt"
	"os"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

func cssCheck() {
	css := `:root {
  --color-primary: #007bff;
  --spacing-md: 1rem;
  --font-size-base: 16px;
}

.button {
  background: var(--color-primary);
}

@keyframes fade-in {
  from { opacity: 0; }
  to   { opacity: 1; }
}

@font-face {
  font-family: "Inter";
  src: url("inter.woff2");
}
`
	g := graph.New("css-check")
	if err := os.WriteFile("/tmp/_audit_test.css", []byte(css), 0644); err != nil {
		fmt.Println("write err:", err)
		return
	}
	src, _ := os.ReadFile("/tmp/_audit_test.css")
	parser.NewCSSParser().Parse(g, "/tmp/_audit_test.css", src)

	fmt.Println("CSS extraction on file with custom props, keyframes, font-face:")
	for _, n := range g.AllNodes() {
		if n.Type != graph.NodeFile {
			fmt.Printf("  [%-15s] %-30s meta=%v\n", n.Type, n.Name, n.Metadata)
		}
	}
}
