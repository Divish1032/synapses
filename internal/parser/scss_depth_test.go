package parser

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSCSSParser_LocalVarsNotCaptured(t *testing.T) {
	src := []byte(`
$primary: #333;
$font-size: 16px;

@mixin button($color) {
  $hover: darken($color, 10%);
  $border: 1px solid $hover;
  background: $hover;
}

@function rem($px) {
  $result: $px / 16;
  @return #{$result}rem;
}
`)
	g := graph.New("testrepo")
	p := NewSCSSParser()
	err := p.Parse(g, "test.scss", src)
	require.NoError(t, err)

	vars := g.FindByType(graph.NodeVariable)
	names := make([]string, 0, len(vars))
	for _, v := range vars {
		names = append(names, v.Name)
	}

	assert.Contains(t, names, "$primary", "top-level $primary should be captured")
	assert.Contains(t, names, "$font-size", "top-level $font-size should be captured")
	assert.NotContains(t, names, "$hover", "local var inside @mixin must NOT be captured")
	assert.NotContains(t, names, "$border", "local var inside @mixin must NOT be captured")
	assert.NotContains(t, names, "$result", "local var inside @function must NOT be captured")
	assert.Len(t, vars, 2, "exactly 2 top-level variables expected")
}
