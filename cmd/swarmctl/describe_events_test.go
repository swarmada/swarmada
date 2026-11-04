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

package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/cli"
)

// The Events block of `swarmctl describe` is read from the cluster. It used to be
// the literal "Events:  <none>", which told an operator a Warning they could see
// with kubectl did not exist. These tests pin the three answers the block may give:
// the Events that are really there, "<none>" when the list is really empty, and
// "<unavailable: …>" when it could not be read — never "<none>" for the last.

const evNS = "warehouse-a"

func describedRobot() *fleetv1.Robot {
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: "sim-robot-004", Namespace: evNS, UID: "uid-robot-004"},
		Spec:       fleetv1.RobotSpec{Zone: evNS, Adapter: fleetv1.AdapterRef{Name: "sim-adapter", Version: "1.0"}},
		Status:     fleetv1.RobotStatus{Phase: fleetv1.RobotPhaseIdle},
	}
}

// event builds a core/v1 Event about kind/name/uid, last seen ago before now.
func event(name, kind, objName, uid, typ, reason, msg string, ago time.Duration) *corev1.Event {
	at := metav1.NewTime(time.Now().Add(-ago))
	return &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: evNS},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: fleetv1.GroupVersion.String(),
			Kind:       kind, Namespace: evNS, Name: objName, UID: types.UID(uid),
		},
		Type: typ, Reason: reason, Message: msg,
		Source:         corev1.EventSource{Component: "fleetadapter-controller"},
		FirstTimestamp: at, LastTimestamp: at, Count: 1,
	}
}

// eventClient is a fake client carrying objs, with the field indexes an API server
// provides for Events so a field-selected List behaves as it would on a cluster.
func eventClient(t *testing.T, funcs *interceptor.Funcs, objs ...client.Object) client.Client {
	t.Helper()
	b := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...)
	for field, get := range map[string]func(*corev1.Event) string{
		"involvedObject.kind": func(e *corev1.Event) string { return e.InvolvedObject.Kind },
		"involvedObject.name": func(e *corev1.Event) string { return e.InvolvedObject.Name },
		"involvedObject.uid":  func(e *corev1.Event) string { return string(e.InvolvedObject.UID) },
	} {
		get := get
		b = b.WithIndex(&corev1.Event{}, field, func(o client.Object) []string { return []string{get(o.(*corev1.Event))} })
	}
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	return b.Build()
}

func describeRobotOutput(t *testing.T, c client.Client) string {
	t.Helper()
	var out bytes.Buffer
	o := newTestOptions(&out, cli.OutputTable)
	r := &fleetv1.Robot{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: evNS, Name: "sim-robot-004"}, r); err != nil {
		t.Fatal(err)
	}
	o.describeRobot(context.Background(), c, r)
	return out.String()
}

// eventsBlock is everything from the "Events:" line on.
func eventsBlock(t *testing.T, out string) string {
	t.Helper()
	i := strings.Index(out, "Events:")
	if i < 0 {
		t.Fatalf("no Events block:\n%s", out)
	}
	return out[i:]
}

func TestDescribeEventsNoneOnlyWhenTheListIsReallyEmpty(t *testing.T) {
	got := eventsBlock(t, describeRobotOutput(t, eventClient(t, nil, describedRobot())))
	if strings.TrimSpace(got) != "Events:  <none>" {
		t.Errorf("an empty Events list must render as <none>, got:\n%s", got)
	}
}

func TestDescribeEventsShowsTheRobotsWarning(t *testing.T) {
	c := eventClient(t, nil, describedRobot(),
		event("e1", "Robot", "sim-robot-004", "uid-robot-004", corev1.EventTypeWarning, "AdapterOffline", "adapter stopped answering heartbeats", 3*time.Minute))
	got := eventsBlock(t, describeRobotOutput(t, c))
	if strings.Contains(got, "<none>") {
		t.Fatalf("a Robot with a Warning Event must not print <none>:\n%s", got)
	}
	for _, want := range []string{"Type", "Reason", "Age", "From", "Message",
		"Warning", "AdapterOffline", "3m", "fleetadapter-controller", "adapter stopped answering heartbeats"} {
		if !strings.Contains(got, want) {
			t.Errorf("Events block missing %q:\n%s", want, got)
		}
	}
}

