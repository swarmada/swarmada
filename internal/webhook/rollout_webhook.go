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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Rollout delete guards. `docs/operations.md` restricts `delete rollout` to a terminal record
// ("remove a terminal (Succeeded/Failed) record only"); these webhooks ENFORCE that instead of only
// documenting it, for both rollout kinds.
//
// Why a non-terminal rollout must not be deleted: a FirmwareRollout or ModelRollout in flight has
// robots mid-update — a ModelRollout has marked models `Updating`, which suspends the capabilities
// those models grant (§9.3.6), and the rollout's own status is what restores them. Deleting the
// record strands that state: nothing is left to advance the batch, complete it, or roll it back, and
// a robot can sit with suspended capabilities and no owner. The rollout is allowed to reach a
// terminal phase first (it can be Paused on its way there), and then the record is deletable.
//
// The permit-set is expressed positively so a phase added to the API later is refused by default
// rather than silently becoming deletable (fail closed).
var rolloutDeletablePhases = map[fleetv1.RolloutPhase]bool{
	fleetv1.RolloutPhaseSucceeded: true,
	fleetv1.RolloutPhaseFailed:    true,
}

var (
	firmwareRolloutGR = schema.GroupResource{Group: fleetv1.GroupVersion.Group, Resource: "firmwarerollouts"}
	// GroupKind for NewInvalid denials on the signing gate (NewInvalid takes a Kind, not a Resource).
	firmwareRolloutGK = schema.GroupKind{Group: fleetv1.GroupVersion.Group, Kind: "FirmwareRollout"}
	modelRolloutGR    = schema.GroupResource{Group: fleetv1.GroupVersion.Group, Resource: "modelrollouts"}
)

// rolloutDeleteForbidden builds the shared Forbidden response. It names only mechanisms that exist:
// a rollout is paused/allowed to settle, never "abandoned" (there is no abandon verb today).
func rolloutDeleteForbidden(gr schema.GroupResource, kind, ns, name string, phase fleetv1.RolloutPhase) error {
	return apierrors.NewForbidden(gr, name, fmt.Errorf(
		"%s is %s: only a terminal record (Succeeded/Failed) may be deleted, because a rollout that is "+
			"still live owns in-flight per-robot update state (a ModelRollout suspends model-granted "+
			"capabilities while Updating). Let it reach a terminal phase — pause it if you need it to "+
			"stop progressing (swarmctl pause rollout %s -n %s) — then delete the record",
		kind, phaseOrUnsetRollout(phase), name, ns))
}

// phaseOrUnsetRollout renders an empty phase readably in an admission message.
func phaseOrUnsetRollout(p fleetv1.RolloutPhase) string {
	if p == "" {
		return "in an unset phase (the controller has not reconciled it yet)"
	}
	return "in phase " + string(p)
}

// +kubebuilder:webhook:path=/validate-swarmada-io-v1-firmwarerollout,mutating=false,failurePolicy=fail,sideEffects=None,groups=swarmada.io,resources=firmwarerollouts,verbs=create;update;delete,versions=v1,name=vfirmwarerollout.swarmada.io,admissionReviewVersions=v1

// +kubebuilder:rbac:groups=swarmada.io,resources=swarmadaconfigs,verbs=get;list;watch

// FirmwareRolloutValidator guards FirmwareRollout admission in two ways: it refuses to delete a
// rollout that is not terminal, and it refuses to CREATE or UPDATE one that lacks
// spec.firmwareSignatureRef while the namespace enforces signature verification.
//
// The signing check is DEFENCE IN DEPTH, not the safety boundary. FirmwareRolloutReconciler already
// refuses to dispatch an unsigned artifact when signing is enforced ("refusing to dispatch unsigned
// artifact"), and that check stays authoritative — it runs against the artifact at dispatch time,
// after this one. What the webhook buys is FEEDBACK: `kubectl apply` fails immediately with the
// reason, instead of the rollout being admitted and then silently never progressing.
//
// Because it duplicates a rule, it must not DIVERGE from it: this validator resolves the signing
// policy exactly as the controller does (namespace SwarmadaConfig, absent config = cannot determine
// = deny) so an object the webhook admits is never one the controller will refuse.
//
// It runs failurePolicy=Fail, so the gate cannot be bypassed by taking the webhook down.
type FirmwareRolloutValidator struct{ Client client.Client }

// SetupWebhookWithManager registers the validator with the manager's webhook server.
func (v *FirmwareRolloutValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&fleetv1.FirmwareRollout{}).
		WithValidator(v).
		Complete()
}

var _ webhook.CustomValidator = &FirmwareRolloutValidator{}

// ValidateCreate refuses an unsigned rollout when the namespace enforces signing.
func (v *FirmwareRolloutValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.validateSigned(ctx, obj)
}

