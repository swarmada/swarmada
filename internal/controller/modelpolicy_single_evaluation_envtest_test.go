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
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// ONE TRIGGER IS ONE EVALUATION.
//
// The reconciler records a decision with a status write and then consumes the trigger
// with a second, separate metadata write. The status write re-queues the policy. If
// the next pass reads the policy before the second write is visible to it (an
// informer cache that has delivered the status event but not the annotation event),
// or if the second write never lands, that pass still sees the trigger. Before this
// was fixed it evaluated the trigger again: two identical history records, and
// rejectionCount and consecutiveRejections both at 2 for one bad model. That halves
// the tolerance of the suspension rule (RFC-0001 #modelpolicy-repeated-failure-handling).
//
// A cache is not deterministic, so these tests replay its worst cases: the second
// pass reads, in turn, each copy of the policy that still carries the trigger.

// staleEvalPolicy is a Robot-targeted ModelPolicy carrying one trigger.
func staleEvalPolicy(ns string, trigger modelTriggerPayload) *fleetv1.ModelPolicy {
	pol := policyWithGate(fleetv1.AutoDeployQualityGatePass, strictGate(), trigger)
	pol.Namespace = ns
	pol.Spec.ConsecutiveRejectionLimit = 5
	pol.Spec.TargetKind = fleetv1.RolloutTargetRobot
	pol.Spec.Trigger = fleetv1.ModelPolicyTrigger{
		Type:    fleetv1.ModelPolicyTriggerWebhook,
		Webhook: &fleetv1.WebhookTriggerConfig{Enabled: true, AuthSecretRef: "modelpolicy-hmac"},
	}
	return pol
}

// stateRecordingClient records the object every successful ModelPolicy write
// returns, status writes included: each is a state an informer cache can still hold
// when the next pass reads.
type stateRecordingClient struct {
	client.Client
	mu     sync.Mutex
	states []*fleetv1.ModelPolicy
}

func (c *stateRecordingClient) keep(obj client.Object) {
	if p, ok := obj.(*fleetv1.ModelPolicy); ok {
		c.mu.Lock()
		c.states = append(c.states, p.DeepCopy())
		c.mu.Unlock()
	}
}

func (c *stateRecordingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if err := c.Client.Patch(ctx, obj, patch, opts...); err != nil {
		return err
	}
	c.keep(obj)
	return nil
}

func (c *stateRecordingClient) Status() client.SubResourceWriter {
	return &recordingStatusWriter{SubResourceWriter: c.Client.Status(), c: c}
}

type recordingStatusWriter struct {
	client.SubResourceWriter
	c *stateRecordingClient
}

func (w *recordingStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	if err := w.SubResourceWriter.Patch(ctx, obj, patch, opts...); err != nil {
		return err
	}
	w.c.keep(obj)
	return nil
}

// staleStates is every state a lagging cache could serve the next pass that still
// carries the trigger: the object as the trigger left it, then each write's result.
func (c *stateRecordingClient) staleStates(t *testing.T, initial *fleetv1.ModelPolicy) []*fleetv1.ModelPolicy {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*fleetv1.ModelPolicy
	for _, p := range append([]*fleetv1.ModelPolicy{initial}, c.states...) {
		if _, pending := p.Annotations[triggerAnnotation]; pending {
			out = append(out, p.DeepCopy())
		}
	}
	if len(out) < 2 {
		t.Fatalf("the first pass left %d trigger-bearing states to replay, want at least 2 "+
			"(the triggered object and the status write)", len(out))
	}
	return out
}

// laggingCacheClient serves every ModelPolicy read from a fixed snapshot and passes
// everything else, all writes included, to the API server.
type laggingCacheClient struct {
	client.Client
	cached *fleetv1.ModelPolicy
}

