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
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/controller"
	"github.com/swarmada/swarmada/internal/controlstream"
	"github.com/swarmada/swarmada/internal/estopscope"
	"github.com/swarmada/swarmada/internal/safety"
	"github.com/swarmada/swarmada/internal/scheduler"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// The estop hold invariant (RFC-0001 #safety-estop-scopes, #safety-estop-state-machine).
//
// After any sequence of creates, moves, re-parents, clears, resumes, deletes, manager
// restarts and one refused write, a robot that a stop still holds — a stop the sequence
// issued to it, or put it under, and has not cleared — reads Stopped or Failed, or is
// withheld from dispatch, and no action it holds progresses. Which robots a stop still
// holds is written in each row, from the sequence, not asked of the code under test.
//
// The rows share one API server with the admission webhooks served, and run in parallel,
// each in its own namespace with its own manager, whose cache sees that namespace only. The
// manager, as the control plane's identity, runs the real estop dispatcher over a
// scripted SafetyStream, the robot-, zone- and namespace-estop controllers and, where a
// row says so, the Zone Controller. Every row that edits the zone tree runs both with
// and without the Zone Controller: a stop must not depend on another controller's write
// to wake it. The manager's client can refuse one chosen write, so the API server proves
// everything else.

// ── The rig ────────────────────────────────────────────────────────────────────────

type rigOpts struct {
	zoneController bool
	// deferStart leaves the manager stopped until the row calls start, so a row can lay
	// out state the manager first meets on start (an upgrade, a restart).
	deferStart bool
	// zoneEventsWithheld registers the zone stop controller on its own zones' events only: no
	// Robot watch and no ancestor watch, so a stop in force reaches a robot that arrives
	// under it only through its periodic resync.
	zoneEventsWithheld bool
}

type ackMode int

const (
	ackStopped  ackMode = iota // EstopAck STOPPED
	ackFailed                  // EstopAck FAILED
	ackStopping                // STOPPING, then STOPPED
	ackNone                    // never answers: the delivery window ends Failed
)

type rig struct {
	t    *testing.T
	w    *safetyWire
	opts rigOpts
	rf   *refuser
	res  *countingResumer
	aud  *memAudit

	mu    sync.Mutex
	modes map[string]ackMode
	sends map[string]int

	d       *safety.Dispatcher
	dereg   func()
	stopMgr func()
}

func newRig(t *testing.T, shared *safetyWire, opts rigOpts) *rig {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "hold-"}}
	if err := shared.admin.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating the row's namespace: %v", err)
	}
	w := *shared
	w.ns = ns.Name
	markerFixture(t, &w, w.ns) // zone bay-1, adapter acme, Connected and conformance-Passed
	r := &rig{t: t, w: &w, opts: opts, rf: &refuser{}, res: &countingResumer{}, aud: &memAudit{},
		modes: map[string]ackMode{}, sends: map[string]int{}}
	if !opts.deferStart {
		r.start()
	}
	return r
}

// start brings up a fresh manager: a new dispatcher (no in-memory estop ordering), new
// reconcilers, and a SafetyStream registered for the adapter.
func (r *rig) start() {
	t := r.t
	t.Helper()
	cfg := rest.CopyConfig(r.w.cfg)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: controllerUser, Groups: []string{"system:masters"}}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: r.w.scheme, Metrics: metricsserver.Options{BindAddress: "0"},
		Cache: cache.Options{DefaultNamespaces: map[string]cache.Config{r.w.ns: {}}}})
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	c := &refusingClient{Client: mgr.GetClient(), rf: r.rf}
	d := safety.New(c, mgr.GetEventRecorderFor("estop-dispatcher"))
	d.APIReader = mgr.GetAPIReader()
	id := controlstream.TLSIdentity{AdapterName: markerRobotAdapter, Namespace: r.w.ns, Verified: true}
	r.d = d
	r.dereg = d.RegisterStream(id, &scriptSender{r: r, d: d, id: id})
	r.res.c = r.w.as(t, controllerUser)

	ze := &controller.ZoneEstopReconciler{Client: c, Scheme: r.w.scheme, Estopper: d,
		Recorder: mgr.GetEventRecorderFor("zone-estop")}
	ne := &controller.NamespaceEstopReconciler{Client: c, Scheme: r.w.scheme, Estopper: d,
		Recorder: mgr.GetEventRecorderFor("namespace-estop")}
	re := &controller.RobotEstopReconciler{Client: c, Scheme: r.w.scheme, Estopper: d, Resumer: r.res,
		APIReader: mgr.GetAPIReader(), Recorder: mgr.GetEventRecorderFor("robotestop"), Audit: r.aud}
	wireEstopReaders(ze, ne, mgr.GetAPIReader())
	setups := map[string]func(ctrl.Manager) error{
		"robot": re.SetupWithManager, "zone": ze.SetupWithManager, "ns": ne.SetupWithManager,
	}
	if r.opts.zoneEventsWithheld {
		setups["zone"] = func(mgr ctrl.Manager) error {
			return ctrl.NewControllerManagedBy(mgr).For(&fleetv1.FleetZone{}).Named("zoneestop").Complete(ze)
		}
	}
	if r.opts.zoneController {
		setups["zonecontroller"] = (&controller.ZoneController{Client: mgr.GetClient(), Scheme: r.w.scheme,
			Recorder: record.NewFakeRecorder(1024)}).SetupWithManager
	}
	for name, setup := range setups {
		if err := setup(mgr); err != nil {
			t.Fatalf("registering %s: %v", name, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- mgr.Start(ctx) }()
	var once sync.Once
	r.stopMgr = func() {
		once.Do(func() {
			cancel()
			select {
			case <-errCh:
			case <-time.After(20 * time.Second):
				t.Error("the rig's manager did not stop")
			}
		})
	}
	t.Cleanup(r.stopMgr)
}

// restart stops the manager and starts a new one: every process-local record is gone.
func (r *rig) restart() {
	r.t.Helper()
	r.stopMgr()
	r.start()
}

func (r *rig) setMode(robot string, m ackMode) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.modes[robot] = m
}

func (r *rig) sendsTo(robot string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sends[robot]
}

// scriptSender answers each estop as the rig's mode for that robot says.
type scriptSender struct {
	r  *rig
	d  *safety.Dispatcher
	id controlstream.TLSIdentity
}

