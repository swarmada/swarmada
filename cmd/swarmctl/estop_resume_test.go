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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/cli"
)

func resumingRobotObj(ns, name string, state fleetv1.RobotEstopState) *fleetv1.Robot {
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status:     fleetv1.RobotStatus{EstopState: state},
	}
}

// answerResume stands in for the control plane: it waits for the robot's release
// request, then records it handled and, when release is true, writes Normal.
func answerResume(t *testing.T, c client.Client, ns, name string, release bool) {
	t.Helper()
	go func() {
		ctx := context.Background()
		for i := 0; i < 200; i++ {
			robot := &fleetv1.Robot{}
			if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, robot); err == nil {
				if req, ok := robot.Annotations[fleetv1.EstopResumeAnnotation]; ok {
					if release {
						robot.Status.EstopState = fleetv1.RobotEstopNormal
						_ = c.Status().Update(ctx, robot)
						_ = c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, robot)
					}
					base := robot.DeepCopy()
					robot.Annotations[fleetv1.EstopResumeProcessedAnnotation] = req
					_ = c.Patch(ctx, robot, client.MergeFrom(base))
					return
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
}

func withFastResumePoll(t *testing.T) {
	t.Helper()
	old := resumePollInterval
	resumePollInterval = 5 * time.Millisecond
	t.Cleanup(func() { resumePollInterval = old })
}

func TestEstopResume_ReleasedRobotIsReported(t *testing.T) {
	withFastResumePoll(t)
	ns := "warehouse-a"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&fleetv1.Robot{}).
		WithObjects(resumingRobotObj(ns, "amr-1", fleetv1.RobotEstopResuming)).Build()
	out := &bytes.Buffer{}
	o := newTestOptions(out, cli.OutputTable)
	answerResume(t, c, ns, "amr-1", true)

	if err := o.estopResume(context.Background(), c, authorizer(true), ns, []string{"amr-1"}, "inspected", true, 5*time.Second); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !strings.Contains(out.String(), "robot.swarmada.io/amr-1 released: estopState Normal") {
		t.Fatalf("output %q does not report the release", out.String())
	}
	got := &fleetv1.Robot{}
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "amr-1"}, got)
	if req := got.Annotations[fleetv1.EstopResumeAnnotation]; !strings.HasSuffix(req, " inspected") {
		t.Fatalf("request = %q, want a unique value ending in the reason", req)
	}
}

// A refusal is reported with the control plane's reason, and the command fails.
func TestEstopResume_RefusalIsReportedWithTheReason(t *testing.T) {
	withFastResumePoll(t)
	ns := "warehouse-a"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&fleetv1.Robot{}).
		WithObjects(resumingRobotObj(ns, "amr-1", fleetv1.RobotEstopResuming)).Build()
	cs := authorizer(true)
	if _, err := cs.CoreV1().Events(ns).Create(context.Background(), &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: ns, Name: "amr-1.refused"},
		InvolvedObject: corev1.ObjectReference{Kind: "Robot", Name: "amr-1", Namespace: ns},
		Reason:         "EstopResumeRefused",
		Message:        `resume refused; the robot stays Resuming: hardware component "lidar" is reported Failed`,
		LastTimestamp:  metav1.Now(),
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	o := newTestOptions(out, cli.OutputTable)
	answerResume(t, c, ns, "amr-1", false)

	err := o.estopResume(context.Background(), c, cs, ns, []string{"amr-1"}, "inspected", true, 5*time.Second)
	if err == nil {
		t.Fatal("a refused release must fail the command")
	}
	if !strings.Contains(out.String(), `"lidar" is reported Failed`) {
		t.Fatalf("output %q does not carry the refusal reason", out.String())
	}
}

// A robot that is not Resuming is refused before anything is written.
func TestEstopResume_NotResumingIsRefusedWithoutAWrite(t *testing.T) {
	ns := "warehouse-a"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&fleetv1.Robot{}).
		WithObjects(resumingRobotObj(ns, "amr-1", fleetv1.RobotEstopStopped)).Build()
	out := &bytes.Buffer{}
	o := newTestOptions(out, cli.OutputTable)
	if err := o.estopResume(context.Background(), c, authorizer(true), ns, []string{"amr-1"}, "inspected", true, time.Second); err == nil {
		t.Fatal("releasing a Stopped robot must fail")
	}
	if !strings.Contains(out.String(), "not in Resuming (estopState Stopped)") {
		t.Fatalf("output %q does not say why", out.String())
	}
	got := &fleetv1.Robot{}
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "amr-1"}, got)
	if _, written := got.Annotations[fleetv1.EstopResumeAnnotation]; written {
		t.Fatal("a request was written for a robot that is not Resuming")
	}
}

// Releasing is admin-only: a denied SelfSubjectAccessReview writes nothing.
func TestEstopResume_DeniedFailsClosed(t *testing.T) {
	ns := "warehouse-a"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&fleetv1.Robot{}).
		WithObjects(resumingRobotObj(ns, "amr-1", fleetv1.RobotEstopResuming)).Build()
	out := &bytes.Buffer{}
	o := newTestOptions(out, cli.OutputTable)
	if err := o.estopResume(context.Background(), c, authorizer(false), ns, []string{"amr-1"}, "inspected", true, time.Second); err == nil {
		t.Fatal("a denied release must fail")
	}
	if !strings.Contains(out.String(), "not authorized") {
		t.Fatalf("output %q does not report the denial", out.String())
	}
	got := &fleetv1.Robot{}
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "amr-1"}, got)
	if _, written := got.Annotations[fleetv1.EstopResumeAnnotation]; written {
		t.Fatal("a denied release wrote a request")
	}
}

// --reason is required.
func TestEstopResume_ReasonRequired(t *testing.T) {
	o := newTestOptions(&bytes.Buffer{}, cli.OutputTable)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	if err := o.estopResume(context.Background(), c, authorizer(true), "ns", []string{"amr-1"}, "  ", true, time.Second); err == nil {
		t.Fatal("an empty --reason must be refused")
	}
}
