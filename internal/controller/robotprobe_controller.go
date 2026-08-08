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
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/probe"
)

// Fail-safe defaults when spec.intervalSeconds / spec.timeoutSeconds are unset AND
// the namespace SwarmadaConfig is unreadable. They match the CRD defaults for
// spec.health.defaultHardwareProbeIntervalSeconds (30) and
// defaultProbeTimeoutSeconds (5); the namespace config overrides them when present.
const (
	defaultProbeIntervalSeconds int32 = 30
	defaultProbeTimeoutSeconds  int32 = 5
	// Fail-safe debounce thresholds (ADR-0012) when neither the per-probe field nor
	// the namespace SwarmadaConfig supplies a positive value.
	defaultProbeFailureThreshold  int32 = 3
	defaultProbeRecoveryThreshold int32 = 2
)

// RobotProbeReconciler runs active RobotProbe health checks (RFC-0001 §9.1.6): it
// verifies each selected robot via its Fleet Adapter and binds the proto
// ProbeStatus into RobotProbe.status. Binding is fail-safe: a probe that cannot
// confirm health (RPC error, timeout, or unsupported) is never reported Healthy.
type RobotProbeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Audit seals PROBE_FAILURE (§9.6.5.1) into the tamper-evident chain. Nil disables
	// recording; probing is unaffected.
	Audit audit.Recorder
	// Prober runs the verify RPC. Nil means no prober is wired (the ControlStream
	// command-push path is not up); every robot then reports Unknown, never Healthy.
	Prober probe.Prober
}

// +kubebuilder:rbac:groups=swarmada.io,resources=robotprobes,verbs=get;list;watch
// +kubebuilder:rbac:groups=swarmada.io,resources=robotprobes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=swarmada.io,resources=robots,verbs=get;list;watch
// +kubebuilder:rbac:groups=swarmada.io,resources=robots/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=swarmada.io,resources=swarmadaconfigs,verbs=get;list;watch

