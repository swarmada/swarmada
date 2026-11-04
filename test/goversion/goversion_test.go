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

// Package goversion_test checks that every Go minimum the documentation states
// is the one a go.mod actually requires.
//
// The documents drifted three ways at once: CONTRIBUTING.md said Go 1.22+, the
// README said 1.25+, docs/quickstart.md said 1.26+, and the go directives read
// 1.25.0 (the control plane) and 1.26 (tools/swarmtop, its own module). A reader
// who followed CONTRIBUTING could not build swarmtop, and nothing noticed.
//
// A document states one of two minimums, and docScope says which:
//
//   - "root": the go directive of the root go.mod. Right for a document about
//     the control plane alone.
//   - "tree": the highest go directive of any go.mod in the tree. Right for a
//     document about the whole development environment, which builds every module.
//
// A document that states a Go minimum and is not in docScope fails, so a new one
// is classified when it is written rather than when it has drifted.
package goversion_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// docScope maps each document that states a Go minimum to the minimum it states.
var docScope = map[string]string{
	"README.md":                "root", // "Run from source": the control plane only
	"docs/coding-standards.md": "root", // "for the control plane, CRD types, scheduler, and CLI"
	"CONTRIBUTING.md":          "tree", // the full development environment
	"docs/quickstart.md":       "tree",
}

// docRoots are the parts of the tree whose Markdown is read.
var docRoots = []string{".", "docs", "examples", "tools", "deploy", "config", "adapters"}

var (
	goDirective = regexp.MustCompile(`(?m)^go\s+(\d+)\.(\d+)`)
	// "Go 1.25+", "**Go 1.25+**" and "go 1.25+"; the "+" is what makes it a minimum.
	statedMinimum = regexp.MustCompile(`\b[Gg]o\s+(1\.\d+)\+`)
)

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
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}

// skipDir reports directories that hold no document or module of this tree:
// hidden directories, and adapters/external, which the fetch script fills with
// other repositories and which is not checked in.
func skipDir(rel string, d fs.DirEntry) bool {
	name := d.Name()
	return rel != "." && (strings.HasPrefix(name, ".") || name == "node_modules" ||
		filepath.ToSlash(rel) == "adapters/external")
}

// minorOf reads the go directive of a go.mod as its minor version.
func minorOf(t *testing.T, path string) (string, int) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m := goDirective.FindSubmatch(b)
	if m == nil {
		t.Fatalf("%s has no go directive", path)
	}
	minor := 0
	for _, c := range m[2] {
		minor = minor*10 + int(c-'0')
	}
	return string(m[1]) + "." + string(m[2]), minor
}

func TestDocumentedGoMinimumMatchesGoMod(t *testing.T) {
	root := repoRoot(t)

	rootVersion, _ := minorOf(t, filepath.Join(root, "go.mod"))
	treeVersion, treeMinor, treeFrom := rootVersion, -1, "go.mod"
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			if skipDir(rel, d) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == "go.mod" {
			if v, minor := minorOf(t, path); minor > treeMinor {
				treeVersion, treeMinor, treeFrom = v, minor, filepath.ToSlash(rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	want := map[string]string{"root": rootVersion, "tree": treeVersion}
	from := map[string]string{"root": "go.mod", "tree": treeFrom}

	seen := map[string]bool{}
	for _, dr := range docRoots {
		base := filepath.Join(root, dr)
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			if d.IsDir() {
				if (dr == "." && path != base) || skipDir(rel, d) {
					return filepath.SkipDir
				}
				return nil
			}
			rel = filepath.ToSlash(rel)
			if !strings.HasSuffix(rel, ".md") || seen[rel] {
				return nil
			}
			seen[rel] = true
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for i, line := range strings.Split(string(b), "\n") {
				for _, m := range statedMinimum.FindAllStringSubmatch(line, -1) {
					scope, ok := docScope[rel]
					if !ok {
						t.Errorf("%s:%d states Go %s+ but is not in docScope; add it as \"root\" (%s, from go.mod) or \"tree\" (%s, from %s)",
							rel, i+1, m[1], rootVersion, treeVersion, treeFrom)
						continue
					}
					if m[1] != want[scope] {
						t.Errorf("%s:%d states Go %s+; its scope is %q, and %s requires %s",
							rel, i+1, m[1], scope, from[scope], want[scope])
					}
				}
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("walk %s: %v", dr, err)
		}
	}
}
