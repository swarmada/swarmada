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
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	swarmadav1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/contract"
)

// A FleetAdapter whose conformance report names a contract version the control plane does
// not support is Passed and still useless: the dispatch gate excludes every robot bound to
// it (RFC-0001 #fleetadapter-status-fields-and-phase-machine, ADR-0032), so a demo built on
// it assigns nothing. Two demos shipped a hand-written report at 1.0.0 after the supported
// range had moved to >=1.2.0, and nothing noticed. These tests read every report a demo or
// sample can hand the controller and ask contract.Supports the question the dispatch gate
// asks.

// reportSources are the trees whose files can carry a conformance report to a FleetAdapter:
// demo scripts, manifests, and the chart.
var reportSources = []string{"examples", "config", "deploy"}

// contractLiteral matches a contract_version key with a string value in JSON, whether
// written plainly or with its quotes escaped inside a shell string.
var contractLiteral = regexp.MustCompile(`\\?"contract_version\\?"\s*:\s*\\?"([^"\\]*)\\?"`)

// conformantLiteral matches a conformant verdict written as a JSON literal.
var conformantLiteral = regexp.MustCompile(`\\?"conformant\\?"\s*:\s*(true|false)`)

func walkTextFiles(t *testing.T, root string, rels []string, fn func(rel string, body []byte)) int {
	t.Helper()
	var n int
	for _, rel := range rels {
		err := filepath.WalkDir(filepath.Join(root, rel), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if name := d.Name(); name == "__pycache__" || name == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			switch filepath.Ext(path) {
			case ".sh", ".yaml", ".yml", ".json", ".py", ".tpl", ".txt":
			default:
				return nil
			}
			body, err := os.ReadFile(path) //nolint:gosec // repo-relative test input
			if err != nil {
				return err
			}
			r, _ := filepath.Rel(root, path)
			n++
			fn(r, body)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", rel, err)
		}
	}
	return n
}

// Every contract_version written into a file a demo or a sample applies must be one the
// control plane accepts.
func TestConformanceReportLiteralsAreInsideTheContractWindow(t *testing.T) {
	root := repoRoot(t)
	var found int
	scanned := walkTextFiles(t, root, reportSources, func(rel string, body []byte) {
		for _, m := range contractLiteral.FindAllSubmatch(body, -1) {
			found++
			if ok, why := contract.Supports(string(m[1])); !ok {
				t.Errorf("%s: a conformance report stamped contract_version %q would be refused by the "+
					"dispatch gate, so every robot bound to its FleetAdapter is withheld from work: %s",
					rel, m[1], why)
			}
		}
	})
	if scanned == 0 {
		t.Fatalf("no files scanned under %v; the check read nothing", reportSources)
	}
	t.Logf("scanned %d files, %d contract_version literals", scanned, found)
}

// No demo or manifest writes a conformance verdict by hand. A report comes from the harness
// (hack/sim-conformance-report.sh), so that the gate it demonstrates passes on a result a
// run produced. A .json file is a report the harness wrote, and is checked by
// TestPinnedHarnessReportsAreSupported instead.
func TestNoDemoWritesAConformanceVerdict(t *testing.T) {
	root := repoRoot(t)
	scanned := walkTextFiles(t, root, []string{"examples", "config"}, func(rel string, body []byte) {
		if filepath.Ext(rel) == ".json" {
			return
		}
		if loc := conformantLiteral.FindIndex(body); loc != nil {
			line := bytes.Count(body[:loc[0]], []byte("\n")) + 1
			t.Errorf("%s:%d writes a conformance verdict (%s) by hand; generate the report with "+
				"hack/sim-conformance-report.sh instead", rel, line, body[loc[0]:loc[1]])
		}
	})
	if scanned == 0 {
		t.Fatalf("no files scanned under examples/ and config/; the check read nothing")
	}
}

type harnessReport struct {
	Adapter         string         `json:"adapter"`
	ContractVersion string         `json:"contract_version"`
	Conformant      *bool          `json:"conformant"`
	Counts          map[string]int `json:"counts"`
	Results         []any          `json:"results"`
}

func readHarnessReport(t *testing.T, path string) (harnessReport, []byte) {
	t.Helper()
	body, err := os.ReadFile(path) //nolint:gosec // repo-relative test input
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var r harnessReport
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("%s is not a harness report: %v", path, err)
	}
	if r.Conformant == nil || len(r.Results) == 0 || r.Counts == nil {
		t.Fatalf("%s lacks the harness's verdict, counts or per-check results; it was not written by the harness", path)
	}
	return r, body
}

// recordedReport is the harness report this repository publishes for the simulation adapter:
// a real harness run, committed. hack/sim-conformance-report.sh copies it when the harness
// cannot run, and the manifest sets below pin byte copies of it. It is NOT
// conformance-reports/_adapter.json: that directory is a local, gitignored output of
// `make conformance-profiles`, absent from a fresh clone, and a fresh run's timings never
// equal a pinned file's bytes.
const recordedReport = "test/samples/testdata/recorded-sim-adapter-report.json"

