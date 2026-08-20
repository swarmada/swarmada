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
	"crypto"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/artifact"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/registry"
	"github.com/swarmada/swarmada/internal/rekor"
	"github.com/swarmada/swarmada/internal/signing"
)

// Pending-firmware annotations drive the PullOnIdle delivery mechanism (§9.1.7.3):
// the Robot Agent polls these on each Idle transition and applies the update.
const (
	annPendingFirmwareVersion  = "swarmada.io/pending-firmware-version"
	annPendingFirmwareURI      = "swarmada.io/pending-firmware-uri"
	annPendingFirmwareChecksum = "swarmada.io/pending-firmware-checksum"

	// conditionSignatureVerified records the artifact verification outcome.
	conditionSignatureVerified = "SignatureVerified"
)

// FirmwareRolloutReconciler drives FirmwareRollout (§9.1.7). Before ANY robot is
// dispatched, it verifies the artifact's signature over its checksum against the
// SwarmadaConfig trust roots and fails closed on any problem — an unverified
// artifact is never annotated onto a robot, even temporarily (§9.2.8).
type FirmwareRolloutReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// Fetcher retrieves a detached signature from a non-inline signatureRef (an
	// https:// URL). Nil defaults to the production https fetcher; tests inject a
	// fake. What it returns is trusted only after signing.Verify succeeds.
	Fetcher artifact.Fetcher
	// OCIFetcher retrieves a detached signature stored as an OCI blob (an oci://
	// digest-addressed ref). Nil defaults to the production registry client.
	OCIFetcher OCIBlobFetcher
	// Rekor checks the transparency log when signing.rekorUrl is set. Nil defaults
	// to the production Rekor client.
	Rekor rekor.Checker
	// Audit records FIRMWARE_ROLLOUT_CREATED into the §9.5.4 chain the first time a
	// verified rollout enters its lifecycle. Nil disables audit recording.
	Audit audit.Recorder
}

// OCIBlobFetcher fetches OCI artifact material — a content-addressed blob and a
// manifest's layer list. [github.com/swarmada/swarmada/internal/registry.Client]
// satisfies it.
type OCIBlobFetcher interface {
	Blob(ctx context.Context, reg, repo, digest string, cred *registry.Credential) ([]byte, error)
	Layers(ctx context.Context, reg, repo, ref string, cred *registry.Credential) ([]registry.Layer, error)
}

// registryCredentialsSecret is the conventional per-namespace Secret the fetcher
// reads registry/artifact credentials from (keys: token, or username+password).
// Absent means anonymous fetch. There is no api field for this yet (§9.2.8).
const registryCredentialsSecret = "swarmada-registry-credentials"

