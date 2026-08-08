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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/contract"
	"github.com/swarmada/swarmada/internal/controlstream"
	"github.com/swarmada/swarmada/internal/metrics"
	"github.com/swarmada/swarmada/internal/signing"
)

// conformanceReportKey is the conventional ConfigMap data key holding the
// machine-readable conformance report JSON.
const conformanceReportKey = "report.json"

const (
	// defaultAdapterHeartbeatInterval matches spec.heartbeatIntervalSeconds default.
	defaultAdapterHeartbeatInterval = 10
	// adapterMissedProbeThreshold is how many consecutive intervals of silence move
	// an adapter to Disconnected (§9.1.12). Missing at least one but fewer than this
	// many intervals is the Degraded intermediate state.
	adapterMissedProbeThreshold = 3

	// conformanceSignatureKey is the ConfigMap data key holding the detached,
	// "bundle:<base64>"-encoded signature over the conformance report (§9.1.12).
	conformanceSignatureKey = "signature"
)

// FleetAdapterReconciler drives FleetAdapter.status.phase from adapter
// connectivity (RFC-0001 §9.1.12). It is BOTH a controlstream.AdapterPresence
// (event-driven Connected/Heartbeat/Disconnected from the ControlStream) AND a
// reconciler (the staleness backstop for a half-open stream). The RobotAdmissionGate
// consumes status.phase == Connected (ANDed with Conformance == Passed).
//
// RA-1: phase and lastHeartbeat are written from connect / liveness Heartbeat /
// stream-loss events — never from a telemetry frame — and only on a material
// change.
type FleetAdapterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	now    func() time.Time
}

var _ controlstream.AdapterPresence = (*FleetAdapterReconciler)(nil)

// +kubebuilder:rbac:groups=swarmada.io,resources=fleetadapters,verbs=get;list;watch
// +kubebuilder:rbac:groups=swarmada.io,resources=fleetadapters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=swarmada.io,resources=swarmadaconfigs,verbs=get;list;watch
// Liveness projection onto served Robots (ADR-0026):
// +kubebuilder:rbac:groups=swarmada.io,resources=robots,verbs=get;list;watch
// +kubebuilder:rbac:groups=swarmada.io,resources=robots/status,verbs=get;update;patch

