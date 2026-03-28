package store

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

func TestNameToWords(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", ""},
		{"send_static_file", "send static file"},
		{"SendStaticFile", "send static file"},
		{"CarveEgoGraph", "carve ego graph"},
		{"getHTTPResponse", "get http response"},
		{"HTTP", "http"},
		{"HTMLParser", "html parser"},
		{"Context.JSON", "context json"},
		{"Flask.send_static_file", "flask send static file"},
		{"main", "main"},
		{"io", "io"},
		{"URL", "url"},
		{"parseURL", "parse url"},
		{"XMLHTTPRequest", "xmlhttp request"},  // all-caps run treated as one acronym
		{"XMLHttpRequest", "xml http request"}, // real-world mixed case
		{"get_http_response", "get http response"},
		{"MY_CONST_VALUE", "my const value"},
		{"router-group", "router group"},
	}
	for _, tt := range tests {
		got := nameToWords(tt.input)
		if got != tt.want {
			t.Errorf("nameToWords(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestNameOwnerAndWords(t *testing.T) {
	tests := []struct {
		input     string
		wantOwner string
		wantWords string
	}{
		{"Flask.send_static_file", "flask", "send static file"},
		{"send_static_file", "", "send static file"},
		{"Graph.CarveEgoGraph", "graph", "carve ego graph"},
		{"View.dispatch", "view", "dispatch"},
	}
	for _, tt := range tests {
		owner, words := nameOwnerAndWords(tt.input)
		if owner != tt.wantOwner || words != tt.wantWords {
			t.Errorf("nameOwnerAndWords(%q) = (%q, %q), want (%q, %q)",
				tt.input, owner, words, tt.wantOwner, tt.wantWords)
		}
	}
}

func TestExtractParams(t *testing.T) {
	tests := []struct {
		sig  string
		want string
	}{
		{"", ""},
		{"func send()", ""},
		{"func send(filename string, ctx context.Context)", "filename, ctx"},
		{"def send(self, filename, content_type=None)", "filename, content type"},
		{"(self, request, *args, **kwargs)", "request"},
		{"public void process(String inputData, int count)", "input data, count"},
		{"def foo(cls, x: int, y: str = 'hello')", "x, y"},
	}
	for _, tt := range tests {
		got := extractParams(tt.sig)
		if got != tt.want {
			t.Errorf("extractParams(%q) = %q, want %q", tt.sig, got, tt.want)
		}
	}
}

func TestFirstDocSentence(t *testing.T) {
	tests := []struct {
		doc  string
		want string
	}{
		{"", ""},
		{"Send a static file.", "Send a static file."},
		{"Send a static file. Uses the configured path.", "Send a static file."},
		{"Short doc no period", "Short doc no period"},
	}
	for _, tt := range tests {
		got := firstDocSentence(tt.doc)
		if got != tt.want {
			t.Errorf("firstDocSentence(%q) = %q, want %q", tt.doc, got, tt.want)
		}
	}
}

func TestExtractReturnHint(t *testing.T) {
	tests := []struct {
		sig  string
		want string
	}{
		{"", ""},
		{"def foo(x) -> str", "str"},
		{"def foo(x) -> None", ""},
		{"func foo(x int) (string, error)", "string"},
		{"func foo(x int) error", ""},
		{"func foo(x int)", ""},
	}
	for _, tt := range tests {
		got := extractReturnHint(tt.sig)
		if got != tt.want {
			t.Errorf("extractReturnHint(%q) = %q, want %q", tt.sig, got, tt.want)
		}
	}
}

func TestGenerateNLDescription(t *testing.T) {
	tests := []struct {
		name     string
		nodeType graph.NodeType
		sig      string
		doc      string
		callees  []string
		callers  []string
		want     string
	}{
		{
			name:     "Flask.send_static_file",
			nodeType: graph.NodeMethod,
			sig:      "def send_static_file(self, filename)",
			doc:      "Send a static file from the configured directory.",
			callees:  []string{"GetPath", "SendFile"},
			want:     "flask: send static file, given filename. Send a static file from the configured directory. involves get path, send file",
		},
		{
			name:     "CarveEgoGraph",
			nodeType: graph.NodeFunction,
			sig:      "func CarveEgoGraph(g *Graph, seed NodeID) *Graph",
			doc:      "",
			want:     "carve ego graph, given g, seed", // *Graph return skipped (pointer type)
		},
		{
			name:     "RouterGroup",
			nodeType: graph.NodeStruct,
			sig:      "",
			doc:      "A group of routes with common prefix and middleware.",
			want:     "router group. A group of routes with common prefix and middleware.",
		},
		{
			name:     "Handler",
			nodeType: graph.NodeInterface,
			sig:      "",
			doc:      "Serves HTTP requests.",
			want:     "handler: interface. Serves HTTP requests.",
		},
		{
			name:     "maxRetries",
			nodeType: graph.NodeVariable,
			sig:      "",
			doc:      "Maximum number of retries for failed requests.",
			want:     "max retries. Maximum number of retries for failed requests.",
		},
		{
			name:     "main",
			nodeType: graph.NodeFunction,
			sig:      "",
			doc:      "",
			want:     "main",
		},
		// Non-code types return empty
		{
			name:     "Getting Started",
			nodeType: graph.NodeSection,
			want:     "",
		},
	}
	for _, tt := range tests {
		got := GenerateNLDescription(tt.name, tt.nodeType, tt.sig, tt.doc, tt.callees, tt.callers)
		if got != tt.want {
			t.Errorf("GenerateNLDescription(%q, %q) =\n  %q\nwant:\n  %q",
				tt.name, tt.nodeType, got, tt.want)
		}
	}
}

func TestIsCodeNodeType(t *testing.T) {
	codeTypes := []graph.NodeType{
		graph.NodeFunction, graph.NodeMethod, graph.NodeStruct,
		graph.NodeInterface, graph.NodeVariable, graph.NodeRoute,
	}
	for _, nt := range codeTypes {
		if !IsCodeNodeType(nt) {
			t.Errorf("IsCodeNodeType(%q) = false, want true", nt)
		}
	}
	nonCodeTypes := []graph.NodeType{
		graph.NodeFile, graph.NodePackage, graph.NodeSection,
		graph.NodeConcept, graph.NodeEntity,
	}
	for _, nt := range nonCodeTypes {
		if IsCodeNodeType(nt) {
			t.Errorf("IsCodeNodeType(%q) = true, want false", nt)
		}
	}
}