// +kubebuilder:rbac:groups=swarmada.io,resources=firmwarerollouts,verbs=get;list;watch
// +kubebuilder:rbac:groups=swarmada.io,resources=firmwarerollouts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=swarmada.io,resources=robots,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=swarmada.io,resources=swarmadaconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile drives one FirmwareRollout.
func (r *FirmwareRolloutReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("firmwarerollout", req.NamespacedName)

	rollout := &fleetv1.FirmwareRollout{}
	if err := r.Get(ctx, req.NamespacedName, rollout); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// A rollout already Failed by verification stays failed — a bad signature is
	// not retried into a dispatch.
	if rollout.Status.Phase == fleetv1.RolloutPhaseFailed && conditionIsFalse(rollout.Status.Conditions, conditionSignatureVerified) {
		return ctrl.Result{}, nil
	}

	// ── Verification gate (fail closed, BEFORE any dispatch) ───────────────────
	signer, verifyErr := r.verifyArtifact(ctx, req.Namespace, rollout.Spec.FirmwareChecksum, rollout.Spec.FirmwareSignatureRef)
	if verifyErr != nil {
		return r.failVerification(ctx, rollout, verifyErr)
	}

	// ── Dispatch (only reached once the artifact is verified) ──────────────────
	var robots fleetv1.RobotList
	sel, err := metav1.LabelSelectorAsSelector(&rollout.Spec.TargetSelector)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("invalid targetSelector: %w", err)
	}
	if err := r.List(ctx, &robots, client.InNamespace(req.Namespace),
		client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing target robots: %w", err)
	}

	// An operator resume converts the robots that are currently failed into this rollout's
	// excluded set (ADR-0041), so the pause cannot re-latch off the same robots on the very
	// next reconcile. Applied BEFORE classification so this pass already sees them excluded.
	if resumed, err := r.applyResume(ctx, rollout); err != nil {
		return ctrl.Result{}, err
	} else if resumed {
		return ctrl.Result{Requeue: true}, nil
	}
	excludedSet := make(map[string]bool, len(rollout.Status.ExcludedRobots))
	for _, n := range rollout.Status.ExcludedRobots {
		excludedSet[n] = true
	}

	now := time.Now()
	windowOnly := rollout.Spec.SafetyConstraints.MaintenanceWindowOnly
	var done, updating, eligible, failed, excluded []*fleetv1.Robot
	for i := range robots.Items {
		rob := &robots.Items[i]
		if excludedSet[rob.Name] {
			// An operator resumed past this robot: out of every bucket, so it neither
			// re-pauses the rollout nor is re-dispatched to, and it counts as settled so
			// the rollout can still reach a terminal phase.
			excluded = append(excluded, rob)
			continue
		}
		entry := batchEntryFor(rollout.Status.CurrentBatch, rob.Name)
		switch {
		case rob.Status.FirmwareVersion == rollout.Spec.NewVersion:
			done = append(done, rob)
			// Completed only if it was in the batch we are still carrying: a robot that
			// already ran this version before the rollout started never installed anything.
			if entry != nil {
				r.recordFirmwareInstalled(ctx, rollout, rob, entry)
			}
			// The install is over, so the PullOnIdle dispatch markers must not outlive it.
			r.clearPendingFirmware(ctx, rob)
		// Checked BEFORE the pending-annotation case below. A failed robot keeps that
		// annotation — nothing clears it — so without this it would classify as forever
		// "updating", which is precisely how a failed install used to wedge a rollout.
		case installFailedForRollout(rob, entry):
			failed = append(failed, rob)
			r.recordFirmwareInstallFailed(ctx, rollout, rob)
		case rob.Annotations[annPendingFirmwareVersion] == rollout.Spec.NewVersion:
			updating = append(updating, rob)
		case eligibleForFirmwareUpdate(rob, rollout) && withinRolloutWindow(rob, windowOnly, now):
			eligible = append(eligible, rob)
		}
	}

	total := len(robots.Items)

	// ── pauseOnError (§9.1.8.5) ────────────────────────────────────────────────
	// A failed install halts the rollout: no FURTHER robot enters the batch while the
	// failure stands, though in-flight updaters continue. This is the guard that stops a
	// bad image reaching the whole fleet one batch at a time — firmware has no
	// rollbackPolicy: Auto, so it is the only guard on this path.
	//
	// Derived per reconcile rather than latched, matching ModelRollout: the rollout
	// un-pauses when nothing is failed any more, which happens when an operator resumes
	// (the failed robots move to status.excludedRobots) or repairs them out of band.
	paused := len(failed) > 0 && rollout.Spec.Strategy.RollingUpdateOrDefault().PauseOnError

	if !paused {
		slots := maxUnavailable(rollout.Spec.Strategy.RollingUpdateOrDefault().MaxUnavailable, total) - len(updating)
		sort.Slice(eligible, func(i, j int) bool { return eligible[i].Name < eligible[j].Name })
		for _, rob := range eligible {
			if slots <= 0 {
				break
			}
			if err := r.dispatch(ctx, rob, rollout); err != nil {
				return ctrl.Result{}, err
			}
			updating = append(updating, rob)
			slots--
		}
	}

	// Was this rollout already verified on a previous pass? The condition is re-asserted
	// every reconcile, so the entry has to hang off the edge, not the assertion.
	prevVerified := conditionIsTrue(rollout.Status.Conditions, conditionSignatureVerified)

	newStatus := computeFirmwareStatus(total, len(done), updating, failed, excluded, paused, rollout.Status.CurrentBatch)
	upsertCondition(&newStatus.Conditions, conditionSignatureVerified, metav1.ConditionTrue,
		"Verified", verifiedReason(rollout.Spec.FirmwareSignatureRef, signer))
	if !prevVerified {
		r.sealFirmwareEvent(ctx, rollout, audit.EventFirmwareSignatureVerified, "verify", audit.OutcomeAllowed,
			map[string]string{
				"firmware_uri":    rollout.Spec.FirmwareURI,
				"artifact_digest": rollout.Spec.FirmwareChecksum,
				"verified_signer": signer,
			})
	}
	// PausedAt anchors the halt for an operator and for the audit entry. Stamped on the EDGE
	// into Paused so it records when the rollout stopped, not when it was last reconciled
	// while stopped — and so it rides the material-change patch below.
	if newStatus.Phase == fleetv1.RolloutPhasePaused {
		if rollout.Status.PausedAt != nil {
			newStatus.PausedAt = rollout.Status.PausedAt
		} else {
			pausedAt := metav1.Now()
			newStatus.PausedAt = &pausedAt
		}
	}
	pauseEdge := newStatus.Phase == fleetv1.RolloutPhasePaused && rollout.Status.Phase != fleetv1.RolloutPhasePaused
	if !equality.Semantic.DeepEqual(rollout.Status, newStatus) {
		base := rollout.DeepCopy()
		rollout.Status = newStatus
		if err := r.Status().Patch(ctx, rollout, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, fmt.Errorf("patching rollout status: %w", err)
		}
		logger.V(1).Info("firmware rollout progress", "phase", newStatus.Phase, "updated", newStatus.RobotsUpdated)
		// §9.5.4 requires FIRMWARE_ROLLOUT_PAUSED. Until pauseOnError was implemented the
		// transition could not occur, so the required event had no reachable writer.
		// Sealed on the edge: a rollout sitting Paused seals one entry, not one per pass.
		if pauseEdge {
			failedNames := make([]string, 0, len(newStatus.FailedRobots))
			for _, f := range newStatus.FailedRobots {
				failedNames = append(failedNames, f.RobotName)
			}
			r.sealFirmwareEvent(ctx, rollout, audit.EventFirmwareRolloutPaused, "pause", audit.OutcomeError,
				map[string]string{
					"version":       rollout.Spec.NewVersion,
					"failed_robots": strings.Join(failedNames, ","),
				})
		}
		// The first time the phase leaves empty is the first (verified) processing of
		// a newly-created rollout — record it once. Tied to the successful patch so a
		// mid-reconcile error never records a phantom creation.
		if base.Status.Phase == "" && newStatus.Phase != "" {
			r.recordRolloutCreated(ctx, rollout)
		}
	}
	return ctrl.Result{}, nil
}

