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

// Package samples_test validates that every checked-in example manifest still
// decodes against the CURRENT api/v1 types.
//
// It exists because a sample manifest is documentation that nothing compiles. A
// field renamed in api/v1 leaves every YAML naming the old field silently wrong
// — it keeps parsing as YAML, keeps looking plausible to a reader, and only
// fails when someone applies it against a live cluster. The 2026-07-23
// FleetTask/FleetAction rename did exactly that to a set of demo manifests,
// which stated `kind: FleetTask` with `spec.type`/`spec.zone`/`spec.priority` —
// the shape that is now FleetActionSpec — and nothing in CI noticed.
//
// The guard has two halves, because neither alone is sufficient:
//
//   - STRICT DECODING catches a field that no longer exists on the type. This is
//     what a rename produces, and it is invisible to a plain YAML parse.
//   - STRUCTURAL CHECKS catch a REQUIRED field that is absent. Strict decoding
//     cannot see these: an absent field decodes to the Go zero value without
//     error, so a FleetTask with no spec.actions at all decodes cleanly while
//     violating the CRD's own MinItems=1.
package samples_test

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	swarmadav1 "github.com/swarmada/swarmada/api/v1"
)

// manifestRoots are the repo-relative directories whose YAML documents must
// decode against api/v1. Each is REQUIRED to exist: a root that silently
// vanished would turn this suite green while checking nothing.
//
// To bring another manifest set under the guard, add its directory here. Only
// directories that ship in this repository belong on the list.
var manifestRoots = []string{
	"config/samples",
}

// document is one YAML document from one file, kept with enough provenance that
// a failure names the exact document a reader has to open.
type document struct {
	path  string // repo-relative
	index int    // 0-based document index within the file
	raw   []byte
}

func (d document) String() string { return fmt.Sprintf("%s[doc %d]", d.path, d.index) }

// repoRoot walks up from the test's working directory to the module root.
// Resolving it rather than hard-coding "../.." keeps the test working if the
// package is ever moved.
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

// strictDecoder decodes YAML into api/v1 types and REJECTS unknown fields.
// Strictness is the whole point: a permissive decode would accept a manifest
// naming fields the API dropped, which is precisely the drift being guarded.
func strictDecoder(t *testing.T) runtime.Decoder {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := swarmadav1.AddToScheme(scheme); err != nil {
		t.Fatalf("register swarmada scheme: %v", err)
	}
	return json.NewSerializerWithOptions(
		json.DefaultMetaFactory, scheme, scheme,
		json.SerializerOptions{Yaml: true, Strict: true},
	)
}

// isEmptyDoc reports whether a YAML document carries no content — a file's
// leading comment block before the first "---" splits out as one such document.
func isEmptyDoc(raw []byte) bool {
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		return false
	}
	return true
}

// collectDocuments gathers every YAML document under the configured roots,
// sorted for deterministic subtest order (docs/testing.md: Determinism).
func collectDocuments(t *testing.T) []document {
	t.Helper()
	root := repoRoot(t)

	var files []string
	for _, rel := range manifestRoots {
		dir := filepath.Join(root, rel)
		info, err := os.Stat(dir)
		if err != nil {
			// A configured root that does not exist is a FAILURE, not a skip.
			// Skipping would let this suite pass while guarding nothing.
			t.Fatalf("manifest root %q does not exist: %v", rel, err)
		}
		if !info.IsDir() {
			t.Fatalf("manifest root %q is not a directory", rel)
		}
		err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if ext := strings.ToLower(filepath.Ext(path)); ext == ".yaml" || ext == ".yml" {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %q: %v", rel, err)
		}
	}
	sort.Strings(files)

	if len(files) == 0 {
		t.Fatalf("no YAML files found under %v — the guard would be vacuous", manifestRoots)
	}

	var docs []document
	for _, path := range files {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		f, err := os.Open(path) //nolint:gosec // repo-relative test fixture
		if err != nil {
			t.Fatalf("open %s: %v", rel, err)
		}
		reader := utilyaml.NewYAMLReader(bufio.NewReader(f))
		for i := 0; ; i++ {
			raw, err := reader.Read()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				_ = f.Close()
				t.Fatalf("split %s: %v", rel, err)
			}
			if isEmptyDoc(raw) {
				continue
			}
			docs = append(docs, document{path: rel, index: i, raw: raw})
		}
		_ = f.Close()
	}
	return docs
}

// decodeAll strict-decodes every document, reporting each failure against the
// document that caused it, and returns the objects that decoded.
func decodeAll(t *testing.T, docs []document) []runtime.Object {
	t.Helper()
	dec := strictDecoder(t)
	objs := make([]runtime.Object, 0, len(docs))
	for _, d := range docs {
		obj, _, err := dec.Decode(d.raw, nil, nil)
		if err != nil {
			t.Errorf("%s does not decode against api/v1: %v", d, err)
			continue
		}
		objs = append(objs, obj)
	}
	return objs
}

