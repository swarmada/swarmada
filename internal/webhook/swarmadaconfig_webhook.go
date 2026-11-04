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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// swarmadaConfigGK is the GroupKind reported in admission error responses.
var swarmadaConfigGK = schema.GroupKind{Group: fleetv1.GroupVersion.Group, Kind: "SwarmadaConfig"}

// swarmadaConfigGR is the GroupResource used for SubjectAccessReview attributes on
// the namespace-scope estop custom verbs (the SwarmadaConfig is the namespace
// singleton that carries the namespace-wide estop-triggered annotation).
var swarmadaConfigGR = schema.GroupResource{Group: fleetv1.GroupVersion.Group, Resource: "swarmadaconfigs"}

// realTelemetrySinks are the sink types that write to an external store and so
// require an endpoint. The unset default ("") and Drop are the informed opt-outs
// and need none.
var realTelemetrySinks = map[fleetv1.TelemetrySinkType]bool{
	fleetv1.TelemetrySinkPrometheusRemoteWrite: true,
	fleetv1.TelemetrySinkVictoriaMetrics:       true,
	fleetv1.TelemetrySinkMimir:                 true,
}

// +kubebuilder:webhook:path=/validate-swarmada-io-v1-swarmadaconfig,mutating=false,failurePolicy=fail,sideEffects=None,groups=swarmada.io,resources=swarmadaconfigs,verbs=create;update,versions=v1,name=vswarmadaconfig.swarmada.io,admissionReviewVersions=v1

// SwarmadaConfigValidator enforces the cross-field consistency rules that CEL
// per-field validation cannot express (RFC-0001 #swarmadaconfig). The fixed name
// (swarmada-config) is already pinned by a CEL rule on the type; this webhook adds
// the six inter-block invariants:
//
//  1. actionCancellation.onDisconnect=AfterTimeout requires disconnectTimeoutSeconds.
//  2. trafficDeconfliction.recovery.disconnectedReservationTTLSeconds MUST exceed
//     actionCancellation.disconnectTimeoutSeconds (so a Revoking action's reservation
//     outlives the wall-clock cancellation, preventing a dual-execution window).
//  3. A real telemetry sink (PrometheusRemoteWrite/VictoriaMetrics/Mimir) requires
//     an endpoint; the unset default and Drop do not.
//  4. signing.requireSignatureVerification with no trustRoot is WARNED about, not
//     rejected (ADR-0051): it is incomplete rather than invalid, the control plane's own
//     bootstrap config is born in exactly that state, and every consumer of the policy
//     already fails closed on an empty trust-root set.
//  7. signing/estop informational fields that cannot be honoured are WARNED about rather
//     than accepted in silence (ADR-0052).
//  8. provisioning.mode: DirectRegister, which nothing implements, is WARNED about: the
//     write succeeds and robots are onboarded as under TwoPhase.
//  6. health.connectivityCriticalThresholdSeconds MUST strictly exceed
//     health.connectivityOfflineThresholdSeconds (RFC-0001 #safety-thresholds). The
//     graduated connectivity response is defined as Offline at T1 then Critical at T2;
//     with T2 <= T1 the escalation is due at or before the transition it escalates
//     from, so the two-stage response collapses. Each field is separately bounded by
//     CEL, but a comparison between two fields is exactly what CEL per-field
//     validation cannot express, which is why it belongs here.
//  5. coordinateSystem.referenceFrame=Geodetic requires the geodetic block, and
//     referenceFrame=Local (the default) MUST NOT carry one — the two frames are
//     mutually exclusive (RFC-0001 #swarmadaconfig-coordinate-system).
//
// It runs failurePolicy=Fail: a SwarmadaConfig that cannot be validated is not
// admitted. Beyond the intra-object cross-field checks it also gates the
// namespace-scope estop custom verbs (RFC-0001 #security-authorization-swarmada-rbac-roles) when the estop-triggered annotation
// is written on the config.
//
// It writes no audit entry: SWARMADA_CONFIG_MODIFIED records a change the API server stored,
// and the SwarmadaConfig controller writes it once it observes one.
type SwarmadaConfigValidator struct {
	// EstopAuthz authorizes the namespace-scope estop-trigger / estop-clear custom
	// verbs when the swarmada.io/estop-triggered annotation changes on the
	// SwarmadaConfig. A nil authorizer makes any estop-annotation change fail closed;
	// the cross-field validation below is unaffected.
	EstopAuthz VerbAuthorizer
}