func (s *scriptSender) Send(m *fav1.ControlPlaneSafetyMessage) error {
	e := m.GetEstop()
	if e == nil {
		return nil
	}
	robot := m.GetRobotId()
	s.r.mu.Lock()
	s.r.sends[robot]++
	mode := s.r.modes[robot]
	s.r.mu.Unlock()
	ack := func(st fav1.EstopState) {
		_ = s.d.RouteAck(s.id, s, robot, &fav1.EstopAck{EstopId: e.GetEstopId(), State: st, ConfirmedAtMs: 1})
	}
	go func() {
		switch mode {
		case ackFailed:
			ack(fav1.EstopState_ESTOP_STATE_FAILED)
		case ackStopping:
			ack(fav1.EstopState_ESTOP_STATE_STOPPING)
			time.Sleep(300 * time.Millisecond)
			ack(fav1.EstopState_ESTOP_STATE_STOPPED)
		case ackNone:
		default:
			ack(fav1.EstopState_ESTOP_STATE_STOPPED)
		}
	}()
	return nil
}

// ── Refusing one write ─────────────────────────────────────────────────────────────

type refuseRule struct {
	name   string
	status bool // a robots/status patch; otherwise a patch of the object itself
	match  func(obj client.Object, data []byte) bool
	left   int // refusals left; -1 refuses every match
	hits   int
}

type refuser struct {
	mu    sync.Mutex
	rules []*refuseRule
}

func (f *refuser) add(rule *refuseRule) *refuseRule {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = append(f.rules, rule)
	return rule
}

func (f *refuser) hitsOf(rule *refuseRule) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return rule.hits
}

func (f *refuser) check(status bool, obj client.Object, p client.Patch) error {
	data, err := p.Data(obj)
	if err != nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rule := range f.rules {
		if rule.status != status || rule.left == 0 || !rule.match(obj, data) {
			continue
		}
		if rule.left > 0 {
			rule.left--
		}
		rule.hits++
		return fmt.Errorf("injected: %s was refused", rule.name)
	}
	return nil
}

type refusingClient struct {
	client.Client
	rf *refuser
}

func (c *refusingClient) Patch(ctx context.Context, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
	if err := c.rf.check(false, obj, p); err != nil {
		return err
	}
	return c.Client.Patch(ctx, obj, p, opts...)
}

func (c *refusingClient) Status() client.SubResourceWriter {
	return &refusingStatus{SubResourceWriter: c.Client.Status(), rf: c.rf}
}

type refusingStatus struct {
	client.SubResourceWriter
	rf *refuser
}

func (s *refusingStatus) Patch(ctx context.Context, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
	if err := s.rf.check(true, obj, p); err != nil {
		return err
	}
	return s.SubResourceWriter.Patch(ctx, obj, p, opts...)
}

func robotNamed(obj client.Object, name string) bool {
	r, ok := obj.(*fleetv1.Robot)
	return ok && r.Name == name
}

// refuseState refuses n robots/status writes that set robot's estopState to state.
func refuseState(robot string, state fleetv1.RobotEstopState, n int) *refuseRule {
	want := []byte(fmt.Sprintf(`"estopState":%q`, state))
	return &refuseRule{name: fmt.Sprintf("the %s write of estopState %s", robot, state), status: true, left: n,
		match: func(obj client.Object, data []byte) bool {
			return robotNamed(obj, robot) && bytes.Contains(data, want)
		}}
}

// refuseResumeHandled refuses n writes of robot's resume-processed marker.
func refuseResumeHandled(robot string, n int) *refuseRule {
	return &refuseRule{name: "the " + robot + " resume-handled write", left: n,
		match: func(obj client.Object, data []byte) bool {
			return robotNamed(obj, robot) && bytes.Contains(data, []byte(fleetv1.EstopResumeProcessedAnnotation))
		}}
}

// refuseResumeWriteAfterEvaluation refuses the first write of a resume annotation on
// robot made after the resume was evaluated.
func refuseResumeWriteAfterEvaluation(robot string, res *countingResumer) *refuseRule {
	return &refuseRule{name: "the " + robot + " resume write after evaluation", left: 1,
		match: func(obj client.Object, data []byte) bool {
			return robotNamed(obj, robot) && res.count() > 0 && bytes.Contains(data, []byte("estop-resume"))
		}}
}

// countingResumer stands in for the dispatcher's resume checks: it releases a Resuming
// robot (writing Normal as the control plane), unless told to refuse the first request.
// What a row tests is whether the request reaches it at all, and how often.
type countingResumer struct {
	mu          sync.Mutex
	calls       int
	refuseFirst bool
	c           client.Client
}

func (s *countingResumer) count() int { s.mu.Lock(); defer s.mu.Unlock(); return s.calls }

func (s *countingResumer) ConfirmResume(ctx context.Context, ns, name, _ string) (fleetv1.RobotEstopState, string, error) {
	s.mu.Lock()
	s.calls++
	refuse := s.refuseFirst && s.calls == 1
	s.mu.Unlock()
	robot := &fleetv1.Robot{}
	if err := s.c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, robot); err != nil {
		return "", "", err
	}
	if robot.Status.EstopState != fleetv1.RobotEstopResuming {
		return robot.Status.EstopState, "not Resuming", nil
	}
	if refuse {
		return fleetv1.RobotEstopResuming, "the rig refuses the first request", nil
	}
	base := robot.DeepCopy()
	robot.Status.EstopState = fleetv1.RobotEstopNormal
	if err := s.c.Status().Patch(ctx, robot, client.MergeFrom(base)); err != nil {
		return fleetv1.RobotEstopResuming, "", err
	}
	return fleetv1.RobotEstopNormal, "the rig confirms", nil
}

type memAudit struct {
	mu      sync.Mutex
	entries []audit.Entry
}

func (m *memAudit) Record(e audit.Entry) (audit.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, e)
	return e, nil
}

func (m *memAudit) triggered(robot string) []audit.Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []audit.Entry
	for _, e := range m.entries {
		if e.EventType == audit.EventEstopTriggered && e.Resource.Kind == "Robot" && e.Resource.Name == robot {
			out = append(out, e)
		}
	}
	return out
}

// ── Steps ─────────────────────────────────────────────────────────────────────────

