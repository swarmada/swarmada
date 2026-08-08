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
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

func intp(i int) *int { return &i }

func TestColorEnabled(t *testing.T) {
	t.Setenv("NO_COLOR", "") // ensure a known baseline; presence, not value, matters
	if ColorEnabled(ColorNever, nil) {
		t.Error("never must disable color")
	}
	if !ColorEnabled(ColorAlways, nil) {
		t.Error("always must enable color even without a TTY")
	}
	// auto with NO_COLOR set (any value) disables regardless of TTY.
	if ColorEnabled(ColorAuto, nil) {
		t.Error("auto with NO_COLOR set must disable color")
	}
}

func TestColorEnabledAutoNoTTY(t *testing.T) {
	// A nil file is not a terminal; auto must be off (this is the piped/redirected
	// and `go test` case). t.Setenv registers restoration of the original value;
	// Unsetenv then guarantees NO_COLOR is absent for a clean auto check.
	t.Setenv("NO_COLOR", "")
	if err := os.Unsetenv("NO_COLOR"); err != nil {
		t.Fatal(err)
	}
	if ColorEnabled(ColorAuto, nil) {
		t.Error("auto with no TTY must disable color")
	}
}

func TestParseOutputFormat(t *testing.T) {
	cases := map[string]struct {
		want OutputFormat
		ok   bool
	}{
		"":      {OutputTable, true},
		"table": {OutputTable, true},
		"wide":  {OutputWide, true},
		"yaml":  {OutputYAML, true},
		"json":  {OutputJSON, true},
		"toml":  {"", false},
	}
	for in, exp := range cases {
		got, ok := ParseOutputFormat(in)
		if ok != exp.ok || (ok && got != exp.want) {
			t.Errorf("ParseOutputFormat(%q) = (%q,%v), want (%q,%v)", in, got, ok, exp.want, exp.ok)
		}
	}
}

func TestPadRuneWidth(t *testing.T) {
	// The em-dash is one rune; pad must count runes, not bytes (3 bytes).
	got := pad(Unknown, 5)
	if utf8.RuneCountInString(got) != 5 {
		t.Errorf("pad(%q,5) has %d runes, want 5: %q", Unknown, utf8.RuneCountInString(got), got)
	}
	// Already at/over width: unchanged.
	if pad("abcdef", 3) != "abcdef" {
		t.Error("pad must not truncate")
	}
}

