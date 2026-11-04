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
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// The documentation-resolution gate.
//
// WHY THIS EXISTS. `docs/operations.md` once printed seven `swarmctl` commands in its
// Tasks section of which one existed, and nothing in the tree noticed for months. The
// registry test walks the registry outward and the surface tests exercise individual
// commands, but nothing ever read a document and asked whether what it prints can be
// typed. This does exactly that: it extracts every `swarmctl …` invocation from the
// documentation corpus and resolves each one against the real cobra tree and the real
// resource registry — the same two lookups the binary performs at runtime.
//
// It resolves SPELLING, not behaviour. It cannot tell you a command does the right
// thing, only that a reader who types what the document prints reaches a command
// instead of an "unknown resource" or "unknown command" error. That is the failure
// mode that actually occurred.

// docCorpus is the set of directories scanned, relative to the repository root.
// `rfcs/` is deliberately NOT in it: those documents are assembled from chapter
// sources and carry a published-specification review trail, so a lint that fails
// on them would fail on text this package must not edit. Widening the corpus is a
// one-line change once that trail closes.
var docCorpus = []string{"docs"}

// invocation is one `swarmctl …` line lifted out of a document.
type invocation struct {
	File string
	Line int
	Text string   // the invocation as printed, comment stripped
	Args []string // tokenised, flags and their values removed
}

func (i invocation) String() string { return fmt.Sprintf("%s:%d: %s", i.File, i.Line, i.Text) }

// swarmctlLine matches an invocation at the start of a code line or after a
// shell prompt or pipe. The trailing capture runs to end-of-line; splitInvocation
// trims what is not part of the command.
var swarmctlLine = regexp.MustCompile("(?:^|[\\s`$|(])swarmctl\\s+([^`\n]*)")

// repoRoot walks up from the package directory to the directory holding go.mod.
// Tests run with the package directory as their working directory, so the corpus
// has to be located rather than assumed.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod found above %s", dir)
		}
		dir = parent
	}
}

// codeSpans returns the portions of a markdown line that this gate treats as a
// runnable invocation. Only fenced code blocks qualify, and a shell comment
// inside one is dropped.
//
// WHY THE LINE IS DRAWN AT THE FENCE. A fenced block is the copy-paste surface:
// it is offered to the reader as something to type. An inline backtick span in
// prose is a *mention*, and prose legitimately mentions commands that do not
// exist — the "Tasks" section of docs/operations.md says "a `swarmctl resume` verb
// ... is planned", and ADR-0041 (rollout-resume excludes rather than retries) quotes
// a doc comment naming a command it then explains "had never existed". Failing on
// those would make the gate wrong about correct documents, and a gate that is wrong
// about correct documents gets suppressed.
// The drift this gate exists to catch — the Tasks block that printed seven commands
// of which one existed — was fenced, so nothing escapes through the line.
func codeSpans(line string, inFence bool) []string {
	if !inFence {
		return nil
	}
	// A shell comment is not an invocation, whether or not it names one. This is
	// how a document says "planned, not current" beside a real command without
	// the gate reading the annotation as a command.
	if i := strings.Index(line, "#"); i >= 0 {
		line = line[:i]
	}
	return []string{line}
}

var fenceLine = regexp.MustCompile("^\\s*(```|~~~)")

// collectDocInvocations returns every swarmctl invocation in the corpus, in file
// and line order. Only code contexts are read (see codeSpans).
func collectDocInvocations(root string) ([]invocation, error) {
	var out []invocation
	for _, sub := range docCorpus {
		base := filepath.Join(root, sub)
		// The walk is scoped to one open directory handle rather than re-resolving each
		// path at read time (gosec G122). filepath.WalkDir yields a path string, and
		// whatever that string resolves to when os.ReadFile opens it is what gets read,
		// so a symlink swapped in between the two is followed. The corpus is a
		// repo-local docs tree this command owns, so that window is not reachable by
		// anything the gate does not already trust -- but os.Root costs one open per
		// corpus directory and removes it, which is cheaper than a suppression every
		// later reader has to re-evaluate. Each read below is relative to this
		// descriptor and cannot leave the directory.
		dir, err := os.OpenRoot(base)
		if err != nil {
			return nil, err
		}
		err = fs.WalkDir(dir.FS(), ".", func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".md") {
				return nil
			}
			body, err := dir.ReadFile(path)
			if err != nil {
				return err
			}
			// path is relative to base and slash-separated, so the reported name is
			// what filepath.Rel(root, …) produced before the walk was scoped.
			rel := filepath.Join(sub, filepath.FromSlash(path))
			inFence := false
			for n, line := range strings.Split(string(body), "\n") {
				if fenceLine.MatchString(line) {
					inFence = !inFence
					continue
				}
				for _, span := range codeSpans(line, inFence) {
					for _, m := range swarmctlLine.FindAllStringSubmatch(span, -1) {
						text, args, ok := splitInvocation(m[1])
						if !ok {
							continue
						}
						out = append(out, invocation{File: rel, Line: n + 1, Text: "swarmctl " + text, Args: args})
					}
				}
			}
			return nil
		})
		_ = dir.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// splitInvocation turns the tail of a `swarmctl …` match into the printed text
