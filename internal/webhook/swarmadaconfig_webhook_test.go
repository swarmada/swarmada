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
				RequireSignatureVerification: true,
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

func TestSwarmadaConfig_SignatureVerificationRequiresTrustRoots(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Signing.TrustRoots = nil
	assertRejected(t, cfg, "trustRoot is required")

	// Not requiring verification → no trust roots needed.
	off := validConfig()
	off.Spec.Signing.RequireSignatureVerification = false
	off.Spec.Signing.TrustRoots = nil
	assertValid(t, off)
}

// Multiple violations are all reported, not just the first.
func TestSwarmadaConfig_MultipleViolations(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Telemetry.Sink = fleetv1.TelemetrySink{Type: fleetv1.TelemetrySinkMimir} // no endpoint
	cfg.Spec.Signing.TrustRoots = nil                                                 // require but none
	_, err := validateSwarmadaConfig(cfg)
	if err == nil {
		t.Fatal("expected rejection")
	}
	if !strings.Contains(err.Error(), "endpoint is required") || !strings.Contains(err.Error(), "trustRoot is required") {
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
