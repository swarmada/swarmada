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

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swarmada/swarmada/internal/cli"
)

// TestCommandTreeSurface pins the exact command tree. A command added or removed
// without updating this list fails here, which is what makes the documentation
// gate below trustworthy: a lint that resolves documented spellings against a
// tree nobody asserts can go green because a command silently disappeared and
// the document was never updated either.
func TestCommandTreeSurface(t *testing.T) {
	root, _ := newRootCommand(cli.IOStreams{})
	want := []string{
		"admit",
		"cancel",
		"delete",
		"delete robot",
		"describe",
		"estop",
		"estop resume",
		"estop trigger",
		"estop-clear",
		"export",
		"export audit",
		"get",
		"get audit",
		"modelpolicy",
		"modelpolicy reset",
		"robotclass",
		"robotclass rollout",
		"rollout",
		"rollout resume",
		"sign",
		"sign conformance-report",
		"verify",
		"verify audit",
		"version",
	}
	got := leafCommandPaths(root)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("command tree changed.\n got:\n  %s\nwant:\n  %s\n\n"+
			"If this change is intended, update the list here AND the commands printed in docs/ "+
			"— TestDocumentedInvocationsResolve reads those.",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}

	// The resolver's per-command argument model must cover the tree exactly. A
	// command with no entry is unchecked by the documentation gate; an entry with
	// no command is a typo that silently disables checking for the real one.
	inTree := map[string]bool{}
	for _, p := range got {
		inTree[p] = true
	}
	for _, p := range got {
		if _, ok := commandArgShape[p]; !ok {
			t.Errorf("command %q has no commandArgShape entry, so the documentation gate "+
				"cannot check its arguments", p)
		}
	}
	for p := range commandArgShape {
		if !inTree[p] {
			t.Errorf("commandArgShape names %q, which is not a command in the tree", p)
		}
	}
}

// TestDocumentedInvocationsResolve is the documentation-resolution gate. It reads
// every `swarmctl …` invocation printed in the documentation corpus and resolves
// it against the real command tree and the real resource registry.
//
// It is the check that would have caught the Tasks-section drift: seven commands
// printed, one of which existed.
func TestDocumentedInvocationsResolve(t *testing.T) {
	root, _ := newRootCommand(cli.IOStreams{})

	repo, err := repoRoot()
	if err != nil {
		t.Fatalf("locating repository root: %v", err)
	}
	invocations, err := collectDocInvocations(repo)
	if err != nil {
		t.Fatalf("collecting documented invocations: %v", err)
	}
	// A gate that reads nothing passes trivially. Assert the corpus is non-empty
	// so a broken extractor fails loudly instead of reporting success.
	if len(invocations) < 30 {
		t.Fatalf("extracted only %d invocations from %v — the extractor is probably broken, "+
			"since docs/operations.md alone prints more than that", len(invocations), docCorpus)
	}

	var failures []string
	for _, inv := range invocations {
		if err := resolveInvocation(root, inv.Args); err != nil {
			failures = append(failures, inv.String()+"\n      "+err.Error())
		}
	}
	if len(failures) > 0 {
		t.Fatalf("%d documented swarmctl invocation(s) do not resolve against the CLI "+
			"(checked %d in %v):\n\n  %s\n\nEither the command was renamed or removed and the "+
			"document is stale, or the document is right and the command is missing. Fix one; "+
			"do not silence this by narrowing the extractor.",
			len(failures), len(invocations), docCorpus, strings.Join(failures, "\n\n  "))
	}
	t.Logf("%d documented swarmctl invocation(s) resolve", len(invocations))
}