// and the positional argument tokens that have to resolve.
//
// It drops, in order: a trailing `#` comment, everything after a shell operator
// that ends the command, flags together with any value that follows them, and
// placeholder tokens (`<name>`, `[-n ns]`, `…`). What remains is what a reader
// types verbatim.
func splitInvocation(tail string) (text string, args []string, ok bool) {
	tail = strings.TrimSpace(tail)
	if tail == "" {
		return "", nil, false
	}
	if i := strings.Index(tail, "#"); i >= 0 {
		tail = tail[:i]
	}
	for _, op := range []string{"|", "&&", ";", ">", "\\"} {
		if i := strings.Index(tail, op); i >= 0 {
			tail = tail[:i]
		}
	}
	tail = strings.TrimSpace(tail)
	if tail == "" {
		return "", nil, false
	}

	fields := tokenize(tail)
	skipNext := false
	for _, f := range fields {
		if skipNext {
			skipNext = false
			// A flag value is anything that is not itself a flag.
			if !strings.HasPrefix(f, "-") {
				continue
			}
		}
		switch {
		case strings.HasPrefix(f, "--") && strings.Contains(f, "="):
			// --output=json: the value is attached, nothing to skip.
		case strings.HasPrefix(f, "-"):
			skipNext = true
		case isPlaceholder(f):
			// <name>, [-n <ns>], … — a reader substitutes these. They are KEPT, as
			// a sentinel, because they still occupy a positional slot: without them
			// `cancel action <name>` collapses to one positional and reads as the
			// bare-name form, which is exactly the invocation that is broken.
			args = append(args, argPlaceholder)
		default:
			args = append(args, f)
		}
	}
	if len(args) == 0 {
		return "", nil, false
	}
	if !plainWord.MatchString(args[0]) {
		return "", nil, false
	}
	return tail, args, true
}

