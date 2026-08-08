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

package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/admission"
	"github.com/swarmada/swarmada/internal/audit"
)

// DiscoveredRobotReconciler sweeps un-admitted DiscoveredRobots toward their TTL
// (RFC-0001 §9.2.5): it marks one Stale as it approaches expiry and deletes it
// once expired. Admission (`swarmctl admit`) and rejection delete the resource, so
// a DiscoveredRobot that reaches its TTL was never acted on and is reaped.
//
// It cooperates with the registrar without coordination: a re-announce (Discover)
// rewrites phase=Discovered and a fresh ttlExpiresAt, so an actively-reconnecting
// robot is never reaped and its Stale marking clears on reconnect.
type DiscoveredRobotReconciler struct {
	client.Client

	// Recorder emits an audit Event when a robot is auto-admitted (ADR-0014), so the
	// zero-touch admission is visible in `kubectl get events` — auto-admit bypasses
	// the discoveredrobots/admit custom-verb gate that `swarmctl admit` enforces, so
	// the record of intent matters. Nil is tolerated (unit tests, no-op).
	Recorder record.EventRecorder

	// Audit seals ROBOT_REJECTED (§9.6.5.1) into the tamper-evident chain when an
	// operator rejects a staged robot. Nil disables recording; the rejection proceeds.
	Audit audit.Recorder

	// now overrides the clock in tests. Nil means time.Now.
	now func() time.Time
}