// applyResume consumes a pending swarmada.io/rollout-resume annotation (ADR-0041).
//
// It moves the robots this rollout records as failed into status.excludedRobots and clears
// status.pausedAt, so `paused` — derived per reconcile from len(failed), not latched — cannot
// immediately re-latch off the same robots, and the rollout can reach a terminal phase and
// become deletable.
//
// Returns resumed=true when it wrote status; the caller ends the reconcile and the write
// requeues. Idempotent, and a rollout that is NOT paused still records the request as
// processed so a stale annotation cannot silently resume a FUTURE pause.
func (r *FirmwareRolloutReconciler) applyResume(ctx context.Context, rollout *fleetv1.FirmwareRollout) (bool, error) {
	req, pending := pendingResume(rollout.Annotations)
	if !pending {
		return false, nil
	}

	resumed := false
	if rollout.Status.Phase == fleetv1.RolloutPhasePaused {
		newlyExcluded := make([]string, 0, len(rollout.Status.FailedRobots))
		for _, f := range rollout.Status.FailedRobots {
			newlyExcluded = append(newlyExcluded, f.RobotName)
		}

		base := rollout.DeepCopy()
		rollout.Status.ExcludedRobots = mergeExcluded(rollout.Status.ExcludedRobots, newlyExcluded)
		// The failure list is what re-derives `paused`; leaving it would re-pause on the very
		// next reconcile and make the resume look like it did nothing.
		rollout.Status.FailedRobots = nil
		rollout.Status.PausedAt = nil
		if err := r.Status().Patch(ctx, rollout, client.MergeFrom(base)); err != nil {
			return false, fmt.Errorf("resuming firmware rollout: %w", err)
		}
		// The operator who wrote the resume annotation, stamped at admission (ADR-0046).
		// The identity rides the envelope's Actor — never the Detail map, which must not
		// duplicate envelope-carried identity (scripts/specdiff.py _ENVELOPE_FIELDS).
		r.sealFirmwareEventAs(ctx, rollout,
			estopActor(rollout.Annotations, "firmwarerollout-controller"),
			audit.EventRolloutResumed, "resume", audit.OutcomeAllowed,
			map[string]string{
				"reason":          req,
				"excluded_robots": strings.Join(newlyExcluded, ","),
			})
		resumed = true
	}

	if err := markResumeProcessed(ctx, r.Client, rollout, req); err != nil {
		return false, err
	}
	return resumed, nil
}

// sealFirmwareEvent appends one §9.6.5.1 entry about a firmware rollout. Best-effort and
// nil-safe: verification and dispatch decisions are made before this is called, and an
// audit sink must never be able to stop a rollout being blocked — least of all the
// signature-failure path, where refusing to dispatch is the safety-relevant behaviour.
func (r *FirmwareRolloutReconciler) sealFirmwareEvent(ctx context.Context, rollout *fleetv1.FirmwareRollout,
	eventType, action string, outcome audit.Outcome, detail map[string]string) {
	r.sealFirmwareEventAs(ctx, rollout,
		audit.Actor{Type: audit.ActorServiceAccount, Identity: "firmwarerollout-controller"},
		eventType, action, outcome, detail)
}

// sealFirmwareEventAs is sealFirmwareEvent with an explicit actor. Only the operator-driven
// events use it: ROLLOUT_RESUMED is an intent a person expressed (ADR-0041's resume
// annotation), so it carries the person (ADR-0046). Everything else this controller seals —
// install outcomes, the pause edge — is genuinely the controller's own act and keeps the
// service-account actor, because claiming a user there would be a false attribution.
func (r *FirmwareRolloutReconciler) sealFirmwareEventAs(ctx context.Context, rollout *fleetv1.FirmwareRollout,
	actor audit.Actor, eventType, action string, outcome audit.Outcome, detail map[string]string) {
	if r.Audit == nil {
		return
	}
	if _, err := r.Audit.Record(audit.Entry{
		EventType: eventType,
		Namespace: rollout.Namespace,
		Actor:     actor,
		Resource:  audit.Resource{Kind: "FirmwareRollout", Namespace: rollout.Namespace, Name: rollout.Name},
		Action:    action,
		Outcome:   outcome,
		Detail:    detail,
	}); err != nil {
		log.FromContext(ctx).Error(err, "recording audit entry", "event", eventType, "rollout", rollout.Name)
	}
}

// recordFirmwareInstalled seals FIRMWARE_INSTALL_SUCCEEDED when a robot that was in this
// rollout's batch reports the target version.
//
// duration_seconds is measured from the batch entry's UpdateStartedAt — the moment the
// robot was dispatched — because that is the span an operator is asking about when a
// rollout looks slow. It is omitted rather than zeroed when the stamp is missing: a
// recorded 0 would assert an instantaneous install.
func (r *FirmwareRolloutReconciler) recordFirmwareInstalled(ctx context.Context, rollout *fleetv1.FirmwareRollout,
	rob *fleetv1.Robot, entry *fleetv1.RolloutBatchRobot) {
	detail := map[string]string{
		"new_version": rollout.Spec.NewVersion,
		"robot":       rob.Name,
	}
	if entry.UpdateStartedAt != nil {
		detail["duration_seconds"] = strconv.Itoa(int(time.Since(entry.UpdateStartedAt.Time).Seconds()))
	}
	r.sealFirmwareEvent(ctx, rollout, audit.EventFirmwareInstallSucceeded, "install", audit.OutcomeAllowed, detail)
}

// batchEntryFor returns the batch record for a robot, or nil when it is not in the batch.
func batchEntryFor(batch []fleetv1.RolloutBatchRobot, robotName string) *fleetv1.RolloutBatchRobot {
	for i := range batch {
		if batch[i].RobotName == robotName {
			return &batch[i]
		}
	}
	return nil
}

// conditionIsTrue reports whether a named condition is present and True.
func conditionIsTrue(conds []metav1.Condition, condType string) bool {
	for i := range conds {
		if conds[i].Type == condType {
			return conds[i].Status == metav1.ConditionTrue
		}
	}
	return false
}

