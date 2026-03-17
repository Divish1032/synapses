package main

import (
	"fmt"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

func svelte5Check() {
	src := []byte(`<script>
	import { page } from '$app/state';
	import ArticlePreview from './ArticlePreview.svelte';

	const { articles, user } = $props();
	let count = $state(0);
	const doubled = $derived(count * 2);
	const title = $bindable('default');

	async function loadMore() {
		count += 1;
	}
</script>

<div>
	{#each articles as article}
		<ArticlePreview {article} />
	{/each}
</div>
`)

	g := graph.New("svelte5-check")
	parser.NewSvelteParser().Parse(g, "ArticleList.svelte", src)

	fmt.Println("Svelte 5 extraction (props, state, derived, bindable, function):")
	for _, n := range g.AllNodes() {
		if n.Type == graph.NodeFile {
			continue
		}
		fmt.Printf("  [%-10s] %-25s exported=%-5v meta=%v\n",
			n.Type, n.Name, n.Exported, n.Metadata)
	}

	fmt.Println()
	// Also run against the actual realworld src
	r := auditParser("svelte5-realworld", "/tmp/parser-audit/svelte/src", parser.NewSvelteParser())
	fmt.Printf("Svelte realworld after fix: %d files → %d structural nodes (was 12)\n",
		r.filesScanned, r.counts["function"]+r.counts["variable"])
}
