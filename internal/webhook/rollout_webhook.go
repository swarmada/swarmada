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
// those models grant (RFC-0001 #control-plane-ota-model-update-manager), and the rollout's own status is what restores them. Deleting the
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
	modelRolloutGK    = schema.GroupKind{Group: fleetv1.GroupVersion.Group, Kind: "ModelRollout"}
)

// signatureRefRequired resolves the namespace's signing policy and answers whether an artifact
// reference is required. Both rollout validators go through it, so the firmware and model gates
// cannot drift apart on what "signing is enforced" means or on which uncertainties deny.
//
// Fail-closed at every step: no client, an unreadable policy, and a namespace with no
// SwarmadaConfig all return a denial rather than a permissive answer. The absent-config case is
// deliberate — FirmwareRolloutReconciler treats "cannot determine the policy" as a refusal to
// dispatch, so admitting here would create a rollout that can never dispatch, which is a worse
// operator experience than a clear rejection at apply time.
func signatureRefRequired(ctx context.Context, c client.Client, gk schema.GroupKind, path *field.Path,
	namespace, name string) (bool, error) {
	if c == nil {
		return false, fmt.Errorf("%s admission cannot resolve the namespace signing policy "+
			"(no client configured); refusing %q fail-closed", gk.Kind, name)
	}
	var configs fleetv1.SwarmadaConfigList
	if err := c.List(ctx, &configs, client.InNamespace(namespace)); err != nil {
		return false, fmt.Errorf("resolving signing policy for namespace %q (fail closed): %w", namespace, err)
	}
	if len(configs.Items) == 0 {
		return false, apierrors.NewInvalid(gk, name, field.ErrorList{
			field.Forbidden(path, fmt.Sprintf(
				"no SwarmadaConfig in namespace %q; cannot determine signing policy (fail closed)", namespace)),
		})
	}
	return configs.Items[0].Spec.Signing.SignatureVerificationRequired(), nil
}

// rolloutDeleteForbidden builds the shared Forbidden response. It names only mechanisms that
// exist: there is no abandon verb and no `pause rollout` command, so it points at the one path
// that actually moves a stuck rollout to a terminal phase.
//
// A Paused rollout gets a different instruction from a live one, because it is the case an
// operator is most likely to hit and the one where "let it reach a terminal phase" is useless
// advice: a rollout paused by pauseOnError makes no further progress on its own. `swarmctl
// rollout resume` excludes the robots that failed (ADR-0041), which lets it settle and become
// deletable.
func rolloutDeleteForbidden(gr schema.GroupResource, kind, ns, name string, phase fleetv1.RolloutPhase) error {
	remedy := fmt.Sprintf("let it reach a terminal phase, then delete the record (namespace %s)", ns)
	if phase == fleetv1.RolloutPhasePaused {
		remedy = fmt.Sprintf(
			"it is halted by pauseOnError and will not progress on its own: resume it with "+
				"`swarmctl rollout resume %s --kind %s -n %s`, which excludes the robots that failed "+
				"so the rollout can settle, then delete the record",
			name, rolloutKindFlag(kind), ns)
	}
	return apierrors.NewForbidden(gr, name, fmt.Errorf(
		"%s is %s: only a terminal record (Succeeded/Failed) may be deleted, because a rollout that is "+
			"still live owns in-flight per-robot update state (a ModelRollout suspends model-granted "+
			"capabilities while Updating). %s",
		kind, phaseOrUnsetRollout(phase), remedy))
}