// recordRolloutCreated seals a FIRMWARE_ROLLOUT_CREATED entry into the §9.5.4
// chain. Best-effort: a record failure is logged, never blocking the rollout.
// recordFirmwareInstallFailed seals FIRMWARE_INSTALL_FAILED (§9.6.5.1) on the edge into
// failure, once per robot per rollout.
//
// Sealed ONLY on a confirmed report from the robot. A rollout that abandons an unresponsive
// robot records that on the rollout as unconfirmed and reaches this not at all: never
// hearing is not evidence of failing, and the chain admits only confirmed facts.
func (r *FirmwareRolloutReconciler) recordFirmwareInstallFailed(ctx context.Context,
	rollout *fleetv1.FirmwareRollout, rob *fleetv1.Robot,
) {
	// The edge: a failed robot stays failed across reconciles, so without this the chain
	// would gain an entry per reconcile per robot and bury the incident it records.
	if wasAlreadyFailed(rollout.Status.FailedRobots, rob.Name) {
		return
	}
	fi := rob.Status.FirmwareInstall
	detail := map[string]string{
		"reason":       "no reason reported by the adapter",
		"new_version":  rollout.Spec.NewVersion,
		"rollout_name": rollout.Name,
	}
	if fi != nil {
		if fi.FailureReason != "" {
			detail["reason"] = fi.FailureReason
		}
		// The version the robot is left on. Reported, never assumed — a failed install may
		// leave it on the old version, a recovery image, or elsewhere, and this is the field
		// an operator acts on when deciding whether the robot is safe to keep running.
		detail["robot_remains_on_version"] = fi.RunningVersion
	}
	r.sealFirmwareEvent(ctx, rollout, audit.EventFirmwareInstallFailed, "install", audit.OutcomeError, detail)
}

func (r *FirmwareRolloutReconciler) recordRolloutCreated(ctx context.Context, rollout *fleetv1.FirmwareRollout) {
	if r.Audit == nil {
		return
	}
	if _, err := r.Audit.Record(audit.Entry{
		EventType: audit.EventFirmwareRolloutCreat,
		Namespace: rollout.Namespace,
		Actor:     audit.Actor{Type: audit.ActorServiceAccount, Identity: "firmwarerollout-controller"},
		Resource:  audit.Resource{Kind: "FirmwareRollout", Namespace: rollout.Namespace, Name: rollout.Name},
		Action:    "create",
		Outcome:   audit.OutcomeAllowed,
		Detail:    map[string]string{"version": rollout.Spec.NewVersion},
	}); err != nil {
		log.FromContext(ctx).Error(err, "recording FIRMWARE_ROLLOUT_CREATED audit entry", "rollout", rollout.Name)
	}
}

// verifyArtifact fails closed: it returns an error unless the artifact's checksum
// is well-formed and (when signing is enforced) its signature verifies against a
// configured trust root. A nil error means the rollout may dispatch.
func (r *FirmwareRolloutReconciler) verifyArtifact(ctx context.Context, namespace, checksum, sigRef string) (string, error) {
	if !signing.ValidChecksum(checksum) {
		return "", fmt.Errorf("malformed firmwareChecksum %q (want sha256:<64 hex>)", checksum)
	}

	cfg, err := r.namespaceSigningConfig(ctx, namespace)
	if err != nil {
		return "", err
	}
	if !cfg.RequireSignatureVerification {
		return "", nil // signing not enforced; checksum format already validated
	}

	if sigRef == "" {
		return "", fmt.Errorf("firmwareSignatureRef is required when signing is enforced; refusing to dispatch unsigned artifact")
	}
	sig, inline := signing.ParseInlineSignature(sigRef)
	var cosignPayload []byte
	if !inline {
		// A non-inline ref is fetched. https and oci:// are wired; anything else fails closed with
		// an honest message — an artifact we cannot fetch is never dispatched.
		fetched, ferr := r.fetchSignature(ctx, namespace, sigRef)
		if ferr != nil {
			return "", ferr
		}
		sig, cosignPayload = fetched.Sig, fetched.Payload
	}

	roots, err := r.loadTrustRoots(ctx, namespace, cfg.TrustRoots)
	if err != nil {
		return "", err
	}

	// Two signature forms, verified over different payloads.
	//
	//   native  — the signature is over the artifact checksum string directly.
	//   cosign  — the signature is over a simplesigning PAYLOAD, which in turn names the digest it
	//             attests. Verifying the signature alone would prove only that SOME payload is
	//             authentic; the binding check below is what ties it to THIS artifact. Without it a
	//             genuine cosign signature for a different image would satisfy the gate.
	verifyOver := []byte(checksum)
	if cosignPayload != nil {
		verifyOver = cosignPayload
	}
	signer, err := signing.Verify(verifyOver, sig, roots)
	if err != nil {
		return "", err
	}
	if cosignPayload != nil {
		attested, perr := cosignPayloadDigest(cosignPayload)
		if perr != nil {
			return "", fmt.Errorf("cosign signature verified but its payload is unusable: %w", perr)
		}
		if attested != checksum {
			return "", fmt.Errorf("cosign signature attests artifact %s but this rollout dispatches %s; refusing to dispatch", attested, checksum)
		}
	}

	// Transparency-log requirement (§9.1.12): when rekorUrl is configured, the
	// artifact's entry MUST appear in the log — an additional fail-closed gate on
	// top of the trust-root signature check.
	if cfg.RekorURL != "" {
		// §9.1.7.3. With a pinned log key this verifies the entry's inclusion proof AND
		// signed-entry-timestamp; without one it degrades to index presence and SAYS SO, because
		// an operator must be able to tell a cryptographically verified entry from a server that
		// merely answered "yes".
		logKey, kerr := r.rekorLogKey(ctx, namespace, cfg)
		if kerr != nil {
			return "", fmt.Errorf("transparency-log key unavailable (fail closed): %w", kerr)
		}
		note, rerr := r.rekor().VerifyEntry(ctx, cfg.RekorURL, checksum, logKey)
		if rerr != nil {
			return "", fmt.Errorf("transparency-log verification failed for %s at %s; refusing to dispatch: %w",
				checksum, cfg.RekorURL, rerr)
		}
		log.FromContext(ctx).V(1).Info("transparency log checked", "artifact", checksum, "result", note)
	}
	return signer, nil
}