// Reconcile runs one probe cycle.
func (r *RobotProbeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("robotprobe", req.NamespacedName)

	rp := &fleetv1.RobotProbe{}
	if err := r.Get(ctx, req.NamespacedName, rp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	sel, err := metav1.LabelSelectorAsSelector(&rp.Spec.RobotSelector)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("invalid robotSelector: %w", err)
	}
	var robots fleetv1.RobotList
	if err := r.List(ctx, &robots, client.InNamespace(req.Namespace),
		client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing target robots: %w", err)
	}

	// Namespace kill-switch (ADR-0024): when disableAllProbes is set, issue NO
	// Verify RPCs and report every matched robot as Unknown/paused. Evaluated once,
	// here at the cycle boundary — a cycle already past this point runs to
	// completion; a flip takes effect no later than the next cycle (and promptly via
	// the SwarmadaConfig watch). Passive telemetry is unaffected.
	if r.probesDisabled(ctx, req.Namespace) {
		return r.reconcilePaused(ctx, rp, robots.Items)
	}

	// Interval and per-probe timeout fall back to the namespace defaults
	// (spec.health.*) when the RobotProbe leaves them unset, and to the built-in
	// constants when no SwarmadaConfig is readable.
	defIntervalS, defTimeoutS := r.probeDefaults(ctx, req.Namespace)

	timeoutS := rp.Spec.TimeoutSeconds
	if timeoutS <= 0 {
		timeoutS = defTimeoutS
	}

	expected := toFloatMetrics(rp.Spec.ExpectedMetrics)
	failureT, recoveryT := r.probeThresholds(ctx, rp)

	// Carry per-robot streaks forward from the previous cycle's results.
	prevByRobot := make(map[string]fleetv1.RobotProbeRobotResult, len(rp.Status.RobotResults))
	for _, pr := range rp.Status.RobotResults {
		prevByRobot[pr.RobotName] = pr
	}

	results := make([]fleetv1.RobotProbeRobotResult, 0, len(robots.Items))
	for i := range robots.Items {
		robot := &robots.Items[i]
		status, msg, probeMetrics := r.probeRobot(ctx, rp, robot.Name, expected, timeoutS)

		prev := prevByRobot[robot.Name]
		failures, successes := prev.ConsecutiveFailures, prev.ConsecutiveSuccesses
		failedAt := prev.FailedAt
		switch {
		case isFailingProbe(status):
			failures++
			successes = 0
			if failedAt == nil {
				now := metav1.Now()
				failedAt = &now
			}
		case status == probe.StatusHealthy:
			successes++
			failures = 0
			failedAt = nil
			// Unknown: hold streaks and failedAt unchanged.
		}
		if failures > failureT {
			failures = failureT
		}
		if successes > recoveryT {
			successes = recoveryT
		}

		// §9.1.6.5 step 2 — propagate a SUSTAINED result into the robot's own hardware status.
		//
		// TRANSITION-ONLY (RA-1). The streaks above are clamped at their thresholds, so a sustained
		// failure sits at failures == failureT on EVERY subsequent tick; acting on that state would
		// put a Robot status write on every probe cycle. Only the CROSSING acts, which is also what
		// makes a sub-threshold flap (fail, fail, recover) write nothing at all.
		if crossed, target := probeHardwareEdge(rp, prev, failures, successes, failureT, recoveryT); crossed != hwEdgeNone {
			hwStatus, reason := fleetv1.HardwareHealthy, ""
			if crossed == hwEdgeDegraded {
				hwStatus, reason = fleetv1.HardwareDegraded, msg
				// Sealed on the same crossing the demotion rides: this is the moment a
				// sustained failure starts costing the robot capabilities, and it fires
				// once per crossing rather than once per probe tick (RA-1). A
				// sub-threshold flap demotes nothing and records nothing.
				r.recordProbeFailure(ctx, rp, robot, failures, expected, probeMetrics)
			}
			if err := r.propagateHardwareStatus(ctx, robot, target, hwStatus, reason, prev.LastProbeTime); err != nil {
				// Non-fatal: the probe's own status still records the streak, and the next crossing
				// retries. Failing the whole cycle would stop probing the rest of the fleet over one
				// robot's write conflict.
				logger.Error(err, "propagating probe result to robot hardware status",
					"robot", robot.Name, "component", target)
			}
		}

		now := metav1.Now()
		results = append(results, fleetv1.RobotProbeRobotResult{
			RobotName:            robot.Name,
			Namespace:            robot.Namespace,
			ProbeStatus:          fleetv1.ProbeResult(status),
			ConsecutiveFailures:  failures,
			ConsecutiveSuccesses: successes,
			FailedAt:             failedAt,
			LastProbeTime:        &now,
			Message:              msg,
			ActualMetrics:        probeMetrics,
		})
	}
	sort.Slice(results, func(i, j int) bool { return results[i].RobotName < results[j].RobotName })

	newStatus := aggregateProbeStatus(rp.Status, results, failureT, recoveryT)
	now := metav1.Now()
	newStatus.LastProbeTime = &now
	if !equality.Semantic.DeepEqual(stripProbeTime(rp.Status), stripProbeTime(newStatus)) {
		base := rp.DeepCopy()
		rp.Status = newStatus
		if err := r.Status().Patch(ctx, rp, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, fmt.Errorf("patching probe status: %w", err)
		}
		logger.V(1).Info("probe cycle", "result", newStatus.LastProbeResult, "consecutiveFailures", newStatus.ConsecutiveFailures)
	}

	intervalS := rp.Spec.IntervalSeconds
	if intervalS <= 0 {
		intervalS = defIntervalS
	}
	return ctrl.Result{RequeueAfter: time.Duration(intervalS) * time.Second}, nil
}

// probesDisabled reports whether the namespace kill-switch
// SwarmadaConfig.spec.health.disableAllProbes is set. It FAILS OPEN (probes run)
// when no config is readable — an unreadable policy never suspends health
// verification, and a probe that cannot confirm health still reports Unknown/Failed,
// never Healthy.
func (r *RobotProbeReconciler) probesDisabled(ctx context.Context, namespace string) bool {
	if cfg, ok := namespaceConfig(ctx, r.Client, namespace); ok {
		return cfg.Spec.Health.DisableAllProbes != nil && *cfg.Spec.Health.DisableAllProbes
	}
	return false
}