// TestExampleInvocationsResolve applies the same resolution to the CLI's own
// cobra Example blocks. A command whose printed example cannot be typed is the
// same defect as a document that prints one, and it is the copy an operator is
// most likely to paste — `swarmctl cancel --help` is closer to hand than the docs.
func TestExampleInvocationsResolve(t *testing.T) {
	root, _ := newRootCommand(cli.IOStreams{})

	var failures []string
	checked := 0

	for _, c := range allCommands(root) {
		for _, line := range strings.Split(c.Example, "\n") {
			for _, m := range swarmctlLine.FindAllStringSubmatch(line, -1) {
				_, args, ok := splitInvocation(m[1])
				if !ok {
					continue
				}
				checked++
				if err := resolveInvocation(root, args); err != nil {
					failures = append(failures,
						c.CommandPath()+" Example: swarmctl "+strings.Join(args, " ")+"\n      "+err.Error())
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no Example invocations found — the walker or the extractor is broken")
	}
	if len(failures) > 0 {
		t.Fatalf("%d Example invocation(s) printed by `--help` do not resolve (checked %d):\n\n  %s",
			len(failures), checked, strings.Join(failures, "\n\n  "))
	}
	t.Logf("%d Example invocation(s) resolve", checked)
}

// TestDocGateCanFail is the gate's own liveness check.
//
// WHY IT IS HERE. This repository already carries checks that cannot fail — the
// Makefile's `versions` target ends `sys.exit(0 if sys.prefix != sys.base_prefix
// else 0)`, both branches zero — and checks that are always red and therefore
// ignored. Either shape is worse than no check, because it consumes the attention
// a real one would get. This asserts the resolver rejects each class of drift it
// exists to catch and accepts a known-good invocation, so a future refactor that
// quietly turns the gate into a no-op fails here rather than going green.
func TestDocGateCanFail(t *testing.T) {
	root, _ := newRootCommand(cli.IOStreams{})

	mustFail := []struct {
		why  string
		args []string
	}{
		{"a verb that does not exist", []string{"resume", "rollout"}},
		{"a resource spelling that is not registered", []string{"get", "task"}},
		{"a kind token that is not registered", []string{"cancel", "widget", "x"}},
		{"a subcommand a group does not have", []string{"delete", "fleetaction", "x"}},
	}
	for _, tc := range mustFail {
		if err := resolveInvocation(root, tc.args); err == nil {
			t.Errorf("gate accepted %v (%s) — it must reject this", tc.args, tc.why)
		}
	}

	mustPass := [][]string{
		{"get", "robot"},
		{"get", "fact"},
		{"describe", "fleettask", argPlaceholder},
		{"delete", "robot", argPlaceholder},
		{"estop", "trigger", argPlaceholder},
		{"cancel", argPlaceholder},
	}
	for _, args := range mustPass {
		if err := resolveInvocation(root, args); err != nil {
			t.Errorf("gate rejected the valid invocation %v: %v", args, err)
		}
	}

	// The extractor must also be able to see a defect, not just the resolver.
	// A fenced invocation is read; a shell comment and a prose mention are not.
	const doc = "```\nswarmctl get widget\nswarmctl get robot  # swarmctl get nonesuch\n```\n" +
		"A `swarmctl resume` verb is planned.\n"
	found := 0
	inFence := false
	for _, line := range strings.Split(doc, "\n") {
		if fenceLine.MatchString(line) {
			inFence = !inFence
			continue
		}
		for _, span := range codeSpans(line, inFence) {
			for _, m := range swarmctlLine.FindAllStringSubmatch(span, -1) {
				if _, _, ok := splitInvocation(m[1]); ok {
					found++
				}
			}
		}
	}
	if found != 2 {
		t.Errorf("extractor found %d invocations in the fixture, want 2 "+
			"(the two fenced commands; not the shell comment, not the prose mention)", found)
	}
}

// TestDocWalkDoesNotEscapeTheCorpus holds the gosec G122 fix in collectDocInvocations. The
// collector reads through an os.Root scoped to each corpus directory, so an entry
// that resolves outside it is never read. Before that change the walk handed
// os.ReadFile a path string to resolve a second time, and a symlink planted in
// docs/ was followed: text from outside the corpus entered the gate as though the
// documentation had printed it.
//
// The assertion is on the content, not on the error. Refusing the escaping entry
// and skipping it are both acceptable; reading it is not. A symlink that stays
// inside the corpus still resolves, which is why the refusal is scoped to escape
// rather than to symlinks.
func TestDocWalkDoesNotEscapeTheCorpus(t *testing.T) {
	fake := t.TempDir()
	outside := filepath.Join(fake, "outside.md")
	const planted = "swarmctl definitely-not-a-command"
	if err := os.WriteFile(outside, []byte("```\n"+planted+"\n```\n"), 0o600); err != nil {
		t.Fatalf("writing the document outside the corpus: %v", err)
	}
	docs := filepath.Join(fake, docCorpus[0])
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatalf("creating the corpus directory: %v", err)
	}
	const inside = "swarmctl get robots"
	if err := os.WriteFile(filepath.Join(docs, "real.md"), []byte("```\n"+inside+"\n```\n"), 0o600); err != nil {
		t.Fatalf("writing the in-corpus document: %v", err)
	}
	escape := filepath.Join(docs, "escape.md")
	if err := os.Symlink(outside, escape); err != nil {
		t.Skipf("symlinks are unavailable here: %v", err)
	}

	invocations, walkErr := collectDocInvocations(fake)
	for _, inv := range invocations {
		if strings.Contains(inv.Text, "definitely-not-a-command") {
			t.Fatalf("text from outside the corpus was read through %s: %s", escape, inv)
		}
	}
	if walkErr == nil {
		// Skipped rather than refused: then the in-corpus document must still have
		// been read, or this test would pass on a collector that reads nothing.
		if len(invocations) != 1 || invocations[0].Text != inside {
			t.Fatalf("expected exactly the in-corpus invocation %q, got %v", inside, invocations)
		}
		return
	}
	// Refused: prove the refusal was caused by the escaping entry and not by a
	// broken fixture, by removing it and collecting again.
	t.Logf("walk refused the escaping entry: %v", walkErr)
	if err := os.Remove(escape); err != nil {
		t.Fatalf("removing the escaping symlink: %v", err)
	}
	invocations, err := collectDocInvocations(fake)
	if err != nil {
		t.Fatalf("collecting from a corpus with no escaping entry: %v", err)
	}
	if len(invocations) != 1 || invocations[0].Text != inside {
		t.Fatalf("expected exactly the in-corpus invocation %q, got %v", inside, invocations)
	}
}
