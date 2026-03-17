package parser

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/dockerfile"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// dockerfileStage tracks a build stage for COPY --from resolution.
type dockerfileStage struct {
	nodeID graph.NodeID
	name   string
}

// DockerfileParser parses Dockerfile source files.
// It extracts build stages (FROM), COPY --from edges, ARG/ENV declarations,
// EXPOSE ports, LABEL metadata, and base image imports.
type DockerfileParser struct {
	language *sitter.Language
}

// NewDockerfileParser creates a ready-to-use DockerfileParser.
func NewDockerfileParser() *DockerfileParser {
	return &DockerfileParser{language: dockerfile.GetLanguage()}
}

// Extensions returns the file extensions handled by this parser.
func (p *DockerfileParser) Extensions() []string {
	return []string{".dockerfile", ".containerfile"}
}

// Filenames returns the exact base filenames handled by this parser.
// These are files that have no extension (e.g. "Dockerfile") or use
// a dot-separated variant (e.g. "Dockerfile.dev"). The Walker checks
// this interface when no extension-based match is found.
func (p *DockerfileParser) Filenames() []string {
	return []string{"Dockerfile", "Dockerfile.dev", "Dockerfile.prod", "Dockerfile.test", "Dockerfile.local", "Containerfile"}
}

// FilenamePrefixes returns filename prefixes that this parser handles.
// Any file whose base name starts with "Dockerfile" or "Containerfile"
// (e.g. Dockerfile.staging, Dockerfile.ci, Containerfile.dev) is parsed.
func (p *DockerfileParser) FilenamePrefixes() []string {
	return []string{"Dockerfile", "Containerfile"}
}

// Parse extracts code entities from a single Dockerfile and merges them into
// the graph.
//
// Extracts:
//   - FROM stages  -> NodeStruct with kind="stage"
//   - Base image imports -> EdgeImports to NodePackage
//   - COPY --from edges -> EdgeCalls between stages
//   - ARG declarations  -> NodeVariable with kind="arg"
//   - ENV declarations  -> NodeVariable with kind="env"
//   - EXPOSE ports      -> metadata on file node
//   - LABEL metadata    -> metadata on file node
func (p *DockerfileParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	parser := sitter.NewParser()
	parser.SetLanguage(p.language)

	tree, _ := parser.ParseCtx(context.Background(), nil, src)
	if tree == nil {
		return nil
	}
	root := tree.RootNode()
	if root == nil {
		return nil
	}

	fileNodeID := g.MakeNodeID(filePath, filePath)
	fileMeta := make(map[string]string)
	g.AddNode(&graph.Node{
		ID:   fileNodeID,
		Type: graph.NodeFile,
		Name: filepath.Base(filePath),
		File: filePath,
		Line: 1,
	})

	lines := strings.Split(string(src), "\n")

	// Track stages by index and by alias for COPY --from resolution.
	var stages []dockerfileStage
	var currentStage graph.NodeID

	// Walk top-level instructions.
	for i := 0; i < int(root.ChildCount()); i++ {
		child := root.Child(i)
		if child == nil {
			continue
		}

		switch child.Type() {
		case "from_instruction":
			p.handleFrom(g, child, src, filePath, fileNodeID, lines, &stages, &currentStage)

		case "copy_instruction":
			p.handleCopy(g, child, src, filePath, stages, currentStage)

		case "arg_instruction":
			p.handleArg(g, child, src, filePath, fileNodeID, lines)

		case "env_instruction":
			p.handleEnv(g, child, src, filePath, fileNodeID, lines)

		case "expose_instruction":
			p.handleExpose(child, src, fileMeta)

		case "label_instruction":
			p.handleLabel(child, src, fileMeta)

		case "cmd_instruction":
			p.handleCmdOrEntrypoint(g, child, src, filePath, fileNodeID, "cmd")

		case "entrypoint_instruction":
			p.handleCmdOrEntrypoint(g, child, src, filePath, fileNodeID, "entrypoint")
		}
	}

	// Attach collected metadata to the file node.
	if len(fileMeta) > 0 {
		if node := g.GetNode(fileNodeID); node != nil {
			if node.Metadata == nil {
				node.Metadata = make(map[string]string)
			}
			for k, v := range fileMeta {
				node.Metadata[k] = v
			}
		}
	}

	return nil
}

