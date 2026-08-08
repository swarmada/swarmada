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

package cli

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Cell is one table value: its visible text plus an optional SGR color that is
// applied only after padding, so color codes never count toward column width.
// This mirrors the reference generator's cc()/pad() split: pad by *character*
// count, then colorize — the only way alignment survives a multi-byte glyph
// like ⚡ or — alongside an ANSI escape.
type Cell struct {
	text string
	sgr  string
}

// TextCell is an uncolored value (the default for non-status columns like ZONE,
// TASK, AGE, which are never colorized).
func TextCell(s string) Cell { return Cell{text: s} }

// Text returns the cell's visible text without color.
func (c Cell) Text() string { return c.text }

// Render returns the cell's text wrapped in its color when enabled — used to
// colorize a single value outside a table (e.g. a describe field).
func (c Cell) Render(enabled bool) string { return paint(enabled, c.sgr, c.text) }

// styledCell is a colored value; the SGR is dropped when color is disabled.
func styledCell(sgr, s string) Cell { return Cell{text: s, sgr: sgr} }

// Table renders a kubectl-style aligned table: NAME first, status columns in
// the middle, AGE last is the caller's responsibility; this type only handles
// width computation, padding, and per-cell color.
type Table struct {
	headers []string
	rows    [][]Cell
	color   bool
}

// NewTable starts a table with the given header row. Headers are plain text
// (already uppercased by the caller, kubectl-style) and never colorized.
func NewTable(color bool, headers ...string) *Table {
	return &Table{headers: headers, color: color}
}

// AddRow appends one row. Excess cells beyond the header count are ignored and
// missing trailing cells render empty, so callers cannot misalign the grid.
func (t *Table) AddRow(cells ...Cell) {
	row := make([]Cell, len(t.headers))
	for i := range row {
		if i < len(cells) {
			row[i] = cells[i]
		}
	}
	t.rows = append(t.rows, row)
}

// Render writes the padded, optionally-colored table to w. Every column except
// the last is padded to the widest visible cell in that column and separated by
// a single space; the last column is never trailing-padded (no dangling
// whitespace at end of line).
func (t *Table) Render(w io.Writer) error {
	widths := make([]int, len(t.headers))
	for i, h := range t.headers {
		widths[i] = utf8.RuneCountInString(h)
	}
	for _, row := range t.rows {
		for i, c := range row {
			if n := utf8.RuneCountInString(c.text); n > widths[i] {
				widths[i] = n
			}
		}
	}

	var b strings.Builder
	writeLine := func(cells []Cell) {
		last := len(cells) - 1
		for i, c := range cells {
			if i == last {
				b.WriteString(paint(t.color, c.sgr, c.text))
			} else {
				b.WriteString(paint(t.color, c.sgr, pad(c.text, widths[i])))
				b.WriteByte(' ')
			}
		}
		b.WriteByte('\n')
	}

	hdr := make([]Cell, len(t.headers))
	for i, h := range t.headers {
		hdr[i] = TextCell(h)
	}
	writeLine(hdr)
	for _, row := range t.rows {
		writeLine(row)
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// pad left-aligns text and space-pads to width *characters* (runes). A cell
// already at or over width is returned unchanged.
func pad(text string, width int) string {
	n := utf8.RuneCountInString(text)
	if n >= width {
		return text
	}
	return text + strings.Repeat(" ", width-n)
}

// KeyValue writes a describe-style "Label:<pad>value" line, with the value
// starting at a fixed column so the block scans vertically.
func KeyValue(w io.Writer, label, value string, valueCol int) {
	labelWidth := valueCol - 2 // label + ": " occupies up to valueCol
	if labelWidth < 0 {
		labelWidth = 0
	}
	_, _ = fmt.Fprintf(w, "%s %s\n", pad(label+":", labelWidth+1), value)
}
