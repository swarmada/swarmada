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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Three operator-facing messages were corrected so that each says what to do, or what the code
// does, and promises no later change. Nothing pinned them, so a revert stayed green. This pins the
// load-bearing clause of each in its source.
func TestOperatorMessages_PinnedInSource(t *testing.T) {
	pins := []struct{ file, clause string }{
		{filepath.Join("..", "contract", "version.go"),
			"move the adapter to a build that declares a contract version in that range"},
		{filepath.Join("..", "controller", "robot_controller.go"),
			"is removed only with its namespace: no command decommissions one robot"},
		{filepath.Join("robot_delete_guard.go"),
			"command decommissions one robot. A delete is admitted while the namespace itself is being deleted"},
	}
	for _, p := range pins {
		data, err := os.ReadFile(p.file)
		if err != nil {
			t.Fatalf("reading %s: %v", p.file, err)
		}
		// Go string concatenation splits a message across lines; join the literal pieces first.
		src := strings.ReplaceAll(strings.ReplaceAll(string(data), "\"+\n", ""), "\t\t\t\"", "")
		if !strings.Contains(src, p.clause) {
			t.Errorf("%s no longer carries the clause %q", p.file, p.clause)
		}
	}
}