// rekorLogKey loads the operator-pinned transparency-log public key. A nil key with no error means
// none is configured — the caller then runs the degraded presence-only check. A configured but
// unreadable or unparseable key is an ERROR: a key that was meant to be enforced must never
// silently downgrade the gate.
func (r *FirmwareRolloutReconciler) rekorLogKey(ctx context.Context, namespace string, cfg fleetv1.SwarmadaSigningConfig) (crypto.PublicKey, error) {
	ref := cfg.RekorPublicKey
	if ref == nil {
		return nil, nil
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: namespace}, secret); err != nil {
		return nil, fmt.Errorf("loading rekorPublicKey secret %q: %w", ref.Name, err)
	}
	pemBytes, ok := secret.Data[ref.Key]
	if !ok {
		return nil, fmt.Errorf("rekorPublicKey: key %q absent from secret %q", ref.Key, ref.Name)
	}
	root, err := signing.ParseTrustRoot("rekor-log", pemBytes)
	if err != nil {
		return nil, fmt.Errorf("parsing rekorPublicKey: %w", err)
	}
	return root.PublicKey, nil
}

func (r *FirmwareRolloutReconciler) rekor() rekor.Checker {
	if r.Rekor != nil {
		return r.Rekor
	}
	return rekor.New()
}

// fetchSignature resolves a non-inline signatureRef to its raw detached-signature
// bytes over https, applying the namespace's optional registry credentials. The
// returned bytes are untrusted transport output — the caller verifies them against
// a trust root before use. An oci:// (or other unsupported) ref fails closed with
// an honest "not yet wired" message.
func (r *FirmwareRolloutReconciler) fetchSignature(ctx context.Context, namespace, sigRef string) (fetchedSignature, error) {
	cred, err := r.registryCredential(ctx, namespace)
	if err != nil {
		return fetchedSignature{}, err
	}
	if strings.HasPrefix(sigRef, "oci://") {
		return r.fetchOCISignature(ctx, sigRef, cred)
	}
	sig, err := r.fetcher().Fetch(ctx, sigRef, cred)
	if err != nil {
		if errors.Is(err, artifact.ErrUnsupportedScheme) {
			return fetchedSignature{}, fmt.Errorf("signatureRef %q: unsupported scheme (only https:// and oci://<registry>/<repo>[@sha256:<digest>|:<tag>] are wired); refusing to dispatch unverified artifact", sigRef)
		}
		return fetchedSignature{}, fmt.Errorf("fetching signature %q: %w", sigRef, err)
	}
	return fetchedSignature{Sig: sig}, nil
}

// fetchedSignature is a detached signature and, for a cosign artifact, the simplesigning payload it
// was made over. Payload nil means the NATIVE form: the signature is over the artifact checksum
// directly. Payload non-nil means the caller must verify over the payload and then BIND it to the
// artifact — see cosign.go.
type fetchedSignature struct {
	Sig     []byte
	Payload []byte
}

// fetchOCISignature retrieves a detached signature stored as a digest-addressed
// OCI blob (oci://<registry>/<repo>@sha256:<digest>). It verifies the fetched
// bytes hash to the requested digest (content-address integrity — a registry
// serving wrong bytes fails here) BEFORE they reach signing.Verify. The bytes are
// still untrusted transport: only signing.Verify against a trust root makes them
// authoritative.
func (r *FirmwareRolloutReconciler) fetchOCISignature(ctx context.Context, sigRef string, cred *artifact.Credential) (fetchedSignature, error) {
	reg, repo, ref, isDigest, err := parseOCIRef(sigRef)
	if err != nil {
		return fetchedSignature{}, err
	}
	rc := toRegistryCred(cred)

	// Resolve the signature blob's digest. Digest-addressed refs carry it directly;
	// a tag-addressed ref resolves the tag's manifest and requires exactly one layer
	// (the signature blob) — the deterministic, unambiguous case.
	digest := ref
	var cosignSig []byte
	if !isDigest {
		layers, lerr := r.ociFetcher().Layers(ctx, reg, repo, ref, rc)
		if lerr != nil {
			return fetchedSignature{}, fmt.Errorf("resolving oci signature manifest %q: %w", sigRef, lerr)
		}
		// Cosign convention (the `sha256-<digest>.sig` tag): the signature is in a layer
		// ANNOTATION and the blob is the payload it was made over. Detected first, because such a
		// manifest legitimately carries layers the native single-layer rule would reject.
		cl, isCosign, cerr := selectCosignLayer(layers)
		if cerr != nil {
			return fetchedSignature{}, fmt.Errorf("oci signatureRef %q: %w", sigRef, cerr)
		}
		switch {
		case isCosign:
			digest, cosignSig = cl.Digest, cl.Signature
		case len(layers) == 1:
			digest = layers[0].Digest // native form: the blob IS the signature
		default:
			return fetchedSignature{}, fmt.Errorf("oci signatureRef %q: expected one signature layer or a cosign signature layer, got %d layers and no cosign annotation (use a digest-addressed ref)", sigRef, len(layers))
		}
	}

	blob, err := r.ociFetcher().Blob(ctx, reg, repo, digest, rc)
	if err != nil {
		return fetchedSignature{}, fmt.Errorf("fetching oci signature %q: %w", sigRef, err)
	}
	// Content-verify the fetched bytes against the (manifest-declared or ref-pinned) digest — a
	// registry serving wrong bytes fails closed HERE, before anything cryptographic runs. This
	// holds for the cosign payload too: the payload is what the signature covers, so serving a
	// different payload would break the binding check silently otherwise.
	sum := sha256.Sum256(blob)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != digest {
		return fetchedSignature{}, fmt.Errorf("oci signature %q content digest mismatch: got %s, want %s (refusing to dispatch)", sigRef, got, digest)
	}
	if cosignSig != nil {
		return fetchedSignature{Sig: cosignSig, Payload: blob}, nil
	}
	return fetchedSignature{Sig: blob}, nil
}

