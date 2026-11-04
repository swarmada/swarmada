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

// Package crdkustomize_test checks that the kustomize install path deploys every
// CRD that `make manifests` generates.
//
// config/crd/bases/kustomization.yaml is HAND-MAINTAINED. controller-gen writes
// the CRD files beside it and never touches it, so a new CRD lands on disk and
// stays undeployed until someone remembers to add a line. Every overlay under
// config/ reaches the CRDs through that one file (config/default, and each
// config/overlays/* through config/default or config/controlstream-tls).
//
// That already happened once. SemanticEvent, SemanticEventSource and
// TaskTrigger were generated and never listed. The manager starts
// TaskTriggerReconciler unconditionally, and its informer on SemanticEvent could
// never sync, so every kustomize install crash-looped about every two minutes.
// The Helm chart was unaffected because hack/sync-helm-crds.sh globs the
// directory and `make helm-verify-sync` guards that copy. This test is the
// same guard for the kustomize copy.
//
// No count is asserted. Adding a CRD needs no edit here, only the one line in
// the kustomization, and this test says which line is missing.
package crdkustomize_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// crdDir is the controller-gen output directory (Makefile: manifests,
// output:crd:artifacts:config=config/crd/bases).
const crdDir = "config/crd/bases"

// repoRoot walks up from the test's working directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate the module root (no go.mod above the test directory)")
		}
		dir = parent
	}
}

// generatedCRDs lists every YAML file in crdDir except the kustomization itself.
// It refuses an empty result: a directory that silently lost its CRDs would
// otherwise match an empty resource list and pass while checking nothing.
func generatedCRDs(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") || e.Name() == "kustomization.yaml" {
			continue
		}
		out = append(out, e.Name())
	}
	if len(out) == 0 {
		t.Fatalf("%s holds no CRD files; run `make manifests`", dir)
	}
	return out
}

// listedResources reads the resources: list of a kustomization file.
func listedResources(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var k struct {
		Resources []string `json:"resources"`
	}
	if err := yaml.Unmarshal(raw, &k); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return k.Resources
}

// drift compares the generated files against the listed resources. missing are
// generated but not listed (the defect this test exists for); extra are listed
// but not generated (a stale entry, which kustomize itself rejects); dup are
// listed more than once.
func drift(generated, listed []string) (missing, extra, dup []string) {
	gen := map[string]bool{}
	for _, g := range generated {
		gen[g] = true
	}
	seen := map[string]int{}
	for _, l := range listed {
		seen[l]++
		if seen[l] == 2 {
			dup = append(dup, l)
		}
		if !gen[l] {
			extra = append(extra, l)
		}
	}
	for _, g := range generated {
		if seen[g] == 0 {
			missing = append(missing, g)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	sort.Strings(dup)
	return missing, extra, dup
}

// TestKustomizationListsEveryGeneratedCRD fails when a CRD that `make manifests`
// generates is not deployed by `kubectl apply -k`.
func TestKustomizationListsEveryGeneratedCRD(t *testing.T) {
	dir := filepath.Join(repoRoot(t), crdDir)
	kust := filepath.Join(dir, "kustomization.yaml")

	missing, extra, dup := drift(generatedCRDs(t, dir), listedResources(t, kust))

	for _, m := range missing {
		t.Errorf("%s is generated but not listed in %s/kustomization.yaml; "+
			"no kustomize overlay installs it. Add `  - %s` to its resources.", m, crdDir, m)
	}
	for _, e := range extra {
		t.Errorf("%s is listed in %s/kustomization.yaml but no such file is generated; "+
			"remove the entry or run `make manifests`.", e, crdDir)
	}
	for _, d := range dup {
		t.Errorf("%s is listed more than once in %s/kustomization.yaml.", d, crdDir)
	}
}

// TestDriftCheckCanFail feeds drift() one instance of each defect it reports. It
// guards against a refactor that turns the check above into one that passes
// while checking nothing.
func TestDriftCheckCanFail(t *testing.T) {
	generated := []string{"a.yaml", "b.yaml", "c.yaml"}
	listed := []string{"a.yaml", "a.yaml", "stale.yaml"}

	missing, extra, dup := drift(generated, listed)

	if got, want := strings.Join(missing, ","), "b.yaml,c.yaml"; got != want {
		t.Errorf("missing = %q, want %q", got, want)
	}
	if got, want := strings.Join(extra, ","), "stale.yaml"; got != want {
		t.Errorf("extra = %q, want %q", got, want)
	}
	if got, want := strings.Join(dup, ","), "a.yaml"; got != want {
		t.Errorf("dup = %q, want %q", got, want)
	}

	// And the agreeing case reports nothing.
	if m, e, d := drift(generated, generated); len(m)+len(e)+len(d) != 0 {
		t.Errorf("agreeing lists reported drift: missing=%v extra=%v dup=%v", m, e, d)
	}
}