func (r *rig) zone(name, parent string, propagate *bool) {
	r.t.Helper()
	z := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.w.ns},
		Spec: fleetv1.FleetZoneSpec{ParentZone: parent}}
	if propagate != nil {
		z.Spec.EstopPolicy = &fleetv1.ZoneEstopPolicy{PropagateToChildren: propagate}
	}
	if err := r.w.admin.Create(context.Background(), z); err != nil {
		r.t.Fatalf("creating zone %s: %v", name, err)
	}
}

func (r *rig) config() {
	r.t.Helper()
	if err := r.w.admin.Create(context.Background(), &fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{
		Name: controller.SwarmadaConfigName, Namespace: r.w.ns}}); err != nil {
		r.t.Fatalf("creating the SwarmadaConfig: %v", err)
	}
}

func (r *rig) robot(name, zone string) {
	r.t.Helper()
	// The Robot webhook reads the row's FleetAdapter through the manager's cache, which can lag
	// the fixture's status write (Connected, conformance Passed) under load; the refusal then names
	// an adapter with no status at all. Only that refusal is retried, for up to 5 s.
	var err error
	for deadline := time.Now().Add(5 * time.Second); ; {
		rb := markerRobot(r.w.ns, name, nil)
		rb.Spec.Zone = zone
		err = r.w.admin.Create(context.Background(), rb)
		if err == nil || !strings.Contains(err.Error(), "is not ready: phase=Pending conformance=Unknown") ||
			time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		r.t.Fatalf("creating robot %s: %v", name, err)
	}
	holdSetPhase(r.t, r.w, name, fleetv1.RobotPhaseIdle, false)
}

// update re-reads obj as c and applies mutate, retried on a conflict.
func (r *rig) update(c client.Client, obj client.Object, status bool, mutate func()) error {
	r.t.Helper()
	ctx := context.Background()
	var err error
	for i := 0; i < 10; i++ {
		if err = c.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			return err
		}
		base := obj.DeepCopyObject().(client.Object)
		mutate()
		if status {
			err = c.Status().Patch(ctx, obj, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		} else {
			err = c.Patch(ctx, obj, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		}
		if !apierrors.IsConflict(err) {
			return err
		}
	}
	return err
}

func (r *rig) zoneObj(name string) *fleetv1.FleetZone {
	return &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.w.ns}}
}

func (r *rig) robotObj(name string) *fleetv1.Robot {
	return &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.w.ns}}
}

func (r *rig) configObj() *fleetv1.SwarmadaConfig {
	return &fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Name: controller.SwarmadaConfigName, Namespace: r.w.ns}}
}

func (r *rig) must(what string, err error) {
	r.t.Helper()
	if err != nil {
		r.t.Fatalf("%s: %v", what, err)
	}
}

// setAnnotation sets (value non-nil) or removes the annotation key on obj, as the admin.
func (r *rig) setAnnotation(obj client.Object, key string, value *string) {
	r.t.Helper()
	r.must(fmt.Sprintf("annotating %s %s", obj.GetName(), key), r.update(r.w.admin, obj, false, func() {
		ann := obj.GetAnnotations()
		if ann == nil {
			ann = map[string]string{}
		}
		if value == nil {
			delete(ann, key)
		} else {
			ann[key] = *value
		}
		obj.SetAnnotations(ann)
	}))
}

func strp(s string) *string { return &s }

func (r *rig) trigger(obj client.Object, value string) {
	r.setAnnotation(obj, estopTriggeredAnnotation, strp(value))
}

func (r *rig) clearStop(obj client.Object) { r.setAnnotation(obj, estopTriggeredAnnotation, nil) }

func (r *rig) reparent(zone, parent string) {
	r.t.Helper()
	z := r.zoneObj(zone)
	r.must("re-parenting "+zone, r.update(r.w.admin, z, false, func() { z.Spec.ParentZone = parent }))
}

func (r *rig) setPropagate(zone string, v bool) {
	r.t.Helper()
	z := r.zoneObj(zone)
	r.must("estopPolicy on "+zone, r.update(r.w.admin, z, false, func() {
		z.Spec.EstopPolicy = &fleetv1.ZoneEstopPolicy{PropagateToChildren: boolp(v)}
	}))
}

func (r *rig) setSpecZone(robot, zone string) {
	r.t.Helper()
	rb := r.robotObj(robot)
	r.must("moving "+robot, r.update(r.w.admin, rb, false, func() { rb.Spec.Zone = zone }))
}

// setCurrentZone writes status.currentZone as c: the control plane's from a position
// report, or another identity's.
func (r *rig) setCurrentZone(c client.Client, robot, zone string) error {
	rb := r.robotObj(robot)
	return r.update(c, rb, true, func() { rb.Status.CurrentZone = zone })
}

func (r *rig) get(name string) *fleetv1.Robot { return holdRobot(r.t, r.w, name) }

func stopState(s fleetv1.RobotEstopState) bool {
	return s == fleetv1.RobotEstopStopped || s == fleetv1.RobotEstopFailed || s == fleetv1.RobotEstopStopping
}

func (r *rig) waitFor(what string, d time.Duration, cond func() (bool, string)) (bool, string) {
	deadline := time.Now().Add(d)
	var last string
	for time.Now().Before(deadline) {
		ok, desc := cond()
		if ok {
			r.t.Logf("RESULT %s: yes (%s)", what, desc)
			return true, desc
		}
		last = desc
		time.Sleep(50 * time.Millisecond)
	}
	r.t.Logf("RESULT %s: NO within %s; last %s", what, d, last)
	return false, last
}

func (r *rig) waitStopped(robot string) {
	r.t.Helper()
	if ok, last := r.waitFor(robot+" stopped", 15*time.Second, func() (bool, string) {
		s := r.get(robot).Status.EstopState
		return s == fleetv1.RobotEstopStopped || s == fleetv1.RobotEstopFailed, string(s)
	}); !ok {
		r.t.Fatalf("setup: %s never read Stopped or Failed (last %s)", robot, last)
	}
}

func (r *rig) waitMarker(obj client.Object, present bool) {
	r.t.Helper()
	if ok, last := r.waitFor(fmt.Sprintf("%s marker present=%v", obj.GetName(), present), 15*time.Second, func() (bool, string) {
		if err := r.w.admin.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
			return false, err.Error()
		}
		_, has := obj.GetAnnotations()[estopProcessedAnnotation]
		return has == present, fmt.Sprintf("%v", obj.GetAnnotations())
	}); !ok {
		r.t.Fatalf("setup: the processed marker on %s never reached present=%v (last %s)", obj.GetName(), present, last)
	}
}

