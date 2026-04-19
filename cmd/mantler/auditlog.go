package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/Borgels/mantlerd/internal/audit"
	"github.com/spf13/cobra"
)

var (
	auditLines  int
	auditJSON   bool
	auditDenied bool
)

var auditCmd = &cobra.Command{
	Use:   "audit",
	Short: "View the local agent audit log",
	Long: `Display recent entries from the agent security audit log.

The audit log records every server-originated command that was dispatched,
including whether it was allowed, denied by local policy, or failed.

Examples:
  mantler audit
  mantler audit --lines 100
  mantler audit --denied
  mantler audit --json`,
	RunE: runAudit,
}

func init() {
	rootCmd.AddCommand(auditCmd)
	auditCmd.Flags().IntVarP(&auditLines, "lines", "n", 50, "Number of most-recent events to show")
	auditCmd.Flags().BoolVarP(&auditJSON, "json", "j", false, "Print raw JSON (one object per line)")
	auditCmd.Flags().BoolVar(&auditDenied, "denied", false, "Show only denied events")
}

func runAudit(cmd *cobra.Command, args []string) error {
	logger := audit.Default()
	events, err := logger.ReadRecent(auditLines)
	if err != nil {
		return fmt.Errorf("read audit log: %w", err)
	}

	if len(events) == 0 {
		fmt.Printf("No audit events found (log: %s)\n", logger.Path())
		return nil
	}

	if auditDenied {
		filtered := events[:0]
		for _, ev := range events {
			if ev.Outcome == "denied" || ev.Outcome == "failed" {
				filtered = append(filtered, ev)
			}
		}
		events = filtered
	}

	if auditJSON {
		enc := json.NewEncoder(os.Stdout)
		for _, ev := range events {
			_ = enc.Encode(ev)
		}
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TIME\tCOMMAND\tOUTCOME\tDESTRUCTIVE\tREASON")
	for _, ev := range events {
		ts := ev.Timestamp
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			ts = t.Local().Format("2006-01-02 15:04:05")
		}
		destructive := ""
		if ev.Destructive {
			destructive = "yes"
		}
		reason := ev.Reason
		if len(reason) > 80 {
			reason = reason[:77] + "..."
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", ts, ev.CommandType, ev.Outcome, destructive, reason)
	}
	_ = w.Flush()
	fmt.Printf("\n(log: %s)\n", logger.Path())
	return nil
}
