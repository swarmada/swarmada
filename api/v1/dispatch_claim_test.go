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

package v1

// The executor godoc states a v0.4 fact, and this test is what makes it
// checkable rather than a sentence that is true on the day it is written.
//
// ExecutorBinding's godoc once said "Http and Mcp are implemented; Grpc is
// specified, not implemented" — false in both directions, while RFC-0001
// said the opposite, correctly. Godoc on this package is the generated CRD field
// documentation and `kubectl explain` output, so the false claim was the one an
// operator would read. The replacement is RFC-0001's own sentence, verbatim, and
// this test holds it to two things:
//
//  1. RFC-0001 still says it. The spec is what a dispatcher's author must
//     update; when they do, this test fails until the godoc follows. One fact,
//     one wording, coupled.
//  2. No production code dials the compute runtime. internal/computeclient
//     exists and is tested, but nothing outside it calls computeclient.Dial; the
//     first caller that does falsifies the claim, and this test says so.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// noDispatchLead and noDispatchBody are RFC-0001's sentence, from the
// FleetAction maturity note; the lead also opens the ComputeEndpoint one. Read
// from rfcs/dist/RFC-0001-core-spec.md, the normative rendering (AGENTS.md).
const (
	noDispatchLead = "There is no dispatch path at v0.4."
	noDispatchBody = "Nothing in the reference control plane resolves an endpoint address, " +
		"activates a run, or calls the compute runtime service"
)

var spaces = regexp.MustCompile(`\s+`)

func normalize(s string) string { return strings.TrimSpace(spaces.ReplaceAllString(s, " ")) }

// godocSites returns, for each declaration that carries the claim, its doc
// comment text: the two types (Go doc) and the two fields (the CRD
// descriptions `kubectl explain` prints — a field's own comment, not its
// type's, is what reaches the CRD).
func godocSites(t *testing.T) map[string]string {
	t.Helper()
	want := map[string]bool{
		"ExecutorBinding": true, "ActionExecutor": true,
		"ComputeEndpointSpec.Binding": true, "FleetActionSpec.Executor": true,
	}
	got := map[string]string{}
	fset := token.NewFileSet()
	for _, file := range []string{"fleetaction_types.go", "computeendpoint_types.go"} {
		f, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts := spec.(*ast.TypeSpec)
				if want[ts.Name.Name] {
					got[ts.Name.Name] = gd.Doc.Text()
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				for _, fld := range st.Fields.List {
					for _, n := range fld.Names {
						if key := ts.Name.Name + "." + n.Name; want[key] {
							got[key] = fld.Doc.Text()
						}
					}
				}
			}
		}
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Fatalf("declaration %s not found — renamed? Update this test with it, do not drop it", k)
		}
	}
	return got
}

func TestNoDispatchClaim_GodocCarriesRFCWording(t *testing.T) {
	for site, doc := range godocSites(t) {
		d := normalize(doc)
		if !strings.Contains(d, noDispatchLead+" "+noDispatchBody) {
			t.Errorf("%s's godoc does not carry RFC-0001's sentence %q %q; got:\n%s",
				site, noDispatchLead, noDispatchBody, doc)
		}
		for _, stale := range []string{"are implemented", "dispatches this action"} {
			if strings.Contains(d, stale) {
				t.Errorf("%s's godoc still says %q, which RFC-0001 contradicts", site, stale)
			}
		}
	}
}

func TestNoDispatchClaim_RFCStillSaysIt(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "rfcs", "dist", "RFC-0001-core-spec.md"))
	if err != nil {
		t.Fatalf("read RFC-0001: %v", err)
	}
	// Strip blockquote markers and bold so the sentence matches across the
	// RFC's line wrapping.
	text := strings.ReplaceAll(string(raw), "**", "")
	text = normalize(regexp.MustCompile(`(?m)^>\s?`).ReplaceAllString(text, ""))
	want := noDispatchLead + " " + noDispatchBody
	if !strings.Contains(text, want) {
		t.Fatalf("RFC-0001 no longer says %q. If a dispatch path has landed, the api/v1 godoc "+
			"that repeats it (ExecutorBinding, ActionExecutor, ComputeEndpointSpec.Binding, "+
			"FleetActionSpec.Executor) is now false: correct it to the RFC's new wording and "+
			"update this test", want)
	}
}

func TestNoDispatchClaim_NothingDialsTheComputeRuntime(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Fatalf("go toolchain not on PATH: this check lists production packages with `go list` "+
			"and must not pass by not running (%v)", err)
	}
	cmd := exec.Command("go", "list", "-f", `{{$d := .Dir}}{{range .GoFiles}}{{$d}}/{{.}}{{"\n"}}{{end}}`, "./...")
	cmd.Dir = filepath.Join("..", "..")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list ./...: %v", err)
	}
	files := strings.Fields(string(out))
	if len(files) < 100 {
		t.Fatalf("go list returned %d production files; this check cannot have looked", len(files))
	}
	var callers []string
	for _, f := range files {
		if strings.Contains(filepath.ToSlash(f), "/internal/computeclient/") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if strings.Contains(string(b), "computeclient.Dial(") {
			callers = append(callers, f)
		}
	}
	if len(callers) > 0 {
		t.Fatalf("production code dials the compute runtime (%v), so %q is no longer true. "+
			"Correct RFC-0001 (the FleetAction and ComputeEndpoint maturity notes) and the api/v1 godoc "+
			"that repeats it together", callers, noDispatchLead)
	}
}
