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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/contract"
	"github.com/swarmada/swarmada/internal/controlstream"
)

// ADR-0032 assignment gate, first condition: the negotiated contract version. An adapter that agreed
// no compatible contract version lands in phase Rejected, which the RobotAdmissionGate's existing
// phase==Connected requirement already turns into "its robots are not admissible".
//
// The gate bars WORK, not observation: the session stands, telemetry and heartbeats keep flowing, and
// estop is always delivered (proven at the protocol layer in
// internal/controlstream/server_contract_gate_test.go).

// incompatibleNegotiation is a handshake in which the adapter reported a contract version this build
// cannot drive.
func incompatibleNegotiation(reported string) controlstream.Negotiation {
	return controlstream.Negotiation{
		ProtocolVersion:    "fleet_adapter.v1",
		ContractVersion:    reported,
		ContractCompatible: false,
	}
}

// A compatible handshake records the agreed version and connects.
func TestContractPhase_CompatibleHandshakeConnects(t *testing.T) {
	r, c := newFAReconciler(t, time.Unix(1000, 0), fleetv1.FleetAdapterPhasePending, nil)
	r.AdapterConnected(context.Background(), faIdentity(), compatibleNegotiation())

	fa := getFA(t, c)
	if fa.Status.Phase != fleetv1.FleetAdapterPhaseConnected {
		t.Errorf("phase = %s, want Connected", fa.Status.Phase)
	}
	if got, want := fa.Status.NegotiatedContractVersion, contract.Version; got != want {
		t.Errorf("negotiatedContractVersion = %q, want %q", got, want)
	}
}

// An incompatible handshake is REJECTED, and records no agreed version — so an empty field always
// means "no compatible contract", never "compatible but unrecorded".
func TestContractPhase_IncompatibleHandshakeRejects(t *testing.T) {
	for _, reported := range []string{"", "0.9.0", "2.0.0", "1.1.0", "garbage"} {
		t.Run(reported, func(t *testing.T) {
			r, c := newFAReconciler(t, time.Unix(1000, 0), fleetv1.FleetAdapterPhasePending, nil)
			r.AdapterConnected(context.Background(), faIdentity(), incompatibleNegotiation(reported))

			fa := getFA(t, c)
			if fa.Status.Phase != fleetv1.FleetAdapterPhaseRejected {
				t.Errorf("phase = %s, want Rejected for reported contract %q", fa.Status.Phase, reported)
			}
			if fa.Status.NegotiatedContractVersion != "" {
				t.Errorf("negotiatedContractVersion = %q, want empty (nothing compatible was agreed)",
					fa.Status.NegotiatedContractVersion)
			}
			// The message must be actionable: the range this build accepts, and that the refusal is
			// scoped to work rather than to observation.
			if !strings.Contains(fa.Status.Message, contract.SupportedRange()) {
				t.Errorf("message = %q, want it to name the supported range %q", fa.Status.Message, contract.SupportedRange())
			}
			if !strings.Contains(fa.Status.Message, "estop") {
				t.Errorf("message = %q, want it to say estop is unaffected", fa.Status.Message)
			}
			// The session is still live — liveness was recorded, not withheld.
			if fa.Status.LastHeartbeat == nil {
				t.Error("lastHeartbeat is nil; a rejected adapter is still connected and observed")
			}
		})
	}
}

// THE OSCILLATION BUG this guards: a rejected adapter deliberately keeps heartbeating (the handshake
// gate refuses registration, not the connection). Rejected is a negotiation verdict, so a heartbeat —
// which proves only liveness — must never clear it and silently re-admit robots.
func TestContractPhase_HeartbeatDoesNotResurrectRejected(t *testing.T) {
	r, c := newFAReconciler(t, time.Unix(1000, 0), fleetv1.FleetAdapterPhasePending, nil)
	r.AdapterConnected(context.Background(), faIdentity(), incompatibleNegotiation("2.0.0"))
	if getFA(t, c).Status.Phase != fleetv1.FleetAdapterPhaseRejected {
		t.Fatal("precondition: adapter should be Rejected")
	}

	for i := 0; i < 3; i++ {
		r.AdapterHeartbeat(context.Background(), faIdentity())
	}
	if got := getFA(t, c).Status.Phase; got != fleetv1.FleetAdapterPhaseRejected {
		t.Fatalf("phase = %s after heartbeats, want it to stay Rejected", got)
	}
}

