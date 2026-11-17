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
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controller"
	"github.com/swarmada/swarmada/internal/metrics"
	"github.com/swarmada/swarmada/internal/safety"
)

// Every zone or namespace stop is sent to its robots within a second of its trigger, whatever
// another stop, or another carrier's clear, is still writing (RFC-0001 #safety-robot-stop-table,
// row S1: "Nothing is written, admitted or awaited before either is sent"; #safety-estop-scopes:
// "delays the send to no other").
//
// The safety dispatcher sends a robot's stop before it writes anything, and only then writes the
// outcome; the fan-out also writes each robot's hold record, and a clear writes each robot in
// turn. Under a busy Robot write path those writes trail by many seconds (up to about 20 s at 100
// robots on the default client budget). trailingEstopper holds them for as long as a test says.

// overlapTrail is how long a slow stop's per-robot outcome is held after its send.
const overlapTrail = 10 * time.Second

// stopKey names one stop: the namespace and the object that carries it (a FleetZone, or the
// namespace's SwarmadaConfig, which has the same name in every namespace).
type stopKey struct {
	namespace string
	kind      fleetv1.StopCarrierKind
	name      string
}

// trailingEstopper stands in for the safety dispatcher. It records the instant each robot's stop
// is handed to it, which is when the dispatcher sends (row S1), then, for the stop named slow,
// holds overlapTrail before writing the confirmed outcome. Each clear of a robot is held
// clearDelay, as a clear's per-robot writes on a busy write path would be.
type trailingEstopper struct {
	c          client.Client
	slow       stopKey
	clearDelay time.Duration

	mu    sync.Mutex
	sends map[stopKey][]time.Time
}

func newTrailingEstopper(c client.Client, slow stopKey) *trailingEstopper {
	return &trailingEstopper{c: c, slow: slow, sends: map[stopKey][]time.Time{}}
}

func (e *trailingEstopper) TriggerEstop(ctx context.Context, namespace, robotID, _, _ string,
	scope metrics.EstopScope, carrier safety.Carrier) (safety.Result, error) {
	k := stopKey{namespace, carrier.Kind, carrier.Name}
	e.mu.Lock()
	e.sends[k] = append(e.sends[k], time.Now())
	e.mu.Unlock()
	if k == e.slow {
		select {
		case <-time.After(overlapTrail):
		case <-ctx.Done():
			return safety.Result{}, ctx.Err()
		}
	}
	var r fleetv1.Robot
	if err := e.c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: robotID}, &r); err != nil {
		return safety.Result{}, err
	}
	base := r.DeepCopy()
	r.Status.EstopState = fleetv1.RobotEstopStopped
	recordLikeTheDispatcher(&r, scope, carrier)
	if err := e.c.Status().Patch(ctx, &r, client.MergeFrom(base)); err != nil {
		return safety.Result{}, err
	}
	return safety.Result{Delivered: true, Confirmed: true, State: fleetv1.RobotEstopStopped,
		Latency: time.Millisecond}, nil
}

func (e *trailingEstopper) ClearEstop(ctx context.Context, _, _, _ string, _ safety.Ends) (fleetv1.RobotEstopState, error) {
	if e.clearDelay > 0 {
		select {
		case <-time.After(e.clearDelay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return fleetv1.RobotEstopResuming, nil
}

// firstSendAfter is the first send of stop k at or after t.
func (e *trailingEstopper) firstSendAfter(k stopKey, t time.Time) (time.Time, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, at := range e.sends[k] {
		if !at.Before(t) {
			return at, true
		}
	}
	return time.Time{}, false
}

// waitSend waits up to by for stop k's first send at or after t.
func (e *trailingEstopper) waitSend(k stopKey, t time.Time, by time.Duration) (time.Time, bool) {
	deadline := time.Now().Add(by)
	for time.Now().Before(deadline) {
		if at, ok := e.firstSendAfter(k, t); ok {
			return at, true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return time.Time{}, false
}

// runStopControllers runs the namespace- and zone-estop controllers as the manager runs them,
// with maxConcurrent passes per kind (0: the controllers' default), until the test ends.
func runStopControllers(t *testing.T, w *safetyWire, est controller.ZoneEstopper, maxConcurrent int) {
	t.Helper()
	cfg := rest.CopyConfig(w.cfg)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: controllerUser, Groups: []string{"system:masters"}}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: w.scheme, Metrics: metricsserver.Options{BindAddress: "0"}})
	if err != nil {
		t.Fatalf("building the controller manager: %v", err)
	}
	if err := (&controller.NamespaceEstopReconciler{Client: mgr.GetClient(), Scheme: w.scheme, Estopper: est,
		APIReader: mgr.GetAPIReader(), MaxConcurrentStops: maxConcurrent}).SetupWithManager(mgr); err != nil {
		t.Fatalf("registering the namespace estop reconciler: %v", err)
	}
	if err := (&controller.ZoneEstopReconciler{Client: mgr.GetClient(), Scheme: w.scheme, Estopper: est,
		APIReader: mgr.GetAPIReader(), MaxConcurrentStops: maxConcurrent}).SetupWithManager(mgr); err != nil {
		t.Fatalf("registering the zone estop reconciler: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(20 * time.Second):
			t.Error("the controller manager did not stop")
		}
	})
}

// overlapNamespace prepares ns for the cases: the marker fixture (zone bay-1, adapter acme), a
// SwarmadaConfig, and one robot in each of the zones named, each zone created if missing.
func overlapNamespace(t *testing.T, w *safetyWire, ns string, zones ...string) {
	t.Helper()
	ctx := context.Background()
	markerFixture(t, w, ns)
	if err := w.admin.Create(ctx, &fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{
		Name: controller.SwarmadaConfigName, Namespace: ns}}); err != nil {
		t.Fatalf("creating the SwarmadaConfig in %s: %v", ns, err)
	}
	for _, z := range zones {
		if z != markerRobotZone {
			if err := w.admin.Create(ctx, &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: z, Namespace: ns}}); err != nil {
				t.Fatalf("creating zone %s in %s: %v", z, ns, err)
			}
		}
		r := markerRobot(ns, "robot-"+z, nil)
		r.Spec.Zone = z
		if err := w.admin.Create(ctx, r); err != nil {
			t.Fatalf("creating robot-%s in %s: %v", z, ns, err)
		}
	}
}

