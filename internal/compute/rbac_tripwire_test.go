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

package compute_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// workloadKinds are the resources whose create verb IS the privilege escalation:
// a principal who can create any of them can create a pod, and a principal who can
// create a pod can reach the node.
var workloadKinds = map[string]bool{
	"pods": true, "jobs": true, "cronjobs": true, "deployments": true,
	"replicasets": true, "statefulsets": true, "daemonsets": true,
	"replicationcontrollers": true, "persistentvolumeclaims": true,
}

// escalatingVerbs are the verbs that make the grant matter. `create` is the one
// that counts; `*` includes it.
var escalatingVerbs = map[string]bool{"create": true, "*": true}

type rbacDoc struct {
	Kind  string `json:"kind"`
	Rules []struct {
		APIGroups []string `json:"apiGroups"`
		Resources []string `json:"resources"`
		Verbs     []string `json:"verbs"`
	} `json:"rules"`
}

// managerRBAC are every file that grants the CONTROLLER-MANAGER's ServiceAccount.
// Both are checked because they are maintained separately — kustomize generates
// one and the Helm chart hand-maintains the other — so a grant could land in
// either alone.
var managerRBAC = []string{
	filepath.Join("..", "..", "config", "rbac", "role.yaml"),
	filepath.Join("..", "..", "deploy", "swarmada", "templates", "rbac-manager.yaml"),
}

// ─────────────────────────────────────────────────────────────────────────────
// THE ORDERING TRIPWIRE.
//
// The residual risk this test exists for is not a grant that is present — it is a
// grant that ARRIVES. provision: workload is built and unreachable: the builder
// exists, no shipped binary calls it, and the manager holds no workload-create
// verbs. A future session wiring mode 3 would add the +kubebuilder:rbac marker to
// make it work, and without this test that commit would open pod-create with
// nothing failing to mark the moment.
//
// So this does not merely assert "no grant today", which would go stale the day
// the feature lands and be deleted as an obstacle. It asserts the ORDER: either
// there is no workload-create authority, or the containment narrowing is already
// in place. Whoever adds the grant is told, by a failing test, which guards must
// exist first.
// ─────────────────────────────────────────────────────────────────────────────
func TestManagerRBAC_WorkloadCreateAuthorityImpliesContainment(t *testing.T) {
	var granted []string
	var read []string

	for _, path := range managerRBAC {
		raw, err := os.ReadFile(path)
		if err != nil {
			// A missing file is not a pass. If neither file is readable the check
			// proved nothing, and that is caught below.
			t.Logf("not readable, skipped: %s (%v)", path, err)
			continue
		}
		read = append(read, path)
		// The Helm file is a Go template; comment the actions out so the document
		// still parses as YAML. The same trick conformance/controlplane.py uses.
		text := strings.ReplaceAll(string(raw), "{{", "#{{")
		for _, chunk := range strings.Split(text, "\n---") {
			if strings.TrimSpace(chunk) == "" {
				continue
			}
			var doc rbacDoc
			if err := yaml.Unmarshal([]byte(chunk), &doc); err != nil {
				continue // not an RBAC document
			}
			if doc.Kind != "ClusterRole" && doc.Kind != "Role" {
				continue
			}
			for _, rule := range doc.Rules {
				var hits []string
				for _, r := range rule.Resources {
					if workloadKinds[strings.ToLower(r)] {
						hits = append(hits, r)
					}
				}
				if len(hits) == 0 {
					continue
				}
				for _, v := range rule.Verbs {
					if escalatingVerbs[strings.ToLower(v)] {
						granted = append(granted,
							filepath.Base(path)+": "+strings.Join(hits, ",")+" ["+v+"]")
					}
				}
			}
		}
	}

	if len(read) == 0 {
		t.Fatal("no manager RBAC file was readable, so this check established nothing; " +
			"a tripwire that cannot read its subject must fail, not pass silently")
	}

	if len(granted) == 0 {
		t.Logf("no workload-create authority granted (read %d file(s)); provision: workload "+
			"remains unreachable, which is the declined-profile branch (RFC-0001 "+
			"#computeworkloadtemplate-rbac-surface)", len(read))
		return
	}

	// The grant exists. Every containment guard must therefore already be in place.
	t.Logf("workload-create authority IS granted: %v — checking the containment narrowing landed first",
		granted)
	assertContainmentInPlace(t)
}

// assertContainmentInPlace verifies the narrowing that MUST precede any
// workload-create grant. Each check reads the artefact an operator would read.
func assertContainmentInPlace(t *testing.T) {
	t.Helper()

	crd, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases",
		"swarmada.io_computeworkloadtemplates.yaml"))
	if err != nil {
		t.Fatalf("workload-create is granted but the ComputeWorkloadTemplate CRD is unreadable: %v", err)
	}
	for _, want := range []string{
		"serviceAccountName", // vector 1, at admission
		"HostPath Volumes",   // vector 2, at admission
		"Host Namespaces",
	} {
		if !strings.Contains(string(crd), want) {
			t.Errorf("workload-create is granted but the CRD carries no CEL rule mentioning %q — "+
				"the containment narrowing MUST land before the grant, not with it", want)
		}
	}

	whk, err := os.ReadFile(filepath.Join("..", "..", "config", "webhook", "manifests.yaml"))
	if err != nil || !strings.Contains(string(whk), "computeworkloadtemplates") {
		t.Errorf("workload-create is granted but no validating webhook is registered for " +
			"computeworkloadtemplates — the container half of the policy (privileged mode, added " +
			"capabilities, host ports) cannot be expressed in CEL and would be unenforced")
	}

	helm, err := os.ReadFile(filepath.Join("..", "..", "deploy", "swarmada", "templates", "webhook.yaml"))
	if err != nil || !strings.Contains(string(helm), "vcomputeworkloadtemplate.swarmada.io") {
		t.Errorf("workload-create is granted but the Helm chart registers no " +
			"ComputeWorkloadTemplate validating webhook — a Helm-deployed cluster would have " +
			"the grant and none of the container-level gate")
	}
}

// The deny-list must stay wired into the builder. Deleting the ValidatePodTemplate
// call would leave every test in podpolicy_test.go passing (they call the policy
// directly) while the construction path silently stopped enforcing it.
func TestBuildWorkloadStillCallsThePolicy(t *testing.T) {
	src, err := os.ReadFile("workload.go")
	if err != nil {
		t.Fatalf("reading workload.go: %v", err)
	}
	if !regexp.MustCompile(`ValidatePodTemplate\(&template\.Spec\.PodTemplate\.Spec\)`).Match(src) {
		t.Error("BuildWorkload no longer calls ValidatePodTemplate; the builder must never trust " +
			"admission as the only gate for a privilege-escalation-relevant check")
	}
}