func (r *FleetAdapterReconciler) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// Reconcile is the liveness-staleness backstop: a Connected adapter whose last
// heartbeat is older than heartbeatIntervalSeconds × missedProbeThreshold is moved
// to Disconnected even if the stream never cleanly closed.
func (r *FleetAdapterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	fa := &fleetv1.FleetAdapter{}
	if err := r.Get(ctx, req.NamespacedName, fa); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	base := fa.DeepCopy()

	// Conformance verification (§9.1.12): the digest-verified report drives
	// status.conformance. The RobotAdmissionGate ANDs this with phase==Connected.
	conformance, contractVersion, conformanceMsg := r.verifyConformance(ctx, fa)
	fa.Status.Conformance = conformance
	// ADR-0032 version-bound conformance: record the contract version the result was earned
	// against, taken ONLY from the digest-verified report body. An unverifiable report yields "",
	// so a stale version never outlives the result it belonged to. This rides the existing
	// change-gated status patch below, so it is transition-only (RA-1): a reconcile that computes
	// the same value writes nothing.
	fa.Status.ConformanceContractVersion = contractVersion

	// §9.1.12 status.connectedRobots. Counted from the spec.adapter.name index — the same
	// definition of "served by this adapter" projectLiveness uses — NOT from
	// spec.servesRobotClasses: authority follows the named binding (§9.5), so a robot bound to a
	// different adapter is not driven through this one even when their served classes overlap.
	//
	// Deliberately computed HERE rather than in mutateStatus: that path runs on every heartbeat,
	// and a liveness tick must not trigger a fleet-wide List or a status write (RA-1). This rides
	// the change-gated patch below, so a reconcile that computes the same count writes nothing.
	if n, err := r.countConnectedRobots(ctx, fa); err != nil {
		// Fail soft: keep the previous count rather than publishing a wrong one. A transient List
		// error must not make an operator think the fleet vanished.
		log.FromContext(ctx).Error(err, "counting connected robots", "adapter", fa.Name)
	} else {
		fa.Status.ConnectedRobots = n
	}

	// Liveness-staleness backstop with a Degraded intermediate (§9.1.12): as
	// heartbeats lapse an adapter moves Connected → Degraded (missed at least one
	// but fewer than the threshold of intervals) → Disconnected (threshold reached).
	// A fresh heartbeat within one interval recovers a Degraded adapter to Connected
	// (the event-driven ControlStream path also does this on each heartbeat).
	interval := adapterInterval(fa.Spec.HeartbeatIntervalSeconds)
	disconnectAfter := interval * adapterMissedProbeThreshold
	live := fa.Status.Phase == fleetv1.FleetAdapterPhaseConnected ||
		fa.Status.Phase == fleetv1.FleetAdapterPhaseDegraded
	switch {
	case live && fa.Status.LastHeartbeat != nil && r.clock().Sub(fa.Status.LastHeartbeat.Time) >= disconnectAfter:
		fa.Status.Phase = fleetv1.FleetAdapterPhaseDisconnected
		fa.Status.Message = fmt.Sprintf("no adapter heartbeat for over %s (liveness lost)", disconnectAfter)
	case live && fa.Status.LastHeartbeat != nil && r.clock().Sub(fa.Status.LastHeartbeat.Time) >= interval:
		elapsed := r.clock().Sub(fa.Status.LastHeartbeat.Time).Round(time.Second)
		fa.Status.Phase = fleetv1.FleetAdapterPhaseDegraded
		fa.Status.Message = fmt.Sprintf("missed adapter heartbeat; last seen %s ago (degraded)", elapsed)
	case fa.Status.Phase == fleetv1.FleetAdapterPhaseDegraded:
		// A heartbeat arrived within the last interval → recovered.
		fa.Status.Phase = fleetv1.FleetAdapterPhaseConnected
		fa.Status.Message = "adapter heartbeat resumed"
	case conformanceMsg != "" && fa.Status.Phase != fleetv1.FleetAdapterPhaseRejected:
		// A Rejected adapter keeps the reason it was rejected for: overwriting it with the
		// conformance note would leave an operator reading "conformance verified" on an adapter that
		// cannot be given work.
		fa.Status.Message = conformanceMsg
	}

	if !reflect.DeepEqual(base.Status, fa.Status) {
		if err := r.Status().Patch(ctx, fa, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, fmt.Errorf("patching FleetAdapter status: %w", err)
		}
	}

	// Re-check liveness after one interval while Connected or Degraded (a Degraded
	// adapter must keep being re-evaluated toward Disconnected or recovery).
	if fa.Status.Phase == fleetv1.FleetAdapterPhaseConnected ||
		fa.Status.Phase == fleetv1.FleetAdapterPhaseDegraded {
		return ctrl.Result{RequeueAfter: interval}, nil
	}
	return ctrl.Result{}, nil
}