// reconcilePaused reports every matched robot as Unknown/paused without issuing any
// Verify RPC (disableAllProbes, ADR-0024). Debounce streaks and failedAt are frozen
// (carried forward) so probing resumes cleanly on re-enable, and lastProbeResult is
// set to Unknown directly, bypassing the debounce aggregate — a pause is neither a
// failing nor a recovering cycle. The write is transition-driven: the paused status
// is written once and then compares equal across disabled cycles (RA-1).
func (r *RobotProbeReconciler) reconcilePaused(ctx context.Context, rp *fleetv1.RobotProbe, robots []fleetv1.Robot) (ctrl.Result, error) {
	prevByRobot := make(map[string]fleetv1.RobotProbeRobotResult, len(rp.Status.RobotResults))
	for _, pr := range rp.Status.RobotResults {
		prevByRobot[pr.RobotName] = pr
	}
	now := metav1.Now()
	results := make([]fleetv1.RobotProbeRobotResult, 0, len(robots))
	for i := range robots {
		prev := prevByRobot[robots[i].Name]
		results = append(results, fleetv1.RobotProbeRobotResult{
			RobotName:            robots[i].Name,
			Namespace:            robots[i].Namespace,
			ProbeStatus:          fleetv1.ProbeResult(probe.StatusUnknown),
			ConsecutiveFailures:  prev.ConsecutiveFailures,
			ConsecutiveSuccesses: prev.ConsecutiveSuccesses,
			FailedAt:             prev.FailedAt,
			LastProbeTime:        &now,
			Message:              "probing disabled (SwarmadaConfig.spec.health.disableAllProbes)",
		})
	}
	sort.Slice(results, func(i, j int) bool { return results[i].RobotName < results[j].RobotName })

	newStatus := fleetv1.RobotProbeStatus{
		LastProbeResult:      fleetv1.ProbeResult(probe.StatusUnknown),
		ConsecutiveFailures:  rp.Status.ConsecutiveFailures,
		ConsecutiveSuccesses: rp.Status.ConsecutiveSuccesses,
		RobotResults:         results,
		LastProbeTime:        &now,
	}
	if !equality.Semantic.DeepEqual(stripProbeTime(rp.Status), stripProbeTime(newStatus)) {
		base := rp.DeepCopy()
		rp.Status = newStatus
		if err := r.Status().Patch(ctx, rp, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, fmt.Errorf("patching paused probe status: %w", err)
		}
		log.FromContext(ctx).V(1).Info("probes disabled for namespace; reporting Unknown/paused")
	}

	intervalS := rp.Spec.IntervalSeconds
	if intervalS <= 0 {
		intervalS, _ = r.probeDefaults(ctx, rp.Namespace)
	}
	return ctrl.Result{RequeueAfter: time.Duration(intervalS) * time.Second}, nil
}

// probeDefaults resolves the namespace's default probe interval and timeout
// (seconds) from SwarmadaConfig.spec.health, failing safe to the built-in
// constants when no config is readable or a value is non-positive.
func (r *RobotProbeReconciler) probeDefaults(ctx context.Context, namespace string) (interval, timeout int32) {
	interval, timeout = defaultProbeIntervalSeconds, defaultProbeTimeoutSeconds
	if cfg, ok := namespaceConfig(ctx, r.Client, namespace); ok {
		if v := cfg.Spec.Health.DefaultHardwareProbeIntervalSeconds; v > 0 {
			interval = v
		}
		if v := cfg.Spec.Health.DefaultProbeTimeoutSeconds; v > 0 {
			timeout = v
		}
	}
	return interval, timeout
}

// probeThresholds resolves the debounce failure/recovery thresholds for a probe
// (ADR-0012), in precedence order: the per-probe spec value if set and positive →
// the namespace SwarmadaConfig default if positive → the built-in constants (3/2).
func (r *RobotProbeReconciler) probeThresholds(ctx context.Context, rp *fleetv1.RobotProbe) (failure, recovery int32) {
	failure, recovery = defaultProbeFailureThreshold, defaultProbeRecoveryThreshold
	if cfg, ok := namespaceConfig(ctx, r.Client, rp.Namespace); ok {
		if v := cfg.Spec.Health.DefaultProbeFailureThreshold; v > 0 {
			failure = v
		}
		if v := cfg.Spec.Health.DefaultProbeRecoveryThreshold; v > 0 {
			recovery = v
		}
	}
	if rp.Spec.FailureThreshold != nil && *rp.Spec.FailureThreshold > 0 {
		failure = *rp.Spec.FailureThreshold
	}
	if rp.Spec.RecoveryThreshold != nil && *rp.Spec.RecoveryThreshold > 0 {
		recovery = *rp.Spec.RecoveryThreshold
	}
	return failure, recovery
}