func TestTableAlignsWithColorAndGlyphs(t *testing.T) {
	tbl := NewTable(true, "NAME", "PHASE", "BATTERY")
	tbl.AddRow(TextCell("sim-robot-001"), RobotPhaseCell("Idle"), BatteryCell(intp(87), false))
	tbl.AddRow(TextCell("sim-robot-003"), RobotPhaseCell("Charging"), BatteryCell(intp(22), true))
	tbl.AddRow(TextCell("sim-robot-005"), RobotPhaseCell("Offline"), BatteryCell(nil, false))

	var b bytes.Buffer
	if err := tbl.Render(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()

	// Color present when enabled.
	if !strings.Contains(out, "\033[") {
		t.Error("expected ANSI escapes when color enabled")
	}
	// The charging cell carries the ⚡ glyph.
	if !strings.Contains(out, "⚡ 22%") {
		t.Errorf("expected charging battery glyph, got:\n%s", out)
	}
	// Missing battery projection renders the em-dash.
	if !strings.Contains(out, Unknown) {
		t.Errorf("expected em-dash for nil battery, got:\n%s", out)
	}

	// With color disabled, every column must still align: the NAME column pads to
	// the widest name so PHASE starts at the same visible offset on every row.
	tbl2 := NewTable(false, "NAME", "PHASE")
	tbl2.AddRow(TextCell("short"), RobotPhaseCell("Idle"))
	tbl2.AddRow(TextCell("a-longer-name"), RobotPhaseCell("Assigned"))
	var b2 bytes.Buffer
	_ = tbl2.Render(&b2)
	lines := strings.Split(strings.TrimRight(b2.String(), "\n"), "\n")
	col := strings.Index(lines[0], "PHASE")
	for _, ln := range lines[1:] {
		// The phase word begins right after the padded NAME column.
		if got := firstNonSpaceAfter(ln, len("a-longer-name")); got != col {
			// Not a strict requirement that it equals header col, but every data
			// row must agree with each other; assert against the header offset.
			t.Errorf("misaligned PHASE column: line %q value starts at %d, header at %d", ln, got, col)
		}
	}
}

func firstNonSpaceAfter(s string, from int) int {
	for i := from; i < len(s); i++ {
		if s[i] != ' ' {
			return i
		}
	}
	return -1
}

func TestBatteryCellThresholds(t *testing.T) {
	cases := []struct {
		pct      *int
		charging bool
		wantSGR  string
		wantText string
	}{
		{intp(87), false, sgrGreen, "87%"},
		{intp(50), false, sgrGreen, "50%"},
		{intp(49), false, sgrYellow, "49%"},
		{intp(20), false, sgrYellow, "20%"},
		{intp(19), false, sgrBoldRed, "19%"},
		{intp(8), true, sgrYellow, "⚡ 8%"},
		{nil, false, "", Unknown},
	}
	for _, c := range cases {
		got := BatteryCell(c.pct, c.charging)
		if got.text != c.wantText || got.sgr != c.wantSGR {
			t.Errorf("BatteryCell(%v,%v) = {%q,%q}, want {%q,%q}", c.pct, c.charging, got.text, got.sgr, c.wantText, c.wantSGR)
		}
	}
}

func TestRobotPhaseCellOfflineReverse(t *testing.T) {
	if got := RobotPhaseCell("Offline"); got.sgr != sgrOffline {
		t.Errorf("Offline must use reverse-video SGR, got %q", got.sgr)
	}
	if got := RobotPhaseCell("Nonsense"); got.sgr != "" {
		t.Errorf("unknown phase must render plain, got sgr %q", got.sgr)
	}
}

func TestBatteryBarWidth(t *testing.T) {
	label, bar := BatteryBar(false, 45, false)
	if label != "45%" {
		t.Errorf("label = %q, want 45%%", label)
	}
	if utf8.RuneCountInString(bar) != 20 {
		t.Errorf("bar must be 20 cells, got %d: %q", utf8.RuneCountInString(bar), bar)
	}
	// 45% rounds to 9 filled cells (round(45/5)=9).
	if got := strings.Count(bar, "█"); got != 9 {
		t.Errorf("45%% -> %d filled cells, want 9", got)
	}
}

func TestPrintMarshaled(t *testing.T) {
	obj := versionShim{Version: "v1", Commit: "abc"}
	var y bytes.Buffer
	if err := PrintMarshaled(&y, OutputYAML, obj); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(y.String(), "version: v1") {
		t.Errorf("yaml missing field:\n%s", y.String())
	}
	var j bytes.Buffer
	if err := PrintMarshaled(&j, OutputJSON, obj); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(j.String(), "\"version\": \"v1\"") {
		t.Errorf("json missing field:\n%s", j.String())
	}
	if err := PrintMarshaled(&j, OutputTable, obj); err == nil {
		t.Error("table format must not be machine-marshaled")
	}
}

type versionShim struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

func TestPrintError(t *testing.T) {
	var b bytes.Buffer
	PrintError(&b, false, errors.New("something broke"))
	if got := b.String(); got != "error: something broke\n" {
		t.Errorf("PrintError = %q", got)
	}
	// Multi-line (admission rejection) keeps the bullet body verbatim.
	b.Reset()
	PrintError(&b, false, errors.New("Robot is invalid:\n* spec.x: bad"))
	if !strings.Contains(b.String(), "* spec.x: bad") {
		t.Errorf("multiline body dropped: %q", b.String())
	}
	// Colored prefix wraps only "error:".
	b.Reset()
	PrintError(&b, true, errors.New("boom"))
	if !strings.HasPrefix(b.String(), sgrBoldRed+"error:"+sgrReset) {
		t.Errorf("colored prefix wrong: %q", b.String())
	}
}
