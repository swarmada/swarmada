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

package samples_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
)

// hack/sim-conformance-report.sh prints the harness verdict on the documented quickstart. The
// harness grades obligations (ADR-0048): a failing SHOULD or MAY row is an advisory, not a
// failure, and its own summary says "0 failed, 1 advisory". The wrapper printed the raw
// per-status tally instead, so a conformant run read "fail 1" to a first-time user.

var verdictLine = regexp.MustCompile(`sim-conformance-report: harness verdict conformant=(\w+) contract_version=\S+ \(pass (\d+), fail (\d+), advisory (\d+), skip (\d+)\)`)

// runWrapperOn drives the wrapper's harness path (SIM_CONFORMANCE_REPORT=run) with `make`
// replaced by a stub that writes report as the harness's --json output, and returns the
// verdict line it prints.
func runWrapperOn(t *testing.T, root string, report map[string]any) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatal("the wrapper's harness path needs python3, as the quickstart does")
	}
	dir := t.TempDir()
	fixture := filepath.Join(dir, "fixture.json")
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := "#!/usr/bin/env bash\n" +
		"for a in \"$@\"; do case \"$a\" in CONFORMANCE_ARGS=--json\\ *) cp \"$FIXTURE\" \"${a#CONFORMANCE_ARGS=--json }\" ;; esac; done\n"
	if err := os.WriteFile(filepath.Join(bin, "make"), []byte(stub), 0o700); err != nil { //nolint:gosec // test stub must be executable
		t.Fatal(err)
	}
	out := filepath.Join(dir, "report.json")
	cmd := exec.Command("bash", filepath.Join(root, "hack", "sim-conformance-report.sh"), out) //nolint:gosec // fixed repo script
	cmd.Env = append(os.Environ(),
		"SIM_CONFORMANCE_REPORT=run",
		"FIXTURE="+fixture,
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	msg, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("SIM_CONFORMANCE_REPORT=run hack/sim-conformance-report.sh: %v\n%s", err, msg)
	}
	line := verdictLine.FindString(string(msg))
	if line == "" {
		t.Fatalf("the wrapper printed no verdict line of the form `(pass N, fail N, advisory N, skip N)`:\n%s", msg)
	}
	return line
}

func loadRecordedReport(t *testing.T, root string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(recordedReport))) //nolint:gosec // fixed repo path
	if err != nil {
		t.Fatal(err)
	}
	var r map[string]any
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// The recorded report is a conformant run with one failing SHOULD row (C13.2). Its verdict line
// must count that row as an advisory and print no failure.
func TestSimConformanceReportPrintsAdvisoriesApartFromFailures(t *testing.T) {
	root := repoRoot(t)
	report := loadRecordedReport(t, root)
	counts, _ := report["counts"].(map[string]any)
	var advisories int
	for _, row := range report["results"].([]any) {
		r := row.(map[string]any)
		if r["status"] == "fail" && (r["level"] == "SHOULD" || r["level"] == "MAY") {
			advisories++
		}
	}
	if report["conformant"] != true || advisories == 0 {
		t.Fatalf("the recorded report is no longer a conformant run with an advisory; this test needs one")
	}
	want := fmt.Sprintf("conformant=true contract_version=%v (pass %v, fail %v, advisory %d, skip %v)",
		report["contract_version"], counts["pass"], int(counts["fail"].(float64))-advisories, advisories, counts["skip"])

	if line := runWrapperOn(t, root, report); line != "sim-conformance-report: harness verdict "+want {
		t.Errorf("verdict line:\n got %s\nwant sim-conformance-report: harness verdict %s", line, want)
	}
}

// A failing MUST is a failure, and a row whose level cannot be read is treated as one: the
// split may overstate failures, never hide one.
func TestSimConformanceReportNeverHidesAFailure(t *testing.T) {
	root := repoRoot(t)
	for name, mutate := range map[string]func(map[string]any){
		"a failing MUST": func(r map[string]any) {
			for _, row := range r["results"].([]any) {
				if row.(map[string]any)["status"] == "fail" {
					row.(map[string]any)["level"] = "MUST"
				}
			}
			r["conformant"] = false
		},
		"a failing row with no level": func(r map[string]any) {
			for _, row := range r["results"].([]any) {
				if row.(map[string]any)["status"] == "fail" {
					delete(row.(map[string]any), "level")
				}
			}
		},
		"no rows at all": func(r map[string]any) { delete(r, "results") },
	} {
		t.Run(name, func(t *testing.T) {
			report := loadRecordedReport(t, root)
			fails := int(report["counts"].(map[string]any)["fail"].(float64))
			mutate(report)
			m := verdictLine.FindStringSubmatch(runWrapperOn(t, root, report))
			if m[3] != fmt.Sprint(fails) || m[4] != "0" {
				t.Errorf("want fail %d, advisory 0; got fail %s, advisory %s", fails, m[3], m[4])
			}
		})
	}
}
