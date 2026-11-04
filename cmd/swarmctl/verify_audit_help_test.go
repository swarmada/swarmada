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
	"strings"
	"testing"

	"github.com/swarmada/swarmada/internal/cli"
)

// TestVerifyAuditHelp_StatesTheTailLimit pins the one limit RFC-0001
// #safety-log-entry-format states for the chain: removing its newest entries and re-sealing
// leaves no gap and no hash mismatch, so `verify audit` passes a shortened log. The help is
// what an operator reads before presenting a passing run as a complete record.
func TestVerifyAuditHelp_StatesTheTailLimit(t *testing.T) {
	root, _ := newRootCommand(cli.IOStreams{})
	cmd, _, err := root.Find([]string{"verify", "audit"})
	if err != nil || cmd.Name() != "audit" {
		t.Fatalf("verify audit not found: %v", err)
	}
	long := strings.Join(strings.Fields(cmd.Long), " ")
	for _, claim := range []string{"newest entries", "not detected", "highest sequence number"} {
		if !strings.Contains(long, claim) {
			t.Errorf("verify audit --help does not say %q:\n%s", claim, long)
		}
	}
}
