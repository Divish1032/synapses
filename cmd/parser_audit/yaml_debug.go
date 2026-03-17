package main

import (
	"fmt"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

func yamlDebug() {
	src := []byte(`
- name: Install packages
  apt:
    name: "{{ item }}"
    state: present
  with_items:
    - git
    - curl

- name: Start nginx
  service:
    name: nginx
    state: started
`)
	g := graph.New("yaml-debug")
	parser.NewYAMLParser().Parse(g, "/tmp/playbook.yml", src)
	fmt.Println("YAML Ansible debug:")
	for _, n := range g.AllNodes() {
		fmt.Printf("  [%-15s] %-40s meta=%v\n", n.Type, n.Name, n.Metadata)
	}
	nodeID := g.MakeNodeID("/tmp/playbook.yml", "task:Install packages")
	fmt.Printf("  task:Install packages node = %v\n", g.GetNode(nodeID))
}
