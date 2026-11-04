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

// The registry test used to assert a literal resource count, with a hand-written
// table that gave `ft` to FleetAction. Both were true of the tree and both were
// wrong, and because they were assertions the bug could not be fixed without the
// test failing: the test was evidence FOR the omission.
//
// That was fixed by raising the literal and correcting the table. It is a better
// number than the one before it, but it is the same KIND of assertion — a count
// and a transcription, maintained by hand, that agree with the registry because
// someone updated both. Nothing about it is derived from the thing it describes.
//
// So these tests assert no count at all. They read the +kubebuilder:resource
// markers out of api/v1 — the CRD's own declaration of its kind and short name,
// the same source `kubectl get <short>` resolves against — and check the registry
// against that. A count cannot pin an omission if no count is written down.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// crdKind is one CRD as the cluster will serve it: the kind, and the names the
// API server resolves. Both halves are read from the tree rather than typed here.
type crdKind struct {
	kind       string
	singular   string
	plural     string
	shortNames []string
	file       string
}

// crdFile is the sliver of a generated CustomResourceDefinition this test reads.
// A local struct rather than k8s.io/apiextensions-apiserver, which is an indirect
// dependency and would become a direct one for four string fields.
type crdFile struct {
	Spec struct {
		Names struct {
			Kind       string   `json:"kind"`
			Singular   string   `json:"singular"`
			Plural     string   `json:"plural"`
			ShortNames []string `json:"shortNames"`
		} `json:"names"`
	} `json:"spec"`
}

var (
	resourceMarker = regexp.MustCompile(`^//\s*\+kubebuilder:resource:(.*)$`)
	typeDecl       = regexp.MustCompile(`^type\s+([A-Za-z0-9_]+)\s+struct\b`)
)

// markedKindsInAPIV1 returns every root type in api/v1 carrying a
// +kubebuilder:resource marker. That marker is what makes a type a CRD, so this
// is the list the CLI's coverage is measured against.
func markedKindsInAPIV1(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join("..", "..", "api", "v1")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		lines := strings.Split(string(b), "\n")
		for i, line := range lines {
			if resourceMarker.FindStringSubmatch(strings.TrimSpace(line)) == nil {
				continue
			}
			kind := ""
			for j := i + 1; j < len(lines) && j < i+40; j++ {
				if td := typeDecl.FindStringSubmatch(lines[j]); td != nil {
					kind = td[1]
					break
				}
			}
			if kind == "" {
				t.Fatalf("%s:%d: +kubebuilder:resource marker with no type declaration under it",
					e.Name(), i+1)
			}
			out = append(out, kind)
		}
	}
	if len(out) == 0 {
		t.Fatal("no +kubebuilder:resource markers found in api/v1 — this test is reading the wrong " +
			"directory and would pass vacuously")
	}
	sort.Strings(out)
	return out
}

// declaredCRDs reads the generated CRDs. controller-gen derives them from the
// api/v1 markers, and the API server serves exactly these names — so the plural,
// singular and shortNames here are the spellings `kubectl` resolves, not a guess.
// The count is cross-checked against the markers so a kind that failed to
// regenerate cannot quietly drop out of this test's denominator.
func declaredCRDs(t *testing.T) []crdKind {
	t.Helper()
	dir := filepath.Join("..", "..", "config", "crd", "bases")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var out []crdKind
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".yaml") || e.Name() == "kustomization.yaml" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		var c crdFile
		if err := yaml.Unmarshal(b, &c); err != nil {
			t.Fatalf("parsing %s: %v", e.Name(), err)
		}
		if c.Spec.Names.Kind == "" {
			t.Fatalf("%s: no spec.names.kind", e.Name())
		}
		out = append(out, crdKind{
			kind: c.Spec.Names.Kind, singular: c.Spec.Names.Singular,
			plural: c.Spec.Names.Plural, shortNames: c.Spec.Names.ShortNames, file: e.Name(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].kind < out[j].kind })

	marked := markedKindsInAPIV1(t)
	generated := make([]string, len(out))
	for i := range out {
		generated[i] = out[i].kind
	}
	if strings.Join(marked, ",") != strings.Join(generated, ",") {
		t.Fatalf("api/v1 markers and config/crd/bases disagree about which kinds exist.\n"+
			"  markers:   %v\n  generated: %v\n"+
			"Run `make generate manifests`. Until they agree, nothing derived from either is "+
			"trustworthy.", marked, generated)
	}
	return out
}

