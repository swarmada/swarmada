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

package registrar

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controlstream"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// swarmadaConfigName is the fixed name of the per-namespace SwarmadaConfig
// singleton (mirrors controller.SwarmadaConfigName; inlined to avoid importing the
// controller package from a registrar-package test).
const swarmadaConfigName = "swarmada-config"

// swarmadaConfig builds the namespace SwarmadaConfig singleton carrying the given
// discoveredRobotTTLMinutes.
func swarmadaConfig(minutes int32) *fleetv1.SwarmadaConfig {
	return &fleetv1.SwarmadaConfig{
		ObjectMeta: metav1.ObjectMeta{Name: swarmadaConfigName, Namespace: regNS},
		Spec: fleetv1.SwarmadaConfigSpec{
			Provisioning: fleetv1.SwarmadaProvisioningConfig{
				DiscoveredRobotTTLMinutes: minutes,
			},
		},
	}
}

// TestDiscoverTTLFromConfig verifies Discover records status.ttlExpiresAt from
// SwarmadaConfig.spec.provisioning.discoveredRobotTTLMinutes, and fails safe to the
// registrar fallback (r.TTL, else DefaultTTL) when the config is absent or carries
// a non-positive value.
func TestDiscoverTTLFromConfig(t *testing.T) {
	fixed := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		config   *fleetv1.SwarmadaConfig // nil ⇒ no config in the namespace
		override time.Duration           // r.TTL fallback seam
		wantTTL  time.Duration
	}{
		{name: "config honored", config: swarmadaConfig(90), wantTTL: 90 * time.Minute},
		{name: "no config falls back to DefaultTTL", config: nil, wantTTL: DefaultTTL},
		{name: "no config uses r.TTL override", config: nil, override: 5 * time.Minute, wantTTL: 5 * time.Minute},
		{name: "zero minutes falls back to DefaultTTL", config: swarmadaConfig(0), wantTTL: DefaultTTL},
		{name: "negative minutes uses r.TTL override", config: swarmadaConfig(-5), override: 7 * time.Minute, wantTTL: 7 * time.Minute},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var objs []client.Object
			if tc.config != nil {
				objs = append(objs, tc.config)
			}
			r, c := newRegistrar(t, objs...)
			r.Now = func() time.Time { return fixed }
			r.TTL = tc.override

			ack := r.Discover(context.Background(), adapterID(), controlstream.TLSIdentity{}, &fav1.DiscoverRobot{RobotId: regRobotID})
			if !ack.GetAccepted() {
				t.Fatalf("Discover rejected: %s", ack.GetMessage())
			}

			dr := getDR(t, c, regRobotID)
			if dr.Status.TTLExpiresAt == nil {
				t.Fatal("ttlExpiresAt not set")
			}
			if got := dr.Status.TTLExpiresAt.Sub(fixed); got != tc.wantTTL {
				t.Errorf("ttl = %s, want %s", got, tc.wantTTL)
			}
		})
	}
}