func (r *DiscoveredRobotReconciler) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// +kubebuilder:rbac:groups=swarmada.io,resources=discoveredrobots,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=swarmada.io,resources=discoveredrobots/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=swarmada.io,resources=robots,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=swarmada.io,resources=robotclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=swarmada.io,resources=fleetzones,verbs=get;list;watch
// +kubebuilder:rbac:groups=swarmada.io,resources=swarmadaconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile advances one DiscoveredRobot toward its TTL.
func (r *DiscoveredRobotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("discoveredrobot", req.NamespacedName)

	dr := &fleetv1.DiscoveredRobot{}
	if err := r.Get(ctx, req.NamespacedName, dr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Operator rejection (§9.1.2.7). `swarmctl reject` marks the object rather than
	// deleting it, and the deletion happens HERE, after the chain entry is sealed.
	//
	// That indirection is the whole point. A rejection and a TTL sweep both end in the
	// same delete, so a controller watching deletions cannot tell an operator's refusal
	// from a robot that simply never got admitted — and recording a sweep as a rejection
	// would put a decision nobody made into the safety record. The annotation is the
	// discriminating signal: only the SAR-gated `reject` verb writes it.
	if reason, rejected := dr.Annotations[annRobotRejected]; rejected {
		r.recordRejected(ctx, dr, reason)
		if err := r.Delete(ctx, dr); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		logger.Info("discovered robot rejected by operator; deleted", "reason", reason)
		return ctrl.Result{}, nil
	}

	// Operator admission (§9.1.2.5). `swarmctl admit` records the operator's parameters
	// here rather than creating the Robot itself, so promotion from discovered to
	// schedulable happens in exactly one place for both admission paths.
	//
	// Ordered ahead of auto-admit because an explicit decision outranks a namespace
	// default, and ahead of the TTL sweep so a marked robot is never reaped out from
	// under a decision that was already made.
	if _, marked := dr.Annotations[admission.AdmitAnnotation]; marked {
		handled, err := r.completeOperatorAdmit(ctx, dr)
		if err != nil || handled {
			return ctrl.Result{}, err
		}
		// An unusable mark falls through to the sweep below rather than pinning the object.
	}

	// Auto-admit (ADR-0014): if the namespace enables it and this robot's suggested
	// class matches, promote it to a Robot and delete the staging object. Runs before
	// the TTL sweep so a matching robot is admitted rather than reaped.
	if admitted, err := r.maybeAutoAdmit(ctx, dr); err != nil || admitted {
		return ctrl.Result{}, err
	}

	ttl := dr.Status.TTLExpiresAt
	if ttl == nil {
		return ctrl.Result{}, nil // no TTL to sweep against
	}
	now := r.clock()

	// Expired and still un-admitted → reap.
	if !now.Before(ttl.Time) {
		logger.Info("discovered robot TTL expired without admission; deleting")
		if err := r.Delete(ctx, dr); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		return ctrl.Result{}, nil
	}

	// Approaching expiry → mark Stale (the last quarter of the TTL window).
	staleAt := staleThreshold(dr)
	if !staleAt.IsZero() && !now.Before(staleAt) {
		if dr.Status.Phase != fleetv1.DiscoveredRobotPhaseStale {
			base := dr.DeepCopy()
			dr.Status.Phase = fleetv1.DiscoveredRobotPhaseStale
			if err := r.Status().Patch(ctx, dr, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, fmt.Errorf("marking discovered robot stale: %w", err)
			}
		}
		return ctrl.Result{RequeueAfter: ttl.Sub(now)}, nil
	}

	// Still fresh: requeue at the stale-transition time (else at expiry).
	next := ttl.Time
	if !staleAt.IsZero() && staleAt.Before(next) {
		next = staleAt
	}
	return ctrl.Result{RequeueAfter: next.Sub(now)}, nil
}

// staleThreshold is the instant a DiscoveredRobot becomes Stale: the last quarter
// of its connectedAt→ttlExpiresAt window. Returns the zero time when the window is
// missing/malformed, so staleness is skipped and only reaping applies.
func staleThreshold(dr *fleetv1.DiscoveredRobot) time.Time {
	ttl := dr.Status.TTLExpiresAt
	if ttl == nil {
		return time.Time{}
	}
	connected := dr.Status.ConnectedAt.Time
	window := ttl.Sub(connected)
	if connected.IsZero() || window <= 0 {
		return time.Time{}
	}
	return ttl.Add(-window / 4)
}

// maybeAutoAdmit promotes a DiscoveredRobot to a schedulable Robot when the
// namespace enables auto-admit (spec.provisioning.autoAdmitRobotClass +
// autoAdmitZone) and this robot's adapter-suggested class matches (ADR-0014). It is
// conservative and fail-safe: any missing precondition (config, matching class,
// zone, or an inert class-without-zone) is skipped, leaving the two-phase operator
// path intact. Returns (true, _) once the Robot is created and the DiscoveredRobot
// removed. Create-before-delete: a failed Create never loses the staging object.
func (r *DiscoveredRobotReconciler) maybeAutoAdmit(ctx context.Context, dr *fleetv1.DiscoveredRobot) (bool, error) {
	logger := log.FromContext(ctx)

	cfg, ok := namespaceConfig(ctx, r.Client, dr.Namespace)
	if !ok {
		return false, nil
	}
	prov := cfg.Spec.Provisioning
	if prov.AutoAdmitRobotClass == "" || dr.Status.SuggestedRobotClass != prov.AutoAdmitRobotClass {
		return false, nil
	}
	if prov.AutoAdmitZone == "" {
		logger.Info("auto-admit configured without autoAdmitZone; leaving robot for operator admission",
			"class", prov.AutoAdmitRobotClass, "robot", dr.Name)
		return false, nil
	}

	class := &fleetv1.RobotClass{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: dr.Namespace, Name: prov.AutoAdmitRobotClass}, class); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("auto-admit RobotClass not found; skipping", "class", prov.AutoAdmitRobotClass, "robot", dr.Name)
			return false, nil
		}
		return false, fmt.Errorf("auto-admit: getting robotclass: %w", err)
	}
	if class.Spec.BaseAdapter.Name == "" {
		logger.Info("auto-admit RobotClass has no baseAdapter; skipping", "class", class.Name, "robot", dr.Name)
		return false, nil
	}

	zone := &fleetv1.FleetZone{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: dr.Namespace, Name: prov.AutoAdmitZone}, zone); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("auto-admit zone not found; skipping", "zone", prov.AutoAdmitZone, "robot", dr.Name)
			return false, nil
		}
		return false, fmt.Errorf("auto-admit: getting fleetzone: %w", err)
	}

	robot := buildAutoAdmitRobot(dr, class, prov.AutoAdmitZone)
	if robot == nil {
		// Unreachable given the baseAdapter check above; skipping leaves the robot for the
		// operator path rather than admitting one the class failed to configure.
		logger.Info("auto-admit could not build a robot from the class; skipping", "class", class.Name, "robot", dr.Name)
		return false, nil
	}
	if err := r.Create(ctx, robot); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// A Robot with this name already exists (a prior admit): the staging
			// object is stale, so remove it.
			return true, client.IgnoreNotFound(r.Delete(ctx, dr))
		}
		return false, fmt.Errorf("auto-admit: creating robot: %w", err)
	}
	// Audit the privileged auto-admission before removing the staging object (the
	// Event references the DiscoveredRobot by UID and outlives it).
	r.recordAutoAdmit(dr, class.Name, prov.AutoAdmitZone)
	if err := r.Delete(ctx, dr); err != nil {
		return true, client.IgnoreNotFound(err)
	}
	logger.Info("auto-admitted discovered robot", "robot", robot.Name, "class", class.Name, "zone", prov.AutoAdmitZone)
	return true, nil
}