// rolloutKindFlag maps a Kind to the --kind value `swarmctl rollout resume` expects.
func rolloutKindFlag(kind string) string {
	if kind == "ModelRollout" {
		return "model"
	}
	return "firmware"
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

// ValidateUpdate applies the same rule to the NEW object when the update changes the spec.
// Validating the new object rather than diffing means an update that ADDS a missing signature ref
// is admitted (an operator can repair a rollout created before enforcement was switched on), while
// one that REMOVES it is refused.
//
// An update that leaves spec unchanged is admitted without the check (rolloutSpecUnchanged).
func (v *FirmwareRolloutValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	if rolloutSpecUnchanged(oldObj, newObj) {
		return nil, nil
	}
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
	path := field.NewPath("spec").Child("firmwareSignatureRef")
	enforced, err := signatureRefRequired(ctx, v.Client, firmwareRolloutGK, path, r.Namespace, r.Name)
	if err != nil {
		return nil, err
	}
	if !enforced {
		return nil, nil // signing not enforced; the artifact checksum still governs at dispatch
	}
	if r.Spec.FirmwareSignatureRef == "" {
		return nil, apierrors.NewInvalid(firmwareRolloutGK, r.Name, field.ErrorList{
			field.Required(path,
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

// +kubebuilder:webhook:path=/validate-swarmada-io-v1-modelrollout,mutating=false,failurePolicy=fail,sideEffects=None,groups=swarmada.io,resources=modelrollouts,verbs=create;update;delete,versions=v1,name=vmodelrollout.swarmada.io,admissionReviewVersions=v1

// ModelRolloutValidator guards ModelRollout admission in two ways: it refuses to delete a rollout
// that is not terminal, and it refuses to CREATE or UPDATE one that lacks spec.modelSignatureRef
// while the namespace enforces signature verification. The delete stakes are higher than for
// firmware: a live ModelRollout has marked models `Updating`, which suspends the capabilities those
// models grant, and its status is what un-suspends them.
//
// THE SIGNING CHECK IS NOT DEFENCE IN DEPTH HERE, and that asymmetry with FirmwareRollout is the
// point. The control plane verifies a firmware artifact itself, in process, before any robot is
// dispatched, so the firmware webhook only moves an existing refusal earlier. It never holds the
// model bytes: ModelRolloutReconciler imports no signing, transparency-log or registry package, and
// the model artifact is verified by the Fleet Adapter on the robot, against the checksum and
// signature reference carried on the model_update Command, before it is installed.
//
// So this gate guarantees exactly one thing, and no more: under a namespace policy that requires
// signature verification, a ModelRollout cannot be created with no signature reference for the
// adapter to check against. It does not verify the artifact, and admission is not the model path's
// trust boundary — the adapter is.
//
// It resolves the signing policy through the same helper as the firmware gate, so the two cannot
// diverge on what enforcement means, and runs failurePolicy=Fail, so it cannot be bypassed by
// taking the webhook down.
type ModelRolloutValidator struct{ Client client.Client }

// SetupWebhookWithManager registers the validator with the manager's webhook server.
//
// It fills in the manager's client when none was injected. The signing gate is useless without one
// — it would deny every create — and the caller that registers this validator does so with a bare
// struct, so defaulting here is what makes the gate work rather than a convenience.
func (v *ModelRolloutValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	if v.Client == nil {
		v.Client = mgr.GetClient()
	}
	return ctrl.NewWebhookManagedBy(mgr).
		For(&fleetv1.ModelRollout{}).
		WithValidator(v).
		Complete()
}

var _ webhook.CustomValidator = &ModelRolloutValidator{}

// ValidateCreate refuses an unsigned rollout when the namespace enforces signing.
func (v *ModelRolloutValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.validateSigned(ctx, obj)
}

// ValidateUpdate applies the same rule to the NEW object when the update changes the spec, so an
// update that ADDS a missing signature ref is admitted and one that REMOVES it is refused. An
// update that leaves spec unchanged is admitted without the check (rolloutSpecUnchanged).
func (v *ModelRolloutValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	if rolloutSpecUnchanged(oldObj, newObj) {
		return nil, nil
	}
	return v.validateSigned(ctx, newObj)
}

// validateSigned refuses a ModelRollout with no signature reference under an enforcing policy.
func (v *ModelRolloutValidator) validateSigned(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	r, ok := obj.(*fleetv1.ModelRollout)
	if !ok {
		return nil, fmt.Errorf("expected a ModelRollout object but got %T", obj)
	}
	path := field.NewPath("spec").Child("modelSignatureRef")
	enforced, err := signatureRefRequired(ctx, v.Client, modelRolloutGK, path, r.Namespace, r.Name)
	if err != nil {
		return nil, err
	}
	if !enforced {
		return nil, nil // signing not enforced; modelChecksum still governs on the robot
	}
	if r.Spec.ModelSignatureRef == "" {
		return nil, apierrors.NewInvalid(modelRolloutGK, r.Name, field.ErrorList{
			field.Required(path,
				"modelSignatureRef is required when signing is enforced; the Fleet Adapter verifies the model "+
					"artifact on the robot and has nothing to verify against without it "+
					"(SwarmadaConfig.spec.signing.requireSignatureVerification is true)"),
		})
	}
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
