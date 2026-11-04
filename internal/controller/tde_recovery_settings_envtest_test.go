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

// The two recovery settings (RFC-0001 #swarmadaconfig-traffic-deconfliction-engine-configuration)
// are read from the SwarmadaConfig in the manager's own namespace, which cmd/manager
// learns from POD_NAMESPACE. With the variable set as the shipped deployments set it,
// a configured fallback and wait replace the built-in ReleaseAll after 30s.

import (
	"context"
	"os"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/tde"
)

func TestEnvtest_TDERecoverySettingsTakeEffectFromPodNamespace(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	// The manager's namespace, as the downward API hands it to cmd/manager.
	t.Setenv("POD_NAMESPACE", ns)

	cfg := &fleetv1.SwarmadaConfig{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "swarmada-config"},
		Spec: fleetv1.SwarmadaConfigSpec{TrafficDeconfliction: fleetv1.SwarmadaTrafficDeconflictionConfig{
			Recovery: fleetv1.TDERecoveryConfig{
				ZoneControllerWaitTimeoutSeconds: 5,
				ConservativeRecoveryFallback:     fleetv1.TDEReleaseReservedOnly,
			},
		}},
	}
	if err := envK8s.Create(ctx, cfg); err != nil {
		t.Fatalf("create SwarmadaConfig: %v", err)
	}

	// An Occupied reservation whose robot the (never-ready) Zone Controller cannot place:
	// ReleaseReservedOnly keeps it, the built-in ReleaseAll drops it.
	zone := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "bay", Namespace: ns}}
	if err := envK8s.Create(ctx, zone); err != nil {
		t.Fatalf("create zone: %v", err)
	}
	zone.Status.Reservations = []fleetv1.ZoneReservation{
		{RobotID: "r1", ActionID: "a1", Priority: fleetv1.ActionPriorityNormal,
			State: fleetv1.ReservationOccupied, GrantedAt: metav1.Now()},
		{RobotID: "r2", ActionID: "a2", Priority: fleetv1.ActionPriorityNormal,
			State: fleetv1.ReservationReserved, GrantedAt: metav1.Now()},
	}
	if err := envK8s.Status().Update(ctx, zone); err != nil {
		t.Fatalf("seed reservations: %v", err)
	}

	start := time.Now()
	startTDEManager(t, ns, func() bool { return false }, func(r *tde.RecoveryRunnable) {
		// As cmd/manager wires it.
		r.ReadyTimeout = 30 * time.Second
		r.Fallback = tde.RecoverReleaseAll
		r.ConfigNamespace = os.Getenv("POD_NAMESPACE")
	})
	took := time.Since(start)

	if took >= 30*time.Second {
		t.Errorf("recovery took %v: the configured 5s wait did not replace the built-in 30s", took)
	}
	var got fleetv1.FleetZone
	if err := envK8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "bay"}, &got); err != nil {
		t.Fatalf("get zone: %v", err)
	}
	if len(got.Status.Reservations) != 1 || got.Status.Reservations[0].ActionID != "a1" {
		t.Errorf("reservations after recovery = %+v; want only a1 (the configured ReleaseReservedOnly, "+
			"not the built-in ReleaseAll)", got.Status.Reservations)
	}
	t.Logf("recovered in %v with reservations %v", took.Round(time.Second), got.Status.Reservations)
}