// verifyConformance resolves status.conformance from spec.conformanceReport
// (§9.1.12). It fails closed to a non-Passed state: an absent report is Unknown;
// a missing/unreadable ConfigMap or a digest that does not match the pinned
// spec.conformanceReport.digest is Failed — the report is only honoured Passed
// when the digest verifies AND the report attests conformant.
func (r *FleetAdapterReconciler) verifyConformance(ctx context.Context, fa *fleetv1.FleetAdapter) (fleetv1.ConformanceState, string, string) {
	rep := fa.Spec.ConformanceReport
	if rep == nil {
		return fleetv1.ConformanceStateUnknown, "", "no conformanceReport referenced"
	}
	if rep.ConfigMapRef == "" || rep.Digest == "" {
		return fleetv1.ConformanceStateFailed, "", "conformanceReport missing configMapRef or digest"
	}

	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Name: rep.ConfigMapRef, Namespace: fa.Namespace}, cm); err != nil {
		if errors.IsNotFound(err) {
			return fleetv1.ConformanceStateFailed, "", "conformance report ConfigMap " + rep.ConfigMapRef + " not found"
		}
		return fleetv1.ConformanceStateFailed, "", "fetching conformance report: " + err.Error()
	}

	content, ok := conformanceReportContent(cm)
	if !ok {
		return fleetv1.ConformanceStateFailed, "", "conformance report ConfigMap has no usable report data"
	}

	// §9.1.12 MUST: verify the digest before honouring any Passed state.
	sum := sha256.Sum256([]byte(content))
	got := "sha256:" + hex.EncodeToString(sum[:])
	if got != rep.Digest {
		return fleetv1.ConformanceStateFailed, "", "conformance report digest mismatch (report altered or wrong digest pinned)"
	}

	var parsed struct {
		Conformant bool `json:"conformant"`
		// ContractVersion is the semver the harness stamped (adapters/conformance/report.py).
		// Absent in a report from a pre-versioning harness — recorded as unknown, not a failure:
		// flipping such an adapter to Failed would silently invalidate every attestation made
		// before this field existed. Enforcement of version COMPATIBILITY is the handshake/
		// assignment gate's job (ADR-0032), not this recorder's.
		ContractVersion string `json:"contract_version"`
	}
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		return fleetv1.ConformanceStateFailed, "", "conformance report is not valid JSON"
	}
	if !parsed.Conformant {
		return fleetv1.ConformanceStateFailed, "", "adapter is non-conformant (failed a MUST/MUST NOT check)"
	}

	// §9.1.12 authenticity: the digest proves the report was not altered, but not that
	// it came from the conformance authority. When the namespace enforces signature
	// verification, the report MUST additionally carry a signature that verifies
	// against a configured trust root. Fail closed on any problem.
	signingCfg, err := r.namespaceSigning(ctx, fa.Namespace)
	if err != nil {
		return fleetv1.ConformanceStateFailed, "", "resolving signing policy (fail closed): " + err.Error()
	}
	if signingCfg.RequireSignatureVerification {
		sigStr, ok := cm.Data[conformanceSignatureKey]
		if !ok {
			return fleetv1.ConformanceStateFailed, "", "signature verification required but the report ConfigMap has no signature"
		}
		sig, ok := signing.ParseInlineSignature(sigStr)
		if !ok {
			return fleetv1.ConformanceStateFailed, "", "conformance report signature is malformed (want bundle:<base64>)"
		}
		roots, err := r.loadTrustRoots(ctx, fa.Namespace, signingCfg.TrustRoots)
		if err != nil {
			return fleetv1.ConformanceStateFailed, "", "loading conformance trust roots (fail closed): " + err.Error()
		}
		signer, err := signing.Verify([]byte(content), sig, roots)
		if err != nil {
			return fleetv1.ConformanceStateFailed, "", "conformance report signature failed verification: " + err.Error()
		}
		return fleetv1.ConformanceStatePassed, parsed.ContractVersion,
			"conformance verified (" + rep.SuiteVersion + contractVersionNote(parsed.ContractVersion) + "), signed by " + signer
	}
	return fleetv1.ConformanceStatePassed, parsed.ContractVersion,
		"conformance verified (" + rep.SuiteVersion + contractVersionNote(parsed.ContractVersion) + ")"
}

// reportedOrNone renders the contract version an adapter reported for status.Message, naming the
// absent case rather than printing an empty string — "no contract version" and "an unsupported
// contract version" are different operator problems with different fixes.
func reportedOrNone(reported string) string {
	if reported == "" {
		return "<none reported>"
	}
	return reported
}

// contractVersionNote renders the contract version the result was earned against for
// status.Message, and says so plainly when the report carries none — an operator reading
// "conformance verified" should be able to see whether it is version-bound (ADR-0032) or is a
// pre-versioning attestation. Absence is reported, never inferred and never a failure.
func contractVersionNote(contractVersion string) string {
	if contractVersion == "" {
		return "; no contract_version in report"
	}
	return "; contract " + contractVersion
}

// namespaceSigning returns the namespace SwarmadaConfig's signing policy. A genuine
// list error fails closed (returned); an ABSENT SwarmadaConfig is not an error —
// there is no enforcement policy, so signature verification is simply not required
// (digest-only), preserving prior behaviour.
func (r *FleetAdapterReconciler) namespaceSigning(ctx context.Context, namespace string) (fleetv1.SwarmadaSigningConfig, error) {
	var configs fleetv1.SwarmadaConfigList
	if err := r.List(ctx, &configs, client.InNamespace(namespace)); err != nil {
		return fleetv1.SwarmadaSigningConfig{}, fmt.Errorf("listing SwarmadaConfig: %w", err)
	}
	if len(configs.Items) == 0 {
		return fleetv1.SwarmadaSigningConfig{}, nil
	}
	return configs.Items[0].Spec.Signing, nil
}