func (r *FirmwareRolloutReconciler) ociFetcher() OCIBlobFetcher {
	if r.OCIFetcher != nil {
		return r.OCIFetcher
	}
	return registry.New()
}

// parseOCIRef parses an OCI signature reference in either form:
//
//	oci://<registry>/<repo>@sha256:<digest>   (digest-addressed, isDigest=true)
//	oci://<registry>/<repo>:<tag>             (tag-addressed,    isDigest=false)
//
// ref is the digest or the tag accordingly. The registry may carry a port
// (reg:5000) — the tag separator is only sought AFTER the first '/', so the port
// colon is never mistaken for a tag.
func parseOCIRef(ociRef string) (reg, repo, ref string, isDigest bool, err error) {
	rest := strings.TrimPrefix(ociRef, "oci://")

	// Digest form: split on '@'.
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		digest := rest[at+1:]
		reg, repo, ok := splitRegistryRepo(rest[:at])
		if !ok || !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
			return "", "", "", false, fmt.Errorf("malformed digest-addressed oci signatureRef %q", ociRef)
		}
		return reg, repo, digest, true, nil
	}

	// Tag form: the tag is after the last ':' that follows the first '/'.
	reg, rof, ok := splitRegistryRepo(rest) // rof = "<repo>[:tag]"
	if !ok {
		return "", "", "", false, fmt.Errorf("oci signatureRef %q missing repository path", ociRef)
	}
	colon := strings.LastIndex(rof, ":")
	if colon < 0 {
		return "", "", "", false, fmt.Errorf("oci signatureRef %q needs a tag or @digest", ociRef)
	}
	repo, tag := rof[:colon], rof[colon+1:]
	if repo == "" || tag == "" {
		return "", "", "", false, fmt.Errorf("malformed tag-addressed oci signatureRef %q", ociRef)
	}
	return reg, repo, tag, false, nil
}

// splitRegistryRepo splits "<registry>/<repo...>" at the first '/'.
func splitRegistryRepo(s string) (reg, repo string, ok bool) {
	slash := strings.IndexByte(s, '/')
	if slash < 0 {
		return "", "", false
	}
	reg, repo = s[:slash], s[slash+1:]
	return reg, repo, reg != "" && repo != ""
}

// toRegistryCred adapts the shared artifact Credential to the registry client's.
func toRegistryCred(c *artifact.Credential) *registry.Credential {
	if c == nil {
		return nil
	}
	return &registry.Credential{BearerToken: c.BearerToken, Username: c.Username, Password: c.Password}
}

func (r *FirmwareRolloutReconciler) fetcher() artifact.Fetcher {
	if r.Fetcher != nil {
		return r.Fetcher
	}
	return artifact.DefaultFetcher()
}

// registryCredential reads the conventional per-namespace credentials Secret. A
// missing Secret means an anonymous fetch (nil); a present one supplies a bearer
// token (key "token") or basic auth (keys "username"/"password").
func (r *FirmwareRolloutReconciler) registryCredential(ctx context.Context, namespace string) (*artifact.Credential, error) {
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: registryCredentialsSecret}, secret)
	if apierrors.IsNotFound(err) {
		return nil, nil // anonymous
	}
	if err != nil {
		return nil, fmt.Errorf("reading registry credentials: %w", err)
	}
	cred := &artifact.Credential{
		BearerToken: string(secret.Data["token"]),
		Username:    string(secret.Data["username"]),
		Password:    string(secret.Data["password"]),
	}
	if cred.BearerToken == "" && cred.Username == "" && cred.Password == "" {
		return nil, nil
	}
	return cred, nil
}

// namespaceSigningConfig returns the signing config from the namespace's
// SwarmadaConfig. If none exists, it fails closed: signature verification is
// required but unconfigurable, so no artifact may be dispatched.
func (r *FirmwareRolloutReconciler) namespaceSigningConfig(ctx context.Context, namespace string) (fleetv1.SwarmadaSigningConfig, error) {
	var configs fleetv1.SwarmadaConfigList
	if err := r.List(ctx, &configs, client.InNamespace(namespace)); err != nil {
		return fleetv1.SwarmadaSigningConfig{}, fmt.Errorf("listing SwarmadaConfig: %w", err)
	}
	if len(configs.Items) == 0 {
		return fleetv1.SwarmadaSigningConfig{}, fmt.Errorf("no SwarmadaConfig in namespace %q; cannot determine signing policy (fail closed)", namespace)
	}
	return configs.Items[0].Spec.Signing, nil
}

