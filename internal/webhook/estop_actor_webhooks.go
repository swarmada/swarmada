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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// The estop-actor stamping mutators (ADR-0046). One per carrier kind, each a thin shell
// over the shared helpers in estop_actor_defaulter.go.
//
// failurePolicy=Ignore on every one of them, and that is the whole design in a marker:
// attribution is best-effort, authorization is not. If the webhook server is unreachable
// these mutators are skipped and the estop still lands — it degrades to an unattributed
// audit entry. The VALIDATING webhooks on the same resources keep failurePolicy=fail and
// keep enforcing the estop-trigger/estop-clear SubjectAccessReview, so nothing about
// authorization is weakened by this file.
//
// Robot is absent here: it already has a mutating webhook (RobotDefaulter), and adding a
// second on the same resource+path is not possible. RobotDefaulter.Default calls
// stampEstopActor directly.

// ── FleetZone: zone-scoped estop (§9.6.2.5) ──────────────────────────────────

// +kubebuilder:webhook:path=/mutate-swarmada-io-v1-fleetzone,mutating=true,failurePolicy=ignore,sideEffects=None,groups=swarmada.io,resources=fleetzones,verbs=create;update,versions=v1,name=mfleetzone.swarmada.io,admissionReviewVersions=v1

// FleetZoneEstopActorDefaulter stamps the authenticated operator onto a zone estop
// trigger or clear, so ZoneEstopReconciler can attribute ESTOP_TRIGGERED/ESTOP_CLEARED.
type FleetZoneEstopActorDefaulter struct{}

// SetupWebhookWithManager registers the mutating path derived from the
// +kubebuilder:webhook marker above.
func (d *FleetZoneEstopActorDefaulter) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&fleetv1.FleetZone{}).WithDefaulter(d).Complete()
}

var _ webhook.CustomDefaulter = &FleetZoneEstopActorDefaulter{}

// Default stamps the authenticated operator when this request is a zone estop trigger or clear.
func (d *FleetZoneEstopActorDefaulter) Default(ctx context.Context, obj runtime.Object) error {
	z, ok := obj.(*fleetv1.FleetZone)
	if !ok {
		return fmt.Errorf("expected a FleetZone object but got %T", obj)
	}
	stampEstopActor(ctx, z)
	return nil
}

// ── SwarmadaConfig: namespace-scoped estop (§9.6.2.5) ────────────────────────

// +kubebuilder:webhook:path=/mutate-swarmada-io-v1-swarmadaconfig,mutating=true,failurePolicy=ignore,sideEffects=None,groups=swarmada.io,resources=swarmadaconfigs,verbs=create;update,versions=v1,name=mswarmadaconfig.swarmada.io,admissionReviewVersions=v1

// SwarmadaConfigEstopActorDefaulter stamps the authenticated operator onto a
// namespace-wide estop trigger or clear.
type SwarmadaConfigEstopActorDefaulter struct{}

// SetupWebhookWithManager registers the mutating path derived from the
// +kubebuilder:webhook marker above.
func (d *SwarmadaConfigEstopActorDefaulter) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&fleetv1.SwarmadaConfig{}).WithDefaulter(d).Complete()
}

var _ webhook.CustomDefaulter = &SwarmadaConfigEstopActorDefaulter{}

// Default stamps the authenticated operator when this request is a namespace estop trigger or clear.
func (d *SwarmadaConfigEstopActorDefaulter) Default(ctx context.Context, obj runtime.Object) error {
	cfg, ok := obj.(*fleetv1.SwarmadaConfig)
	if !ok {
		return fmt.Errorf("expected a SwarmadaConfig object but got %T", obj)
	}
	stampEstopActor(ctx, cfg)
	return nil
}

// ── FirmwareRollout / ModelRollout: operator resume (ADR-0041) ───────────────
//
// Both kinds seal ROLLOUT_RESUMED with the same synthetic controller actor, so both are
// stamped. Leaving ModelRollout out would make attribution differ between two paths an
// operator reasonably reads as one feature.

// +kubebuilder:webhook:path=/mutate-swarmada-io-v1-firmwarerollout,mutating=true,failurePolicy=ignore,sideEffects=None,groups=swarmada.io,resources=firmwarerollouts,verbs=create;update,versions=v1,name=mfirmwarerollout.swarmada.io,admissionReviewVersions=v1

// FirmwareRolloutResumeActorDefaulter stamps the authenticated operator onto a
// swarmada.io/rollout-resume write.
type FirmwareRolloutResumeActorDefaulter struct{}

// SetupWebhookWithManager registers the mutating path derived from the
// +kubebuilder:webhook marker above.
func (d *FirmwareRolloutResumeActorDefaulter) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&fleetv1.FirmwareRollout{}).WithDefaulter(d).Complete()
}

var _ webhook.CustomDefaulter = &FirmwareRolloutResumeActorDefaulter{}

// Default stamps the authenticated operator when this request is a firmware rollout resume.
func (d *FirmwareRolloutResumeActorDefaulter) Default(ctx context.Context, obj runtime.Object) error {
	r, ok := obj.(*fleetv1.FirmwareRollout)
	if !ok {
		return fmt.Errorf("expected a FirmwareRollout object but got %T", obj)
	}
	stampResumeActor(ctx, r)
	return nil
}

// +kubebuilder:webhook:path=/mutate-swarmada-io-v1-modelrollout,mutating=true,failurePolicy=ignore,sideEffects=None,groups=swarmada.io,resources=modelrollouts,verbs=create;update,versions=v1,name=mmodelrollout.swarmada.io,admissionReviewVersions=v1

// ModelRolloutResumeActorDefaulter stamps the authenticated operator onto a
// swarmada.io/rollout-resume write.
type ModelRolloutResumeActorDefaulter struct{}

// SetupWebhookWithManager registers the mutating path derived from the
// +kubebuilder:webhook marker above.
func (d *ModelRolloutResumeActorDefaulter) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&fleetv1.ModelRollout{}).WithDefaulter(d).Complete()
}

var _ webhook.CustomDefaulter = &ModelRolloutResumeActorDefaulter{}

// Default stamps the authenticated operator when this request is a model rollout resume.
func (d *ModelRolloutResumeActorDefaulter) Default(ctx context.Context, obj runtime.Object) error {
	r, ok := obj.(*fleetv1.ModelRollout)
	if !ok {
		return fmt.Errorf("expected a ModelRollout object but got %T", obj)
	}
	stampResumeActor(ctx, r)
	return nil
}
