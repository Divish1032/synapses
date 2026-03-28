package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	mcpsrv "github.com/SynapsesOS/synapses/internal/mcp"
)

// cmdApprove is the `synapses approve` command.
// It lists pending cross-project write approvals and lets the user confirm them
// interactively. The approval token lives only on disk — agents never see it.
//
// Usage:
//
//	synapses approve          # Interactive: prompt for each pending approval
//	synapses approve --all    # Approve all pending requests non-interactively
func cmdApprove(args []string) error {
	approveAll := false
	for _, arg := range args {
		if arg == "--all" || arg == "-a" {
			approveAll = true
		}
	}

	pending, err := mcpsrv.ListPendingApprovals()
	if err != nil {
		return fmt.Errorf("list approvals: %w", err)
	}

	if len(pending) == 0 {
		fmt.Println("  No pending cross-project approvals.")
		return nil
	}

	fmt.Printf("\n  Pending cross-project approvals (%d)\n", len(pending))
	fmt.Println("  ─────────────────────────────────────────────────────────────")
	for i, p := range pending {
		fmt.Printf("\n  [%d] Operation : %s\n", i+1, p.Operation)
		fmt.Printf("      Agent     : %s\n", p.AgentID)
		fmt.Printf("      Details   : %s\n", p.Details)
		fmt.Printf("      Expires   : %s\n", p.ExpiresAt.Local().Format("15:04:05"))
	}
	fmt.Println()

	if approveAll {
		approved := 0
		for _, p := range pending {
			if err := mcpsrv.ApproveRequest(p.Token); err != nil {
				fmt.Printf("  \033[31m✗\033[0m [%s] %v\n", p.Operation, err)
			} else {
				fmt.Printf("  \033[32m✓\033[0m Approved: %s (agent=%s)\n", p.Operation, p.AgentID)
				approved++
			}
		}
		fmt.Printf("\n  Approved %d/%d requests. The agent's next retry will proceed.\n\n", approved, len(pending))
		return nil
	}

	// Interactive mode: prompt for each.
	scanner := bufio.NewScanner(os.Stdin)
	approved := 0
	for i, p := range pending {
		fmt.Printf("  Approve [%d] %s (agent=%s)? [y/N] ", i+1, p.Operation, p.AgentID)
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if strings.EqualFold(line, "y") || strings.EqualFold(line, "yes") {
			if err := mcpsrv.ApproveRequest(p.Token); err != nil {
				fmt.Printf("  \033[31m✗\033[0m Error: %v\n", err)
			} else {
				fmt.Printf("  \033[32m✓\033[0m Approved. The agent's next retry will proceed.\n")
				approved++
			}
		} else {
			fmt.Println("  — Skipped.")
		}
	}

	if approved > 0 {
		fmt.Printf("\n  %d approval(s) confirmed.\n\n", approved)
	}
	return nil
}
