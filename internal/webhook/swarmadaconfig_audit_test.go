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
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
)

// SWARMADA_CONFIG_MODIFIED (§9.6.5.1). The webhook is the only component that sees
// both the diff and the authenticated user, which is why the entry is written here.
//
// The properties that matter: a spec change is recorded, a non-spec change is NOT
// (otherwise every status write would bury the real modifications), and a failing
// sink never turns an already-valid update into a rejection.

type cfgAuditSpy struct {
	entries []audit.Entry
	err     error
}

func (s *cfgAuditSpy) Record(e audit.Entry) (audit.Entry, error) {
	if s.err != nil {
		return e, s.err
	}
	s.entries = append(s.entries, e)
	return e, nil
}

func baseConfig() *fleetv1.SwarmadaConfig {
	return &fleetv1.SwarmadaConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "swarmada-config", Namespace: "warehouse-a"},
		Spec: fleetv1.SwarmadaConfigSpec{
			Health: fleetv1.SwarmadaHealthConfig{TelemetryIntervalSeconds: 10},
		},
	}
}

// A real spec change is recorded.
func TestConfigAudit_SpecChangeRecorded(t *testing.T) {
	spy := &cfgAuditSpy{}
	v := &SwarmadaConfigValidator{Audit: spy}
	oldCfg := baseConfig()
	newCfg := baseConfig()
	newCfg.Spec.Health.TelemetryIntervalSeconds = 20

	if _, err := v.ValidateUpdate(context.Background(), oldCfg, newCfg); err != nil {
		t.Fatalf("ValidateUpdate: %v", err)
	}
	if len(spy.entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(spy.entries))
	}
	e := spy.entries[0]
	if e.EventType != audit.EventSwarmadaConfigMod {
		t.Errorf("event type = %q, want %q", e.EventType, audit.EventSwarmadaConfigMod)
	}
	if e.Namespace != "warehouse-a" || e.Resource.Name != "swarmada-config" {
		t.Errorf("entry does not identify the config: ns=%q res=%+v", e.Namespace, e.Resource)
	}
	if e.Outcome != audit.OutcomeAllowed {
		t.Errorf("outcome = %q, want Allowed (the change was admitted)", e.Outcome)
	}
}

// A metadata-only update is not a configuration modification. Recording one would
// mean every label edit and every controller annotation lands in the safety chain.
func TestConfigAudit_NonSpecChangeNotRecorded(t *testing.T) {
	spy := &cfgAuditSpy{}
	v := &SwarmadaConfigValidator{Audit: spy}
	oldCfg := baseConfig()
	newCfg := baseConfig()
	newCfg.Labels = map[string]string{"team": "ops"}

	if _, err := v.ValidateUpdate(context.Background(), oldCfg, newCfg); err != nil {
		t.Fatalf("ValidateUpdate: %v", err)
	}
	if len(spy.entries) != 0 {
		t.Errorf("entries = %d, want 0 — a metadata-only update is not a spec change",
			len(spy.entries))
	}
}

// A failing audit sink must not reject an update that already passed validation.
// The change is admitted by the time the record is attempted; failing closed here
// would make an unreachable audit sink an outage of the configuration API.
func TestConfigAudit_SinkFailureDoesNotRejectUpdate(t *testing.T) {
	spy := &cfgAuditSpy{err: errors.New("sink unavailable")}
	v := &SwarmadaConfigValidator{Audit: spy}
	oldCfg := baseConfig()
	newCfg := baseConfig()
	newCfg.Spec.Health.TelemetryIntervalSeconds = 25

	if _, err := v.ValidateUpdate(context.Background(), oldCfg, newCfg); err != nil {
		t.Fatalf("a failing audit sink must not reject a valid update: %v", err)
	}
}

// An INVALID update is rejected and leaves no audit entry: the chain records what was
// admitted, and a refused change was never applied.
func TestConfigAudit_RejectedUpdateNotRecorded(t *testing.T) {
	spy := &cfgAuditSpy{}
	v := &SwarmadaConfigValidator{Audit: spy}
	oldCfg := baseConfig()
	newCfg := baseConfig()
	// Fails invariant 3: a real sink with no endpoint.
	newCfg.Spec.Telemetry.Sink = fleetv1.TelemetrySink{Type: fleetv1.TelemetrySinkPrometheusRemoteWrite}

	if _, err := v.ValidateUpdate(context.Background(), oldCfg, newCfg); err == nil {
		t.Fatal("expected the invalid update to be rejected")
	}
	if len(spy.entries) != 0 {
		t.Errorf("entries = %d, want 0 — a rejected update must not be recorded as a modification",
			len(spy.entries))
	}
}

// Nil Audit disables recording without affecting validation.
func TestConfigAudit_NilRecorderIsSafe(t *testing.T) {
	v := &SwarmadaConfigValidator{}
	oldCfg := baseConfig()
	newCfg := baseConfig()
	newCfg.Spec.Health.TelemetryIntervalSeconds = 30

	if _, err := v.ValidateUpdate(context.Background(), oldCfg, newCfg); err != nil {
		t.Fatalf("ValidateUpdate with nil Audit: %v", err)
	}
}
