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

package webhook

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// No admission webhook writes the audit chain. A webhook sees a request before the API server
// has stored anything, and sees it again for a server-side dry run or an update another check
// refuses, so an entry it wrote would record a change that did not happen; and any caller that
// reaches the webhook port can send it a request (RFC-0001 #safety-required-events: an entry
// records an event that occurred). The controllers that observe stored objects write the
// entries. This pins it on the source: no type in this package holds an audit recorder, and
// the manager wires none into one.

const auditImportPath = "github.com/swarmada/swarmada/internal/audit"

// importName returns the name file uses for path, or "" when it does not import it.
func importName(file *ast.File, path string) string {
	for _, imp := range file.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == path {
			if imp.Name != nil {
				return imp.Name.Name
			}
			return path[strings.LastIndex(path, "/")+1:]
		}
	}
	return ""
}

func isSelector(e ast.Expr, pkg, name string) bool {
	s, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := s.X.(*ast.Ident)
	return ok && x.Name == pkg && s.Sel.Name == name
}

func TestNoAdmissionWebhookHoldsAnAuditRecorder(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		checked++
		pkg := importName(f, auditImportPath)
		if pkg == "" {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			field, ok := n.(*ast.Field)
			if ok && isSelector(field.Type, pkg, "Recorder") {
				t.Errorf("%s holds an audit recorder: an admission webhook would write the audit chain "+
					"for a request the API server never stored", fset.Position(field.Pos()))
			}
			return true
		})
	}
	if checked < 20 {
		t.Fatalf("read %d source files in internal/webhook; the scan is not looking at the package", checked)
	}

	// The manager's wiring: no webhook value is given an audit recorder.
	mainGo := filepath.Join("..", "..", "cmd", "manager", "main.go")
	f, err := parser.ParseFile(fset, mainGo, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", mainGo, err)
	}
	whPkg := importName(f, "github.com/swarmada/swarmada/internal/webhook")
	if whPkg == "" {
		t.Fatalf("%s does not import the webhook package; the scan is not looking at the wiring", mainGo)
	}
	literals := 0
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		s, ok := lit.Type.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if x, ok := s.X.(*ast.Ident); !ok || x.Name != whPkg {
			return true
		}
		literals++
		for _, el := range lit.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Audit" {
					t.Errorf("%s gives a webhook an audit recorder", fset.Position(kv.Pos()))
				}
			}
		}
		return true
	})
	if literals < 10 {
		t.Fatalf("found %d webhook values in %s; the scan is not looking at the wiring", literals, mainGo)
	}
}
