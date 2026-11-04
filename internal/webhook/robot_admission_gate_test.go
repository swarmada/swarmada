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
	"context"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/contract"
)

// The FleetAdapter admission keystone (RFC-0001 #fleetadapter, #security-fleet-adapter-authentication-robot-identity) plus the ADR-0032 assignment gate.
// A Robot is admissible only against its NAMED adapter being Connected, conformance-Passed, and
// qualified against a contract version this control plane can drive.
//
// The third condition is the ADR-0032 addition. It is checked here rather than by rewriting
// status.conformance to Failed, because the report is not defective — a genuine attestation earned
// against contract 0.9.0 is a true statement that simply does not bind a 1.x control plane. Refusing
// the ROBOT keeps that distinction visible to an operator.

const gateNS = "warehouse-gate"

// gateAdapter builds a FleetAdapter that satisfies every condition, before per-test spoiling.
func gateAdapter() *fleetv1.FleetAdapter {
	return &fleetv1.FleetAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: gateNS},
		Status: fleetv1.FleetAdapterStatus{
			Phase:                      fleetv1.FleetAdapterPhaseConnected,
			Conformance:                fleetv1.ConformanceStatePassed,
			ConformanceContractVersion: contract.Version,
		},
	}
}

// gateRobot names that adapter and no class, so a serves-any adapter matches.
func gateRobot() *fleetv1.Robot {
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: "amr-1", Namespace: gateNS},
		Spec:       fleetv1.RobotSpec{Adapter: fleetv1.AdapterRef{Name: "acme"}},
	}
}

func gateFor(t *testing.T, adapter *fleetv1.FleetAdapter) *RobotAdmissionGate {
	t.Helper()
	sch := runtime.NewScheme()
	if err := fleetv1.AddToScheme(sch); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(adapter).Build()
	return &RobotAdmissionGate{Client: c}
}

// The happy path: every condition met → admitted.
func TestAdmissionGate_FullyReadyAdapterAdmits(t *testing.T) {
	g := gateFor(t, gateAdapter())
	if _, err := g.ValidateCreate(context.Background(), gateRobot()); err != nil {
		t.Fatalf("a Connected, Passed, version-bound adapter must admit its robot, got: %v", err)
	}
}

// The two pre-existing conditions, which had no test before this file.
func TestAdmissionGate_RefusesOnPhaseAndConformance(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spoil func(*fleetv1.FleetAdapter)
	}{
		{"disconnected", func(a *fleetv1.FleetAdapter) {
			a.Status.Phase = fleetv1.FleetAdapterPhaseDisconnected
		}},
		{"degraded", func(a *fleetv1.FleetAdapter) {
			a.Status.Phase = fleetv1.FleetAdapterPhaseDegraded
		}},
		{"pending", func(a *fleetv1.FleetAdapter) { a.Status.Phase = "" }},
		{"conformance failed", func(a *fleetv1.FleetAdapter) {
			a.Status.Conformance = fleetv1.ConformanceStateFailed
		}},
		{"conformance unknown", func(a *fleetv1.FleetAdapter) { a.Status.Conformance = "" }},
		// The ADR-0032 rejection phase, which is what an out-of-range HANDSHAKE resolves to. The
		// negotiated version needs no separate condition here precisely because of this.
		{"rejected on negotiation", func(a *fleetv1.FleetAdapter) {
			a.Status.Phase = fleetv1.FleetAdapterPhaseRejected
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := gateAdapter()
			tc.spoil(a)
			if _, err := gateFor(t, a).ValidateCreate(context.Background(), gateRobot()); err == nil {
				t.Fatal("robot was admitted against an ineligible adapter")
			}
		})
	}
}