// recordProbeFailure seals PROBE_FAILURE (§9.6.5.1) when a probe crosses its failure
// threshold for a robot.
//
// failed_metrics names WHICH thresholds were missed, not merely that some were. A probe
// can carry several expectations, and "the probe failed" leaves an investigator re-running
// it to learn what a recorded entry could have told them — by which time the robot's state
// has usually moved on. A metric the adapter did not report at all is named as missing
// rather than omitted: an absent reading and a low one are different faults.
//
// Best-effort and nil-safe: the demotion this rides has already been decided, and an audit
// sink must never be able to stop a failing component being taken out of scheduling.
func (r *RobotProbeReconciler) recordProbeFailure(ctx context.Context, rp *fleetv1.RobotProbe,
	robot *fleetv1.Robot, failures int32, expected, actual map[string]float64) {
	if r.Audit == nil {
		return
	}
	if _, err := r.Audit.Record(audit.Entry{
		EventType: audit.EventProbeFailure,
		Namespace: rp.Namespace,
		Actor:     audit.Actor{Type: audit.ActorServiceAccount, Identity: "robotprobe-controller"},
		Resource:  audit.Resource{Kind: "Robot", Namespace: robot.Namespace, Name: robot.Name},
		Action:    "probe",
		Outcome:   audit.OutcomeError,
		Detail: map[string]string{
			"probe_name":           rp.Name,
			"consecutive_failures": strconv.Itoa(int(failures)),
			"failed_metrics":       strings.Join(failedMetricNames(expected, actual), ","),
		},
	}); err != nil {
		log.FromContext(ctx).Error(err, "recording PROBE_FAILURE", "probe", rp.Name, "robot", robot.Name)
	}
}

// failedMetricNames lists the expected metrics the actual reading did not satisfy, using
// the same rule the probe itself applies (probe.MetricsMet): a metric fails when it is
// absent or below its threshold. Sorted so a diff of two entries is meaningful.
func failedMetricNames(expected, actual map[string]float64) []string {
	var out []string
	for name, threshold := range expected {
		got, ok := actual[name]
		if !ok {
			out = append(out, name+"=missing")
			continue
		}
		if got < threshold {
			out = append(out, fmt.Sprintf("%s=%g<%g", name, got, threshold))
		}
	}
	sort.Strings(out)
	return out
}

// probeRobot verifies one robot and returns its bound status (fail-safe: never
// Healthy unless the adapter confirmed HEALTHY and the thresholds are met).
func (r *RobotProbeReconciler) probeRobot(ctx context.Context, rp *fleetv1.RobotProbe, robotID string, expected map[string]float64, timeoutSeconds int32) (probe.Status, string, map[string]float64) {
	if r.Prober == nil {
		return probe.StatusUnknown, "no prober configured (adapter command-push not available)", nil
	}

	if timeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
		defer cancel()
	}

	// SyntheticInput is a model-probe input only; never attach it to a
	// hardware/capability request.
	var syntheticInput []byte
	if rp.Spec.ProbeType == fleetv1.ProbeTypeModel {
		syntheticInput = rp.Spec.SyntheticInput
	}
	res, err := r.Prober.Verify(ctx, rp.Namespace, robotID, probe.VerifyRequest{
		ProbeType:      rp.Spec.ProbeType,
		Target:         probeTarget(rp),
		Expected:       expected,
		SyntheticInput: syntheticInput,
	})
	if err != nil {
		// Unreachable / timeout — the probe could not confirm health (§9.1.6.3
		// FAILED). Never reported Healthy.
		return probe.StatusFailed, "probe RPC failed: " + err.Error(), nil
	}
	if res.Unsupported {
		return probe.StatusUnknown, "adapter does not implement this probe", nil
	}
	// A HEALTHY result still requires the expected thresholds to be met; otherwise
	// downgrade to Degraded (a metric below threshold is not healthy).
	if res.Status == probe.StatusHealthy && !probe.MetricsMet(expected, res.ActualMetrics) {
		return probe.StatusDegraded, "metrics below threshold: " + res.Message, res.ActualMetrics
	}
	return res.Status, res.Message, res.ActualMetrics
}

