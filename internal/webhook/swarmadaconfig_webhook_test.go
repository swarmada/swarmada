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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// validConfig is a SwarmadaConfig that satisfies all five cross-field rules; each
// test mutates one block to trip exactly one rule.
// sigBoolPtr builds a *bool for the signing gate, which is a pointer so that an explicit
// false is distinguishable from unset (unset defaults to true).
func sigBoolPtr(b bool) *bool { return &b }

func validConfig() *fleetv1.SwarmadaConfig {
	return &fleetv1.SwarmadaConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "swarmada-config"},
		Spec: fleetv1.SwarmadaConfigSpec{
			ActionCancellation: fleetv1.SwarmadaActionCancellationConfig{
				OnDisconnect:             fleetv1.ActionCancellationAfterTimeout,
				DisconnectTimeoutSeconds: i32(300),
			},
			TrafficDeconfliction: fleetv1.SwarmadaTrafficDeconflictionConfig{
				DisconnectedReservationTTLSeconds: 360,
			},
			Telemetry: fleetv1.SwarmadaTelemetryConfig{
				Sink: fleetv1.TelemetrySink{
					Type:     fleetv1.TelemetrySinkPrometheusRemoteWrite,
					Endpoint: "https://prom.example/api/v1/write",
				},
			},
			Signing: fleetv1.SwarmadaSigningConfig{
				RequireSignatureVerification: sigBoolPtr(true),
				TrustRoots:                   []fleetv1.SigningTrustRoot{{Name: "ca"}},
			},
		},
	}
}

func assertValid(t *testing.T, cfg *fleetv1.SwarmadaConfig) {
	t.Helper()
	if _, err := validateSwarmadaConfig(cfg); err != nil {
		t.Fatalf("expected valid config to pass, got: %v", err)
	}
}

func assertRejected(t *testing.T, cfg *fleetv1.SwarmadaConfig, wantSubstr string) {
	t.Helper()
	_, err := validateSwarmadaConfig(cfg)
	if err == nil {
		t.Fatalf("expected rejection mentioning %q, got nil", wantSubstr)
	}
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Fatalf("error %q does not mention %q", err.Error(), wantSubstr)
	}
}

func TestSwarmadaConfig_ValidPasses(t *testing.T) {
	assertValid(t, validConfig())
}

// A Drop / unset sink with no endpoint is fine; a real sink without one is rejected.
func TestSwarmadaConfig_SinkEndpoint(t *testing.T) {
	drop := validConfig()
	drop.Spec.Telemetry.Sink = fleetv1.TelemetrySink{Type: fleetv1.TelemetrySinkDrop}
	assertValid(t, drop)

	unset := validConfig()
	unset.Spec.Telemetry.Sink = fleetv1.TelemetrySink{}
	assertValid(t, unset)

	real := validConfig()
	real.Spec.Telemetry.Sink = fleetv1.TelemetrySink{Type: fleetv1.TelemetrySinkVictoriaMetrics}
	assertRejected(t, real, "endpoint is required")
}

func TestSwarmadaConfig_AfterTimeoutRequiresTimeout(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.ActionCancellation.DisconnectTimeoutSeconds = nil
	assertRejected(t, cfg, "disconnectTimeoutSeconds is required")

	// Never / WhenActionExpired do not require the timeout.
	never := validConfig()
	never.Spec.ActionCancellation.OnDisconnect = fleetv1.ActionCancellationNever
	never.Spec.ActionCancellation.DisconnectTimeoutSeconds = nil
	assertValid(t, never)
}

func TestSwarmadaConfig_TTLMustExceedTimeout(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.TrafficDeconfliction.DisconnectedReservationTTLSeconds = 300 // == timeout, not >
	assertRejected(t, cfg, "must exceed disconnectTimeoutSeconds")

	// Equal is rejected; strictly greater passes.
	ok := validConfig()
	ok.Spec.ActionCancellation.DisconnectTimeoutSeconds = i32(300)
	ok.Spec.TrafficDeconfliction.DisconnectedReservationTTLSeconds = 301
	assertValid(t, ok)

	// The TTL rule only applies under AfterTimeout — a low TTL with Never is fine.
	never := validConfig()
	never.Spec.ActionCancellation.OnDisconnect = fleetv1.ActionCancellationNever
	never.Spec.TrafficDeconfliction.DisconnectedReservationTTLSeconds = 60
	assertValid(t, never)
}