// ADR-0032: the conformance result must be bound to a contract version in range. Missing counts as
// incompatible ("never as an implicit pass"), which is the case with real operational consequence —
// an adapter qualified by a pre-versioning harness stops admitting robots until it is re-run.
func TestAdmissionGate_RefusesUnsupportedConformanceContractVersion(t *testing.T) {
	for _, tc := range []struct{ name, version string }{
		{"missing", ""},
		{"older major", "0.9.0"},
		{"newer major", "2.0.0"},
		// Derived, not hardcoded: "the minor after the one we implement" is the property under
		// test, and a literal here silently becomes the IMPLEMENTED minor at the next bump —
		// which is how this case read after 1.0.0 → 1.1.0.
		{"newer minor", contractMinorOffset(t, +1)},
		{"unparseable", "one.oh.oh"},
		{"prerelease", "1.0.0-rc1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := gateAdapter()
			a.Status.ConformanceContractVersion = tc.version
			_, err := gateFor(t, a).ValidateCreate(context.Background(), gateRobot())
			if err == nil {
				t.Fatalf("robot admitted against an adapter qualified at contract %q", tc.version)
			}
			// The denial must be diagnosable: an operator seeing phase=Connected conformance=Passed
			// needs to be told it is the VERSION that failed, or the refusal looks like a bug.
			if !strings.Contains(err.Error(), "contract version") {
				t.Errorf("denial = %q, want it to name the contract version as the cause", err.Error())
			}
		})
	}
}

// The gate is anchored on the NAMED binding (RFC-0001 #fleetadapter-admission-gating): a different healthy adapter confers no authority.
// Re-asserted here because the new condition reads status off that one resolved adapter.
func TestAdmissionGate_UnknownAdapterRefused(t *testing.T) {
	robot := gateRobot()
	robot.Spec.Adapter.Name = "not-installed"
	if _, err := gateFor(t, gateAdapter()).ValidateCreate(context.Background(), robot); err == nil {
		t.Fatal("robot admitted against a FleetAdapter that does not exist")
	}
}

// A status/label update must not be re-gated: a controller has to be able to record status DURING an
// adapter outage (e.g. mark a robot Offline). The version condition must not change that — otherwise
// a contract bump would freeze status writes across the fleet.
func TestAdmissionGate_UnchangedBindingSkipsTheGate(t *testing.T) {
	a := gateAdapter()
	a.Status.ConformanceContractVersion = "0.9.0" // would refuse on create
	g := gateFor(t, a)

	old := gateRobot()
	updated := gateRobot()
	updated.Labels = map[string]string{"zone": "north"}
	if _, err := g.ValidateUpdate(context.Background(), old, updated); err != nil {
		t.Fatalf("a label-only update must not be re-gated on adapter version, got: %v", err)
	}

	// Re-pointing the binding DOES re-run the gate.
	rebound := gateRobot()
	rebound.Spec.Adapter.Name = "acme-2"
	if _, err := g.ValidateUpdate(context.Background(), old, rebound); err == nil {
		t.Error("re-pointing spec.adapter must re-run the gate")
	}
}

// contractMinorOffset returns contract.Version with its minor moved by delta, so a test can name
// a neighbouring contract version without a literal that rots on the next bump.
func contractMinorOffset(t *testing.T, delta int) string {
	t.Helper()
	var major, minor, patch int
	if _, err := fmt.Sscanf(contract.Version, "%d.%d.%d", &major, &minor, &patch); err != nil {
		t.Fatalf("contract.Version = %q, want major.minor.patch: %v", contract.Version, err)
	}
	return fmt.Sprintf("%d.%d.0", major, minor+delta)
}

// ADR-0032 Decision, as amended by Amendment 1 §A1.2: "A minor release never invalidates a
// qualification" (a control plane two minors past an adapter's reported version refuses it). The
// N-1 half of the supported range is load-bearing, so the DISPATCH gate — not only
// contract.Supports — is asserted to still admit a robot against an adapter qualified at the
// previous minor. The in-tree example-noop adapter declares the previous minor; the simulation
// adapter declares contract.Version.
func TestAdmissionGate_AdmitsAgainstThePreviousMinor(t *testing.T) {
	prev := contractMinorOffset(t, -1)
	if prev == contractMinorOffset(t, 0) {
		t.Skipf("contract.Version %s is at minor 0; there is no previous minor to admit", contract.Version)
	}
	// The patch component is ignored by the gate, so any patch of the previous minor qualifies.
	for _, version := range []string{prev, strings.TrimSuffix(prev, "0") + "7"} {
		t.Run(version, func(t *testing.T) {
			a := gateAdapter()
			a.Status.ConformanceContractVersion = version
			if _, err := gateFor(t, a).ValidateCreate(context.Background(), gateRobot()); err != nil {
				t.Fatalf("robot refused against an adapter qualified at contract %q: %v — a MINOR "+
					"bump must not invalidate an existing qualification (ADR-0032 Decision, as amended by §A1.2)", version, err)
			}
		})
	}
}