// Only a fresh, COMPATIBLE handshake clears the rejection — an adapter redeployed against the right
// contract must be able to recover without operator intervention.
func TestContractPhase_CompatibleReconnectClearsRejection(t *testing.T) {
	r, c := newFAReconciler(t, time.Unix(1000, 0), fleetv1.FleetAdapterPhasePending, nil)
	r.AdapterConnected(context.Background(), faIdentity(), incompatibleNegotiation("2.0.0"))
	r.AdapterConnected(context.Background(), faIdentity(), compatibleNegotiation())

	fa := getFA(t, c)
	if fa.Status.Phase != fleetv1.FleetAdapterPhaseConnected {
		t.Errorf("phase = %s after a compatible reconnect, want Connected", fa.Status.Phase)
	}
	if fa.Status.NegotiatedContractVersion != contract.Version {
		t.Errorf("negotiatedContractVersion = %q, want %q", fa.Status.NegotiatedContractVersion, contract.Version)
	}
}

// RA-1 / transition-only writes: repeated heartbeats on a Rejected adapter must not produce status
// writes. The rejection path must not turn a liveness tick into a write storm.
func TestContractPhase_RA1_HeartbeatOnRejectedWritesNoStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	fa := &fleetv1.FleetAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: faNS},
		Spec:       fleetv1.FleetAdapterSpec{HeartbeatIntervalSeconds: 10},
	}
	writes := 0
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(fa).
		WithStatusSubresource(&fleetv1.FleetAdapter{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string,
				obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if _, isFA := obj.(*fleetv1.FleetAdapter); isFA {
					writes++
				}
				return cl.Status().Patch(ctx, obj, patch, opts...)
			},
		}).Build()

	// A fixed clock, so a heartbeat that changes nothing else does not change lastHeartbeat either —
	// otherwise every tick is a "material" change and RA-1 cannot be observed.
	fixed := time.Unix(1000, 0)
	r := &FleetAdapterReconciler{Client: c, Scheme: scheme, now: func() time.Time { return fixed }}

	r.AdapterConnected(context.Background(), faIdentity(), incompatibleNegotiation("2.0.0"))
	afterConnect := writes
	if afterConnect == 0 {
		t.Fatal("the rejection itself must be written once")
	}
	for i := 0; i < 5; i++ {
		r.AdapterHeartbeat(context.Background(), faIdentity())
	}
	if writes != afterConnect {
		t.Errorf("status writes = %d after 5 heartbeats, want %d (RA-1: a heartbeat that changes "+
			"nothing must not write)", writes, afterConnect)
	}
}

// A Reconcile pass must not overwrite the rejection reason with the conformance note: an operator
// must never read "conformance verified" on an adapter that cannot be given work.
func TestContractPhase_ReconcileKeepsTheRejectionMessage(t *testing.T) {
	r, c := newFAReconciler(t, time.Unix(1000, 0), fleetv1.FleetAdapterPhasePending, nil)
	r.AdapterConnected(context.Background(), faIdentity(), incompatibleNegotiation("2.0.0"))
	rejectionMsg := getFA(t, c).Status.Message

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "acme", Namespace: faNS}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	fa := getFA(t, c)
	if fa.Status.Phase != fleetv1.FleetAdapterPhaseRejected {
		t.Errorf("phase = %s after reconcile, want Rejected to persist", fa.Status.Phase)
	}
	if fa.Status.Message != rejectionMsg {
		t.Errorf("message = %q after reconcile, want the rejection reason %q preserved",
			fa.Status.Message, rejectionMsg)
	}
}