// TestSamplesDecodeAgainstAPI is the drift guard: every sample document must
// decode into a registered api/v1 kind with no unknown fields. A field renamed
// or removed from api/v1 fails here, on the document that still names it.
func TestSamplesDecodeAgainstAPI(t *testing.T) {
	docs := collectDocuments(t)
	dec := strictDecoder(t)

	for _, d := range docs {
		t.Run(d.String(), func(t *testing.T) {
			if _, _, err := dec.Decode(d.raw, nil, nil); err != nil {
				t.Fatalf("does not decode against api/v1: %v", err)
			}
		})
	}
}

// TestSamplesStructuralInvariants covers what strict decoding structurally
// cannot: a REQUIRED field that is simply absent decodes to the Go zero value
// without error. These assertions mirror the kubebuilder markers on the types,
// so a sample that would be rejected by the API server is rejected here too,
// without needing a cluster.
func TestSamplesStructuralInvariants(t *testing.T) {
	for _, obj := range decodeAll(t, collectDocuments(t)) {
		switch o := obj.(type) {
		case *swarmadav1.FleetTask:
			checkFleetTask(t, o)
		case *swarmadav1.FleetAction:
			// +kubebuilder:validation:Required on FleetActionSpec.Type — the only
			// field the spec marks required.
			if o.Spec.Type == "" {
				t.Errorf("FleetAction %q: spec.type is required and empty", o.Name)
			}
		}
	}
}

// checkFleetTask asserts the composite invariants the CRD declares but a Go zero
// value hides: at least one member, unique non-empty member names, and dependsOn
// edges that name members that exist and are not the member itself.
func checkFleetTask(t *testing.T, task *swarmadav1.FleetTask) {
	t.Helper()

	// +kubebuilder:validation:MinItems=1 on FleetTaskSpec.Actions.
	if len(task.Spec.Actions) == 0 {
		t.Errorf("FleetTask %q: spec.actions is required and empty (MinItems=1)", task.Name)
		return
	}

	// +listType=map +listMapKey=name — names must be present and unique.
	seen := make(map[string]bool, len(task.Spec.Actions))
	for i, member := range task.Spec.Actions {
		switch {
		case member.Name == "":
			t.Errorf("FleetTask %q: spec.actions[%d].name is empty", task.Name, i)
		case seen[member.Name]:
			t.Errorf("FleetTask %q: duplicate member name %q", task.Name, member.Name)
		}
		seen[member.Name] = true

		if member.Action.Type == "" {
			t.Errorf("FleetTask %q member %q: action.type is required and empty", task.Name, member.Name)
		}
	}

	// dependsOn must name existing members and must not self-reference; admission
	// enforces this, and a sample that violates it would be rejected on apply.
	for _, member := range task.Spec.Actions {
		for _, dep := range member.DependsOn {
			if dep == member.Name {
				t.Errorf("FleetTask %q member %q depends on itself", task.Name, member.Name)
			}
			if !seen[dep] {
				t.Errorf("FleetTask %q member %q depends on %q, which is not a member",
					task.Name, member.Name, dep)
			}
		}
	}
}

// TestSamplesIncludeMultiMemberFleetTask is the ITEM-0017 assertion: the sample
// set must ship a composite a reader can open, copy and adapt — and it must have
// more than one member. A single-member FleetTask never exercises dependsOn,
// startCondition, or a partial actionSummary, which are the behaviours that
// distinguish a composite from a standalone FleetAction.
func TestSamplesIncludeMultiMemberFleetTask(t *testing.T) {
	var tasks []*swarmadav1.FleetTask
	for _, obj := range decodeAll(t, collectDocuments(t)) {
		if task, ok := obj.(*swarmadav1.FleetTask); ok {
			tasks = append(tasks, task)
		}
	}

	if len(tasks) == 0 {
		t.Fatalf("no FleetTask sample found under %v — RFC-0001 §9.1.5 defines the composite, "+
			"but `kubectl apply -f config/samples/` would create none", manifestRoots)
	}

	for _, task := range tasks {
		if len(task.Spec.Actions) > 1 {
			return // satisfied
		}
	}

	names := make([]string, 0, len(tasks))
	for _, task := range tasks {
		names = append(names, fmt.Sprintf("%s(%d member)", task.Name, len(task.Spec.Actions)))
	}
	sort.Strings(names)
	t.Fatalf("every FleetTask sample has a single member: %s — none exercises dependsOn, "+
		"startCondition, or a partial actionSummary", strings.Join(names, ", "))
}