func (c *laggingCacheClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if p, ok := obj.(*fleetv1.ModelPolicy); ok && key == client.ObjectKeyFromObject(c.cached) {
		c.cached.DeepCopyInto(p)
		return nil
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// failTriggerClearClient refuses, once, the metadata write that removes the trigger
// annotation, as a timed-out request or a restart between the two writes would.
type failTriggerClearClient struct {
	client.Client
	failed bool
}

var errInjectedClear = errors.New("injected: the trigger-clearing write did not land")

func (c *failTriggerClearClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if p, ok := obj.(*fleetv1.ModelPolicy); ok && !c.failed {
		if _, pending := p.Annotations[triggerAnnotation]; !pending {
			c.failed = true
			return errInjectedClear
		}
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

// writeCountingClient counts every write it forwards, status writes included.
type writeCountingClient struct {
	client.Client
	writes int
}

func (c *writeCountingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	c.writes++
	return c.Client.Create(ctx, obj, opts...)
}

func (c *writeCountingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	c.writes++
	return c.Client.Update(ctx, obj, opts...)
}

func (c *writeCountingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.writes++
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *writeCountingClient) Status() client.SubResourceWriter {
	return &countingStatusWriter{SubResourceWriter: c.Client.Status(), c: c}
}

type countingStatusWriter struct {
	client.SubResourceWriter
	c *writeCountingClient
}

func (w *countingStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	w.c.writes++
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

func (w *countingStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	w.c.writes++
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

// apiEventRecorder writes each Event to the API server synchronously and remembers the
// resourceVersion of the policy it names, which is the version the emitting pass read.
type apiEventRecorder struct {
	t    *testing.T
	ns   string
	mu   sync.Mutex
	seen []string
}

func (r *apiEventRecorder) Event(obj runtime.Object, eventtype, reason, message string) {
	o, ok := obj.(client.Object)
	if !ok {
		return
	}
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{GenerateName: o.GetName() + ".", Namespace: r.ns},
		InvolvedObject: corev1.ObjectReference{
			Kind: "ModelPolicy", APIVersion: fleetv1.GroupVersion.String(), Namespace: o.GetNamespace(),
			Name: o.GetName(), UID: o.GetUID(), ResourceVersion: o.GetResourceVersion(),
		},
		Type: eventtype, Reason: reason, Message: message,
	}
	if err := envK8s.Create(context.Background(), ev); err != nil {
		r.t.Errorf("recording event %s: %v", reason, err)
		return
	}
	r.mu.Lock()
	r.seen = append(r.seen, reason)
	r.mu.Unlock()
	r.t.Logf("  event %-16s names policy rv %s; the Event object itself is rv %s", reason, o.GetResourceVersion(), ev.ResourceVersion)
}

func (r *apiEventRecorder) Eventf(obj runtime.Object, eventtype, reason, messageFmt string, args ...interface{}) {
	r.Event(obj, eventtype, reason, fmt.Sprintf(messageFmt, args...))
}

func (r *apiEventRecorder) AnnotatedEventf(obj runtime.Object, _ map[string]string, eventtype, reason, messageFmt string, args ...interface{}) {
	r.Eventf(obj, eventtype, reason, messageFmt, args...)
}

func (r *apiEventRecorder) count(reason string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, s := range r.seen {
		if s == reason {
			n++
		}
	}
	return n
}

// settlePolicy runs the passes the work queue would run after the replayed one: live
// reads, until a pass returns without error. A pass that fails on a conflict with a
// newer object is what a stale read must cost after the fix, so it is retried.
func settlePolicy(t *testing.T, r *ModelPolicyReconciler, req ctrl.Request) {
	t.Helper()
	for i := 0; i < 5; i++ {
		_, err := r.Reconcile(context.Background(), req)
		if err == nil {
			return
		}
		if !apierrors.IsConflict(err) {
			t.Fatalf("settling pass %d: %v", i+1, err)
		}
	}
	t.Fatal("the policy did not settle in 5 passes")
}

func livePolicy(t *testing.T, key types.NamespacedName) *fleetv1.ModelPolicy {
	t.Helper()
	p := &fleetv1.ModelPolicy{}
	if err := envK8s.Get(context.Background(), key, p); err != nil {
		t.Fatalf("reading the policy: %v", err)
	}
	return p
}

// replayedPass runs one pass against a snapshot and tolerates only the outcomes a
// correct reconciler may have on a stale read: nothing to do, or a conflict.
func replayedPass(t *testing.T, r *ModelPolicyReconciler, req ctrl.Request) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), req); err != nil && !apierrors.IsConflict(err) {
		t.Fatalf("second pass on the cached object: %v", err)
	}
}

// assertNoFurtherWrites is RA-1 for this path: once the trigger is consumed, further
// passes write nothing at all, status included.
func assertNoFurtherWrites(t *testing.T, key types.NamespacedName, recorder *apiEventRecorder) {
	t.Helper()
	counter := &writeCountingClient{Client: envK8s}
	r := &ModelPolicyReconciler{Client: counter, Scheme: envScheme, Recorder: recorder}
	before := livePolicy(t, key).ResourceVersion
	for i := 0; i < 3; i++ {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("idle pass %d: %v", i+1, err)
		}
	}
	if counter.writes != 0 {
		t.Errorf("RA-1: 3 passes over a consumed trigger made %d writes, want 0", counter.writes)
	}
	after := livePolicy(t, key)
	if after.ResourceVersion != before {
		t.Errorf("RA-1: resourceVersion moved %s -> %s across idle passes", before, after.ResourceVersion)
	}
	// The claim goes in the same write as the trigger: a consumed trigger leaves neither.
	for _, a := range []string{triggerAnnotation, triggerClaimAnnotation} {
		if v, ok := after.Annotations[a]; ok {
			t.Errorf("after the idle passes the policy still carries %s=%q", a, v)
		}
	}
}

func assertOneRejection(t *testing.T, p *fleetv1.ModelPolicy) {
	t.Helper()
	if len(p.Status.History) != 1 || p.Status.RejectionCount != 1 || p.Status.ConsecutiveRejections != 1 {
		raw, _ := json.MarshalIndent(p.Status, "", "  ")
		t.Fatalf("one failing trigger recorded len(history)=%d rejectionCount=%d consecutiveRejections=%d, "+
			"want 1/1/1. A doubled count halves the rejections the suspension rule tolerates.\nstatus:\n%s",
			len(p.Status.History), p.Status.RejectionCount, p.Status.ConsecutiveRejections, raw)
	}
	if _, pending := p.Annotations[triggerAnnotation]; pending {
		t.Error("the trigger annotation is still pending after the policy settled")
	}
}

func failingTrigger() modelTriggerPayload {
	return modelTriggerPayload{ModelVersion: "1.4.0-rc2", ModelURI: "oci://registry.local/nav:1.4-rc2",
		ModelChecksum: testChecksum, Metrics: failingMetrics()}
}

func passingTrigger() modelTriggerPayload {
	return modelTriggerPayload{ModelVersion: "1.4.0", ModelURI: "oci://registry.local/nav:1.4.0",
		ModelChecksum: testChecksum, Metrics: passingMetrics()}
}

// The second pass reads a cached copy with the trigger still on it: the informer has
// some of the first pass's write events and not yet the one that removed the trigger.
func TestEnvtest_ModelPolicyOneFailingTriggerOneRejection_StaleRead(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	pol := staleEvalPolicy(ns, failingTrigger())
	if err := envK8s.Create(ctx, pol); err != nil {
		t.Fatalf("creating the policy: %v", err)
	}
	key := client.ObjectKeyFromObject(pol)
	req := ctrl.Request{NamespacedName: key}
	t.Logf("trigger written at policy rv %s", pol.ResourceVersion)

	events := &apiEventRecorder{t: t, ns: ns}
	initial := pol.DeepCopy()
	first := &stateRecordingClient{Client: envK8s}
	if _, err := (&ModelPolicyReconciler{Client: first, Scheme: envScheme, Recorder: events}).Reconcile(ctx, req); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	t.Logf("first pass done; the policy is at rv %s", livePolicy(t, key).ResourceVersion)
	for _, cached := range first.staleStates(t, initial) {
		t.Logf("second pass reads a cached copy at rv %s (trigger still on it)", cached.ResourceVersion)
		stale := &laggingCacheClient{Client: envK8s, cached: cached}
		replayedPass(t, &ModelPolicyReconciler{Client: stale, Scheme: envScheme, Recorder: events}, req)
	}
	settlePolicy(t, &ModelPolicyReconciler{Client: envK8s, Scheme: envScheme, Recorder: events}, req)

	assertOneRejection(t, livePolicy(t, key))
	if n := events.count("QualityGateFail"); n != 1 {
		t.Errorf("QualityGateFail emitted %d times for one trigger, want 1", n)
	}
	assertNoFurtherWrites(t, key, events)
}

// The first pass records the decision, and then its trigger-clearing write fails, so
// the retry reads the live object with the trigger still on it.
func TestEnvtest_ModelPolicyOneFailingTriggerOneRejection_ClearFails(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	pol := staleEvalPolicy(ns, failingTrigger())
	if err := envK8s.Create(ctx, pol); err != nil {
		t.Fatalf("creating the policy: %v", err)
	}
	key := client.ObjectKeyFromObject(pol)
	req := ctrl.Request{NamespacedName: key}

	events := &apiEventRecorder{t: t, ns: ns}
	flaky := &failTriggerClearClient{Client: envK8s}
	_, err := (&ModelPolicyReconciler{Client: flaky, Scheme: envScheme, Recorder: events}).Reconcile(ctx, req)
	if !errors.Is(err, errInjectedClear) {
		t.Fatalf("first pass returned %v; the injected clear failure did not fire", err)
	}
	settlePolicy(t, &ModelPolicyReconciler{Client: envK8s, Scheme: envScheme, Recorder: events}, req)

	assertOneRejection(t, livePolicy(t, key))
	assertNoFurtherWrites(t, key, events)
}

// assertOneDeploy checks one passing trigger made one rollout and one Deploy record,
// after the rollout has finished and freed the policy's slot, which is when a trigger
// left behind would be evaluated a second time.
func assertOneDeploy(t *testing.T, ns string, key types.NamespacedName, events *apiEventRecorder) {
	t.Helper()
	ctx := context.Background()
	name := rolloutName(key.Name, passingTrigger().ModelVersion)
	ro := &fleetv1.ModelRollout{}
	if err := envK8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, ro); err != nil {
		t.Fatalf("no ModelRollout %s after a passing trigger: %v", name, err)
	}
	ro.Status.Phase = fleetv1.RolloutPhaseSucceeded
	if err := envK8s.Status().Update(ctx, ro); err != nil {
		t.Fatalf("finishing the rollout: %v", err)
	}
	settlePolicy(t, &ModelPolicyReconciler{Client: envK8s, Scheme: envScheme, Recorder: events}, ctrl.Request{NamespacedName: key})

	list := &fleetv1.ModelRolloutList{}
	if err := envK8s.List(ctx, list, client.InNamespace(ns)); err != nil {
		t.Fatalf("listing rollouts: %v", err)
	}
	p := livePolicy(t, key)
	if len(list.Items) != 1 || p.Status.DeploymentCount != 1 || len(p.Status.History) != 1 {
		raw, _ := json.MarshalIndent(p.Status, "", "  ")
		t.Fatalf("one passing trigger: %d rollouts, deploymentCount=%d, len(history)=%d; want 1/1/1\nstatus:\n%s",
			len(list.Items), p.Status.DeploymentCount, len(p.Status.History), raw)
	}
	if _, pending := p.Annotations[triggerAnnotation]; pending {
		t.Error("the trigger annotation is still pending after the policy settled")
	}
	if n := events.count("ModelRolloutCreated"); n != 1 {
		t.Errorf("ModelRolloutCreated emitted %d times for one trigger, want 1", n)
	}
	assertNoFurtherWrites(t, key, events)
}

