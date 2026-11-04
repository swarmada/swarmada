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

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoDefaultTrueBoolWithOmitempty is the recurrence gate for a defect this API group
// grew five times before anyone noticed (ADR-0051, ADR-0052).
//
// A non-pointer bool with `omitempty` and +kubebuilder:default=true CANNOT BE SET TO FALSE.
// encoding/json drops the zero value on the way out, so the field is absent in the
// serialized object, and the API server's structural defaulting puts the default back on the
// way in. The operator's `false` is discarded and no error is reported anywhere — the write
// succeeds and the stored object says the opposite of the manifest.
//
// api-principles.md, "Field conventions": an optional boolean is a pointer, so that unset is
// distinguishable from a meaningful zero. For a default-TRUE boolean, false is precisely the
// value the operator has to be able to express, and it is the only one the bare type eats.
//
// This is checked mechanically rather than by review because every one of the five instances
// passed review. If this test fails, make the field a *bool and give it an accessor that
// resolves nil to the schema default — do not silence it by dropping the marker.
func TestNoDefaultTrueBoolWithOmitempty(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("globbing api/v1: %v", err)
	}

	var offenders []string
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") || strings.HasPrefix(path, "zz_generated") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}

		ast.Inspect(file, func(n ast.Node) bool {
			st, ok := n.(*ast.StructType)
			if !ok || st.Fields == nil {
				return true
			}
			for _, f := range st.Fields.List {
				// Bare `bool` only. A *bool is the correct shape and a named bool type
				// would not be defaulted this way.
				ident, ok := f.Type.(*ast.Ident)
				if !ok || ident.Name != "bool" {
					continue
				}
				if f.Tag == nil || !strings.Contains(f.Tag.Value, "omitempty") {
					continue
				}
				if f.Doc == nil {
					continue
				}
				var markers strings.Builder
				for _, c := range f.Doc.List {
					markers.WriteString(c.Text)
					markers.WriteString("\n")
				}
				if !strings.Contains(markers.String(), "+kubebuilder:default=true") {
					continue
				}
				name := "<embedded>"
				if len(f.Names) > 0 {
					name = f.Names[0].Name
				}
				offenders = append(offenders,
					fset.Position(f.Pos()).String()+" "+name)
			}
			return true
		})
	}

	if len(offenders) > 0 {
		t.Errorf("%d field(s) use `bool` + `omitempty` + `+kubebuilder:default=true`, "+
			"which cannot be set to false:\n  %s\n\nMake each a *bool with an accessor "+
			"resolving nil to true (see SwarmadaSigningConfig.SignatureVerificationRequired).",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}