// handleFrom processes a FROM instruction, creating a stage node and base image import.
func (p *DockerfileParser) handleFrom(
	g *graph.Graph,
	n *sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	lines []string,
	stages *[]dockerfileStage,
	currentStage *graph.NodeID,
) {
	startLine := int(n.StartPoint().Row) + 1
	doc := extractLineDoc(lines, startLine, "#")

	// Extract image spec and alias from the FROM instruction.
	var imageName, imageTag, alias string

	for j := 0; j < int(n.ChildCount()); j++ {
		child := n.Child(j)
		if child == nil {
			continue
		}
		switch child.Type() {
		case "image_spec":
			// image_spec contains image_name and optionally image_tag.
			if nameNode := firstChildOfType(child, "image_name"); nameNode != nil {
				imageName = childText(nameNode, src)
			}
			if tagNode := firstChildOfType(child, "image_tag"); tagNode != nil {
				imageTag = childText(tagNode, src)
			}
			// If we didn't find image_name as a child type, try the full text.
			if imageName == "" {
				imageName = strings.TrimSpace(childText(child, src))
			}
		case "image_alias":
			alias = strings.TrimSpace(childText(child, src))
		}
	}

	if imageName == "" {
		return
	}

	// Build the full image reference (name:tag).
	fullImage := imageName
	if imageTag != "" {
		// imageTag from the grammar may include the leading ":"
		tag := strings.TrimPrefix(imageTag, ":")
		if tag != "" {
			fullImage = imageName + ":" + tag
		}
	}

	// Determine stage name: use alias if present, otherwise use image name.
	stageName := alias
	if stageName == "" {
		// Use the image name (without tag) as stage name.
		stageName = imageName
	}

	// Create a stage node (NodeStruct with kind="stage").
	stageNodeID := g.MakeNodeID(filePath, stageName)
	meta := map[string]string{
		"kind":       "stage",
		"base_image": fullImage,
	}
	if doc != "" {
		meta["doc"] = doc
	}
	g.AddNode(&graph.Node{
		ID:       stageNodeID,
		Type:     graph.NodeStruct,
		Name:     stageName,
		File:     filePath,
		Line:     startLine,
		Exported: true,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: stageNodeID, Type: graph.EdgeDefines})

	// Create a base image import node and edge.
	importNodeID := g.MakeNodeID(fullImage, fullImage)
	g.AddNode(&graph.Node{
		ID:      importNodeID,
		Type:    graph.NodePackage,
		Name:    fullImage,
		Package: fullImage,
		File:    filePath,
		Line:    startLine,
	})
	g.AddEdge(&graph.Edge{From: stageNodeID, To: importNodeID, Type: graph.EdgeImports})

	// Track the stage for COPY --from resolution.
	*stages = append(*stages, dockerfileStage{nodeID: stageNodeID, name: stageName})
	*currentStage = stageNodeID
}