func zoneStop(ns, zone string) stopKey {
	return stopKey{ns, fleetv1.StopCarrierFleetZone, zone}
}

func namespaceStop(ns string) stopKey {
	return stopKey{ns, fleetv1.StopCarrierSwarmadaConfig, controller.SwarmadaConfigName}
}

// setStop writes (value != "") or removes (value == "") the stop trigger on k's carrier, as the
// estop-trigger and estop-clear verbs do. It returns the moment just before the write was made:
// the controller can send the stop before the write's own response arrives, so a time taken after
// it could come after the send. Measuring from before the write counts admission against the stop.
func setStop(t *testing.T, w *safetyWire, k stopKey, value string) time.Time {
	t.Helper()
	ctx := context.Background()
	var obj client.Object = &fleetv1.SwarmadaConfig{}
	if k.kind == fleetv1.StopCarrierFleetZone {
		obj = &fleetv1.FleetZone{}
	}
	if err := w.admin.Get(ctx, client.ObjectKey{Namespace: k.namespace, Name: k.name}, obj); err != nil {
		t.Fatal(err)
	}
	base := obj.DeepCopyObject().(client.Object)
	ann := obj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	if value == "" {
		delete(ann, "swarmada.io/estop-triggered")
	} else {
		ann["swarmada.io/estop-triggered"] = value
	}
	obj.SetAnnotations(ann)
	at := time.Now()
	if err := w.admin.Patch(ctx, obj, client.MergeFrom(base)); err != nil {
		t.Fatalf("writing the stop trigger of %v: %v", k, err)
	}
	return at
}

