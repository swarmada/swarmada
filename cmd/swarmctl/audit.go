/*
Copyright 2026 The Swarmada Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/cli"
)

// auditNote explains where the chain comes from. The control-plane audit read
// API and durable sink are not yet built (the manager records into an in-memory
// sink — internal/audit.MemorySink), so `audit` operates on an EXPORTED chain:
// newline-delimited JSON audit.Entry records from a file or stdin. The parse,
// render, and verify path is the one a future server-side reader will feed.
const auditNote = "reads an exported audit chain (newline-delimited JSON) from --file or stdin"

// newGetAuditCommand is the `audit` subcommand of `get`: `swarmctl get audit`
// prints the audit chain (custom verb: read).
func newGetAuditCommand(o *options) *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Print the safety audit chain (custom verb: read)",
		Long: `Print the Swarmada safety audit log — the per-namespace, hash-chained
record of safety- and security-relevant events (RFC-0001 §9.5.4, §9.6.5) — as a
table. ` + auditNote + `. Reading the log is governed by the safetyauditlogs/read
custom verb (fleet-manager and admin).`,
		Example: `  swarmctl get audit --file audit-export.jsonl
  swarmctl export audit | swarmctl get audit`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			entries, err := o.readAuditChain(file)
			if err != nil {
				return err
			}
			return o.printAuditLog(entries)
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "Path to an exported audit chain (default: stdin)")
	return cmd
}

// newVerifyCommand is the top-level `verify` verb; its only resource today is
// `audit` (`swarmctl verify audit`).
func newVerifyCommand(o *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Verify Swarmada integrity artifacts",
	}
	cmd.AddCommand(newVerifyAuditCommand(o))
	return cmd
}

func newVerifyAuditCommand(o *options) *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Verify the safety audit chain's integrity (custom verb: verify)",
		Long: `Recompute each namespace's hash chain and report any sequence gap, hash
mismatch, or broken genesis — the signatures of a modified, deleted, or reordered
entry (RFC-0001 §9.5.4). ` + auditNote + `. Exits non-zero if any chain fails.
Governed by the safetyauditlogs/verify custom verb (fleet-manager and admin).`,
		Example: `  swarmctl verify audit --file audit-export.jsonl`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			entries, err := o.readAuditChain(file)
			if err != nil {
				return err
			}
			return o.verifyAuditChain(entries)
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "Path to an exported audit chain (default: stdin)")
	return cmd
}

// newExportCommand is the top-level `export` verb; its only resource today is
// `audit` (`swarmctl export audit`).
func newExportCommand(o *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export Swarmada artifacts",
	}
	cmd.AddCommand(newExportAuditCommand(o))
	return cmd
}

func newExportAuditCommand(o *options) *cobra.Command {
	var file, outFile string
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Export the safety audit chain (custom verb: export, admin-only)",
		Long: `Emit the safety audit chain as newline-delimited JSON for archival or a
regulator hand-off (RFC-0001 §9.5.4). ` + auditNote + `, and re-emits it verbatim
to --out-file (default stdout) so the exact sealed bytes round-trip. Exporting the
log is admin-only (safetyauditlogs/export). This is the migration-stage form: it
re-serializes an already-exported chain until the control-plane read API lands.

(--out-file, not -o: the persistent -o/--output selects table|yaml|json output.)`,
		Example: `  swarmctl export audit --file audit-export.jsonl --out-file audit-archive.jsonl
  swarmctl export audit | swarmctl verify audit`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			entries, err := o.readAuditChain(file)
			if err != nil {
				return err
			}
			w := o.streams.Out
			if outFile != "" && outFile != "-" {
				f, err := os.Create(outFile)
				if err != nil {
					return fmt.Errorf("creating export file: %w", err)
				}
				defer func() { _ = f.Close() }()
				w = f
			}
			return o.exportAuditChain(entries, w)
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "Path to an exported audit chain to re-emit (default: stdin)")
	cmd.Flags().StringVar(&outFile, "out-file", "", "Where to write the exported chain (default: stdout)")
	return cmd
}

// exportAuditChain writes entries as newline-delimited JSON — the format `get
// audit` and `verify audit` consume — preserving order.
func (o *options) exportAuditChain(entries []audit.Entry, w io.Writer) error {
	for _, e := range entries {
		data, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("serializing audit entry seq=%d: %w", e.SequenceNumber, err)
		}
		if _, err := w.Write(append(data, '\n')); err != nil {
			return fmt.Errorf("writing audit export: %w", err)
		}
	}
	return nil
}

// readAuditChain parses newline-delimited JSON audit.Entry records from file, or
// from stdin when file is empty or "-". Blank lines are skipped; a malformed line
// is reported with its line number.
func (o *options) readAuditChain(file string) ([]audit.Entry, error) {
	var r io.Reader
	if file == "" || file == "-" {
		r = o.streams.In
	} else {
		f, err := os.Open(file)
		if err != nil {
			return nil, fmt.Errorf("opening audit chain: %w", err)
		}
		defer func() { _ = f.Close() }()
		r = f
	}

	var entries []audit.Entry
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // audit lines can be long
	line := 0
	for sc.Scan() {
		line++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		var e audit.Entry
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			return nil, fmt.Errorf("parsing audit chain at line %d: %w", line, err)
		}
		entries = append(entries, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading audit chain: %w", err)
	}
	return entries, nil
}

// printAuditLog renders the entries as a table (or -o yaml|json), sorted by
// namespace then sequence so each chain reads in order.
func (o *options) printAuditLog(entries []audit.Entry) error {
	if !o.outputFmt.IsTable() {
		return cli.PrintMarshaled(o.streams.Out, o.outputFmt, entries)
	}
	if len(entries) == 0 {
		_, _ = fmt.Fprintln(o.streams.Err, "No audit entries.")
		return nil
	}
	sorted := append([]audit.Entry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Namespace != sorted[j].Namespace {
			return sorted[i].Namespace < sorted[j].Namespace
		}
		return sorted[i].SequenceNumber < sorted[j].SequenceNumber
	})

	tbl := cli.NewTable(o.colorStdout, "SEQ", "NAMESPACE", "TIME", "EVENT", "ACTOR", "RESOURCE", "ACTION", "OUTCOME")
	for _, e := range sorted {
		tbl.AddRow(
			cli.TextCell(fmt.Sprintf("%d", e.SequenceNumber)),
			cli.TextCell(e.Namespace),
			cli.TextCell(e.EventTime),
			cli.TextCell(e.EventType),
			cli.TextCell(orNone(e.Actor.Identity)),
			cli.TextCell(orNone(resourceRef(e))),
			cli.TextCell(orNone(e.Action)),
			cli.OutcomeCell(string(e.Outcome)),
		)
	}
	return tbl.Render(o.streams.Out)
}

// verifyAuditChain groups entries by namespace, runs the real chain verifier on
// each, prints a per-namespace verdict, and returns an error if any chain fails
// so the command exits non-zero (usable in a CI integrity gate).
func (o *options) verifyAuditChain(entries []audit.Entry) error {
	byNS := map[string][]audit.Entry{}
	var order []string
	for _, e := range entries {
		if _, seen := byNS[e.Namespace]; !seen {
			order = append(order, e.Namespace)
		}
		byNS[e.Namespace] = append(byNS[e.Namespace], e)
	}
	sort.Strings(order)

	if len(order) == 0 {
		_, _ = fmt.Fprintln(o.streams.Err, "No audit entries to verify.")
		return nil
	}

	allOK := true
	for _, ns := range order {
		chain := byNS[ns]
		sort.SliceStable(chain, func(i, j int) bool { return chain[i].SequenceNumber < chain[j].SequenceNumber })
		res := audit.Verify(chain)
		if !res.OK {
			allOK = false
		}
		word := "OK"
		if !res.OK {
			word = "TAMPERED"
		}
		_, _ = fmt.Fprintf(o.streams.Out, "%s  namespace=%s entries=%d gaps=%d hash-mismatches=%d genesis-ok=%t\n",
			cli.VerdictCell(res.OK, word).Render(o.colorStdout), ns, res.Entries, res.SequenceGaps, res.HashMismatches, res.GenesisOK)
	}
	if !allOK {
		return fmt.Errorf("audit chain verification FAILED: at least one namespace chain was modified, deleted, or reordered")
	}
	return nil
}

func resourceRef(e audit.Entry) string {
	if e.Resource.Kind == "" && e.Resource.Name == "" {
		return ""
	}
	if e.Resource.Name == "" {
		return e.Resource.Kind
	}
	return e.Resource.Kind + "/" + e.Resource.Name
}