// ValidateUpdate applies the same rule to the NEW object. Validating the new object rather than
// diffing means an update that ADDS a missing signature ref is admitted (an operator can repair a
// rollout created before enforcement was switched on), while one that REMOVES it is refused.
func (v *FirmwareRolloutValidator) ValidateUpdate(ctx context.Context, _, newObj runtime.Object) (admission.Warnings, error) {
	return v.validateSigned(ctx, newObj)
}

// validateSigned mirrors FirmwareRolloutReconciler's unsigned-artifact refusal.
//
// Fail-closed at every step: an object of the wrong type, an unreadable signing policy, or a
// namespace with no SwarmadaConfig all DENY. The absent-config case is deliberate rather than
// permissive — the controller treats "cannot determine the policy" as a refusal, so admitting here
// would create a rollout that can never dispatch, which is a worse operator experience than a clear
// rejection at apply time.
func (v *FirmwareRolloutValidator) validateSigned(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	r, ok := obj.(*fleetv1.FirmwareRollout)
	if !ok {
		return nil, fmt.Errorf("expected a FirmwareRollout object but got %T", obj)
	}
	if v.Client == nil {
		return nil, fmt.Errorf("FirmwareRollout admission cannot resolve the namespace signing policy "+
			"(no client configured); refusing %q fail-closed", r.Name)
	}

	var configs fleetv1.SwarmadaConfigList
	if err := v.Client.List(ctx, &configs, client.InNamespace(r.Namespace)); err != nil {
		return nil, fmt.Errorf("resolving signing policy for namespace %q (fail closed): %w", r.Namespace, err)
	}
	if len(configs.Items) == 0 {
		return nil, apierrors.NewInvalid(firmwareRolloutGK, r.Name, field.ErrorList{
			field.Forbidden(field.NewPath("spec").Child("firmwareSignatureRef"),
				fmt.Sprintf("no SwarmadaConfig in namespace %q; cannot determine signing policy (fail closed) — "+
					"the FirmwareRollout controller refuses to dispatch under the same condition", r.Namespace)),
		})
	}
	if !configs.Items[0].Spec.Signing.RequireSignatureVerification {
		return nil, nil // signing not enforced; the artifact checksum still governs at dispatch
	}
	if r.Spec.FirmwareSignatureRef == "" {
		return nil, apierrors.NewInvalid(firmwareRolloutGK, r.Name, field.ErrorList{
			field.Required(field.NewPath("spec").Child("firmwareSignatureRef"),
				"firmwareSignatureRef is required when signing is enforced; refusing to admit unsigned artifact "+
					"(SwarmadaConfig.spec.signing.requireSignatureVerification is true)"),
		})
	}
	return nil, nil
}

// ValidateDelete permits deletion only of a terminal record.
func (v *FirmwareRolloutValidator) ValidateDelete(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	r, ok := obj.(*fleetv1.FirmwareRollout)
	if !ok {
		return nil, fmt.Errorf("expected a FirmwareRollout object but got %T", obj)
	}
	if rolloutDeletablePhases[r.Status.Phase] {
		return nil, nil
	}
	return nil, rolloutDeleteForbidden(firmwareRolloutGR, "FirmwareRollout", r.Namespace, r.Name, r.Status.Phase)
}

// +kubebuilder:webhook:path=/validate-swarmada-io-v1-modelrollout,mutating=false,failurePolicy=fail,sideEffects=None,groups=swarmada.io,resources=modelrollouts,verbs=delete,versions=v1,name=vmodelrollout.swarmada.io,admissionReviewVersions=v1

// ModelRolloutValidator refuses to delete a ModelRollout that is not terminal. The stakes are higher
// than for firmware: a live ModelRollout has marked models `Updating`, which suspends the
// capabilities those models grant, and its status is what un-suspends them.
type ModelRolloutValidator struct{}

// SetupWebhookWithManager registers the validator with the manager's webhook server.
func (v *ModelRolloutValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&fleetv1.ModelRollout{}).
		WithValidator(v).
		Complete()
}

var _ webhook.CustomValidator = &ModelRolloutValidator{}

// ValidateCreate is a no-op — creation is governed by the CRD schema.
func (v *ModelRolloutValidator) ValidateCreate(context.Context, runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// ValidateUpdate is a no-op — updates are governed by the CRD schema and the rollout controller.
func (v *ModelRolloutValidator) ValidateUpdate(context.Context, runtime.Object, runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// ValidateDelete permits deletion only of a terminal record.
func (v *ModelRolloutValidator) ValidateDelete(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	r, ok := obj.(*fleetv1.ModelRollout)
	if !ok {
		return nil, fmt.Errorf("expected a ModelRollout object but got %T", obj)
	}
	if rolloutDeletablePhases[r.Status.Phase] {
		return nil, nil
	}
	return nil, rolloutDeleteForbidden(modelRolloutGR, "ModelRollout", r.Namespace, r.Name, r.Status.Phase)
}