// loadTrustRoots resolves the configured trust-root Secrets into parsed public keys.
// It fails closed: a missing Secret/key, an unparseable root, or an empty set is an
// error (verification cannot proceed without a trust anchor).
func (r *FleetAdapterReconciler) loadTrustRoots(ctx context.Context, namespace string, refs []fleetv1.SigningTrustRoot) ([]signing.TrustRoot, error) {
	var roots []signing.TrustRoot
	for _, ref := range refs {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Name: ref.SecretRef.Name, Namespace: namespace}, secret); err != nil {
			return nil, fmt.Errorf("loading trust root %q secret: %w", ref.Name, err)
		}
		pemBytes, ok := secret.Data[ref.SecretRef.Key]
		if !ok {
			return nil, fmt.Errorf("trust root %q: key %q absent from secret %q", ref.Name, ref.SecretRef.Key, ref.SecretRef.Name)
		}
		root, err := signing.ParseTrustRoot(ref.Name, pemBytes)
		if err != nil {
			return nil, err
		}
		roots = append(roots, root)
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("signature verification required but no trust roots are configured")
	}
	return roots, nil
}

// conformanceReportContent returns the report JSON from the ConfigMap: the
// conventional report.json key, or the sole data entry if unambiguous.
func conformanceReportContent(cm *corev1.ConfigMap) (string, bool) {
	if v, ok := cm.Data[conformanceReportKey]; ok {
		return v, true
	}
	if len(cm.Data) == 1 {
		for _, v := range cm.Data {
			return v, true
		}
	}
	return "", false
}

// ── controlstream.AdapterPresence ─────────────────────────────────────────────

// AdapterConnected sets phase Connected on an accepted, verified handshake — or Rejected when the
// handshake agreed no compatible contract version (ADR-0032).
func (r *FleetAdapterReconciler) AdapterConnected(ctx context.Context, id controlstream.TLSIdentity, negotiated controlstream.Negotiation) {
	reconnected := false
	r.mutateStatus(ctx, id, func(s *fleetv1.FleetAdapterStatus) {
		// A handshake arriving while Disconnected/Degraded is a ControlStream
		// RE-establishment (§9.3.8); a first connect (Pending/empty) is not.
		if s.Phase == fleetv1.FleetAdapterPhaseDisconnected || s.Phase == fleetv1.FleetAdapterPhaseDegraded {
			reconnected = true
		}
		s.Phase = fleetv1.FleetAdapterPhaseConnected
		now := metav1.NewTime(r.clock())
		s.LastHeartbeat = &now
		if negotiated.ProtocolVersion != "" {
			s.NegotiatedProtocolVersion = negotiated.ProtocolVersion
		}
		s.Message = "adapter session established"

		// ADR-0032 assignment gate, first condition: an adapter speaking a contract version this
		// build cannot drive is REJECTED, which the RobotAdmissionGate's existing phase==Connected
		// requirement already turns into "its robots are not admissible". Nothing else changes — the
		// session stands, telemetry and heartbeats flow, and estop is always delivered.
		//
		// The recorded version is only set when it was AGREED, so an empty
		// status.negotiatedContractVersion always means "no compatible contract", never "compatible
		// but unrecorded".
		if negotiated.ContractCompatible {
			s.NegotiatedContractVersion = negotiated.ContractVersion
		} else {
			s.NegotiatedContractVersion = ""
			s.Phase = fleetv1.FleetAdapterPhaseRejected
			s.Message = "contract version " + reportedOrNone(negotiated.ContractVersion) +
				" is not in the supported range " + contract.SupportedRange() +
				"; robots bound to this adapter are not admissible (telemetry and estop unaffected)"
		}
	})
	if reconnected {
		metrics.IncAdapterReconnect(id.Namespace, id.AdapterName)
	}
	r.projectLiveness(ctx, id)
}