// waitProcessed waits for k's processed marker to equal value ("" waits for it to be gone).
func waitProcessed(t *testing.T, w *safetyWire, k stopKey, value string, by time.Duration) {
	t.Helper()
	deadline := time.Now().Add(by)
	for time.Now().Before(deadline) {
		var obj client.Object = &fleetv1.SwarmadaConfig{}
		if k.kind == fleetv1.StopCarrierFleetZone {
			obj = &fleetv1.FleetZone{}
		}
		if err := w.admin.Get(context.Background(), client.ObjectKey{Namespace: k.namespace, Name: k.name}, obj); err == nil {
			v, ok := obj.GetAnnotations()["swarmada.io/estop-processed"]
			if (value == "" && !ok) || (value != "" && v == value) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%v: processed marker never became %q", k, value)
}

// requireSentWithin fails unless stop k was sent within a second of trigger.
func requireSentWithin(t *testing.T, est *trailingEstopper, k stopKey, trigger time.Time, what string) time.Duration {
	t.Helper()
	at, ok := est.waitSend(k, trigger, overlapTrail+15*time.Second)
	if !ok {
		t.Fatalf("%s: stop %v was never sent", what, k)
	}
	d := at.Sub(trigger)
	t.Logf("MEASURED %s: %v trigger -> first send %v", what, k, d.Round(time.Millisecond))
	if d > time.Second {
		t.Errorf("%s: stop %v reached its first robot %v after its trigger; every stop must be sent within 1 s "+
			"of its trigger, whatever other stops and clears are doing", what, k, d.Round(time.Millisecond))
	}
	return d
}

type overlapStop struct {
	nsIndex int // 0: the wire's namespace, 1: a second namespace
	zone    string
}

// A stop is triggered, its outcome writes held overlapTrail; a second stop is triggered a second
// later, inside that trail. Both must be sent within a second of their triggers.
func TestEnvtest_EveryStopIsSentWithinASecondOfItsTrigger(t *testing.T) {
	for _, tc := range []struct {
		name          string
		first, second overlapStop
	}{
		{"zone then zone, one namespace", overlapStop{0, markerRobotZone}, overlapStop{0, "bay-2"}},
		{"zone then zone, two namespaces", overlapStop{0, markerRobotZone}, overlapStop{1, "bay-2"}},
		{"namespace then zone, one namespace", overlapStop{0, ""}, overlapStop{0, "bay-2"}},
		{"zone then namespace, one namespace", overlapStop{0, markerRobotZone}, overlapStop{0, ""}},
		{"namespace then namespace, two namespaces", overlapStop{0, ""}, overlapStop{1, ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := startSafetyWire(t)
			namespaces := []string{w.ns, freshNamespace(t, w)}
			for _, ns := range namespaces {
				overlapNamespace(t, w, ns, markerRobotZone, "bay-2")
			}
			key := func(s overlapStop) stopKey {
				if s.zone != "" {
					return zoneStop(namespaces[s.nsIndex], s.zone)
				}
				return namespaceStop(namespaces[s.nsIndex])
			}
			est := newTrailingEstopper(w.as(t, controllerUser), key(tc.first))
			runStopControllers(t, w, est, 0)

			t1 := setStop(t, w, key(tc.first), "first-stop")
			requireSentWithin(t, est, key(tc.first), t1, "first stop")
			time.Sleep(time.Second) // inside the first stop's trail
			t2 := setStop(t, w, key(tc.second), "second-stop")
			requireSentWithin(t, est, key(tc.second), t2, "second stop")
		})
	}
}

// 64 zones stopped within a moment of each other are each sent within a second of their trigger:
// one zone's pass waits for no other zone's.
func TestEnvtest_SixtyFourZoneStopsAtOnceAreEachSentWithinASecond(t *testing.T) {
	w := startSafetyWire(t)
	zones := make([]string, 64)
	for i := range zones {
		zones[i] = fmt.Sprintf("z-%02d", i)
	}
	overlapNamespace(t, w, w.ns, zones...)
	// The first zone's outcome is held for the whole test, as on a busy write path; the others
	// are triggered while it is held.
	est := newTrailingEstopper(w.as(t, controllerUser), zoneStop(w.ns, zones[0]))
	runStopControllers(t, w, est, 0)

	triggers := make([]time.Time, len(zones))
	for i, z := range zones {
		triggers[i] = setStop(t, w, zoneStop(w.ns, z), "stop-"+z)
	}
	worst := time.Duration(0)
	for i, z := range zones {
		if d := requireSentWithin(t, est, zoneStop(w.ns, z), triggers[i], "zone "+z); d > worst {
			worst = d
		}
	}
	t.Logf("MEASURED 64 zones: worst trigger -> first send %v", worst.Round(time.Millisecond))
}

// A hundred zones' stops are in force, their passes resyncing and reading; a new zone's stop is
// still sent within a second of its trigger.
func TestEnvtest_AStopIsSentWhileAHundredZonesAreInForce(t *testing.T) {
	w := startSafetyWire(t)
	zones := make([]string, 100)
	for i := range zones {
		zones[i] = fmt.Sprintf("z-%03d", i)
	}
	overlapNamespace(t, w, w.ns, append(zones, "fresh")...)
	est := newTrailingEstopper(w.as(t, controllerUser), stopKey{})
	runStopControllers(t, w, est, 0)
	for _, z := range zones {
		setStop(t, w, zoneStop(w.ns, z), "stop-"+z)
	}
	for _, z := range zones {
		waitProcessed(t, w, zoneStop(w.ns, z), "stop-"+z, 60*time.Second)
	}
	tf := setStop(t, w, zoneStop(w.ns, "fresh"), "stop-fresh")
	requireSentWithin(t, est, zoneStop(w.ns, "fresh"), tf, "new zone among 100 in force")
}

// A zone's clear writes its robots in turn; another zone stopped during that clear is sent at once.
func TestEnvtest_AZoneStopIsSentDuringAnotherZonesClear(t *testing.T) {
	w := startSafetyWire(t)
	cleared := make([]string, 8)
	for i := range cleared {
		cleared[i] = fmt.Sprintf("bay-1-%d", i)
	}
	overlapNamespace(t, w, w.ns, append([]string{markerRobotZone}, "bay-2")...)
	// Eight more robots in bay-1, so its clear has eight robots to write.
	for _, name := range cleared {
		r := markerRobot(w.ns, name, nil)
		if err := w.admin.Create(context.Background(), r); err != nil {
			t.Fatalf("creating %s: %v", name, err)
		}
	}
	est := newTrailingEstopper(w.as(t, controllerUser), stopKey{})
	est.clearDelay = time.Second
	runStopControllers(t, w, est, 0)

	setStop(t, w, zoneStop(w.ns, markerRobotZone), "bay-1-stop")
	waitProcessed(t, w, zoneStop(w.ns, markerRobotZone), "bay-1-stop", 30*time.Second)
	setStop(t, w, zoneStop(w.ns, markerRobotZone), "") // the clear: nine robots, a second each
	time.Sleep(time.Second)
	t2 := setStop(t, w, zoneStop(w.ns, "bay-2"), "bay-2-stop")
	requireSentWithin(t, est, zoneStop(w.ns, "bay-2"), t2, "zone stopped during another zone's clear")
}

// A zone triggered again during its own clear is sent its new stop at once: the clear stops
// before its next robot, and the new trigger is issued as a new stop.
func TestEnvtest_AReTriggerInterruptsItsOwnZonesClear(t *testing.T) {
	w := startSafetyWire(t)
	overlapNamespace(t, w, w.ns, markerRobotZone)
	for i := 0; i < 9; i++ {
		r := markerRobot(w.ns, fmt.Sprintf("bay-1-%d", i), nil)
		if err := w.admin.Create(context.Background(), r); err != nil {
			t.Fatalf("creating a robot: %v", err)
		}
	}
	est := newTrailingEstopper(w.as(t, controllerUser), stopKey{})
	est.clearDelay = 500 * time.Millisecond
	runStopControllers(t, w, est, 0)

	k := zoneStop(w.ns, markerRobotZone)
	setStop(t, w, k, "first")
	waitProcessed(t, w, k, "first", 30*time.Second)
	setStop(t, w, k, "") // the clear: ten robots, half a second each
	time.Sleep(time.Second)
	t2 := setStop(t, w, k, "again")
	requireSentWithin(t, est, k, t2, "zone re-triggered during its own clear")
	waitProcessed(t, w, k, "again", 30*time.Second)
}

// swarmada_estop_fanout_duration_seconds runs from the operator's trigger (RFC-0001
// #control-plane-observability-prometheus-metrics-contract). With one pass of each kind at a time,
// a second zone's stop waits in the queue behind the first's trail; the episode observed for it
// must include that wait.
func TestEnvtest_TheFanOutMetricCountsTheWaitBeforeThePass(t *testing.T) {
	w := startSafetyWire(t)
	namespaces := []string{w.ns, freshNamespace(t, w)}
	for _, ns := range namespaces {
		overlapNamespace(t, w, ns, markerRobotZone)
	}
	first, second := zoneStop(namespaces[0], markerRobotZone), zoneStop(namespaces[1], markerRobotZone)
	est := newTrailingEstopper(w.as(t, controllerUser), first)
	runStopControllers(t, w, est, 1) // one zone pass at a time: the queue wait is forced

	t1 := setStop(t, w, first, "first-stop")
	if _, ok := est.waitSend(first, t1, 15*time.Second); !ok {
		t.Fatal("the first stop was never sent")
	}
	time.Sleep(time.Second)
	t2 := setStop(t, w, second, "second-stop")
	s2, ok := est.waitSend(second, t2, overlapTrail+15*time.Second)
	if !ok {
		t.Fatal("the second stop was never sent")
	}
	waitProcessed(t, w, second, "second-stop", 30*time.Second)
	wait := s2.Sub(t2)

	h, err := metrics.EstopFanoutDurationSeconds.GetMetricWithLabelValues(namespaces[1], string(metrics.ScopeZone))
	if err != nil {
		t.Fatal(err)
	}
	var m dto.Metric
	if err := h.(prometheus.Metric).Write(&m); err != nil {
		t.Fatal(err)
	}
	count, sum := m.GetHistogram().GetSampleCount(), m.GetHistogram().GetSampleSum()
	t.Logf("MEASURED second stop: queue wait %v; observed fan-out %.3fs over %d episode(s)",
		wait.Round(time.Millisecond), sum, count)
	if count != 1 {
		t.Fatalf("observed %d episodes for the second zone, want exactly 1", count)
	}
	if got := time.Duration(sum * float64(time.Second)); got < wait-200*time.Millisecond {
		t.Errorf("the observed fan-out %v does not include the %v the stop waited before its pass: the metric "+
			"must run from the operator's trigger", got.Round(time.Millisecond), wait.Round(time.Millisecond))
	}
}