// completeOperatorAdmit creates the Robot an operator's admission decided on, then removes
// the staging object (§9.1.2.5).
//
// Returns handled=false when the mark cannot be acted on, so the caller falls through to the
// rest of Reconcile. That matters: a corrupt annotation must not shield the object from the
// TTL sweep, or a single bad payload would pin a DiscoveredRobot in the namespace forever.
func (r *DiscoveredRobotReconciler) completeOperatorAdmit(ctx context.Context, dr *fleetv1.DiscoveredRobot) (bool, error) {
	logger := log.FromContext(ctx)

	params, err := admission.DecodeParams(dr.Annotations[admission.AdmitAnnotation])
	if err != nil {
		// Not retryable: the payload will not decode differently next time. Surfaced as an
		// Event so the operator can see why their admission did not take and re-issue it.
		logger.Error(err, "unusable admission mark; ignoring", "discoveredrobot", dr.Name)
		r.recordAdmitFailed(dr, err)
		return false, nil
	}

	var class *fleetv1.RobotClass
	if params.RobotClass != "" {
		class = &fleetv1.RobotClass{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: dr.Namespace, Name: params.RobotClass}, class); err != nil {
			// Retryable, including NotFound: the CLI verified the class existed when the
			// operator admitted, so its absence now is a race the operator can resolve by
			// recreating it. Backing off beats silently admitting without the template.
			r.recordAdmitFailed(dr, fmt.Errorf("robotclass %q: %w", params.RobotClass, err))
			return false, fmt.Errorf("operator-admit: getting robotclass: %w", err)
		}
	}

	robot, err := admission.BuildRobot(dr, params, class, dr.Namespace)
	if err != nil {
		logger.Error(err, "admission parameters do not build a robot", "discoveredrobot", dr.Name)
		r.recordAdmitFailed(dr, err)
		return false, nil
	}

	if err := r.Create(ctx, robot); err != nil && !apierrors.IsAlreadyExists(err) {
		return false, fmt.Errorf("operator-admit: creating robot: %w", err)
	}
	// AlreadyExists is success, not a conflict: it means a previous attempt created the
	// Robot and failed before removing the staging object. Retrying the delete here is what
	// stops that crash window from leaving an orphan behind — the failure mode the old
	// CLI-side create-then-delete could only report as a warning.

	r.recordOperatorAdmit(dr, robot.Name, params)
	if err := r.Delete(ctx, dr); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	logger.Info("admitted discovered robot", "robot", robot.Name, "zone", params.Zone, "class", params.RobotClass)
	return true, nil
}

// recordOperatorAdmit notes the operator-driven promotion on the staging object. The chain
// entry is not written here: ROBOT_ADMITTED seals on the Robot's first reconcile, which is
// the transition that actually makes it schedulable, and already distinguishes this path
// from auto-admit.
func (r *DiscoveredRobotReconciler) recordOperatorAdmit(dr *fleetv1.DiscoveredRobot, robotName string, p admission.Params) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(dr, corev1.EventTypeNormal, "Admitted",
		"admitted to robot %q (class %q, zone %q) by operator", robotName, p.RobotClass, p.Zone)
}