// handleCopy processes a COPY instruction, creating edges for --from references.
func (p *DockerfileParser) handleCopy(
	g *graph.Graph,
	n *sitter.Node,
	src []byte,
	filePath string,
	stages []dockerfileStage,
	currentStage graph.NodeID,
) {
	if currentStage == "" {
		return
	}

	// Look for --from=<stage> parameter.
	var fromStage string
	for j := 0; j < int(n.ChildCount()); j++ {
		child := n.Child(j)
		if child == nil {
			continue
		}
		if child.Type() == "param" {
			paramText := strings.TrimSpace(childText(child, src))
			if strings.HasPrefix(paramText, "--from=") {
				fromStage = strings.TrimPrefix(paramText, "--from=")
				break
			}
		}
	}

	if fromStage == "" {
		return
	}

	// Resolve the --from target: can be a name or numeric index.
	var targetNodeID graph.NodeID
	// Try numeric index first.
	if idx, isNum := dockerfileParseStageIndex(fromStage); isNum && idx >= 0 && idx < len(stages) {
		targetNodeID = stages[idx].nodeID
	} else {
		// Try name match.
		for _, s := range stages {
			if strings.EqualFold(s.name, fromStage) {
				targetNodeID = s.nodeID
				break
			}
		}
	}

	if targetNodeID == "" {
		// Target stage not found in this file; create a placeholder.
		targetNodeID = g.MakeNodeID(filePath, fromStage)
		g.AddNode(&graph.Node{
			ID:       targetNodeID,
			Type:     graph.NodeStruct,
			Name:     fromStage,
			File:     filePath,
			Line:     int(n.StartPoint().Row) + 1,
			Exported: true,
			Metadata: map[string]string{"kind": "stage"},
		})
	}

	g.AddEdge(&graph.Edge{From: currentStage, To: targetNodeID, Type: graph.EdgeCalls})
}