func (r *FirmwareRolloutReconciler) loadTrustRoots(ctx context.Context, namespace string, refs []fleetv1.SigningTrustRoot) ([]signing.TrustRoot, error) {
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

// failVerification records a fail-closed verification result: the rollout goes
// Failed with SignatureVerified=False, a FIRMWARE_SIGNATURE_FAILED audit event is
// emitted, and NO robot is dispatched.
func (r *FirmwareRolloutReconciler) failVerification(ctx context.Context, rollout *fleetv1.FirmwareRollout, cause error) (ctrl.Result, error) {
	base := rollout.DeepCopy()
	rollout.Status.Phase = fleetv1.RolloutPhaseFailed
	upsertCondition(&rollout.Status.Conditions, conditionSignatureVerified, metav1.ConditionFalse,
		"VerificationFailed", cause.Error())
	if r.Recorder != nil {
		r.Recorder.Event(rollout, corev1.EventTypeWarning, "FIRMWARE_SIGNATURE_FAILED",
			fmt.Sprintf("artifact verification failed; rollout blocked: %v", cause))
	}
	// Also seal into the tamper-evident chain. The Kubernetes Event above stays — it is
	// what an operator sees in `kubectl describe` — but an Event is subject to namespace
	// retention and is NOT covered by the hash chain, so on its own it cannot evidence
	// that unverified firmware was refused. That gap is the reason this row existed.
	r.sealFirmwareEvent(ctx, rollout, audit.EventFirmwareSignatureFailed, "verify", audit.OutcomeDenied,
		map[string]string{
			"firmware_uri":    rollout.Spec.FirmwareURI,
			"artifact_digest": rollout.Spec.FirmwareChecksum,
			"reason":          cause.Error(),
		})
	if err := r.Status().Patch(ctx, rollout, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("recording verification failure: %w", err)
	}
	log.FromContext(ctx).Info("firmware rollout blocked by verification (fail closed)", "reason", cause.Error())
	return ctrl.Result{}, nil // terminal; do not requeue a bad signature into a dispatch
}

// clearPendingFirmware removes the PullOnIdle dispatch annotations from a robot that now
// reports the target version running.
//
// Nothing cleared them before. The consequence is not cosmetic: the annotation is also the
// "updating" classifier above, so a stale marker makes a finished robot indistinguishable
// from one still installing, and a later rollout to the same version reads a leftover
// dispatch as its own. Failures deliberately keep their annotation -- installFailedForRollout
// is what reads it -- so only the success edge clears.
//
// Best-effort: a failure here is logged, not returned. The robot is already classified done
// on this pass, and the next reconcile retries the clear.
func (r *FirmwareRolloutReconciler) clearPendingFirmware(ctx context.Context, rob *fleetv1.Robot) {
	if rob.Annotations[annPendingFirmwareVersion] == "" {
		return
	}
	base := rob.DeepCopy()
	delete(rob.Annotations, annPendingFirmwareVersion)
	delete(rob.Annotations, annPendingFirmwareURI)
	delete(rob.Annotations, annPendingFirmwareChecksum)
	if err := r.Patch(ctx, rob, client.MergeFrom(base)); err != nil {
		log.FromContext(ctx).Error(err, "clearing pending-firmware annotations", "robot", rob.Name)
	}
}

// dispatch annotates a robot with the pending-firmware update (PullOnIdle).
func (r *FirmwareRolloutReconciler) dispatch(ctx context.Context, rob *fleetv1.Robot, rollout *fleetv1.FirmwareRollout) error {
	base := rob.DeepCopy()
	if rob.Annotations == nil {
		rob.Annotations = map[string]string{}
	}
	rob.Annotations[annPendingFirmwareVersion] = rollout.Spec.NewVersion
	rob.Annotations[annPendingFirmwareURI] = rollout.Spec.FirmwareURI
	rob.Annotations[annPendingFirmwareChecksum] = rollout.Spec.FirmwareChecksum
	if err := r.Patch(ctx, rob, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("annotating pending firmware on %s: %w", rob.Name, err)
	}
	if r.Recorder != nil {
		r.Recorder.Event(rollout, corev1.EventTypeNormal, "FirmwareRolloutBatchStarted",
			fmt.Sprintf("robot %s scheduled for firmware %s", rob.Name, rollout.Spec.NewVersion))
	}
	// Sealed after the annotation patch lands: the install has begun only once the robot
	// carries the pending-firmware annotation, and the same annotation is what stops this
	// robot being dispatched again, so the entry cannot repeat.
	r.sealFirmwareEvent(ctx, rollout, audit.EventFirmwareInstallStarted, "install", audit.OutcomeAllowed,
		map[string]string{
			"old_version": rob.Status.FirmwareVersion,
			"new_version": rollout.Spec.NewVersion,
			"robot":       rob.Name,
		})
	return nil
}

// ── Pure helpers ──────────────────────────────────────────────────────────────

func eligibleForFirmwareUpdate(rob *fleetv1.Robot, rollout *fleetv1.FirmwareRollout) bool {
	sc := rollout.Spec.SafetyConstraints
	if sc.RequireIdleState && rob.Status.Phase != fleetv1.RobotPhaseIdle {
		return false
	}
	if rob.Status.BatteryPercent == nil || *rob.Status.BatteryPercent < sc.MinBatteryPct {
		return false
	}
	return true
}

func computeFirmwareStatus(total, done int, updating, failed, excluded []*fleetv1.Robot, paused bool,
	prior []fleetv1.RolloutBatchRobot) fleetv1.FirmwareRolloutStatus {
	st := fleetv1.FirmwareRolloutStatus{
		//nolint:gosec // small fleet counts
		RobotsTotal: int32(total),
		//nolint:gosec // small fleet counts
		RobotsUpdated: int32(done),
		// Failed robots are NOT pending: a rollout that keeps counting them as outstanding
		// never settles, and an operator reading "3 pending" cannot tell work still to do
		// from work that has already gone wrong.
		//nolint:gosec // small fleet counts
		RobotsPending: int32(total - done - len(failed) - len(excluded)),
		CurrentBatch:  buildRolloutBatch(updating, prior, firmwareInitialPhase, firmwarePrevVersion, false),
		FailedRobots:  firmwareFailedResults(failed, prior),
	}
	if len(excluded) > 0 {
		names := make([]string, 0, len(excluded))
		for _, r := range excluded {
			names = append(names, r.Name)
		}
		sort.Strings(names)
		st.ExcludedRobots = names
	}
	switch {
	// Excluded robots count as settled, so a resumed rollout can still reach a terminal
	// phase — which is what makes its record deletable (ADR-0041).
	case total > 0 && done+len(excluded) == total:
		st.Phase = fleetv1.RolloutPhaseSucceeded
	case paused:
		st.Phase = fleetv1.RolloutPhasePaused
	case len(updating) > 0 || done > 0 || len(failed) > 0:
		st.Phase = fleetv1.RolloutPhaseInProgress
	default:
		st.Phase = fleetv1.RolloutPhasePending
	}
	return st
}

// installFailedForRollout reports whether a robot's own firmware report is a CONFIRMED
// failure belonging to THIS rollout's dispatch.
//
// Both halves matter. The robot must be in the batch (so the failure concerns work this
// rollout started), and the report must be no older than the dispatch — otherwise a robot
// still carrying a Failed state from an EARLIER rollout would be counted as having failed
// this one the moment it was dispatched, before it had done anything at all.
func installFailedForRollout(rob *fleetv1.Robot, entry *fleetv1.RolloutBatchRobot) bool {
	fi := rob.Status.FirmwareInstall
	if fi == nil || fi.Status != fleetv1.FirmwareInstallFailed || entry == nil {
		return false
	}
	if fi.ReportedAt == nil || entry.UpdateStartedAt == nil {
		return false // cannot establish order; refuse to attribute rather than guess
	}
	return !fi.ReportedAt.Time.Before(entry.UpdateStartedAt.Time)
}

// firmwareFailedResults projects the failed robots onto the status list.
func firmwareFailedResults(failed []*fleetv1.Robot, prior []fleetv1.RolloutBatchRobot) []fleetv1.RolloutRobotResult {
	if len(failed) == 0 {
		return nil
	}
	out := make([]fleetv1.RolloutRobotResult, 0, len(failed))
	for _, rob := range failed {
		res := fleetv1.RolloutRobotResult{RobotName: rob.Name, Namespace: rob.Namespace}
		if fi := rob.Status.FirmwareInstall; fi != nil {
			res.Reason = fi.FailureReason
			res.FailedAt = fi.ReportedAt
		}
		if e := batchEntryFor(prior, rob.Name); e != nil {
			res.PreviousVersion = e.PreviousVersion
		}
		out = append(out, res)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RobotName < out[j].RobotName })
	return out
}