func TestEnvtest_ModelPolicyOnePassingTriggerOneRollout_StaleRead(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	pol := staleEvalPolicy(ns, passingTrigger())
	if err := envK8s.Create(ctx, pol); err != nil {
		t.Fatalf("creating the policy: %v", err)
	}
	key := client.ObjectKeyFromObject(pol)
	req := ctrl.Request{NamespacedName: key}

	events := &apiEventRecorder{t: t, ns: ns}
	initial := pol.DeepCopy()
	first := &stateRecordingClient{Client: envK8s}
	if _, err := (&ModelPolicyReconciler{Client: first, Scheme: envScheme, Recorder: events}).Reconcile(ctx, req); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	for _, cached := range first.staleStates(t, initial) {
		stale := &laggingCacheClient{Client: envK8s, cached: cached}
		replayedPass(t, &ModelPolicyReconciler{Client: stale, Scheme: envScheme, Recorder: events}, req)
	}
	settlePolicy(t, &ModelPolicyReconciler{Client: envK8s, Scheme: envScheme, Recorder: events}, req)

	assertOneDeploy(t, ns, key, events)
}

func TestEnvtest_ModelPolicyOnePassingTriggerOneRollout_ClearFails(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	pol := staleEvalPolicy(ns, passingTrigger())
	if err := envK8s.Create(ctx, pol); err != nil {
		t.Fatalf("creating the policy: %v", err)
	}
	key := client.ObjectKeyFromObject(pol)
	req := ctrl.Request{NamespacedName: key}

	events := &apiEventRecorder{t: t, ns: ns}
	flaky := &failTriggerClearClient{Client: envK8s}
	_, err := (&ModelPolicyReconciler{Client: flaky, Scheme: envScheme, Recorder: events}).Reconcile(ctx, req)
	if !errors.Is(err, errInjectedClear) {
		t.Fatalf("first pass returned %v; the injected clear failure did not fire", err)
	}
	settlePolicy(t, &ModelPolicyReconciler{Client: envK8s, Scheme: envScheme, Recorder: events}, req)

	assertOneDeploy(t, ns, key, events)
}