// probeTarget resolves the verify target for a probe by its type (ADR-0024):
// capability → spec.targetCapability, model → spec.targetModel, hardware (and any
// other) → spec.targetComponent. Reuses the one command-push path; only the target
// name (and, for model, syntheticInput) differs by type.
func probeTarget(rp *fleetv1.RobotProbe) string {
	switch rp.Spec.ProbeType {
	case fleetv1.ProbeTypeCapability:
		return rp.Spec.TargetCapability
	case fleetv1.ProbeTypeModel:
		return rp.Spec.TargetModel
	default:
		return rp.Spec.TargetComponent
	}
}

// hwEdge names the threshold crossing a probe cycle produced for one robot.
type hwEdge int

const (
	hwEdgeNone hwEdge = iota
	hwEdgeDegraded
	hwEdgeHealthy
)

// probeHardwareEdge reports whether this cycle CROSSED a threshold for a robot, and the component
// to act on. Pure, so the transition rule is testable without a cluster.
//
// Only hardware probes propagate. A capability or model probe has no entry in
// Robot.status.hardware[] to address, and degrading hardware from a capability result would invert
// the §6.10 derivation — hardware health is the INPUT to capability derivation, so writing the input
// from the output would let a derived Degraded capability degrade its own hardware, re-derive, and
// never recover.
func probeHardwareEdge(
	rp *fleetv1.RobotProbe,
	prev fleetv1.RobotProbeRobotResult,
	failures, successes, failureT, recoveryT int32,
) (hwEdge, string) {
	if rp.Spec.ProbeType != fleetv1.ProbeTypeHardware {
		return hwEdgeNone, ""
	}
	target := rp.Spec.TargetComponent
	if target == "" {
		return hwEdgeNone, ""
	}
	switch {
	case prev.ConsecutiveFailures < failureT && failures >= failureT:
		return hwEdgeDegraded, target
	case prev.ConsecutiveSuccesses < recoveryT && successes >= recoveryT:
		return hwEdgeHealthy, target
	}
	return hwEdgeNone, ""
}

// propagateHardwareStatus writes one component's health onto Robot.status.hardware[] (§9.1.6.5
// step 2), leaving every other component untouched.
//
// Change-gated: a component already in the target state writes nothing, so a crossing that merely
// confirms what telemetry already reported costs no API write.
//
// A component the robot does not inventory is a no-op rather than an append — the probe's target is
// operator-supplied and may not match the robot's actual hardware, and inventing an inventory entry
// from a probe would make Robot.status.hardware disagree with the adapter's own manifest.
func (r *RobotProbeReconciler) propagateHardwareStatus(
	ctx context.Context,
	robot *fleetv1.Robot,
	component string,
	status fleetv1.HardwareStatus,
	reason string,
	lastHealthy *metav1.Time,
) error {
	idx := -1
	for i := range robot.Status.Hardware {
		if robot.Status.Hardware[i].Name == component {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}
	cur := robot.Status.Hardware[idx]
	if cur.Status == status && cur.DegradationReason == reason {
		return nil // already there — no write
	}

	base := robot.DeepCopy()
	robot.Status.Hardware[idx].Status = status
	robot.Status.Hardware[idx].DegradationReason = reason
	if status == fleetv1.HardwareHealthy {
		now := metav1.Now()
		robot.Status.Hardware[idx].LastHealthyAt = &now
	} else if lastHealthy != nil {
		// §9.1.6.5: lastHealthyAt is the timestamp of the last SUCCESSFUL probe, carried from the
		// previous result so an operator can see how long the component has been bad.
		robot.Status.Hardware[idx].LastHealthyAt = lastHealthy
	}
	return r.Status().Patch(ctx, robot, client.MergeFrom(base))
}

// ── Pure helpers ──────────────────────────────────────────────────────────────

func toFloatMetrics(m map[string]string) map[string]float64 {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]float64, len(m))
	for k, v := range m {
		if f, err := parseFloat(v); err == nil {
			out[k] = f
		}
	}
	return out
}

func parseFloat(s string) (float64, error) {
	var f float64
	_, err := fmt.Sscanf(s, "%g", &f)
	return f, err
}