func TestDescribeEventsSeveralNewestLastAndOnlyThisObject(t *testing.T) {
	c := eventClient(t, nil, describedRobot(),
		event("e-new", "Robot", "sim-robot-004", "uid-robot-004", corev1.EventTypeNormal, "Recovered", "heartbeat resumed", 1*time.Minute),
		event("e-old", "Robot", "sim-robot-004", "uid-robot-004", corev1.EventTypeWarning, "AdapterOffline", "adapter lost", 10*time.Minute),
		event("e-mid", "Robot", "sim-robot-004", "uid-robot-004", corev1.EventTypeNormal, "Reconnecting", "retrying", 5*time.Minute),
		// Same name, another kind: a FleetAction that happens to share the Robot's name.
		event("e-other-kind", "FleetAction", "sim-robot-004", "uid-action", corev1.EventTypeWarning, "OtherKindReason", "not this object", 2*time.Minute),
		// Same kind and name, earlier incarnation (different UID): not this object either.
		event("e-old-uid", "Robot", "sim-robot-004", "uid-deleted-robot", corev1.EventTypeWarning, "PreviousIncarnation", "not this object", 2*time.Minute),
		// Another robot altogether.
		event("e-neighbour", "Robot", "sim-robot-005", "uid-robot-005", corev1.EventTypeWarning, "NeighbourReason", "not this object", 2*time.Minute),
	)
	got := eventsBlock(t, describeRobotOutput(t, c))
	for _, forbidden := range []string{"OtherKindReason", "PreviousIncarnation", "NeighbourReason", "not this object"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("Events block shows another object's Event (%q):\n%s", forbidden, got)
		}
	}
	old, mid, nw := strings.Index(got, "AdapterOffline"), strings.Index(got, "Reconnecting"), strings.Index(got, "Recovered")
	if old < 0 || mid < 0 || nw < 0 {
		t.Fatalf("Events block is missing one of the object's three Events:\n%s", got)
	}
	if old >= mid || mid >= nw {
		t.Errorf("Events must be ordered oldest first, newest last:\n%s", got)
	}
}

func TestDescribeEventsForbiddenIsUnavailableNotNone(t *testing.T) {
	forbidden := &interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*corev1.EventList); ok {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "events"}, "", errors.New(`User "viewer" cannot list resource "events"`))
		}
		return c.List(ctx, list, opts...)
	}}
	got := eventsBlock(t, describeRobotOutput(t, eventClient(t, forbidden, describedRobot(),
		event("e1", "Robot", "sim-robot-004", "uid-robot-004", corev1.EventTypeWarning, "AdapterOffline", "adapter lost", time.Minute))))
	if strings.Contains(got, "<none>") {
		t.Fatalf("a list that could not be read must never render as <none>:\n%s", got)
	}
	if !strings.Contains(got, "Events:  <unavailable: ") || !strings.Contains(got, "forbidden") {
		t.Errorf("a forbidden Events list must say it is unavailable and why:\n%s", got)
	}
}

func TestDescribeEventsFailedListIsUnavailableNotNone(t *testing.T) {
	failing := &interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*corev1.EventList); ok {
			return errors.New("connection refused")
		}
		return c.List(ctx, list, opts...)
	}}
	got := eventsBlock(t, describeRobotOutput(t, eventClient(t, failing, describedRobot())))
	if strings.Contains(got, "<none>") || !strings.Contains(got, "<unavailable: ") || !strings.Contains(got, "connection refused") {
		t.Errorf("a failed Events list must say it is unavailable and why:\n%s", got)
	}
}

// The generic path (every kind but Robot) reads Events the same way.
func TestDescribeGenericShowsItsOwnEvents(t *testing.T) {
	action := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "sim-robot-004", Namespace: evNS, UID: "uid-action"},
		Spec:       fleetv1.FleetActionSpec{Type: "pick", Zone: evNS, Priority: "Normal"},
	}
	c := eventClient(t, nil, action, describedRobot(),
		event("e-action", "FleetAction", "sim-robot-004", "uid-action", corev1.EventTypeWarning, "NoEligibleRobot", "no robot in zone", time.Minute),
		event("e-robot", "Robot", "sim-robot-004", "uid-robot-004", corev1.EventTypeWarning, "AdapterOffline", "robot event", time.Minute),
	)
	def, _ := resolveResource("fleetaction")
	var out bytes.Buffer
	o := newTestOptions(&out, cli.OutputTable)
	o.describeGeneric(context.Background(), c, def, action)
	got := eventsBlock(t, out.String())
	if !strings.Contains(got, "NoEligibleRobot") {
		t.Errorf("describe fleetaction must show its own Event:\n%s", got)
	}
	if strings.Contains(got, "AdapterOffline") {
		t.Errorf("describe fleetaction shows the same-named Robot's Event:\n%s", got)
	}
}
