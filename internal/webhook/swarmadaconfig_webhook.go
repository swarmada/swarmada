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
	"reflect"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
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
// per-field validation cannot express (RFC-0001 §9.1.11). The fixed name
// (swarmada-config) is already pinned by a CEL rule on the type; this webhook adds
// the six inter-block invariants:
//
//  1. actionCancellation.onDisconnect=AfterTimeout requires disconnectTimeoutSeconds.
//  2. trafficDeconfliction.recovery.disconnectedReservationTTLSeconds MUST exceed
//     actionCancellation.disconnectTimeoutSeconds (so a Revoking action's reservation
//     outlives the wall-clock cancellation, preventing a dual-execution window).
//  3. A real telemetry sink (PrometheusRemoteWrite/VictoriaMetrics/Mimir) requires
//     an endpoint; the unset default and Drop do not.
//  4. signing.requireSignatureVerification requires at least one trustRoot —
//     fail-closed verification with no trust anchor would reject every artifact.
//  6. health.connectivityCriticalThresholdSeconds MUST strictly exceed
//     health.connectivityOfflineThresholdSeconds (RFC-0001 #safety-thresholds). The
//     graduated connectivity response is defined as Offline at T1 then Critical at T2;
//     with T2 <= T1 the escalation is due at or before the transition it escalates
//     from, so the two-stage response collapses. Each field is separately bounded by
//     CEL, but a comparison between two fields is exactly what CEL per-field
//     validation cannot express, which is why it belongs here.
//  5. coordinateSystem.referenceFrame=Geodetic requires the geodetic block, and
//     referenceFrame=Local (the default) MUST NOT carry one — the two frames are
//     mutually exclusive (§9.1.11.11).
//
// It runs failurePolicy=Fail: a SwarmadaConfig that cannot be validated is not
// admitted. Beyond the intra-object cross-field checks it also gates the
// namespace-scope estop custom verbs (§F-2b) when the estop-triggered annotation
// is written on the config.
type SwarmadaConfigValidator struct {
	// Audit records SWARMADA_CONFIG_MODIFIED into the tamper-evident chain
	// (§9.6.5.1) when a spec change is admitted. The webhook is the only place that
	// sees BOTH the diff and the authenticated user, which is what makes the entry
	// worth writing here rather than in a controller. Nil disables the record; the
	// validation below is unaffected.
	Audit audit.Recorder

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

// ValidateCreate runs the cross-field rules on creation.
func (v *SwarmadaConfigValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	cfg, ok := obj.(*fleetv1.SwarmadaConfig)
	if !ok {
		return nil, fmt.Errorf("expected a SwarmadaConfig object but got %T", obj)
	}
	return validateSwarmadaConfig(cfg)
}

// ValidateUpdate runs the cross-field rules on every spec update — unlike the Robot
// gate, these are cheap intra-object checks with no external dependency, so there is
// no reason to skip them.
func (v *SwarmadaConfigValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	cfg, ok := newObj.(*fleetv1.SwarmadaConfig)
	if !ok {
		return nil, fmt.Errorf("expected a SwarmadaConfig object but got %T", newObj)
	}
	oldCfg, ok := oldObj.(*fleetv1.SwarmadaConfig)
	if !ok {
		return nil, fmt.Errorf("expected a SwarmadaConfig object but got %T", oldObj)
	}
	// Authorize a namespace-scope estop when the estop-triggered annotation changes
	// (§F-2b): adding/re-valuing needs estop-trigger, removing needs estop-clear.
	if verb, isEstopChange := estopVerbForAnnotations(oldCfg.Annotations, cfg.Annotations); isEstopChange {
		if err := authorizeEstopVerb(ctx, v.EstopAuthz, swarmadaConfigGR, cfg.Namespace, cfg.Name, verb); err != nil {
			return nil, err
		}
	}
	warnings, err := validateSwarmadaConfig(cfg)
	if err != nil {
		return warnings, err
	}
	// Record only an ADMITTED change, and only when the spec actually moved: a
	// status-only or metadata-only update is not a configuration modification, and
	// recording one would bury the real changes in noise.
	if !reflect.DeepEqual(oldCfg.Spec, cfg.Spec) {
		recordConfigModified(ctx, v.Audit, cfg)
	}
	return warnings, nil
}

// recordConfigModified writes SWARMADA_CONFIG_MODIFIED. Best-effort by design: the
// change has already been admitted by the time this runs, so a sink failure must not
// turn a valid update into a rejection — it is logged and the admission stands.
func recordConfigModified(ctx context.Context, rec audit.Recorder, cfg *fleetv1.SwarmadaConfig) {
	if rec == nil {
		return
	}
	actor := audit.Actor{Type: audit.ActorServiceAccount, Identity: "swarmadaconfig-webhook"}
	if req, err := admission.RequestFromContext(ctx); err == nil && req.UserInfo.Username != "" {
		actor = audit.Actor{Type: audit.ActorUser, Identity: req.UserInfo.Username}
	}
	if _, err := rec.Record(audit.Entry{
		EventType: audit.EventSwarmadaConfigMod,
		Namespace: cfg.Namespace,
		Actor:     actor,
		Resource:  audit.Resource{Kind: "SwarmadaConfig", Namespace: cfg.Namespace, Name: cfg.Name},
		Action:    "update",
		Outcome:   audit.OutcomeAllowed,
	}); err != nil {
		log.FromContext(ctx).Error(err, "recording SWARMADA_CONFIG_MODIFIED audit entry",
			"namespace", cfg.Namespace, "name", cfg.Name)
	}
}

// ValidateDelete is a no-op; deletions are always permitted.
func (v *SwarmadaConfigValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func validateSwarmadaConfig(cfg *fleetv1.SwarmadaConfig) (admission.Warnings, error) {
	var errs field.ErrorList
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

	// 4 — fail-closed signature verification requires a trust anchor.
	signing := cfg.Spec.Signing
	if signing.RequireSignatureVerification && len(signing.TrustRoots) == 0 {
		errs = append(errs, field.Required(
			specPath.Child("signing").Child("trustRoots"),
			"at least one trustRoot is required when requireSignatureVerification is true"))
	}

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
		return nil, nil
	}
	return nil, apierrors.NewInvalid(swarmadaConfigGK, cfg.Name, errs)
}
