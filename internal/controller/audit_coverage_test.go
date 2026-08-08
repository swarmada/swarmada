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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
)

// §9.6.5.1 audit coverage.
//
// These tests exist because of a specific failure: the event-name constants for
// ROBOT_ADMITTED, ROBOT_REJECTED, ESTOP_CLEAR_REJECTED and SWARMADA_CONFIG_MODIFIED
// were declared in internal/audit for months with NO production writer, and a review
// that checked "is the constant defined?" reported them as emitted. A declared
// constant is not an audit entry. So the assertions below are deliberately made
// against the recorded chain, never against the existence of a symbol.

// auditSpy captures entries so a test can assert on what was actually written.
type auditSpy struct{ entries []audit.Entry }

func (s *auditSpy) Record(e audit.Entry) (audit.Entry, error) {
	s.entries = append(s.entries, e)
	return e, nil
}

func (s *auditSpy) typesRecorded() []string {
	out := make([]string, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e.EventType)
	}
	return out
}

func (s *auditSpy) count(eventType string) int {
	n := 0
	for _, e := range s.entries {
		if e.EventType == eventType {
			n++
		}
	}
	return n
}

func robotAuditScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sc := runtime.NewScheme()
	if err := fleetv1.AddToScheme(sc); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return sc
}

// A Robot's first reconcile writes ROBOT_ADMITTED — the admission gate (§6.6) is a
// security control, so its outcome belongs in the tamper-evident chain.
func TestAudit_RobotAdmittedOnFirstReconcile(t *testing.T) {
	sc := robotAuditScheme(t)
	robot := &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: "amr-01", Namespace: "warehouse-a"},
		Spec:       fleetv1.RobotSpec{Zone: "zone-a", RobotClass: "picker-v2"},
	}
	c := fake.NewClientBuilder().WithScheme(sc).WithObjects(robot).
		WithStatusSubresource(&fleetv1.Robot{}).Build()
	spy := &auditSpy{}
	r := &RobotReconciler{Client: c, Scheme: sc, Audit: spy}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "amr-01", Namespace: "warehouse-a"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if spy.count(audit.EventRobotAdmitted) != 1 {
		t.Fatalf("ROBOT_ADMITTED written %d times, want 1; recorded=%v",
			spy.count(audit.EventRobotAdmitted), spy.typesRecorded())
	}
	e := spy.entries[0]
	if e.Resource.Name != "amr-01" || e.Namespace != "warehouse-a" {
		t.Errorf("entry does not identify the robot: %+v", e.Resource)
	}
	if e.Detail["zone"] != "zone-a" || e.Detail["robot_class"] != "picker-v2" {
		t.Errorf("detail lost the admission context: %v", e.Detail)
	}
	if e.Detail["admission_path"] != "operator" {
		t.Errorf("admission_path = %q, want operator for a non-auto-admitted robot",
			e.Detail["admission_path"])
	}
}

// THE IDEMPOTENCE PROPERTY. Reconcile is called repeatedly for reasons that have
// nothing to do with admission; if every pass wrote an entry the chain would fill
// with duplicates and the real admission record would be unfindable.
func TestAudit_RobotAdmittedWrittenExactlyOnce(t *testing.T) {
	sc := robotAuditScheme(t)
	robot := &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: "amr-01", Namespace: "warehouse-a"},
		Spec:       fleetv1.RobotSpec{Zone: "zone-a"},
	}
	c := fake.NewClientBuilder().WithScheme(sc).WithObjects(robot).
		WithStatusSubresource(&fleetv1.Robot{}).Build()
	spy := &auditSpy{}
	r := &RobotReconciler{Client: c, Scheme: sc, Audit: spy}

	for i := 0; i < 4; i++ {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "amr-01", Namespace: "warehouse-a"}}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	if got := spy.count(audit.EventRobotAdmitted); got != 1 {
		t.Errorf("ROBOT_ADMITTED written %d times across 4 reconciles, want exactly 1 — "+
			"the unset-phase marker is what makes this once-only", got)
	}
}

// The auto-admit path is distinguishable in the chain: an auditor needs to know
// whether a robot passed an operator review or the zero-touch gate (ADR-0014).
func TestAudit_RobotAdmittedRecordsAutoAdmitPath(t *testing.T) {
	sc := robotAuditScheme(t)
	robot := &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{
			Name: "amr-02", Namespace: "warehouse-a",
			Annotations: map[string]string{fleetv1.AutoAdmittedAnnotation: "true"},
		},
		Spec: fleetv1.RobotSpec{Zone: "zone-a"},
	}
	c := fake.NewClientBuilder().WithScheme(sc).WithObjects(robot).
		WithStatusSubresource(&fleetv1.Robot{}).Build()
	spy := &auditSpy{}
	r := &RobotReconciler{Client: c, Scheme: sc, Audit: spy}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "amr-02", Namespace: "warehouse-a"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(spy.entries) == 0 {
		t.Fatal("no audit entry written for an auto-admitted robot")
	}
	if got := spy.entries[0].Detail["admission_path"]; got != "auto-admit" {
		t.Errorf("admission_path = %q, want auto-admit", got)
	}
}

// A nil Audit must not break reconciliation — auditing is observability, not a gate.
func TestAudit_NilRecorderDoesNotBlockReconcile(t *testing.T) {
	sc := robotAuditScheme(t)
	robot := &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: "amr-03", Namespace: "warehouse-a"},
		Spec:       fleetv1.RobotSpec{Zone: "zone-a"},
	}
	c := fake.NewClientBuilder().WithScheme(sc).WithObjects(robot).
		WithStatusSubresource(&fleetv1.Robot{}).Build()
	r := &RobotReconciler{Client: c, Scheme: sc} // Audit deliberately nil

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "amr-03", Namespace: "warehouse-a"}}); err != nil {
		t.Fatalf("reconcile with nil Audit must succeed: %v", err)
	}
	var got fleetv1.Robot
	if err := c.Get(context.Background(),
		types.NamespacedName{Name: "amr-03", Namespace: "warehouse-a"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Phase != fleetv1.RobotPhaseDiscovered {
		t.Errorf("phase = %q, want Discovered — auditing must not gate the status write", got.Status.Phase)
	}
}