// Verification on with no trust root is INCOMPLETE, not invalid: it is warned about and
// admitted (ADR-0051). The control plane's own bootstrap config is born in exactly that
// state, and rejecting it made the reconciler hot-loop against a webhook running
// failurePolicy=fail — a namespace could reach a state where the config that would break
// the loop could not be written either.
func TestSwarmadaConfig_SignatureVerificationWithoutTrustRootsWarnsAndAdmits(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Signing.TrustRoots = nil
	warnings, err := validateSwarmadaConfig(cfg)
	if err != nil {
		t.Fatalf("an incomplete signing config must be admitted, not rejected: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "no trustRoots are configured") {
		t.Fatalf("expected a trust-root warning, got %v", warnings)
	}

	// Not requiring verification → no trust roots needed, and nothing to warn about.
	off := validConfig()
	off.Spec.Signing.RequireSignatureVerification = sigBoolPtr(false)
	off.Spec.Signing.TrustRoots = nil
	warnings, err = validateSwarmadaConfig(off)
	if err != nil {
		t.Fatalf("verification off with no trust roots is valid: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("no warning expected when verification is off, got %v", warnings)
	}
}

// The bootstrap object the controller actually creates — an entirely empty spec, with every
// field materialized by the API server's defaults — must be admissible. This is the exact
// object SwarmadaConfigBootstrapReconciler writes on the first FleetZone in a namespace.
func TestSwarmadaConfig_BootstrapObjectIsAdmissible(t *testing.T) {
	boot := validConfig()
	boot.Spec.Signing = fleetv1.SwarmadaSigningConfig{} // nil → defaults to true, no trust roots
	if _, err := validateSwarmadaConfig(boot); err != nil {
		t.Fatalf("the auto-created bootstrap config must be admissible, got: %v", err)
	}
}

// nil means unset, which the schema defaults to true — so the accessor must read nil as
// enforcement ON. A Go accessor that disagreed with the schema default would answer
// differently from the API server about the same object.
func TestSwarmadaConfig_NilSignatureVerificationMeansOn(t *testing.T) {
	var s fleetv1.SwarmadaSigningConfig
	if !s.SignatureVerificationRequired() {
		t.Error("nil requireSignatureVerification must read as true (the schema default)")
	}
	s.RequireSignatureVerification = sigBoolPtr(false)
	if s.SignatureVerificationRequired() {
		t.Error("an explicit false must read as false — that is the whole point of the pointer")
	}
	s.RequireSignatureVerification = sigBoolPtr(true)
	if !s.SignatureVerificationRequired() {
		t.Error("an explicit true must read as true")
	}
}

// Multiple violations are all reported, not just the first.
func TestSwarmadaConfig_MultipleViolations(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Telemetry.Sink = fleetv1.TelemetrySink{Type: fleetv1.TelemetrySinkMimir} // no endpoint
	cfg.Spec.CoordinateSystem.ReferenceFrame = fleetv1.ReferenceFrameGeodetic         // no geodetic block
	cfg.Spec.CoordinateSystem.Geodetic = nil
	_, err := validateSwarmadaConfig(cfg)
	if err == nil {
		t.Fatal("expected rejection")
	}
	if !strings.Contains(err.Error(), "endpoint is required") || !strings.Contains(err.Error(), "geodetic is required") {
		t.Fatalf("expected both violations reported, got: %v", err)
	}
}

// Invariant 5: the coordinate reference frame and the geodetic block are mutually
// exclusive — Geodetic requires the block; Local (or unset, which defaults to
// Local) must not carry one.
func TestSwarmadaConfig_CoordinateFrameExclusion(t *testing.T) {
	// Geodetic with the block → valid.
	geo := validConfig()
	geo.Spec.CoordinateSystem = fleetv1.SwarmadaCoordinateSystemConfig{
		ReferenceFrame: fleetv1.ReferenceFrameGeodetic,
		Geodetic: &fleetv1.GeodeticFrame{
			Datum:             fleetv1.GeodeticDatumWGS84,
			AltitudeReference: fleetv1.AltitudeReferenceAGL,
		},
	}
	assertValid(t, geo)

	// Geodetic without the block → rejected.
	geoMissing := validConfig()
	geoMissing.Spec.CoordinateSystem = fleetv1.SwarmadaCoordinateSystemConfig{
		ReferenceFrame: fleetv1.ReferenceFrameGeodetic,
	}
	assertRejected(t, geoMissing, "geodetic is required")

	// Local carrying a geodetic block → rejected.
	localWithGeo := validConfig()
	localWithGeo.Spec.CoordinateSystem = fleetv1.SwarmadaCoordinateSystemConfig{
		ReferenceFrame: fleetv1.ReferenceFrameLocal,
		Geodetic:       &fleetv1.GeodeticFrame{Datum: fleetv1.GeodeticDatumWGS84},
	}
	assertRejected(t, localWithGeo, "geodetic must be unset")

	// Unset frame (defaults to Local) with no geodetic block → valid.
	assertValid(t, validConfig())
}

// The inert estop field warns when set false rather than accepting the value in silence: an
// operator who writes false and sees the write succeed has every reason to believe estopped
// robots will now auto-resume, and they will not (ADR-0052).
func TestSwarmadaConfig_InertEstopFieldWarnsWhenSetFalse(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Estop.RequireExplicitClearAfterEstop = sigBoolPtr(false)
	warnings, err := validateSwarmadaConfig(cfg)
	if err != nil {
		t.Fatalf("setting an inert field must not be rejected: %v", err)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "requireExplicitClearAfterEstop") && strings.Contains(w, "no effect") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a warning that the field is inert, got %v", warnings)
	}

	// Unset and explicit true are the normal cases and warn about nothing.
	for _, v := range []*bool{nil, sigBoolPtr(true)} {
		ok := validConfig()
		ok.Spec.Estop.RequireExplicitClearAfterEstop = v
		warnings, err := validateSwarmadaConfig(ok)
		if err != nil {
			t.Fatalf("unexpected rejection: %v", err)
		}
		for _, w := range warnings {
			if strings.Contains(w, "requireExplicitClearAfterEstop") {
				t.Errorf("no warning expected for %v, got %q", v, w)
			}
		}
	}
}
