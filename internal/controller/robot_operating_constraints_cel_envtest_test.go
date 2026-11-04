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

package controller

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Robot.status.honouredOperatingConstraints[] — the declaration half of ADR-0058 §7.
//
// EVERY RULE HERE IS PATTERN + CEL ON THE GENERATED CRD, SO EVERY TEST HERE IS AN ENVTEST, for
// the reason fleetzone_restrictions_cel_envtest_test.go states beside it: the fake client
// performs no admission, so a marker on the wrong type, a rule CRD regeneration silently
// dropped, or a pattern that does not compile all behave exactly like a correct rule.
//
// The failure this stands between a user and: an adapter declares "Microphone", the API server
// accepts it, filter 12 compares it against the rule's "microphone", never matches, and the
// robot is withheld forever from a zone for a subject it actually honours — with the declaration
// visible in `kubectl get robot -o yaml` and looking right.

func robotForConstraints(ns, name string) *fleetv1.Robot {
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			Annotations: map[string]string{fleetv1.RobotIDAnnotation: name},
		},
		Spec: fleetv1.RobotSpec{
			Manufacturer: "Acme", Model: "X1",
			Adapter: fleetv1.AdapterRef{Name: "sim-adapter", Version: "1.0.0"},
			Zone:    "ward",
		},
	}
}

// createAndSetConstraints applies a Robot, then writes the subject list on its STATUS
// subresource — which is the only path that reaches the field, since it is a projection.
func createAndSetConstraints(t *testing.T, ns, name string, subjects []string) error {
	t.Helper()
	ctx := context.Background()
	r := robotForConstraints(ns, name)
	if err := envK8s.Create(ctx, r); err != nil {
		t.Fatalf("create robot %s: %v", name, err)
	}
	fresh := &fleetv1.Robot{}
	if err := envK8s.Get(ctx, client.ObjectKeyFromObject(r), fresh); err != nil {
		t.Fatalf("get robot %s: %v", name, err)
	}
	fresh.Status.HonouredOperatingConstraints = subjects
	return envK8s.Status().Update(ctx, fresh)
}

func TestEnvtest_RobotHonouredOperatingSubjectVocabulary(t *testing.T) {
	requireEnvtest(t)
	ns := envtestNamespace(t)

	// The whole core seven, declared at once, is the ordinary case.
	if err := createAndSetConstraints(t, ns, "core-all", fleetv1.ZoneCoreOperatingSubjects); err != nil {
		t.Fatalf("the core seven were rejected on the declaration side while the ZONE side accepts "+
			"them; the two halves of one vocabulary must agree or filter 12 can never match: %v", err)
	}

	// A namespaced extension is accepted — the vocabulary is closed AT THE CORE, not closed.
	if err := createAndSetConstraints(t, ns, "ext-ok", []string{"example.com/leaf-blower"}); err != nil {
		t.Errorf("a namespaced extension subject was rejected on the declaration side: %v", err)
	}

	// An adapter that declares nothing is not in breach (FA-R1).
	if err := createAndSetConstraints(t, ns, "declares-none", nil); err != nil {
		t.Errorf("an empty declaration was rejected; FA-R1: \"an adapter that declares none is not "+
			"in breach\": %v", err)
	}

	// THE CEL RULE'S OWN CASES: shaped like a subject, so the PATTERN accepts them, and
	// rejected only because a bare subject must be one of the core seven. These are the ones
	// that prove the CEL rule is live — a rule that CRD regeneration silently dropped, or a
	// marker landed on the wrong type, would let every one of these through while the pattern
	// cases below still passed.
	//
	// "sensor.microphone" is the load-bearing case: it is a CAPABILITY-shaped name, and it is
	// exactly what an implementer who read ADR-0058 §7 too quickly would declare.
	for name, bad := range map[string][]string{
		"capability-name":        {"sensor.microphone"},
		"plausible-but-not-core": {"lidar"},
		"one-bad-of-two":         {"camera", "lidar"},
	} {
		err := createAndSetConstraints(t, ns, "cel-"+name, bad)
		if err == nil {
			t.Errorf("%s: the API server accepted %v. ADR-0058 §7: a subject with no \"/\" MUST be "+
				"one of the core seven, and Robot.status.capabilities is NEVER consulted to resolve "+
				"one — a subject that collides with a capability name means nothing", name, bad)
			continue
		}
		if !strings.Contains(err.Error(), "core seven") {
			t.Errorf("%s: %v was rejected, but NOT by the core-seven CEL rule — so that rule has "+
				"never run and nothing here would notice if it were dropped: %v", name, bad, err)
		}
	}

	// The PATTERN's cases: not even subject-shaped.
	for name, bad := range map[string][]string{
		"capitalised":    {"Microphone"},
		"spaced":         {"micro phone"},
		"leading-slash":  {"/leading"},
		"trailing-slash": {"trailing/"},
	} {
		err := createAndSetConstraints(t, ns, "pattern-"+name, bad)
		if err == nil {
			t.Errorf("%s: the API server accepted %v; the declaration side must apply the same "+
				"pattern the zone side does, or filter 12 compares values the two halves spell "+
				"differently", name, bad)
			continue
		}
		if !strings.Contains(err.Error(), "should match") {
			t.Errorf("%s: %v was rejected, but not by the subject pattern: %v", name, bad, err)
		}
	}
}

// listType=set: the API server must refuse a duplicate, so a declaration cannot claim a subject
// twice and cannot be merged into one that does.
func TestEnvtest_RobotHonouredOperatingConstraintsIsASet(t *testing.T) {
	requireEnvtest(t)
	ns := envtestNamespace(t)

	if err := createAndSetConstraints(t, ns, "dupe", []string{"camera", "camera"}); err == nil {
		t.Error("the API server accepted a duplicate subject; the field is declared listType=set")
	}
}