// AdapterHeartbeat records a liveness probe, re-asserting Connected if it drifted.
func (r *FleetAdapterReconciler) AdapterHeartbeat(ctx context.Context, id controlstream.TLSIdentity) {
	r.mutateStatus(ctx, id, func(s *fleetv1.FleetAdapterStatus) {
		now := metav1.NewTime(r.clock())
		s.LastHeartbeat = &now
		// Rejected is a NEGOTIATION verdict, not a liveness one: a heartbeat proves the adapter is
		// alive, never that it became version-compatible. A rejected adapter deliberately keeps
		// heartbeating (the handshake gate refuses registration, not the connection), so without
		// this exclusion the phase would oscillate Rejected → Connected on its very next beat and
		// silently re-admit robots. Only a fresh, compatible handshake clears it.
		if s.Phase != fleetv1.FleetAdapterPhaseConnected && s.Phase != fleetv1.FleetAdapterPhaseRejected {
			s.Phase = fleetv1.FleetAdapterPhaseConnected
			s.Message = "adapter live"
		}
	})
	r.projectLiveness(ctx, id)
}

// AdapterDisconnected moves a live adapter to Disconnected on stream loss.
func (r *FleetAdapterReconciler) AdapterDisconnected(ctx context.Context, id controlstream.TLSIdentity) {
	r.mutateStatus(ctx, id, func(s *fleetv1.FleetAdapterStatus) {
		if s.Phase == fleetv1.FleetAdapterPhaseConnected || s.Phase == fleetv1.FleetAdapterPhaseDegraded {
			s.Phase = fleetv1.FleetAdapterPhaseDisconnected
			s.Message = "adapter stream closed"
		}
	})
}

// mutateStatus applies a status mutation and patches ONLY on a material change
// (RA-1: no spurious status write). A connect event for a FleetAdapter that does
// not exist is a no-op (nothing to write).
func (r *FleetAdapterReconciler) mutateStatus(ctx context.Context, id controlstream.TLSIdentity, mutate func(*fleetv1.FleetAdapterStatus)) {
	logger := log.FromContext(ctx)
	fa := &fleetv1.FleetAdapter{}
	if err := r.Get(ctx, types.NamespacedName{Name: id.AdapterName, Namespace: id.Namespace}, fa); err != nil {
		if !errors.IsNotFound(err) {
			logger.Error(err, "fetching FleetAdapter for presence update", "adapter", id.AdapterName)
		}
		return
	}
	base := fa.DeepCopy()
	mutate(&fa.Status)
	if reflect.DeepEqual(base.Status, fa.Status) {
		return
	}
	if err := r.Status().Patch(ctx, fa, client.MergeFrom(base)); err != nil {
		logger.Error(err, "patching FleetAdapter presence status", "adapter", id.AdapterName)
	}
}

// robotAdapterNameField indexes Robots by the FleetAdapter they bind
// (spec.adapter.name), so an adapter's presence can fan liveness out to its served
// Robots via the informer cache instead of a full-namespace List (ADR-0026).
const robotAdapterNameField = "spec.adapter.name"

// projectLiveness stamps status.connectivity.lastSeenAt on every Robot served by
// the adapter identified by id (spec.adapter.name == id.AdapterName), scoped
// strictly to id.Namespace — an adapter never writes a Robot in another namespace.
// It is driven by ControlStream presence (connect / Heartbeat), NEVER by a
// TelemetryPayload (RA-1). Writes are throttled to at most one per refresh interval
// (heartbeatTimeout/2) per Robot so a live Robot never falsely times out while the
// write volume stays bounded (≤2 per timeout per Robot).
// countConnectedRobots counts the Robots this adapter currently drives: bound to it through
// spec.adapter.name, and not Offline.
//
// Offline is excluded because of what the field claims. It is named connectedRobots and documented
// as "the number of Robots currently driven through this adapter" — a Disconnected adapter still
// bound to three unreachable robots is driving none of them, and reporting 3 in swarmctl's ROBOTS
// column would read as a healthy fleet.
func (r *FleetAdapterReconciler) countConnectedRobots(ctx context.Context, fa *fleetv1.FleetAdapter) (int32, error) {
	var robots fleetv1.RobotList
	if err := r.List(ctx, &robots,
		client.InNamespace(fa.Namespace),
		client.MatchingFields{robotAdapterNameField: fa.Name}); err != nil {
		return 0, err
	}
	var n int32
	for i := range robots.Items {
		if robots.Items[i].Status.Phase != fleetv1.RobotPhaseOffline {
			n++
		}
	}
	return n, nil
}