// SetupWebhookWithManager registers the validator with the manager's webhook server.
func (v *SwarmadaConfigValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&fleetv1.SwarmadaConfig{}).
		WithValidator(v).
		Complete()
}

var _ webhook.CustomValidator = &SwarmadaConfigValidator{}

// ValidateCreate runs the cross-field rules on creation, refuses a create that
// already carries the estop idempotence marker, and authorizes estop-trigger when the
// config is created already carrying the namespace-wide estop-triggered annotation.
func (v *SwarmadaConfigValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	cfg, ok := obj.(*fleetv1.SwarmadaConfig)
	if !ok {
		return nil, fmt.Errorf("expected a SwarmadaConfig object but got %T", obj)
	}
	// See FleetZoneValidator.ValidateCreate: a namespace-scope fan-out is suppressed
	// just as well by a marker that arrives at birth. The context is needed here
	// because the guard attributes the write to the requesting principal.
	if err := GuardEstopProcessedMarker(ctx, swarmadaConfigGR, cfg.Namespace, cfg.Name,
		nil, cfg.Annotations); err != nil {
		return nil, err
	}
	if err := guardEstopActor(ctx, swarmadaConfigGR, cfg.Namespace, cfg.Name,
		nil, cfg.Annotations, estopActorStampKeys); err != nil {
		return nil, err
	}
	// See FleetZoneValidator.ValidateCreate. Here the estop a create delivers is
	// namespace-wide.
	if verb, isEstopChange := estopVerbForAnnotations(nil, cfg.Annotations); isEstopChange {
		if err := authorizeEstopVerb(ctx, v.EstopAuthz, swarmadaConfigGR, cfg.Namespace, cfg.Name, verb); err != nil {
			return nil, err
		}
	}
	return validateSwarmadaConfig(cfg)
}

// ValidateUpdate runs the cross-field rules on every update but one: an update whose
// only change is to the estop annotations, which is what the namespace estop and its
// clear are. The rules read nothing but this object's spec, and that write leaves the
// spec as it was stored, so they could refuse it only for a spec admitted before a rule
// was added. That is no reason to refuse a stop.
func (v *SwarmadaConfigValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	cfg, ok := newObj.(*fleetv1.SwarmadaConfig)
	if !ok {
		return nil, fmt.Errorf("expected a SwarmadaConfig object but got %T", newObj)
	}
	oldCfg, ok := oldObj.(*fleetv1.SwarmadaConfig)
	if !ok {
		return nil, fmt.Errorf("expected a SwarmadaConfig object but got %T", oldObj)
	}
	// As on a FleetZone: the stop finalizer of a config that carries the namespace stop is the
	// control plane's or an estop-clear holder's to remove.
	if err := refuseStopFinalizerRemoval(ctx, v.EstopAuthz, nil, swarmadaConfigGR, oldCfg, cfg); err != nil {
		return nil, err
	}
	// Authorize a namespace-scope estop when the estop-triggered annotation changes
	// Adding or re-valuing needs estop-trigger; removing needs estop-clear.
	if verb, isEstopChange := estopVerbForAnnotations(oldCfg.Annotations, cfg.Annotations); isEstopChange {
		if err := authorizeEstopVerb(ctx, v.EstopAuthz, swarmadaConfigGR, cfg.Namespace, cfg.Name, verb); err != nil {
			return nil, err
		}
	}
	// Unconditional, NOT inside the branch above: a forged marker arrives on an
	// update that leaves the estop-triggered annotation alone, so the verb gate is never
	// consulted, and the marker then suppresses the fan-out of the trigger that follows.
	if err := GuardEstopProcessedMarker(ctx, swarmadaConfigGR, cfg.Namespace, cfg.Name,
		oldCfg.Annotations, cfg.Annotations); err != nil {
		return nil, err
	}
	// Unconditional too: a rewritten actor also arrives on an update that leaves the trigger alone.
	if err := guardEstopActor(ctx, swarmadaConfigGR, cfg.Namespace, cfg.Name,
		oldCfg.Annotations, cfg.Annotations, estopActorStampKeys); err != nil {
		return nil, err
	}
	// An emergency stop is never refused for something unrelated to the stop: the estop
	// verb and the marker have been checked above, and nothing else in this write changed.
	// RFC-0001 #safety-emergency-stop-protocol.
	if onlyEstopAnnotationsChangedOn(oldCfg, cfg, nil,
		func(c *fleetv1.SwarmadaConfig) { c.Status = fleetv1.SwarmadaConfigStatus{} }) {
		return nil, nil
	}
	return validateSwarmadaConfig(cfg)
}