// reconcileAction runs the FleetAction reconciler once on name, as the control plane.
func (r *rig) reconcileAction(name string) *fleetv1.FleetAction {
	r.t.Helper()
	ctx := context.Background()
	fr := &controller.FleetActionReconciler{GenerationStoreNamespace: rigStoreNamespace,
		Client: r.w.as(r.t, controllerUser), Scheme: r.w.scheme, Scheduler: scheduler.NewDefaultScheduler(),
	}
	_, _ = fr.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: r.w.ns, Name: name}})
	var fa fleetv1.FleetAction
	r.must("reading "+name, r.w.admin.Get(ctx, client.ObjectKey{Namespace: r.w.ns, Name: name}, &fa))
	return &fa
}

// dispatch creates a FleetAction pinned to robot and runs one dispatch pass on it.
func (r *rig) dispatch(name, robot string) *fleetv1.FleetAction {
	r.t.Helper()
	if err := r.w.admin.Create(context.Background(), &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Namespace: r.w.ns, Name: name},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, RobotSelector: robot},
	}); err != nil {
		r.t.Fatalf("creating FleetAction %s: %v", name, err)
	}
	var fa *fleetv1.FleetAction
	for i := 0; i < 5; i++ {
		fa = r.reconcileAction(name)
		if fa.Status.Phase != "" {
			break
		}
	}
	r.t.Logf("RESULT dispatch %s -> %s: phase=%q assignedRobot=%q message=%q", name, robot,
		fa.Status.Phase, fa.Status.AssignedRobot, fa.Status.Message)
	return fa
}

// hold gives robot an action to hold: dispatched to it, and InProgress when asked.
func (r *rig) hold(name, robot string, inProgress bool) {
	r.t.Helper()
	fa := r.dispatch(name, robot)
	// With the Zone Controller wired, its status write can race the dispatch commit, which
	// then reverts to Pending; a further pass assigns it.
	for i := 0; i < 20 && fa.Status.Phase == fleetv1.ActionPhasePending; i++ {
		time.Sleep(100 * time.Millisecond)
		fa = r.reconcileAction(name)
	}
	if fa.Status.Phase != fleetv1.ActionPhaseAssigned || fa.Status.AssignedRobot != robot {
		r.t.Fatalf("setup: %s was not assigned to %s: phase=%q assignedRobot=%q message=%q", name, robot,
			fa.Status.Phase, fa.Status.AssignedRobot, fa.Status.Message)
	}
	if inProgress {
		a := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.w.ns}}
		r.must("marking "+name+" InProgress", r.update(r.w.as(r.t, controllerUser), a, true, func() {
			a.Status.Phase = fleetv1.ActionPhaseInProgress
		}))
	}
}

// resume writes an operator's resume request and returns the answer the control plane
// gave on the robot (the EstopResume* Event), or "" if none came.
func (r *rig) resume(robot, value string) string {
	r.t.Helper()
	answers := func() map[string]string {
		out := map[string]string{}
		var evs corev1.EventList
		if err := r.w.admin.List(context.Background(), &evs, client.InNamespace(r.w.ns)); err != nil {
			return out
		}
		for _, e := range evs.Items {
			if e.InvolvedObject.Name == robot && strings.HasPrefix(e.Reason, "EstopResume") {
				out[string(e.UID)] = e.Reason + ": " + e.Message
			}
		}
		return out
	}
	before := answers()
	r.setAnnotation(r.robotObj(robot), fleetv1.EstopResumeAnnotation, strp(value))
	_, msg := r.waitFor("resume "+value+" on "+robot+" answered", 10*time.Second, func() (bool, string) {
		for uid, m := range answers() {
			if _, seen := before[uid]; !seen {
				return true, m
			}
		}
		return false, "no answer"
	})
	return msg
}

// ownRelease is the robot-scope stop and clear an admin applies to a robot, then waits
// for the clear to be applied.
func (r *rig) ownRelease(robot string) {
	r.t.Helper()
	r.trigger(r.robotObj(robot), "release")
	r.waitMarker(r.robotObj(robot), true)
	r.clearStop(r.robotObj(robot))
	r.waitMarker(r.robotObj(robot), false)
}

