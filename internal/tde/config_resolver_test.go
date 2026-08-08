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

package tde

import (
	"context"
	"testing"
	"time"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// ConfigFromTDE maps the SwarmadaConfig TTLs and falls back per-field to the
// defaults for absent (zero) values, preserving the non-CRD ClockSkew default.
func TestConfigFromTDE(t *testing.T) {
	c := ConfigFromTDE(30, 90)
	if c.ReservationTTL != 30*time.Second {
		t.Errorf("ReservationTTL = %v, want 30s", c.ReservationTTL)
	}
	if c.DisconnectedReservationTTL != 90*time.Second {
		t.Errorf("DisconnectedReservationTTL = %v, want 90s", c.DisconnectedReservationTTL)
	}
	if c.ClockSkew != DefaultConfig().ClockSkew {
		t.Errorf("ClockSkew = %v, want default %v (no CRD surface)", c.ClockSkew, DefaultConfig().ClockSkew)
	}

	d := ConfigFromTDE(0, 0)
	if d.ReservationTTL != DefaultConfig().ReservationTTL {
		t.Errorf("zero ReservationTTL = %v, want default %v", d.ReservationTTL, DefaultConfig().ReservationTTL)
	}
	if d.DisconnectedReservationTTL != DefaultConfig().DisconnectedReservationTTL {
		t.Errorf("zero DisconnectedReservationTTL = %v, want default %v", d.DisconnectedReservationTTL, DefaultConfig().DisconnectedReservationTTL)
	}
}

// A per-namespace resolver overrides the reservation TTL for the grant: the
// granted ExpiresAt reflects the namespace's ReservationTTLSeconds, not the
// constructor default.
func TestConfigResolver_PerNamespaceReservationTTL(t *testing.T) {
	fixed := time.Unix(1000, 0)
	e, _ := newEngine(t, fzone("z", 2))
	e.WithClock(func() time.Time { return fixed })
	e.WithConfigResolver(func(namespace string) Config {
		if namespace == ns {
			return ConfigFromTDE(7, 0) // short 7s reservation TTL for this namespace
		}
		return DefaultConfig()
	})

	res, err := e.RequestReservation(context.Background(), req("t1", fleetv1.ActionPriorityNormal))
	if err != nil || res.Status != Granted {
		t.Fatalf("reserve: status=%s err=%v", res.Status, err)
	}
	if want := fixed.Add(7 * time.Second); !res.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want %v (per-namespace 7s TTL)", res.ExpiresAt, want)
	}
}

// With no resolver the engine uses its constructor Config (the fallback path).
func TestConfigResolver_NilUsesConstructorConfig(t *testing.T) {
	fixed := time.Unix(1500, 0)
	e, _ := newEngine(t, fzone("z", 2)) // DefaultConfig, no resolver
	e.WithClock(func() time.Time { return fixed })

	res, err := e.RequestReservation(context.Background(), req("t1", fleetv1.ActionPriorityNormal))
	if err != nil || res.Status != Granted {
		t.Fatalf("reserve: status=%s err=%v", res.Status, err)
	}
	if want := fixed.Add(DefaultConfig().ReservationTTL); !res.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want %v (constructor DefaultConfig TTL)", res.ExpiresAt, want)
	}
}

// On Revoking the disconnected TTL is resolved per-namespace: the Reserved entry's
// expiry extends by the namespace's DisconnectedReservationTTLSeconds.
func TestConfigResolver_PerNamespaceDisconnectedTTL(t *testing.T) {
	fixed := time.Unix(2000, 0)
	e, c := newEngine(t, fzone("z", 2))
	e.WithClock(func() time.Time { return fixed })
	e.WithConfigResolver(func(string) Config { return ConfigFromTDE(7, 200) })

	if res, _ := e.RequestReservation(context.Background(), req("t1", fleetv1.ActionPriorityNormal)); res.Status != Granted {
		t.Fatalf("reserve: %s", res.Status)
	}
	if err := e.OnActionPhaseChanged(context.Background(), ns, "z", "t1", fleetv1.ActionPhaseRevoking); err != nil {
		t.Fatalf("revoking: %v", err)
	}

	rs := zoneReservations(t, c, "z")
	var found bool
	for _, r := range rs {
		if r.ActionID == "t1" {
			found = true
			if want := fixed.Add(200 * time.Second); r.ExpiresAt == nil || !r.ExpiresAt.Time.Equal(want) {
				t.Fatalf("disconnected ExpiresAt = %v, want %v (per-namespace 200s TTL)", r.ExpiresAt, want)
			}
		}
	}
	if !found {
		t.Fatal("reservation t1 not found in mirrored zone status")
	}
}