// aggregateProbeStatus rolls per-robot results into the debounced cycle status
// (ADR-0012). The raw cycle result is the worst per-robot status; the reported
// status.lastProbeResult flips to a failing state only after failureThreshold
// consecutive failing cycles, and back to Healthy only after recoveryThreshold
// consecutive Healthy cycles. Unknown cycles hold both streaks and the effective
// status. The two streak counters are clamped at their thresholds so a steady probe
// does not churn status every cycle (RA-1). Raw per-robot outcomes stay in
// RobotResults, undebounced.
func aggregateProbeStatus(prev fleetv1.RobotProbeStatus, results []fleetv1.RobotProbeRobotResult, failureThreshold, recoveryThreshold int32) fleetv1.RobotProbeStatus {
	worst := probe.StatusHealthy
	if len(results) == 0 {
		worst = probe.StatusUnknown
	}
	for _, res := range results {
		if probeSeverity(probe.Status(res.ProbeStatus)) > probeSeverity(worst) {
			worst = probe.Status(res.ProbeStatus)
		}
	}

	failures, successes := prev.ConsecutiveFailures, prev.ConsecutiveSuccesses
	switch worst {
	case probe.StatusFailed, probe.StatusDegraded:
		failures++
		successes = 0
	case probe.StatusHealthy:
		successes++
		failures = 0
		// Unknown: neither confirms health nor a failure — hold both streaks.
	}
	if failures > failureThreshold {
		failures = failureThreshold
	}
	if successes > recoveryThreshold {
		successes = recoveryThreshold
	}

	// Debounce transitions only. The first observation (no prior effective result)
	// is adopted immediately — there is nothing to debounce against, and a fresh
	// probe must report what it sees (a failing robot is Failed at once, never masked
	// as Healthy). Once an effective status is established, a Healthy→failing flip
	// waits failureThreshold consecutive failing cycles and a failing→Healthy flip
	// waits recoveryThreshold consecutive Healthy cycles; an Unknown cycle holds.
	effective := probe.Status(prev.LastProbeResult)
	switch {
	case effective == "":
		effective = worst
	case worst == probe.StatusUnknown:
		// hold effective unchanged
	case isFailingProbe(worst) && failures >= failureThreshold:
		effective = worst
	case worst == probe.StatusHealthy && successes >= recoveryThreshold:
		effective = probe.StatusHealthy
	}

	return fleetv1.RobotProbeStatus{
		LastProbeResult:      fleetv1.ProbeResult(effective),
		ConsecutiveFailures:  failures,
		ConsecutiveSuccesses: successes,
		RobotResults:         results,
	}
}

// isFailingProbe reports whether a status counts as a failing cycle for debounce.
func isFailingProbe(s probe.Status) bool {
	return s == probe.StatusFailed || s == probe.StatusDegraded
}

// probeSeverity orders statuses so the worst dominates the cycle: Failed >
// Degraded > Unknown > Healthy.
func probeSeverity(s probe.Status) int {
	switch s {
	case probe.StatusFailed:
		return 3
	case probe.StatusDegraded:
		return 2
	case probe.StatusUnknown:
		return 1
	default:
		return 0
	}
}

// stripProbeTime zeroes the volatile fields (timestamps and raw metric values)
// so the change-detection compare stays transition-driven — status is written on a
// material change, never on every probe tick (RA-1). The stored timestamps/metrics
// are refreshed opportunistically whenever a material change does trigger a write.
func stripProbeTime(s fleetv1.RobotProbeStatus) fleetv1.RobotProbeStatus {
	s.LastProbeTime = nil
	if len(s.RobotResults) > 0 {
		rr := make([]fleetv1.RobotProbeRobotResult, len(s.RobotResults))
		copy(rr, s.RobotResults)
		for i := range rr {
			rr[i].LastProbeTime = nil
			rr[i].ActualMetrics = nil
		}
		s.RobotResults = rr
	}
	return s
}

// SetupWithManager registers the RobotProbe controller. It watches SwarmadaConfig
// so a flip of spec.health.disableAllProbes (either direction) re-enqueues every
// RobotProbe in that namespace and takes effect on the next reconcile, rather than
// up to intervalSeconds later (ADR-0024).
func (r *RobotProbeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	configToProbes := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		var list fleetv1.RobotProbeList
		if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
			return nil
		}
		reqs := make([]reconcile.Request, 0, len(list.Items))
		for i := range list.Items {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
				Name: list.Items[i].Name, Namespace: list.Items[i].Namespace,
			}})
		}
		return reqs
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1.RobotProbe{}).
		Watches(&fleetv1.SwarmadaConfig{}, configToProbes).
		Complete(r)
}