// tokenize splits on whitespace but keeps a quoted run together, so
// `--reason "aisle blocked"` is two tokens and not three. Without this the
// second word of every quoted reason is read as a positional argument.
func tokenize(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
			cur.WriteRune(r)
		case r == '"' || r == '\'':
			quote = r
			cur.WriteRune(r)
		case r == ' ' || r == '\t':
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// argPlaceholder stands for a token a reader substitutes (`<name>`, `[-n <ns>]`).
// It occupies a positional slot but resolves to nothing.
const argPlaceholder = "\x00placeholder"

var plainWord = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// isPlaceholder reports whether a token is documentation punctuation rather than
// something a reader types verbatim.
func isPlaceholder(f string) bool {
	if f == "" {
		return true
	}
	switch f[0] {
	case '<', '[', '{', '(', '"', '\'', '$', '.', '/':
		return true
	}
	if strings.ContainsAny(f, "<>[]{}|…") {
		return true
	}
	return strings.HasPrefix(f, "-")
}

// argShape describes what a command does with its first positional argument.
type argShape int

const (
	// shapeName: the positional is an object name. Nothing to resolve.
	shapeName argShape = iota
	// shapeResource: the first positional MUST be a resource spelling.
	shapeResource
	// shapeOptionalKind: a leading kind token is optional — present only when
	// there is more than one positional (`cancel task <name>` vs `cancel <name>`).
	shapeOptionalKind
	// shapeGroup: the command only dispatches to subcommands, so a leftover
	// positional is an unknown subcommand, not an argument.
	shapeGroup
)

// commandArgShape is the per-command model the resolver uses. It is asserted
// complete against the live tree by TestCommandTreeSurface, so a command added
// without an entry here fails that test rather than being silently unchecked.
//
// Keys are command paths without the "swarmctl " prefix.
var commandArgShape = map[string]argShape{
	"admit":                   shapeOptionalKind,
	"cancel":                  shapeOptionalKind,
	"delete":                  shapeGroup,
	"delete robot":            shapeName,
	"describe":                shapeResource,
	"estop":                   shapeGroup,
	"estop resume":            shapeName,
	"estop trigger":           shapeName,
	"estop-clear":             shapeName,
	"export":                  shapeGroup,
	"export audit":            shapeName,
	"get":                     shapeResource,
	"get audit":               shapeName,
	"modelpolicy":             shapeGroup,
	"modelpolicy reset":       shapeName,
	"robotclass":              shapeGroup,
	"robotclass rollout":      shapeName,
	"rollout":                 shapeGroup,
	"rollout resume":          shapeName,
	"sign":                    shapeGroup,
	"sign conformance-report": shapeName,
	"verify":                  shapeGroup,
	"verify audit":            shapeName,
	"version":                 shapeName,
}

// pathOf returns a command's path with the root name stripped.
func pathOf(c *cobra.Command) string {
	return strings.TrimPrefix(c.CommandPath(), "swarmctl ")
}

// resolveInvocation walks the argument tokens down the cobra tree, then applies
// the deepest matched command's argShape to whatever is left. It returns a
// human-readable reason when the invocation does not resolve.
func resolveInvocation(root *cobra.Command, args []string) error {
	cur := root
	i := 0
	for i < len(args) {
		if args[i] == argPlaceholder {
			break
		}
		next := findSubcommand(cur, args[i])
		if next == nil {
			break
		}
		cur = next
		i++
	}
	if i == 0 {
		return fmt.Errorf("unknown command %q (swarmctl has no such verb)", args[0])
	}

	rest := args[i:]
	shape, known := commandArgShape[pathOf(cur)]
	if !known {
		return fmt.Errorf("`%s` has no entry in commandArgShape; add one so its arguments are checked", cur.CommandPath())
	}

	switch shape {
	case shapeGroup:
		if len(rest) > 0 && rest[0] != argPlaceholder {
			return fmt.Errorf("`%s` has no subcommand %q (it dispatches only to: %s)",
				cur.CommandPath(), rest[0], strings.Join(subcommandNames(cur), ", "))
		}
	case shapeResource:
		if len(rest) == 0 || rest[0] == argPlaceholder {
			// `swarmctl get` with no resource, or `get <resource>` written as a
			// placeholder, is a usage form rather than a spelling to check.
			return nil
		}
		if _, err := resolveResource(rest[0]); err != nil {
			return fmt.Errorf("%q does not resolve as a resource for `%s`: %w", rest[0], cur.CommandPath(), err)
		}
	case shapeOptionalKind:
		// A single positional is a name. Two or more means the first is a kind
		// token, and it has to resolve.
		if len(rest) >= 2 && rest[0] != argPlaceholder {
			if err := resolveKindToken(cur, rest[0]); err != nil {
				return fmt.Errorf("%q does not resolve as a kind token for `%s`: %w", rest[0], cur.CommandPath(), err)
			}
		}
	case shapeName:
		// Nothing to resolve.
	}
	return nil
}

// resolveKindToken resolves a leading kind token the way the command itself
// does. `cancel` accepts a command-local vocabulary (`action`, `task`) on top of
// the registry, so the gate must consult the same table the command consults —
// otherwise the gate and the binary disagree about what is valid, and one of the
// two is wrong about the documentation.
func resolveKindToken(c *cobra.Command, token string) error {
	if pathOf(c) == "cancel" {
		_, err := resolveCancelKind(token)
		return err
	}
	_, err := resolveResource(token)
	return err
}

func subcommandNames(c *cobra.Command) []string {
	var out []string
	for _, sub := range c.Commands() {
		if sub.Name() == "help" || sub.Name() == "completion" {
			continue
		}
		out = append(out, sub.Name())
	}
	sort.Strings(out)
	return out
}

func findSubcommand(parent *cobra.Command, token string) *cobra.Command {
	for _, c := range parent.Commands() {
		if c.Name() == token {
			return c
		}
		for _, a := range c.Aliases {
			if a == token {
				return c
			}
		}
	}
	return nil
}

// allCommands returns the root and every command beneath it, excluding cobra's
// own help and completion commands.
func allCommands(root *cobra.Command) []*cobra.Command {
	out := []*cobra.Command{root}
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			if sub.Name() == "help" || sub.Name() == "completion" {
				continue
			}
			out = append(out, sub)
			walk(sub)
		}
	}
	walk(root)
	return out
}

// leafCommandPaths returns every command path in the tree, sorted. Help and
// completion are cobra's own and are excluded: they are not part of the surface
// this project documents or maintains.
func leafCommandPaths(root *cobra.Command) []string {
	var out []string
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			if sub.Name() == "help" || sub.Name() == "completion" {
				continue
			}
			out = append(out, strings.TrimPrefix(sub.CommandPath(), "swarmctl "))
			walk(sub)
		}
	}
	walk(root)
	sort.Strings(out)
	return out
}