const (
	// firmwareInitialPhase is the updatePhase a robot enters the firmware batch at,
	// before the adapter reports finer progress via UpdateProgress (§6.6).
	firmwareInitialPhase = "Pulling"
	// modelInitialPhase is the equivalent entry phase for a model rollout (§6.7).
	modelInitialPhase = "Downloading"
)

// firmwarePrevVersion is the version a robot is on before a firmware update — its
// currently-running firmware.
func firmwarePrevVersion(r *fleetv1.Robot) string { return r.Status.FirmwareVersion }

// buildRolloutBatch builds the structured active-update batch. It PRESERVES each
// robot's reported updatePhase / updateStartedAt / previousVersion across reconciles
// (only new entrants are initialized to initialPhase and now), so an adapter's
// UpdateProgress is never clobbered when the controller recomputes rollout status —
// the two writers cooperate. Entries are sorted by robot name for a stable diff.
//
// stampSuspended marks the model-rollout path (ADR-0023): a model update suspends
// the robot's model-driven capabilities at batch entry (the model is marked
// Updating), so a NEW entrant records CapabilitiesSuspendedAt at that instant.
// Because a continuing entrant is preserved by name, the stamp is stable for the
// life of an attempt; a robot that left the batch and re-enters (e.g. Auto
// rollback → Updating) is a fresh entrant and gets a fresh stamp — the per-attempt
// semantics come for free. Firmware rollouts suspend nothing and pass false.
func buildRolloutBatch(updating []*fleetv1.Robot, prior []fleetv1.RolloutBatchRobot, initialPhase string, prevVersion func(*fleetv1.Robot) string, stampSuspended bool) []fleetv1.RolloutBatchRobot {
	priorByName := make(map[string]fleetv1.RolloutBatchRobot, len(prior))
	for _, b := range prior {
		priorByName[b.RobotName] = b
	}
	out := make([]fleetv1.RolloutBatchRobot, 0, len(updating))
	for _, r := range updating {
		if p, ok := priorByName[r.Name]; ok {
			out = append(out, p) // preserve reported phase / startedAt / previousVersion
			continue
		}
		now := metav1.Now()
		entry := fleetv1.RolloutBatchRobot{
			RobotName:       r.Name,
			Namespace:       r.Namespace,
			UpdateStartedAt: &now,
			PreviousVersion: prevVersion(r),
			UpdatePhase:     initialPhase,
		}
		if stampSuspended {
			suspendedAt := now
			entry.CapabilitiesSuspendedAt = &suspendedAt
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RobotName < out[j].RobotName })
	return out
}

// setCondition upserts a metav1.Condition in a slice.
func upsertCondition(conds *[]metav1.Condition, condType string, status metav1.ConditionStatus, reason, msg string) {
	now := metav1.Now()
	for i := range *conds {
		if (*conds)[i].Type == condType {
			if (*conds)[i].Status != status {
				(*conds)[i].LastTransitionTime = now
			}
			(*conds)[i].Status = status
			(*conds)[i].Reason = reason
			(*conds)[i].Message = msg
			return
		}
	}
	*conds = append(*conds, metav1.Condition{
		Type: condType, Status: status, Reason: reason, Message: msg, LastTransitionTime: now,
	})
}

func conditionIsFalse(conds []metav1.Condition, condType string) bool {
	for _, c := range conds {
		if c.Type == condType {
			return c.Status == metav1.ConditionFalse
		}
	}
	return false
}

func verifiedReason(sigRef, signer string) string {
	if signer != "" {
		return "signature verified by trust root " + signer
	}
	if sigRef == "" {
		return "signature verification not enforced"
	}
	return "verified"
}

// SetupWithManager registers the FirmwareRollout controller.
func (r *FirmwareRolloutReconciler) SetupWithManager(mgr ctrl.Manager) error {
	robotToRollouts := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		var rollouts fleetv1.FirmwareRolloutList
		if err := r.List(ctx, &rollouts, client.InNamespace(obj.GetNamespace())); err != nil {
			return nil
		}
		reqs := make([]reconcile.Request, 0, len(rollouts.Items))
		for i := range rollouts.Items {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
				Name: rollouts.Items[i].Name, Namespace: rollouts.Items[i].Namespace,
			}})
		}
		return reqs
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1.FirmwareRollout{}).
		Watches(&fleetv1.Robot{}, robotToRollouts).
		Complete(r)
}
