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
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/swarmada/swarmada/internal/cli"
)

// noSuchChild is a token that names no command anywhere in the tree.
const noSuchChild = "no-such-subcommand"

// commandGroups returns every command in the tree that has subcommands, the root
// included. It is derived from the tree, not listed by hand, so a group added
// later is covered without editing this file.
func commandGroups(t *testing.T) []*cobra.Command {
	t.Helper()
	root, _ := newRootCommand(cli.IOStreams{})
	var groups []*cobra.Command
	for _, c := range allCommands(root) {
		if c.HasSubCommands() {
			groups = append(groups, c)
		}
	}
	if len(groups) < 2 {
		t.Fatalf("found %d command group(s); the tree has at least the root and `estop`", len(groups))
	}
	return groups
}

// pathArgs is c's command path without the leading "swarmctl", as argv.
func pathArgs(c *cobra.Command) []string {
	return strings.Fields(strings.TrimPrefix(c.CommandPath(), "swarmctl"))
}

// executeFresh runs args against a freshly built tree, so no flag or Args state
// leaks between cases.
func executeFresh(args []string) (string, error) {
	var out, errOut bytes.Buffer
	root, _ := newRootCommand(cli.IOStreams{Out: &out, Err: &errOut})
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(&errOut)
	err := root.Execute()
	return out.String() + errOut.String(), err
}

// TestEveryCommandGroupRefusesAnUnknownSubcommand is the per-group half: for each
// group, an unknown child is an error, and the error says "unknown command". A
// group whose positional argument is data rather than a subcommand (`get
// <resource>`) must still fail; its own argument check says why.
func TestEveryCommandGroupRefusesAnUnknownSubcommand(t *testing.T) {
	for _, g := range commandGroups(t) {
		path := g.CommandPath()
		t.Run(path, func(t *testing.T) {
			args := append(pathArgs(g), noSuchChild, "zone-a")
			out, err := executeFresh(args)
			if err == nil {
				t.Fatalf("`%s %s zone-a` returned no error, so swarmctl exits 0 and a script reads "+
					"success; output:\n%s", path, noSuchChild, out)
			}
			if !isSubcommandCheck(g) {
				// A group whose own Args treat the token as data (a resource name).
				return
			}
			if !strings.Contains(err.Error(), "unknown command") {
				t.Fatalf("`%s %s` error = %q, want it to say \"unknown command\"", path, noSuchChild, err)
			}
			if !strings.Contains(err.Error(), noSuchChild) {
				t.Fatalf("`%s %s` error = %q, want it to name the token", path, noSuchChild, err)
			}
		})
	}
}

// isSubcommandCheck reports whether g treats a positional argument as a
// subcommand name, so the error must say "unknown command". A group with no Args
// of its own is one: cobra's default treats the token as a subcommand.
func isSubcommandCheck(g *cobra.Command) bool {
	if g.Args == nil {
		return true
	}
	err := g.Args(g, []string{noSuchChild})
	return err != nil && strings.Contains(err.Error(), "unknown command")
}

// TestBareCommandGroupStillPrintsHelp keeps the other half of the old behaviour:
// a group with no argument prints its help and succeeds.
func TestBareCommandGroupStillPrintsHelp(t *testing.T) {
	for _, g := range commandGroups(t) {
		if !isSubcommandCheck(g) {
			continue // `get` needs a resource; bare, it is a usage error, as before
		}
		path := g.CommandPath()
		t.Run(path, func(t *testing.T) {
			out, err := executeFresh(pathArgs(g))
			if err != nil {
				t.Fatalf("bare `%s` returned %v, want help and no error", path, err)
			}
			if !strings.Contains(out, "Available Commands") {
				t.Fatalf("bare `%s` did not print its help; output:\n%s", path, out)
			}
		})
	}
}

// TestUnknownSubcommandSuggestsTheNearestChild: a one-letter typo of a safety
// verb names the verb it was probably meant to be.
func TestUnknownSubcommandSuggestsTheNearestChild(t *testing.T) {
	_, err := executeFresh([]string{"estop", "trigge", "zone-a"})
	if err == nil {
		t.Fatal("`swarmctl estop trigge zone-a` returned no error")
	}
	for _, want := range []string{`unknown command "trigge" for "swarmctl estop"`, "trigger"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to contain %q", err, want)
		}
	}
}

// runMainEnv makes the test binary run main() instead of the tests, so the exit
// code is the one a shell sees.
const runMainEnv = "SWARMCTL_TEST_RUN_MAIN"

// TestMistypedEstopExitsNonZero runs main() in a child process: the exit status,
// not only the returned error, is what a script checks.
func TestMistypedEstopExitsNonZero(t *testing.T) {
	if os.Getenv(runMainEnv) == "1" {
		os.Args = []string{"swarmctl", "estop", "trigge", "zone-a"}
		main()
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMistypedEstopExitsNonZero$")
	cmd.Env = append(os.Environ(), runMainEnv+"=1", "KUBECONFIG="+os.DevNull)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("`swarmctl estop trigge zone-a` exited 0 (err=%v); output:\n%s", err, out)
	}
	if exitErr.ExitCode() == 0 {
		t.Fatalf("exit code 0; output:\n%s", out)
	}
	if !strings.Contains(string(out), "unknown command") {
		t.Fatalf("output does not say \"unknown command\":\n%s", out)
	}
}