// ValidateDelete is a no-op; deletions are always permitted.
func (v *SwarmadaConfigValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func validateSwarmadaConfig(cfg *fleetv1.SwarmadaConfig) (admission.Warnings, error) {
	var errs field.ErrorList
	var warnings admission.Warnings
	specPath := field.NewPath("spec")

	// 1 & 2 — action-cancellation / reservation-TTL consistency.
	tc := cfg.Spec.ActionCancellation
	tcPath := specPath.Child("actionCancellation")
	if tc.OnDisconnect == fleetv1.ActionCancellationAfterTimeout {
		if tc.DisconnectTimeoutSeconds == nil {
			errs = append(errs, field.Required(tcPath.Child("disconnectTimeoutSeconds"),
				"disconnectTimeoutSeconds is required when onDisconnect is AfterTimeout"))
		} else {
			ttl := cfg.Spec.TrafficDeconfliction.DisconnectedReservationTTLSeconds
			if ttl <= *tc.DisconnectTimeoutSeconds {
				errs = append(errs, field.Invalid(
					specPath.Child("trafficDeconfliction").Child("disconnectedReservationTTLSeconds"),
					ttl,
					fmt.Sprintf("disconnectedReservationTTLSeconds must exceed disconnectTimeoutSeconds (%d) to prevent a dual-execution window when onDisconnect is AfterTimeout",
						*tc.DisconnectTimeoutSeconds)))
			}
		}
	}

	// 3 — a real telemetry sink requires an endpoint.
	sink := cfg.Spec.Telemetry.Sink
	if realTelemetrySinks[sink.Type] && sink.Endpoint == "" {
		errs = append(errs, field.Required(
			specPath.Child("telemetry").Child("sink").Child("endpoint"),
			fmt.Sprintf("endpoint is required for a real telemetry sink (type=%s); use type Drop to discard telemetry", sink.Type)))
	}

	// 6 — the connectivity thresholds must stay ordered. The bounds OVERLAP (offline
	// reaches 3600, critical starts at 30), so an inverted pair is individually legal
	// under CEL and only a cross-field check catches it.
	//
	// Zero means UNSET, never "zero seconds": the API server applies the CEL defaults
	// (30/120) before admission, and the CEL minimums (5 and 30) make an explicit zero
	// unrepresentable. So an unset pair must be skipped rather than read as 0 <= 0 —
	// otherwise every object built programmatically before defaulting is rejected, which
	// is a validator that fails closed on the wrong condition.
	h := cfg.Spec.Health
	hPath := specPath.Child("health")
	bothSet := h.ConnectivityCriticalThresholdSeconds > 0 && h.ConnectivityOfflineThresholdSeconds > 0
	if bothSet && h.ConnectivityCriticalThresholdSeconds <= h.ConnectivityOfflineThresholdSeconds {
		errs = append(errs, field.Invalid(
			hPath.Child("connectivityCriticalThresholdSeconds"),
			h.ConnectivityCriticalThresholdSeconds,
			fmt.Sprintf("connectivityCriticalThresholdSeconds must be greater than connectivityOfflineThresholdSeconds (%d); "+
				"the Critical escalation follows the Offline transition, and an equal or lower value collapses the two-stage response",
				h.ConnectivityOfflineThresholdSeconds)))
	}

	// 4 — signature verification with no trust anchor is INCOMPLETE, not invalid, and is
	// warned about rather than rejected (ADR-0051).
	//
	// This was a hard rejection and it made the control plane unable to install its own
	// secure default. SwarmadaConfigBootstrapReconciler creates an empty-spec config on the
	// first FleetZone in a namespace; defaulting fills in requireSignatureVerification=true
	// with no trustRoots, which is precisely the pair this rule rejected — so the reconciler
	// hot-looped, and because this webhook is failurePolicy=fail, a namespace could reach a
	// state where the config that would break the loop could not be written either.
	//
	// The control plane cannot satisfy the invariant on its own: a trust root is a public key
	// in an operator-held Secret, and there is nothing for a controller to put there. Nor can
	// it skip creating the object: a namespace with NO SwarmadaConfig now enforces
	// verification with nowhere to put a trust root (ADR-0053), so nothing in it can qualify
	// until an operator writes the config. The auto-created object is what spares them that,
	// and it is still the control that makes default-on real.
	//
	// Nothing is permitted that was previously refused. Every consumer of this policy fails
	// closed on an empty trust-root set — loadTrustRoots errors, verifyConformance fails the
	// adapter, the rollout webhook demands a signature ref — so an incomplete namespace
	// REFUSES work rather than accepting unverified work. What changes is where the operator
	// is told: at first use, fail-closed and named, instead of at a write that the control
	// plane itself cannot make.
	signing := cfg.Spec.Signing
	if signing.SignatureVerificationRequired() && len(signing.TrustRoots) == 0 {
		warnings = append(warnings, fmt.Sprintf(
			"spec.signing: requireSignatureVerification is true and no trustRoots are configured, so %q "+
				"cannot verify any firmware or model artifact: every signature check will fail closed "+
				"until at least one trustRoot is added. Set spec.signing.requireSignatureVerification "+
				"to false to turn verification off deliberately.", cfg.Name))
	}

	// 7 — requireExplicitClearAfterEstop is inert: the prohibition it names is enforced by
	// the absence of any auto-clear path, not by the field, so setting it false changes
	// nothing. Say so instead of accepting the value silently — an operator who writes false
	// and sees the write succeed has every reason to believe estopped robots will now
	// auto-resume. Warned rather than rejected because rejecting would fail manifests that
	// apply cleanly today, for a field whose behaviour does not change either way (ADR-0052).
	if e := cfg.Spec.Estop.RequireExplicitClearAfterEstop; e != nil && !*e {
		warnings = append(warnings, "spec.estop.requireExplicitClearAfterEstop: this field is "+
			"informational and is always enforced: a robot that acknowledged an estop cannot resume "+
			"without an explicit operator clear, because the control plane has no auto-clear path. "+
			"Setting it to false has no effect.")
	}

	// 8 — provisioning.mode: DirectRegister is served and implemented by nothing (see
	// unread_fields.go). Warned on every write, as 4 and 7 are: this is an object an
	// operator writes by hand, and a namespace that keeps the value keeps the surprise.
	warnings = append(warnings, swarmadaConfigUnreadFieldWarnings(cfg)...)

	// 5 — coordinate-frame consistency: Geodetic requires the geodetic block;
	// Local (the default) must not carry one. The two frames are mutually
	// exclusive and the control plane never re-projects between them.
	cs := cfg.Spec.CoordinateSystem
	csPath := specPath.Child("coordinateSystem")
	switch cs.ReferenceFrame {
	case fleetv1.ReferenceFrameGeodetic:
		if cs.Geodetic == nil {
			errs = append(errs, field.Required(csPath.Child("geodetic"),
				"geodetic is required when referenceFrame is Geodetic"))
		}
	default: // Local, or unset (which defaults to Local)
		if cs.Geodetic != nil {
			errs = append(errs, field.Invalid(csPath.Child("geodetic"), cs.Geodetic,
				"geodetic must be unset when referenceFrame is Local"))
		}
	}

	if len(errs) == 0 {
		return warnings, nil
	}
	return warnings, apierrors.NewInvalid(swarmadaConfigGK, cfg.Name, errs)
}