// assertHeld is the invariant for one robot a stop still holds: Stopped or Failed, or
// withheld from dispatch, and none of the named actions it may hold progresses.
// stopped also requires the stop state itself.
func (r *rig) assertHeld(robot string, stopped bool, actions ...string) {
	r.t.Helper()
	ok, last := r.waitFor(robot+" Stopped or Failed", 10*time.Second, func() (bool, string) {
		s := r.get(robot).Status.EstopState
		return s == fleetv1.RobotEstopStopped || s == fleetv1.RobotEstopFailed, string(s)
	})
	if !ok {
		if stopped {
			r.t.Errorf("INVARIANT %s: a stop holds it and it is not Stopped or Failed (estopState %q)", robot, last)
		}
		probe := fmt.Sprintf("probe-%s-%d", robot, time.Now().UnixNano()%1e6)
		fa := r.dispatch(probe, robot)
		if fa.Status.AssignedRobot == robot {
			r.t.Errorf("INVARIANT %s: a stop holds it, it reads %q, and new work was assigned to it (%s)",
				robot, last, probe)
		}
	}
	for _, a := range actions {
		var fa *fleetv1.FleetAction
		progressed := true
		for i := 0; i < 40; i++ {
			fa = r.reconcileAction(a)
			if fa.Status.AssignedRobot != robot || fa.Status.Phase == fleetv1.ActionPhasePaused ||
				fa.Status.Phase == fleetv1.ActionPhasePending {
				progressed = false
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		r.t.Logf("RESULT held action %s: phase=%q assignedRobot=%q message=%q", a, fa.Status.Phase,
			fa.Status.AssignedRobot, fa.Status.Message)
		if progressed {
			r.t.Errorf("INVARIANT %s: a stop holds it and the action it holds, %s, is %s, not Paused",
				robot, a, fa.Status.Phase)
		}
	}
}

// refusedNaming requires the answer to a resume to be a refusal that names the stop
// holding the robot.
func refusedNaming(t *testing.T, answer, stop string) {
	t.Helper()
	if !strings.HasPrefix(answer, "EstopResumeRefused") || !strings.Contains(answer, stop) {
		t.Errorf("INVARIANT: the release was not refused naming %s: %q", stop, answer)
	}
}

// standing is what swarmctl and every clear ask: which stop holds robot.
func (r *rig) standing(robot string) (estopscope.Holder, bool) {
	r.t.Helper()
	ctx := context.Background()
	var zl fleetv1.FleetZoneList
	var cl fleetv1.SwarmadaConfigList
	r.must("listing zones", r.w.admin.List(ctx, &zl, client.InNamespace(r.w.ns)))
	r.must("listing configs", r.w.admin.List(ctx, &cl, client.InNamespace(r.w.ns)))
	return estopscope.StandingStop(r.get(robot), zl.Items, cl.Items)
}

// ── The table ─────────────────────────────────────────────────────────────────────

type invariantRow struct {
	name string
	opts rigOpts
	run  func(t *testing.T, r *rig)
}

func zoneTreeRows(name string, run func(t *testing.T, r *rig)) []invariantRow {
	return []invariantRow{
		{name: name + "/without-zone-controller", run: run},
		{name: name + "/with-zone-controller", opts: rigOpts{zoneController: true}, run: run},
	}
}

func TestEnvtest_EstopHoldInvariant(t *testing.T) {
	var rows []invariantRow

	// ── A robot that re-enters a still-stopped zone after a release ──
	for _, how := range []string{"spec.zone", "currentZone"} {
		how := how
		rows = append(rows, invariantRow{name: "reentry-after-release/" + how, run: func(t *testing.T, r *rig) {
			r.zone("z", "", nil)
			r.zone("out", "", nil)
			// In the currentZone variant the robot's declared zone is outside z, and its own
			// position report carries it into z and out again.
			start := "z"
			if how == "currentZone" {
				start = "out"
			}
			r.robot("r1", start)
			move := func(zone string) {
				if how == "spec.zone" {
					r.setSpecZone("r1", zone)
					return
				}
				r.must("position report", r.setCurrentZone(r.w.as(t, controllerUser), "r1", zone))
			}
			if how == "currentZone" {
				move("z")
			}
			r.trigger(r.zoneObj("z"), "z-stop")
			r.waitStopped("r1")
			r.waitMarker(r.zoneObj("z"), true) // the move comes after z's stop is applied
			move("out")
			r.ownRelease("r1")
			ans := r.resume("r1", "reentry-release")
			t.Logf("RESULT reentry release: %s", ans)
			refusedNaming(t, ans, "zone z")
			r.dispatch("reentry-work", "r1")
			move("z")
			r.assertHeld("r1", false, "reentry-work")
		}})
	}

	// ── A zone tree edit takes a robot out from under an ancestor stop ──
	for _, variant := range []string{"reparent", "move", "currentZone", "propagate"} {
		variant := variant
		run := func(t *testing.T, r *rig) {
			r.zone("anc", "", nil)
			r.zone("c", "anc", nil)
			r.zone("elsewhere", "", nil)
			r.config()
			r.robot("rk", "c")
			if variant == "currentZone" {
				r.must("position report", r.setCurrentZone(r.w.as(t, controllerUser), "rk", "c"))
			}
			r.hold("a-held", "rk", false)
			r.trigger(r.zoneObj("anc"), "anc-stop")
			r.trigger(r.configObj(), "ns-stop")
			r.waitStopped("rk")
			// The tree edit comes after anc's stop is applied. A re-parent of c out from under anc, or
			// anc's propagateToChildren set to false, is refused while anc carries its stop, pending
			// or applied: a stop is released only through its clear. A robot's own move is admitted.
			r.waitMarker(r.zoneObj("anc"), true)
			switch variant {
			case "reparent":
				refusedWhileInForce(t, "re-parenting c out of anc", `"anc"`, r.tryReparent("c", ""))
			case "move":
				r.setSpecZone("rk", "elsewhere")
			case "currentZone":
				r.must("position report", r.setCurrentZone(r.w.as(t, controllerUser), "rk", "elsewhere"))
			case "propagate":
				anc := r.zoneObj("anc")
				refusedWhileInForce(t, "anc's propagateToChildren set to false", `"anc"`, r.update(r.w.admin, anc, false, func() {
					anc.Spec.EstopPolicy = &fleetv1.ZoneEstopPolicy{PropagateToChildren: boolp(false)}
				}))
			}
			// The namespace stop is cleared only once it was issued (its marker written), so the
			// marker's absence below means the clear was applied, not that the stop is still fanning out.
			r.waitMarker(r.configObj(), true)
			r.clearStop(r.configObj())
			r.waitMarker(r.configObj(), false)
			ans := r.resume("rk", "ancestor-release")
			t.Logf("RESULT ancestor resume: %s", ans)
			refusedNaming(t, ans, "zone anc")
			r.dispatch("ancestor-work", "rk")
			r.assertHeld("rk", false, "a-held", "ancestor-work")
			if variant == "propagate" {
				// What swarmctl names, and every clear asks, agrees with dispatch.
				by, held := r.standing("rk")
				t.Logf("RESULT propagate standing stop: held=%v by %s", held, by)
				if !held || by.Name != "anc" {
					t.Errorf("INVARIANT rk: dispatch withholds it while the standing stop reads held=%v by %s; "+
						"swarmctl and dispatch must name the same stop", held, by)
				}
			}
		}
		if variant == "reparent" || variant == "propagate" {
			rows = append(rows, zoneTreeRows("ancestor-stop-after-tree-edit/"+variant, run)...)
		} else {
			rows = append(rows, invariantRow{name: "ancestor-stop-after-tree-edit/" + variant, run: run})
		}
	}

	// ── Deleting the SwarmadaConfig that carries the namespace stop ──
	rows = append(rows, invariantRow{name: "config-delete-under-the-namespace-stop", run: func(t *testing.T, r *rig) {
		r.config()
		r.robot("r1", markerRobotZone)
		r.trigger(r.configObj(), "ns-stop")
		r.waitStopped("r1")
		err := r.w.admin.Delete(context.Background(), r.configObj())
		t.Logf("RESULT config delete under the stop: err=%v", err)
		if err == nil {
			t.Errorf("INVARIANT: the SwarmadaConfig carrying the namespace stop was deleted")
		} else if !strings.Contains(err.Error(), "kubectl annotate swarmadaconfig") {
			t.Errorf("the refusal does not name the clear: %v", err)
		}
		r.robot("late", markerRobotZone)
		r.dispatch("late-work", "late")
		r.assertHeld("r1", true)
		r.assertHeld("late", false, "late-work")
	}})
	rows = append(rows, invariantRow{name: "config-delete-in-a-terminating-namespace", run: func(t *testing.T, r *rig) {
		r.config()
		r.trigger(r.configObj(), "ns-stop")
		r.waitMarker(r.configObj(), true)
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: r.w.ns}}
		r.must("deleting the namespace", r.w.admin.Delete(context.Background(), ns))
		r.waitFor("the namespace is terminating", 10*time.Second, func() (bool, string) {
			if err := r.w.admin.Get(context.Background(), client.ObjectKeyFromObject(ns), ns); err != nil {
				return false, err.Error()
			}
			return ns.DeletionTimestamp != nil, fmt.Sprint(ns.DeletionTimestamp)
		})
		err := r.w.admin.Delete(context.Background(), r.configObj())
		t.Logf("RESULT config delete in a terminating namespace: err=%v", err)
		if err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("a namespace teardown would deadlock: the config delete was refused: %v", err)
		}
	}})

	// ── An edge identity writes the stopped zone's status ──
	rows = append(rows, invariantRow{name: "edge-identity-writes-the-zone-status", run: func(t *testing.T, r *rig) {
		bindShippedClusterRole(t, r.w, "swarmada_edge_role.yaml", map[string]interface{}{
			"kind": "User", "name": "edge-1", "apiGroup": "rbac.authorization.k8s.io"})
		edge := asIdentity(t, r.w, "edge-1")
		r.zone("z", "", nil)
		r.zone("out", "", nil)
		r.robot("r1", "out")
		r.hold("a-held", "r1", true)
		r.trigger(r.zoneObj("z"), "z-stop")
		r.waitMarker(r.zoneObj("z"), true)
		err := edge.Status().Patch(context.Background(), r.zoneObj("z"),
			client.RawPatch(types.MergePatchType, []byte(`{"status":{"estopStatus":"Clear","lastEstopAt":null}}`)))
		t.Logf("RESULT edge status write: err=%v", err)
		if ok, _ := r.waitFor("the zone reads Triggered again", 10*time.Second, func() (bool, string) {
			z := r.zoneObj("z")
			_ = r.w.admin.Get(context.Background(), client.ObjectKeyFromObject(z), z)
			return z.Status.EstopStatus == fleetv1.ZoneEstopTriggered, string(z.Status.EstopStatus)
		}); !ok {
			t.Errorf("the stopped zone still reads its status as another identity wrote it")
		}
		r.setSpecZone("r1", "z")
		r.assertHeld("r1", true, "a-held")
	}})

	// ── A refused stop-state write, at every outcome ──
	for _, c := range []struct {
		name  string
		mode  ackMode
		state fleetv1.RobotEstopState
		noSt  bool
	}{
		{name: "robot-scope-refused-Stopped", mode: ackStopped, state: fleetv1.RobotEstopStopped},
		{name: "robot-scope-refused-Failed-ack", mode: ackFailed, state: fleetv1.RobotEstopFailed},
		{name: "robot-scope-refused-Failed-no-stream", mode: ackStopped, state: fleetv1.RobotEstopFailed, noSt: true},
		{name: "robot-scope-refused-Stopping", mode: ackStopping, state: fleetv1.RobotEstopStopping},
	} {
		c := c
		rows = append(rows, invariantRow{name: c.name, run: func(t *testing.T, r *rig) {
			r.robot("r1", markerRobotZone)
			r.hold("a-held", "r1", false)
			r.setMode("r1", c.mode)
			if c.noSt {
				r.dereg()
			}
			rule := r.rf.add(refuseState("r1", c.state, 1))
			r.trigger(r.robotObj("r1"), "own")
			r.assertHeld("r1", true, "a-held")
			r.dispatch("t1", "r1")
			r.assertHeld("r1", true, "t1")
			if r.rf.hitsOf(rule) != 1 {
				t.Errorf("the refusal did not fire (%d)", r.rf.hitsOf(rule))
			}
			// The audit chain never says a state was written that was not.
			for _, e := range r.aud.triggered("r1") {
				t.Logf("RESULT audit ESTOP_TRIGGERED outcome=%s detail=%v", e.Outcome, e.Detail)
			}
			if c.state != fleetv1.RobotEstopStopping {
				es := r.aud.triggered("r1")
				if len(es) < 2 || es[0].Detail["state_written"] != "false" || es[len(es)-1].Detail["state_written"] != "true" {
					t.Errorf("the audit entries do not record the unwritten outcome, then the written one: %d entries", len(es))
				}
			}
		}})
	}

	// ── A robot's own trigger holds it before its stop resolves ──
	rows = append(rows, invariantRow{name: "own-trigger-withholds-before-the-outcome", run: func(t *testing.T, r *rig) {
		r.robot("r1", markerRobotZone)
		r.hold("a-held", "r1", false)
		r.setMode("r1", ackNone) // no answer: the delivery window (2s) ends Failed
		r.trigger(r.robotObj("r1"), "own")
		time.Sleep(200 * time.Millisecond)
		fa := r.dispatch("t1", "r1")
		held := r.reconcileAction("a-held")
		t.Logf("RESULT before the outcome: new work phase=%q assignedRobot=%q; held a-held phase=%q",
			fa.Status.Phase, fa.Status.AssignedRobot, held.Status.Phase)
		if fa.Status.AssignedRobot == "r1" {
			t.Errorf("INVARIANT r1: it carries its own stop and new work was assigned before the stop resolved")
		}
		if held.Status.Phase != fleetv1.ActionPhasePaused {
			t.Errorf("INVARIANT r1: it carries its own stop and its action is %s, not Paused", held.Status.Phase)
		}
		r.assertHeld("r1", true)
	}})

	// ── A fan-out whose Stopped write is refused ──
	for _, c := range []struct {
		name       string
		inProgress bool
		scope      string
	}{
		{name: "zone-fanout-refused-Stopped-InProgress", inProgress: true, scope: "zone"},
		{name: "zone-fanout-refused-Stopped-Assigned", scope: "zone"},
		{name: "namespace-fanout-refused-Stopped-InProgress", inProgress: true, scope: "namespace"},
		{name: "zone-late-arrival-refused-Stopped-InProgress", inProgress: true, scope: "late"},
	} {
		c := c
		rows = append(rows, invariantRow{name: c.name, run: func(t *testing.T, r *rig) {
			r.zone("z", "", nil)
			r.zone("out", "", nil)
			start := "z"
			if c.scope == "late" {
				start = "out"
			}
			if c.scope == "namespace" {
				r.config()
			}
			r.robot("r1", start)
			r.hold("a-held", "r1", c.inProgress)
			var carrier client.Object = r.zoneObj("z")
			if c.scope == "namespace" {
				carrier = r.configObj()
			}
			if c.scope == "late" {
				r.trigger(carrier, "stop")
				r.waitMarker(r.zoneObj("z"), true)
			}
			rule := r.rf.add(refuseState("r1", fleetv1.RobotEstopStopped, 1))
			if c.scope == "late" {
				r.setSpecZone("r1", "z")
			} else {
				r.trigger(carrier, "stop")
			}
			r.assertHeld("r1", true, "a-held")
			if r.rf.hitsOf(rule) != 1 {
				t.Errorf("the refusal did not fire (%d)", r.rf.hitsOf(rule))
			}
			// The clear does not leave the robot Normal without a resume.
			r.clearStop(carrier)
			r.waitMarker(carrier, false)
			if s := r.get("r1").Status.EstopState; s == "" || s == fleetv1.RobotEstopNormal {
				t.Errorf("INVARIANT r1: after the clear it reads %q with no resume", s)
			}
		}})
	}

	// ── The stop is issued again after a refused write, and holds on re-entry ──
	rows = append(rows, invariantRow{name: "refused-write-reissued-then-reentry", run: func(t *testing.T, r *rig) {
		r.zone("z", "", nil)
		r.zone("out", "", nil)
		r.robot("r1", "z")
		r.hold("a-held", "r1", true)
		// Refused twice: one stale cached read of the zone can re-run the whole fan-out, and
		// that alone must not be what issues the stop again.
		r.rf.add(refuseState("r1", fleetv1.RobotEstopStopped, 2))
		r.trigger(r.zoneObj("z"), "z-stop")
		r.assertHeld("r1", true, "a-held")
		if n := r.sendsTo("r1"); n < 3 {
			t.Errorf("INVARIANT r1: its stop write was refused and the stop was not issued again (sends=%d)", n)
		}
		r.waitMarker(r.zoneObj("z"), true) // the move comes after z's stop is applied
		r.setSpecZone("r1", "out")
		r.setSpecZone("r1", "z")
		r.assertHeld("r1", true, "a-held")
	}})

	// ── A resume request whose handled write fails ──
	rows = append(rows, invariantRow{name: "pending-resume-does-not-block-a-stop", run: func(t *testing.T, r *rig) {
		r.robot("r1", markerRobotZone)
		r.hold("a-held", "r1", false)
		r.rf.add(refuseResumeHandled("r1", -1))
		r.setAnnotation(r.robotObj("r1"), fleetv1.EstopResumeAnnotation, strp("req-1"))
		r.trigger(r.robotObj("r1"), "own")
		r.assertHeld("r1", true, "a-held")
	}})
	rows = append(rows, invariantRow{name: "a-refused-release-is-not-re-run", run: func(t *testing.T, r *rig) {
		r.robot("r1", markerRobotZone)
		rb := r.robotObj("r1")
		r.must("Resuming", r.update(r.w.as(t, controllerUser), rb, true, func() {
			rb.Status.EstopState = fleetv1.RobotEstopResuming
		}))
		r.res.refuseFirst = true
		r.rf.add(refuseResumeWriteAfterEvaluation("r1", r.res))
		r.setAnnotation(r.robotObj("r1"), fleetv1.EstopResumeAnnotation, strp("req-1"))
		time.Sleep(4 * time.Second)
		t.Logf("RESULT resume evaluations=%d estopState=%q", r.res.count(), r.get("r1").Status.EstopState)
		if r.res.count() != 1 {
			t.Errorf("a refused release was evaluated %d times; it is closed, not queued", r.res.count())
		}
		if s := r.get("r1").Status.EstopState; s != fleetv1.RobotEstopResuming {
			t.Errorf("the refused request released the robot later: %q", s)
		}
	}})

	// ── Re-parenting a zone under a stopped zone ──
	for _, variant := range []string{"leaf", "intermediate", "grandparent-currentZone"} {
		variant := variant
		rows = append(rows, zoneTreeRows("reparent-under-a-stopped-zone/"+variant, func(t *testing.T, r *rig) {
			r.zone("z", "", nil)
			r.zone("b", "", nil)
			r.zone("c", "b", nil)
			r.robot("r1", "c")
			if variant == "grandparent-currentZone" {
				r.must("position report", r.setCurrentZone(r.w.as(t, controllerUser), "r1", "c"))
			}
			r.hold("a-held", "r1", false)
			r.trigger(r.zoneObj("z"), "z-stop")
			r.waitMarker(r.zoneObj("z"), true)
			if variant == "leaf" {
				r.reparent("c", "z")
			} else {
				r.reparent("b", "z")
			}
			r.assertHeld("r1", true, "a-held")
		})...)
	}

	// ── currentZone written by another identity, or changed by a position report ──
	rows = append(rows, invariantRow{name: "foreign-currentZone-write-then-zone-stop", run: func(t *testing.T, r *rig) {
		r.zone("hall", "", nil)
		r.zone("dock", "", nil)
		r.robot("r1", "dock")
		r.must("position report", r.setCurrentZone(r.w.as(t, controllerUser), "r1", "hall"))
		r.hold("a-held", "r1", false)
		err := r.setCurrentZone(r.w.admin, "r1", "dock")
		t.Logf("RESULT admin writes currentZone: err=%v", err)
		r.trigger(r.zoneObj("hall"), "hall-stop")
		r.waitMarker(r.zoneObj("hall"), true)
		r.assertHeld("r1", true, "a-held")
	}})
	for _, who := range []string{"admin", "position-report"} {
		who := who
		rows = append(rows, invariantRow{name: "currentZone-change-then-namespace-clear/" + who, run: func(t *testing.T, r *rig) {
			r.zone("hall", "", nil)
			r.zone("dock", "", nil)
			r.config()
			r.robot("r1", "dock")
			r.must("position report", r.setCurrentZone(r.w.as(t, controllerUser), "r1", "hall"))
			r.hold("a-held", "r1", false)
			r.trigger(r.zoneObj("hall"), "hall-stop")
			// The move below is written straight to robots/status. Neither control-plane writer of
			// currentZone moves a robot out of a stop before that stop is applied (holdZoneMove), so
			// the row moves r1 only once hall's stop is applied: a position report after both stops.
			// The report inside that window is the observe-position row (positionHoldRows).
			r.waitMarker(r.zoneObj("hall"), true)
			r.trigger(r.configObj(), "ns-stop")
			r.waitStopped("r1")
			c := r.w.admin
			if who == "position-report" {
				c = r.w.as(t, controllerUser)
			}
			err := r.setCurrentZone(c, "r1", "dock")
			t.Logf("RESULT %s writes currentZone: err=%v", who, err)
			r.waitMarker(r.configObj(), true) // issued before it is cleared (see the ancestor rows)
			r.clearStop(r.configObj())
			r.waitMarker(r.configObj(), false)
			ans := r.resume("r1", "cz-release")
			t.Logf("RESULT currentZone resume: %s", ans)
			refusedNaming(t, ans, "zone hall")
			r.dispatch("cz-work", "r1")
			r.assertHeld("r1", false, "a-held", "cz-work")
		}})
	}

	// ── A zone deleted, recreated and re-triggered with the same value ──
	rows = append(rows, invariantRow{name: "zone-recreated-same-trigger", run: func(t *testing.T, r *rig) {
		r.zone("z", "", nil)
		r.zone("y", "", nil)
		r.robot("r1", "z")
		r.trigger(r.zoneObj("z"), "same")
		r.waitStopped("r1")
		r.waitMarker(r.zoneObj("z"), true) // the move comes after z's stop is applied
		r.setSpecZone("r1", "y")
		r.ownRelease("r1")
		ans := r.resume("r1", "rel")
		t.Logf("RESULT recreate release: %s", ans)
		refusedNaming(t, ans, "zone z")
		// A zone that carries a stop is not deleted, by any identity, so it cannot be created again
		// with the same trigger to stand in for the old one: the stop is released only through its clear.
		refusedWhileInForce(t, "the admin's delete of z", `"z"`, r.w.admin.Delete(context.Background(), r.zoneObj("z")))
		r.dispatch("rec-work", "r1")
		r.setSpecZone("r1", "z")
		r.assertHeld("r1", false, "rec-work")
	}})

	// ── A manager restart under a stop ──
	rows = append(rows, invariantRow{name: "restart-under-a-zone-stop-then-move-out-and-release", run: func(t *testing.T, r *rig) {
		r.zone("z", "", nil)
		r.zone("out", "", nil)
		r.robot("r1", "z")
		r.trigger(r.zoneObj("z"), "z-stop")
		r.waitStopped("r1")
		r.waitMarker(r.zoneObj("z"), true) // the move comes after z's stop is applied
		r.restart()
		r.setSpecZone("r1", "out")
		r.ownRelease("r1")
		ans := r.resume("r1", "rs")
		t.Logf("RESULT restart release: %s", ans)
		refusedNaming(t, ans, "zone z")
		r.dispatch("rs-work", "r1")
		r.assertHeld("r1", false, "rs-work")
	}})

	// ── Controls (green on every tree) ──
	rows = append(rows, invariantRow{name: "control-zone-clear-then-resume-own-trigger-holds", run: func(t *testing.T, r *rig) {
		r.zone("z", "", nil)
		r.robot("r1", "z")
		r.robot("r2", "z")
		r.trigger(r.robotObj("r1"), "own")
		r.waitMarker(r.robotObj("r1"), true)
		r.trigger(r.zoneObj("z"), "z-stop")
		r.waitStopped("r2")
		r.waitMarker(r.zoneObj("z"), true) // issued before it is cleared (see the ancestor rows)
		r.clearStop(r.zoneObj("z"))
		r.waitMarker(r.zoneObj("z"), false)
		t.Logf("RESULT r1 resume: %s", r.resume("r1", "own-1"))
		t.Logf("RESULT r2 resume: %s", r.resume("r2", "z-1"))
		r.dispatch("r1-work", "r1")
		r.assertHeld("r1", true, "r1-work")
		if s := r.get("r2").Status.EstopState; s != fleetv1.RobotEstopNormal {
			t.Errorf("control: r2, held by the zone only, is %q after the zone's clear and its resume", s)
		}
	}})
	rows = append(rows, invariantRow{name: "control-never-in-the-zone-moved-in", run: func(t *testing.T, r *rig) {
		r.zone("z", "", nil)
		r.zone("out", "", nil)
		r.robot("r1", "out")
		r.hold("a-held", "r1", true)
		r.trigger(r.zoneObj("z"), "z-stop")
		r.waitMarker(r.zoneObj("z"), true)
		r.setSpecZone("r1", "z")
		r.assertHeld("r1", true, "a-held")
	}})

	rows = append(rows, recordRows()...)
	rows = append(rows, rolloutRows()...)
	rows = append(rows, heldWorkRows()...)
	rows = append(rows, pendingScopeRows()...)
	rows = append(rows, positionHoldRows()...)
	rows = append(rows, stopUnconfirmedRows()...)
	rows = append(rows, shedRows()...)
	rows = append(rows, guardRows()...)
	rows = append(rows, taskCancelRows()...)
	rows = append(rows, recordKeyRows()...)
	rows = append(rows, stopOutcomeRows()...)
	rows = append(rows, arbiterRows()...)
	rows = append(rows, movedUnderStopRows()...)
	rows = append(rows, reconnectRows()...)
	rows = append(rows, registrationRows()...)
	rows = append(rows, anyStopFinalizerRows()...)
	rows = append(rows, zoneStopFinalizerRows()...)
	rows = append(rows, startedWorkRows()...)

	shared := startSafetyWire(t)
	for _, row := range rows {
		row := row
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, shared, row.opts)
			row.run(t, r)
		})
	}
}