// TestRegistryMatchesAPIV1Declarations checks every registered resource against
// the marker api/v1 declares for its kind: the short name, the four spellings, and
// that the kind is a real CRD rather than an invented entry. No count is asserted,
// so nothing here has to be edited when a CRD is added — which is the point.
func TestRegistryMatchesAPIV1Declarations(t *testing.T) {
	declared := map[string]crdKind{}
	for _, d := range declaredCRDs(t) {
		declared[d.kind] = d
	}

	seen := map[string]bool{}
	for _, r := range resourceOrder {
		if seen[r.kind] {
			t.Errorf("kind %s is registered more than once", r.kind)
		}
		seen[r.kind] = true

		d, ok := declared[r.kind]
		if !ok {
			t.Errorf("the registry offers kind %s, which no +kubebuilder:resource marker in api/v1 "+
				"declares — `swarmctl get %s` addresses a CRD the API server does not serve",
				r.kind, r.singular)
			continue
		}
		// The short name is the CRD's own, not a spelling the CLI chose. This is
		// the assertion the `ft` collision violated: `kubectl get ft` and
		// `swarmctl get ft` resolved to different kinds.
		for _, short := range d.shortNames {
			found := false
			for _, a := range r.aliases {
				if a == short {
					found = true
				}
			}
			if !found {
				t.Errorf("%s declares shortName %q (%s), but the registry gives it aliases %v — "+
					"`kubectl get %s` and `swarmctl get %s` would resolve to different kinds",
					r.kind, short, d.file, r.aliases, short, short)
			}
		}
		if r.singular != d.singular {
			t.Errorf("%s: singular = %q, want %q (the name the API server serves, %s)",
				r.kind, r.singular, d.singular, d.file)
		}
		if r.plural != d.plural {
			t.Errorf("%s: plural = %q, want %q (the name the API server serves, %s)",
				r.kind, r.plural, d.plural, d.file)
		}
		// Every spelling resolves back to this kind, so a kind cannot be
		// half-registered the way FleetTask was.
		for _, spelling := range append([]string{r.plural, r.singular, strings.ToLower(r.kind)}, r.aliases...) {
			def, err := resolveResource(spelling)
			if err != nil {
				t.Errorf("%s: spelling %q does not resolve: %v", r.kind, spelling, err)
				continue
			}
			if def.kind != r.kind {
				t.Errorf("%s: spelling %q resolves to %s", r.kind, spelling, def.kind)
			}
		}
	}
}

// TestRegistryCoversEveryCRDInAPIV1 is the coverage obligation the count used to
// stand in for: every CRD api/v1 declares is addressable by the CLI.
//
// It is SKIPPED and it does not pass. See the skip message.
func TestRegistryCoversEveryCRDInAPIV1(t *testing.T) {
	t.Skip("the registry does not offer every CRD api/v1 declares — the kinds in " +
		"pendingRegistration are unaddressable by the CLI. Whether the registry is meant to " +
		"cover every CRD or a named operator-facing subset is undecided; this test asserts the " +
		"former and stays red until that decision is recorded. Un-skip it when those kinds are " +
		"registered, or replace it with an assertion against the named subset if they are " +
		"deliberately out of scope. Do NOT close it by writing a number down.")

	registered := map[string]bool{}
	for _, r := range resourceOrder {
		registered[r.kind] = true
	}
	var missing []string
	for _, d := range declaredCRDs(t) {
		if !registered[d.kind] {
			missing = append(missing, d.kind+" (shortNames="+strings.Join(d.shortNames, ",")+", "+d.file+")")
		}
	}
	if len(missing) > 0 {
		t.Errorf("api/v1 declares %d CRDs the registry does not offer:\n  %s\n"+
			"Each is a kind `kubectl get` can address and `swarmctl get` cannot.",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// pendingRegistration names the CRDs that api/v1 declares and the registry does
// not offer. It is a deferral list, not an allowance:
// TestRegistryOffersEveryCRDNotPendingRegistration fails if anything ELSE is
// missing, and fails again if a kind listed here becomes registered, so the list
// can only shrink.
//
// All five are deferred for one reason, and it is the only honest one: nothing
// records whether swarmctl's registry is meant to cover every CRD or a named
// operator-facing subset, so neither registering them nor excluding them can be
// called correct yet. They are consistently absent rather than half-registered —
// no alias, no partial entry, and no document promises a `swarmctl get` for any of
// them — which is what distinguishes this from the FleetTask omission, where the
// CLI half-knew the kind and the docs promised it.
//
// This is not a count in names' clothing. A count says "this many is correct" and
// cannot say why; this says which kinds are absent and that their absence is
// unresolved. Remove an entry when its kind is registered; remove the list, and
// un-skip TestRegistryCoversEveryCRDInAPIV1, when the question is settled.
var pendingRegistration = []string{
	"ArtifactStore",
	"ComputeEndpoint",
	"ComputeWorkloadTemplate",
	"EdgeNode",
	"ResultSink",
}

// TestRegistryOffersEveryCRDNotPendingRegistration is the assertion that replaces
// the count.
//
// The count's real job was to notice when a CRD stopped being addressable. It did
// that exactly once, for FleetTask, and then defended the omission for as long as
// the number matched. This notices the same thing by name: un-registering a kind
// fails with that kind's name rather than with an arithmetic mismatch, and adding
// a CRD to api/v1 fails until someone decides whether the CLI serves it. Neither
// failure can be silenced by editing a number.
func TestRegistryOffersEveryCRDNotPendingRegistration(t *testing.T) {
	registered := map[string]bool{}
	for _, r := range resourceOrder {
		registered[r.kind] = true
	}
	deferred := map[string]bool{}
	for _, k := range pendingRegistration {
		deferred[k] = true
	}
	declared := map[string]bool{}
	for _, d := range declaredCRDs(t) {
		declared[d.kind] = true
		if registered[d.kind] {
			continue
		}
		if deferred[d.kind] {
			t.Logf("%s (%s) is declared in api/v1 and not registered — deferred, see "+
				"pendingRegistration", d.kind, d.file)
			continue
		}
		t.Errorf("%s is declared in api/v1 (%s) but is not in swarmctl's registry, and is not on "+
			"the pendingRegistration list. `kubectl get %s` works and `swarmctl get %s` does not. "+
			"Either register it, or add it to pendingRegistration with the reason it is deferred.",
			d.kind, d.file, d.plural, d.plural)
	}
	for _, kind := range pendingRegistration {
		if registered[kind] {
			t.Errorf("pendingRegistration still defers %s, but it IS registered now — remove the "+
				"entry, or the list starts accumulating history instead of obligations", kind)
		}
		if !declared[kind] {
			t.Errorf("pendingRegistration defers %s, but api/v1 no longer declares it — the entry "+
				"outlived the kind", kind)
		}
	}
}