// An operator reset still works while a claim is on the policy. The rejection that
// suspends the policy is recorded, its trigger-clearing write fails, and the operator
// resets before any retry: the policy is resumed exactly as a reset always resumes it,
// the recorded trigger is consumed and not counted again, and nothing reports it as
// dropped by the suspension.
func TestEnvtest_ModelPolicyResetWithClaimPresent(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	pol := staleEvalPolicy(ns, failingTrigger())
	pol.Spec.ConsecutiveRejectionLimit = 1
	if err := envK8s.Create(ctx, pol); err != nil {
		t.Fatalf("creating the policy: %v", err)
	}
	key := client.ObjectKeyFromObject(pol)
	req := ctrl.Request{NamespacedName: key}

	events := &apiEventRecorder{t: t, ns: ns}
	flaky := &failTriggerClearClient{Client: envK8s}
	if _, err := (&ModelPolicyReconciler{Client: flaky, Scheme: envScheme, Recorder: events}).Reconcile(ctx, req); !errors.Is(err, errInjectedClear) {
		t.Fatalf("first pass returned %v; the injected clear failure did not fire", err)
	}
	p := livePolicy(t, key)
	if _, ok := p.Annotations[triggerClaimAnnotation]; !ok {
		t.Fatal("setup: no claim on the policy after the failed clear; this test would not exercise a reset over a claim")
	}
	if !apimeta.IsStatusConditionTrue(p.Status.Conditions, conditionFailedRepeatedly) {
		t.Fatal("setup: one rejection at consecutiveRejectionLimit 1 did not suspend the policy")
	}

	p.Annotations[resetAnnotation] = "operator-reset-1"
	if err := envK8s.Update(ctx, p); err != nil {
		t.Fatalf("writing the reset request: %v", err)
	}
	settlePolicy(t, &ModelPolicyReconciler{Client: envK8s, Scheme: envScheme, Recorder: events}, req)
	settlePolicy(t, &ModelPolicyReconciler{Client: envK8s, Scheme: envScheme, Recorder: events}, req)

	p = livePolicy(t, key)
	if apimeta.IsStatusConditionTrue(p.Status.Conditions, conditionFailedRepeatedly) {
		t.Error("the reset did not clear FailedRepeatedly")
	}
	if p.Status.ConsecutiveRejections != 0 || p.Status.RejectionCount != 1 || len(p.Status.History) != 1 {
		t.Errorf("after the reset: consecutiveRejections=%d rejectionCount=%d len(history)=%d, want 0/1/1",
			p.Status.ConsecutiveRejections, p.Status.RejectionCount, len(p.Status.History))
	}
	if p.Annotations[resetProcessedAnnotation] != "operator-reset-1" {
		t.Errorf("reset not marked processed: %q", p.Annotations[resetProcessedAnnotation])
	}
	if n := events.count("PolicyReset"); n != 1 {
		t.Errorf("PolicyReset emitted %d times, want 1", n)
	}
	if n := events.count(reasonTriggerDroppedSuspended); n != 0 {
		t.Errorf("%s emitted %d times for a trigger that was evaluated", reasonTriggerDroppedSuspended, n)
	}
	assertNoFurtherWrites(t, key, events)
}