// recordAdmitFailed surfaces an admission that was marked but could not be completed. Without
// it the operator sees a successful `swarmctl admit` and no robot, with nothing to explain
// the gap — the cost of moving the work off the command that reports success.
func (r *DiscoveredRobotReconciler) recordAdmitFailed(dr *fleetv1.DiscoveredRobot, cause error) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(dr, corev1.EventTypeWarning, "AdmissionFailed",
		"marked for admission but the robot could not be created: %v", cause)
}

// annRobotRejected marks a DiscoveredRobot an operator has rejected. Written only by the
// SAR-gated `swarmctl reject` verb; its value is the operator's reason. A TTL sweep never
// sets it, which is what keeps a reaped robot from being recorded as a refused one.
const annRobotRejected = "swarmada.io/rejected"

// recordRejected seals ROBOT_REJECTED (§9.6.5.1) before the staging object is removed.
//
// Sealed here rather than in `swarmctl` because the chain is single-writer by
// construction: audit.Log carries the per-namespace sequence number and previous hash in
// process, so a second writer would fork the chain rather than extend it. The CLI marks
// intent; the manager — which owns the chain — records the fact.
//
// The actor is this controller. The human who rejected is captured by the API-server audit
// for the annotation write, which was SAR-gated on the `reject` verb; the same division the
// estop-clear and cancel paths use.
func (r *DiscoveredRobotReconciler) recordRejected(ctx context.Context, dr *fleetv1.DiscoveredRobot, reason string) {
	if r.Audit == nil {
		return
	}
	if reason == "" {
		// Distinguished from a reason the operator gave: "none recorded" is itself a
		// finding when a rejection is reviewed later.
		reason = "no reason recorded by the operator"
	}
	if _, err := r.Audit.Record(audit.Entry{
		EventType: audit.EventRobotRejected,
		Namespace: dr.Namespace,
		Actor:     audit.Actor{Type: audit.ActorServiceAccount, Identity: "discoveredrobot-controller"},
		Resource:  audit.Resource{Kind: "DiscoveredRobot", Namespace: dr.Namespace, Name: dr.Name},
		Action:    "reject",
		Outcome:   audit.OutcomeDenied,
		Detail:    map[string]string{"reason": reason},
	}); err != nil {
		log.FromContext(ctx).Error(err, "recording ROBOT_REJECTED", "discoveredrobot", dr.Name)
	}
}

// recordAutoAdmit emits a namespace Event capturing a zero-touch admission (ADR-0014).
// Best-effort and nil-safe: a missing recorder (unit tests) or events RBAC gap costs
// only the record of intent, not the admission itself.
func (r *DiscoveredRobotReconciler) recordAutoAdmit(dr *fleetv1.DiscoveredRobot, class, zone string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(dr, corev1.EventTypeNormal, "AutoAdmitted",
		"auto-admitted to Robot (class %q, zone %q) per SwarmadaConfig.provisioning", class, zone)
}

// buildAutoAdmitRobot constructs the schedulable Robot for an auto-admit (ADR-0014).
//
// It delegates to the same builder the operator path uses, so the two admission routes cannot
// drift: when they were separate implementations of §9.1.2.5 they had already diverged, both
// dropping the robot's reported hardware in favour of the class template. The only difference
// that belongs here is the AutoAdmitted marker.
func buildAutoAdmitRobot(dr *fleetv1.DiscoveredRobot, class *fleetv1.RobotClass, zone string) *fleetv1.Robot {
	// Cannot fail: the class is checked for a baseAdapter before this is reached, which is
	// the builder's only error. Falling back to the un-templated robot would be worse than
	// skipping — it would admit a robot the class was supposed to configure.
	robot, err := admission.BuildRobot(dr, admission.Params{
		Zone:       zone,
		RobotClass: class.Name,
	}, class, dr.Namespace)
	if err != nil {
		return nil
	}
	// AutoAdmitted marks this Robot as auto-created, the eligibility gate for opt-in offline
	// auto-removal (ADR-0030) — operator-created robots lack it.
	robot.Annotations[fleetv1.AutoAdmittedAnnotation] = "true"
	return robot
}

// SetupWithManager registers the DiscoveredRobot sweeper.
func (r *DiscoveredRobotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1.DiscoveredRobot{}).
		Complete(r)
}
