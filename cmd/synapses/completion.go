package main

import (
	"fmt"
	"os"
	"strings"
)

// Top-level commands for shell completion.
var completionCommands = []struct {
	Name string
	Desc string
}{
	{"init", "Initialize a project (index + daemon + agent wiring)"},
	{"start", "Start MCP server for a project"},
	{"stop", "Stop the daemon gracefully"},
	{"projects", "List registered projects"},
	{"logs", "Show daemon logs"},
	{"index", "Rebuild the code graph index"},
	{"status", "Show project and daemon status"},
	{"list", "List graph entities"},
	{"reset", "Reset project index and cache"},
	{"version", "Print version"},
	{"brain", "Manage the AI enrichment sidecar"},
	{"query", "Run a graph query"},
	{"export", "Export the graph (JSON/DOT)"},
	{"doctor", "Run a full health check"},
	{"daemon", "Manage the background daemon"},
	{"brief", "Generate a project brief"},
	{"connect", "Connect to a running daemon"},
	{"memory", "Manage episodic memory"},
	{"allow-plugin", "Approve a plugin for use"},
	{"approve", "Approve a pending action"},
	{"benchmark", "Run benchmarks"},
	{"uninstall", "Remove synapses from a project"},
	{"update", "Self-update the synapses binary"},
	{"completion", "Generate shell completion script"},
	{"help", "Show help"},
}

// Daemon subcommands for nested completion.
var daemonSubcommands = []struct {
	Name string
	Desc string
}{
	{"start", "Start the daemon"},
	{"stop", "Stop the daemon"},
	{"restart", "Restart the daemon"},
	{"status", "Show daemon status"},
	{"logs", "Show daemon logs"},
	{"wait", "Wait for daemon to be ready"},
	{"install", "Install as system service"},
	{"uninstall", "Remove system service"},
	{"serve", "Run daemon in foreground"},
}

func cmdCompletion(args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: synapses completion <bash|zsh|fish>")
		return nil
	}

	switch args[0] {
	case "bash":
		fmt.Print(bashCompletion())
	case "zsh":
		fmt.Print(zshCompletion())
	case "fish":
		fmt.Print(fishCompletion())
	default:
		return fmt.Errorf("unsupported shell %q — use bash, zsh, or fish", args[0])
	}
	return nil
}

func bashCompletion() string {
	var cmds []string
	for _, c := range completionCommands {
		cmds = append(cmds, c.Name)
	}
	var daemonCmds []string
	for _, c := range daemonSubcommands {
		daemonCmds = append(daemonCmds, c.Name)
	}

	return `# bash completion for synapses
# Add to ~/.bashrc:  eval "$(synapses completion bash)"

_synapses() {
    local cur prev commands daemon_commands
    COMPREPLY=()
    cur="${COMP_WORDS[COMP_CWORD]}"
    prev="${COMP_WORDS[COMP_CWORD-1]}"

    commands="` + strings.Join(cmds, " ") + `"
    daemon_commands="` + strings.Join(daemonCmds, " ") + `"

    case "${prev}" in
        synapses)
            COMPREPLY=( $(compgen -W "${commands}" -- "${cur}") )
            return 0
            ;;
        daemon)
            COMPREPLY=( $(compgen -W "${daemon_commands}" -- "${cur}") )
            return 0
            ;;
        --path|-path)
            COMPREPLY=( $(compgen -d -- "${cur}") )
            return 0
            ;;
    esac

    # Default: complete with commands
    if [ "${COMP_CWORD}" -eq 1 ]; then
        COMPREPLY=( $(compgen -W "${commands}" -- "${cur}") )
    fi
}

complete -F _synapses synapses
`
}

func zshCompletion() string {
	var b strings.Builder
	b.WriteString(`#compdef synapses
# zsh completion for synapses
# Add to ~/.zshrc:  eval "$(synapses completion zsh)"

_synapses() {
    local -a commands daemon_commands

    commands=(
`)
	for _, c := range completionCommands {
		fmt.Fprintf(&b, "        '%s:%s'\n", c.Name, c.Desc)
	}
	b.WriteString(`    )

    daemon_commands=(
`)
	for _, c := range daemonSubcommands {
		fmt.Fprintf(&b, "        '%s:%s'\n", c.Name, c.Desc)
	}
	b.WriteString(`    )

    _arguments -C \
        '1:command:->command' \
        '*::arg:->args'

    case $state in
        command)
            _describe -t commands 'synapses command' commands
            ;;
        args)
            case ${words[1]} in
                daemon)
                    _describe -t daemon_commands 'daemon subcommand' daemon_commands
                    ;;
                init|start|index|status|export|query)
                    _arguments '--path[Project path]:directory:_directories'
                    ;;
                completion)
                    _values 'shell' bash zsh fish
                    ;;
            esac
            ;;
    esac
}

_synapses "$@"
`)
	return b.String()
}

func fishCompletion() string {
	var b strings.Builder
	b.WriteString(`# fish completion for synapses
# Add to ~/.config/fish/completions/synapses.fish
# Or run:  synapses completion fish > ~/.config/fish/completions/synapses.fish

# Disable file completions by default
complete -c synapses -f

# Top-level commands
`)
	for _, c := range completionCommands {
		fmt.Fprintf(&b, "complete -c synapses -n '__fish_use_subcommand' -a '%s' -d '%s'\n", c.Name, c.Desc)
	}

	b.WriteString("\n# daemon subcommands\n")
	for _, c := range daemonSubcommands {
		fmt.Fprintf(&b, "complete -c synapses -n '__fish_seen_subcommand_from daemon' -a '%s' -d '%s'\n", c.Name, c.Desc)
	}

	b.WriteString("\n# completion subcommands\n")
	b.WriteString("complete -c synapses -n '__fish_seen_subcommand_from completion' -a 'bash zsh fish'\n")

	b.WriteString("\n# --path flag for relevant commands\n")
	for _, cmd := range []string{"init", "start", "index", "status", "export", "query"} {
		fmt.Fprintf(&b, "complete -c synapses -n '__fish_seen_subcommand_from %s' -l path -d 'Project path' -r -F\n", cmd)
	}

	return b.String()
}