// handleArg processes an ARG instruction, creating a NodeVariable with kind="arg".
func (p *DockerfileParser) handleArg(
	g *graph.Graph,
	n *sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	lines []string,
) {
	startLine := int(n.StartPoint().Row) + 1
	doc := extractLineDoc(lines, startLine, "#")

	// Extract the argument name. The ARG instruction text looks like:
	// ARG NAME or ARG NAME=default
	// In the tree-sitter grammar, the children vary by grammar version.
	// We extract the full instruction text and parse the name from it.
	argText := strings.TrimSpace(childText(n, src))
	argText = strings.TrimPrefix(argText, "ARG ")
	argText = strings.TrimPrefix(argText, "arg ")

	// Split on = to get the name.
	name := argText
	if eqIdx := strings.Index(name, "="); eqIdx > 0 {
		name = name[:eqIdx]
	}
	name = strings.TrimSpace(name)

	if name == "" {
		return
	}

	nodeID := g.MakeNodeID(filePath, name)
	meta := map[string]string{"kind": "arg"}
	if doc != "" {
		meta["doc"] = doc
	}
	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeVariable,
		Name:     name,
		File:     filePath,
		Line:     startLine,
		Exported: true,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// handleEnv processes an ENV instruction, creating NodeVariable(s) with kind="env".
func (p *DockerfileParser) handleEnv(
	g *graph.Graph,
	n *sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	lines []string,
) {
	startLine := int(n.StartPoint().Row) + 1
	doc := extractLineDoc(lines, startLine, "#")

	// Extract env pairs. In the dockerfile grammar, env_instruction may contain
	// env_pair children, or we fall back to parsing the instruction text.
	found := false
	for j := 0; j < int(n.ChildCount()); j++ {
		child := n.Child(j)
		if child == nil {
			continue
		}
		if child.Type() == "env_pair" {
			key := ""
			if keyNode := firstChildOfType(child, "env_key"); keyNode != nil {
				key = strings.TrimSpace(childText(keyNode, src))
			}
			// Fallback: try unquoted_string as the key.
			if key == "" {
				if keyNode := firstChildOfType(child, "unquoted_string"); keyNode != nil {
					key = strings.TrimSpace(childText(keyNode, src))
				}
			}
			if key == "" {
				continue
			}
			found = true
			nodeID := g.MakeNodeID(filePath, key)
			meta := map[string]string{"kind": "env"}
			if doc != "" {
				meta["doc"] = doc
			}
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeVariable,
				Name:     key,
				File:     filePath,
				Line:     startLine,
				Exported: true,
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
		}
	}

	// Fallback: if no env_pair children found, parse the text directly.
	if !found {
		envText := strings.TrimSpace(childText(n, src))
		envText = strings.TrimPrefix(envText, "ENV ")
		envText = strings.TrimPrefix(envText, "env ")

		// Handle both "ENV KEY=VALUE" and "ENV KEY VALUE" forms.
		name := envText
		if eqIdx := strings.Index(name, "="); eqIdx > 0 {
			name = name[:eqIdx]
		} else if spIdx := strings.Index(name, " "); spIdx > 0 {
			name = name[:spIdx]
		}
		name = strings.TrimSpace(name)

		if name == "" {
			return
		}

		nodeID := g.MakeNodeID(filePath, name)
		meta := map[string]string{"kind": "env"}
		if doc != "" {
			meta["doc"] = doc
		}
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeVariable,
			Name:     name,
			File:     filePath,
			Line:     startLine,
			Exported: true,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}
}

// handleExpose collects EXPOSE port declarations into the file metadata.
func (p *DockerfileParser) handleExpose(n *sitter.Node, src []byte, meta map[string]string) {
	exposeText := strings.TrimSpace(childText(n, src))
	exposeText = strings.TrimPrefix(exposeText, "EXPOSE ")
	exposeText = strings.TrimPrefix(exposeText, "expose ")
	exposeText = strings.TrimSpace(exposeText)

	if exposeText == "" {
		return
	}

	if existing, ok := meta["expose"]; ok {
		meta["expose"] = existing + "," + exposeText
	} else {
		meta["expose"] = exposeText
	}
}

// handleLabel collects LABEL metadata into the file metadata.
func (p *DockerfileParser) handleLabel(n *sitter.Node, src []byte, meta map[string]string) {
	labelText := strings.TrimSpace(childText(n, src))
	labelText = strings.TrimPrefix(labelText, "LABEL ")
	labelText = strings.TrimPrefix(labelText, "label ")
	labelText = strings.TrimSpace(labelText)

	if labelText == "" {
		return
	}

	// Parse key=value or key="value" pairs.
	if eqIdx := strings.Index(labelText, "="); eqIdx > 0 {
		key := strings.TrimSpace(labelText[:eqIdx])
		val := strings.TrimSpace(labelText[eqIdx+1:])
		val = strings.Trim(val, `"'`)
		meta["label."+key] = val
	} else {
		// Single-word label (no value).
		if existing, ok := meta["labels"]; ok {
			meta["labels"] = existing + "," + labelText
		} else {
			meta["labels"] = labelText
		}
	}
}

// handleCmdOrEntrypoint processes CMD and ENTRYPOINT instructions,
// creating a NodeFunction with the instruction kind.
func (p *DockerfileParser) handleCmdOrEntrypoint(
	g *graph.Graph,
	n *sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	kind string,
) {
	startLine := int(n.StartPoint().Row) + 1

	// Extract the full instruction text and strip the keyword prefix.
	raw := strings.TrimSpace(childText(n, src))
	upper := strings.ToUpper(kind)
	raw = strings.TrimPrefix(raw, upper+" ")
	raw = strings.TrimPrefix(raw, strings.ToLower(upper)+" ")
	raw = strings.TrimSpace(raw)

	// Extract the command name: first token after stripping JSON array brackets.
	// CMD ["executable", "arg1"] or CMD executable arg1
	name := raw
	if strings.HasPrefix(name, "[") {
		// JSON array form: ["executable", ...]
		name = strings.TrimPrefix(name, "[")
		name = strings.Trim(strings.SplitN(name, ",", 2)[0], ` "`)
	} else {
		// Shell form: executable arg1 arg2
		if spIdx := strings.IndexAny(name, " \t"); spIdx > 0 {
			name = name[:spIdx]
		}
	}
	name = strings.TrimSpace(name)
	if name == "" || name == "[" || name == "]" {
		return
	}

	// Use kind+":"+name as the node name to avoid collisions.
	nodeName := kind + ":" + name
	nodeID := g.MakeNodeID(filePath, nodeName)
	if g.GetNode(nodeID) != nil {
		return
	}
	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeFunction,
		Name:     nodeName,
		File:     filePath,
		Line:     startLine,
		Exported: true,
		Metadata: map[string]string{"kind": kind},
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// dockerfileParseStageIndex attempts to parse a string as a non-negative integer
// (stage index for COPY --from=0 syntax). Returns the index and true if valid.
func dockerfileParseStageIndex(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0, false
		}
		n = n*10 + int(ch-'0')
	}
	return n, true
}