// The recorded harness report is what a demo uses when the harness cannot run (the
// status-projected CI path). It must still be admitted and dispatchable.
func TestRecordedHarnessReportIsUsable(t *testing.T) {
	path := filepath.Join(repoRoot(t), filepath.FromSlash(recordedReport))
	r, _ := readHarnessReport(t, path)
	if r.Adapter != "simulation" {
		t.Errorf("%s is a report for adapter %q; the demos pin it for the simulation adapter", path, r.Adapter)
	}
	if !*r.Conformant {
		t.Errorf("%s is non-conformant; every demo that falls back to it would show conformance Failed", path)
	}
	if ok, why := contract.Supports(r.ContractVersion); !ok {
		t.Errorf("%s: contract_version %q: %s", path, r.ContractVersion, why)
	}
}

// The demos' no-python fallback, SIM_CONFORMANCE_REPORT=recorded, must hand over exactly the
// recorded report above, in the tree that ships. It used to copy a gitignored file and exit 1
// in every fresh clone, and nothing ran it.
func TestSimConformanceReportFallbackCopiesTheRecordedReport(t *testing.T) {
	root := repoRoot(t)
	_, want := readHarnessReport(t, filepath.Join(root, filepath.FromSlash(recordedReport)))
	out := filepath.Join(t.TempDir(), "report.json")
	cmd := exec.Command("bash", filepath.Join(root, "hack", "sim-conformance-report.sh"), out) //nolint:gosec // fixed repo script
	cmd.Env = append(os.Environ(), "SIM_CONFORMANCE_REPORT=recorded")
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("SIM_CONFORMANCE_REPORT=recorded hack/sim-conformance-report.sh: %v\n%s", err, msg)
	}
	got, err := os.ReadFile(out) //nolint:gosec // test temp file
	if err != nil {
		t.Fatalf("read the script's output: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("SIM_CONFORMANCE_REPORT=recorded wrote %d bytes that are not %s (%d bytes)", len(got), recordedReport, len(want))
	}
}

// A manifest set whose FleetAdapter pins a harness report by digest: the pinned digest, the
// report the kustomization generates the ConfigMap from, and the recorded harness report
// must agree, and the report must be one the control plane dispatches to.
func TestPinnedHarnessReportsAreSupported(t *testing.T) {
	root := repoRoot(t)
	_, recorded := readHarnessReport(t, filepath.Join(root, filepath.FromSlash(recordedReport)))
	for _, set := range []string{
		"config/samples/first-run",
		"config/overlays/demo-webhooks/sim-adapter",
	} {
		t.Run(set, func(t *testing.T) {
			dir := filepath.Join(root, filepath.FromSlash(set))
			r, body := readHarnessReport(t, filepath.Join(dir, "conformance-report.json"))
			if !bytes.Equal(body, recorded) {
				t.Errorf("%s/conformance-report.json differs from %s; "+
					"regenerate it with SIM_CONFORMANCE_REPORT=recorded hack/sim-conformance-report.sh "+
					"and update the digest in fleetadapter.yaml", set, recordedReport)
			}
			fa := pinnedFleetAdapter(t, filepath.Join(dir, "fleetadapter.yaml"))
			if fa.Spec.ConformanceReport == nil {
				t.Fatalf("%s: the FleetAdapter references no conformance report", set)
			}
			sum := sha256.Sum256(body)
			if want := "sha256:" + hex.EncodeToString(sum[:]); fa.Spec.ConformanceReport.Digest != want {
				t.Errorf("%s: fleetadapter.yaml pins %s, but conformance-report.json hashes to %s; the controller would report Failed",
					set, fa.Spec.ConformanceReport.Digest, want)
			}
			kust, err := os.ReadFile(filepath.Join(dir, "kustomization.yaml")) //nolint:gosec // repo-relative test input
			if err != nil {
				t.Fatalf("read kustomization.yaml: %v", err)
			}
			for _, want := range []string{"name: " + fa.Spec.ConformanceReport.ConfigMapRef, "report.json=conformance-report.json"} {
				if !strings.Contains(string(kust), want) {
					t.Errorf("%s/kustomization.yaml does not generate the report ConfigMap the FleetAdapter names (missing %q)", set, want)
				}
			}
			if !*r.Conformant {
				t.Errorf("%s: the report is non-conformant", set)
			}
			if ok, why := contract.Supports(r.ContractVersion); !ok {
				t.Errorf("%s: the report's contract_version %q: %s", set, r.ContractVersion, why)
			}
		})
	}
}

// pinnedFleetAdapter returns the FleetAdapter document in a file that may hold others.
func pinnedFleetAdapter(t *testing.T, path string) *swarmadav1.FleetAdapter {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // repo-relative test input
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, doc := range strings.Split(string(raw), "\n---") {
		var fa swarmadav1.FleetAdapter
		if yaml.Unmarshal([]byte(doc), &fa) == nil && fa.Kind == "FleetAdapter" {
			return &fa
		}
	}
	t.Fatalf("%s holds no FleetAdapter", path)
	return nil
}