func (r *FleetAdapterReconciler) projectLiveness(ctx context.Context, id controlstream.TLSIdentity) {
	if id.AdapterName == "" || id.Namespace == "" {
		return
	}
	logger := log.FromContext(ctx)
	var robots fleetv1.RobotList
	if err := r.List(ctx, &robots,
		client.InNamespace(id.Namespace),
		client.MatchingFields{robotAdapterNameField: id.AdapterName}); err != nil {
		logger.Error(err, "listing robots for liveness projection", "adapter", id.AdapterName, "namespace", id.Namespace)
		return
	}
	if len(robots.Items) == 0 {
		return
	}
	// One config read for the namespace, reused for all served Robots.
	refresh := robotOfflineTimeout(ctx, r.Client, id.Namespace) / 2
	now := r.clock()
	for i := range robots.Items {
		robot := &robots.Items[i]
		if conn := robot.Status.Connectivity; conn != nil && conn.LastSeenAt != nil &&
			now.Sub(conn.LastSeenAt.Time) < refresh {
			continue // throttle: refreshed recently enough
		}
		if err := r.stampLastSeen(ctx, client.ObjectKeyFromObject(robot), now); err != nil {
			logger.Error(err, "stamping robot lastSeenAt", "robot", robot.Name, "namespace", robot.Namespace)
		}
	}
}

// stampLastSeen sets status.connectivity.lastSeenAt = now on one Robot. It re-Gets
// under retry.RetryOnConflict so a concurrent robot_controller reconcile ("object
// has been modified") is retried, not surfaced, and patches the status subresource
// only on a material change.
func (r *FleetAdapterReconciler) stampLastSeen(ctx context.Context, key client.ObjectKey, now time.Time) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		robot := &fleetv1.Robot{}
		if err := r.Get(ctx, key, robot); err != nil {
			return client.IgnoreNotFound(err)
		}
		base := robot.DeepCopy()
		if robot.Status.Connectivity == nil {
			robot.Status.Connectivity = &fleetv1.ConnectivityStatus{}
		}
		t := metav1.NewTime(now)
		robot.Status.Connectivity.LastSeenAt = &t
		if reflect.DeepEqual(base.Status, robot.Status) {
			return nil // sub-second dup (metav1.Time truncates to seconds) — no write.
		}
		return r.Status().Patch(ctx, robot, client.MergeFrom(base))
	})
}

// robotOfflineTimeout resolves the per-namespace heartbeat/offline threshold
// (spec.health.connectivityOfflineThresholdSeconds), failing safe to the default —
// the same value robot_controller uses to lapse a Robot to Offline.
func robotOfflineTimeout(ctx context.Context, c client.Client, namespace string) time.Duration {
	if cfg, ok := namespaceConfig(ctx, c, namespace); ok {
		if s := cfg.Spec.Health.ConnectivityOfflineThresholdSeconds; s > 0 {
			return time.Duration(s) * time.Second
		}
	}
	return defaultHeartbeatTimeout
}

func adapterInterval(seconds int32) time.Duration {
	if seconds <= 0 {
		seconds = defaultAdapterHeartbeatInterval
	}
	return time.Duration(seconds) * time.Second
}

// SetupWithManager registers the FleetAdapter status controller.
func (r *FleetAdapterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Index Robots by their bound adapter so presence can fan liveness out cheaply
	// (ADR-0026). Registered here because this controller owns the fan-out.
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &fleetv1.Robot{}, robotAdapterNameField,
		func(o client.Object) []string {
			name := o.(*fleetv1.Robot).Spec.Adapter.Name
			if name == "" {
				return nil
			}
			return []string{name}
		}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1.FleetAdapter{}).
		// A Robot arriving, leaving, or going Offline changes status.connectedRobots. Without this
		// watch the count would only converge on the adapter's own requeue (one heartbeat interval
		// while Connected/Degraded, and never once Disconnected — leaving a stale count on exactly
		// the adapter an operator is most likely inspecting).
		Watches(&fleetv1.Robot{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, o client.Object) []reconcile.Request {
				robot, ok := o.(*fleetv1.Robot)
				if !ok || robot.Spec.Adapter.Name == "" {
					return nil
				}
				return []reconcile.Request{{NamespacedName: types.NamespacedName{
					Name: robot.Spec.Adapter.Name, Namespace: robot.Namespace,
				}}}
			})).
		Complete(r)
}
