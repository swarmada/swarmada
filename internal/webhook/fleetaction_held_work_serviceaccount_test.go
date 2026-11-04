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
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/estopscope"
)

// Deleting work a stop holds, or removing its stop finalizer, moves it, and only a holder of
// estop-clear on the stop decides that. A ServiceAccount never qualifies, whatever RBAC grants it,
// except the garbage collector carrying out a delete that cascades from a FleetTask, which the
// finalizer holds (RFC-0001 #safety-fleetaction-behaviour-during-estop, Rules for every A row).
//
// The authorizer here grants estop-clear to every identity, so each refusal below is the
// identity's and not RBAC's.

// ctxAsPrincipal injects an admission request from username in groups.
func ctxAsPrincipal(username string, groups ...string) context.Context {
	return admission.NewContextWithRequest(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UserInfo: authenticationv1.UserInfo{Username: username, Groups: groups},
		},
	})
}

// The identities the rows below act as.
var (
	heldWorkOperator = ctxAsPrincipal("alice", "system:authenticated")
	heldWorkSA       = ctxAsPrincipal("system:serviceaccount:"+zoneNS+":dispatcher",
		serviceAccountsGroup, serviceAccountsGroup+":"+zoneNS, "system:authenticated")
	heldWorkSAByGroup = ctxAsPrincipal("dispatcher", serviceAccountsGroup, "system:authenticated")
	heldWorkSAByName  = ctxAsPrincipal("system:serviceaccount:" + zoneNS + ":dispatcher")
	heldWorkGC        = ctxAsPrincipal(garbageCollectorUser,
		serviceAccountsGroup, serviceAccountsGroup+":kube-system", "system:authenticated")
	heldWorkGCAsKCM = ctxAsPrincipal(kubeControllerManagerUser, "system:authenticated")
	heldWorkManager = ctxAsPrincipal(controllerUser,
		serviceAccountsGroup, serviceAccountsGroup+":swarmada-system", "system:authenticated")
)

// heldReader is the namespace of ownedAction: robot r1 records the action, under its own stop when
// held. terminating marks the namespace as being deleted.
func heldReader(t *testing.T, held, terminating bool) client.Reader {
	t.Helper()
	r1 := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Namespace: zoneNS, Name: "r1"},
		Status: fleetv1.RobotStatus{AssignedAction: ownedAction().Name}}
	if held {
		r1.Annotations = map[string]string{estopscope.TriggerAnnotation: "2026-10-22T00:00:00Z"}
	}
	return zoneReader(t, terminating, r1)
}

func TestFleetActionDelete_AServiceAccountNeverDeletesHeldWork(t *testing.T) {
	yes := true
	child := func() *fleetv1.FleetAction { return ownedAction(taskRef("dock", "1", true)) }
	plain := func() *fleetv1.FleetAction { return ownedAction() }
	foreign := func() *fleetv1.FleetAction {
		return ownedAction(metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: "cm", UID: "uid-cm", Controller: &yes})
	}
	notController := func() *fleetv1.FleetAction { return ownedAction(taskRef("dock", "1", false)) }
	rows := []struct {
		name        string
		ctx         context.Context
		action      func() *fleetv1.FleetAction
		held        bool
		terminating bool
		wantRefused bool
		noGrant     bool
	}{
		// The controls: the work is held, and the authorizer decides for a person.
		{"operator-without-estop-clear", heldWorkOperator, plain, true, false, true, true},
		{"operator-with-estop-clear", heldWorkOperator, plain, true, false, false, false},
		{"serviceaccount", heldWorkSA, plain, true, false, true, false},
		{"serviceaccount-by-group", heldWorkSAByGroup, plain, true, false, true, false},
		{"serviceaccount-by-name", heldWorkSAByName, plain, true, false, true, false},
		{"serviceaccount-deleting-a-task-member", heldWorkSA, child, true, false, true, false},
		// Only held work: no stop holds the robot, or the namespace is being torn down.
		{"serviceaccount-unheld", heldWorkSA, plain, false, false, false, false},
		{"serviceaccount-namespace-terminating", heldWorkSA, plain, true, true, false, false},
		// The garbage collector's cascade from a FleetTask, under each username it runs as.
		{"garbage-collector-task-member", heldWorkGC, child, true, false, false, false},
		{"garbage-collector-as-kcm-task-member", heldWorkGCAsKCM, child, true, false, false, false},
		// Any other delete by the garbage collector is not that cascade.
		{"garbage-collector-no-owner", heldWorkGC, plain, true, false, true, false},
		{"garbage-collector-as-kcm-no-owner", heldWorkGCAsKCM, plain, true, false, true, false},
		{"garbage-collector-other-controller", heldWorkGC, foreign, true, false, true, false},
		{"garbage-collector-task-not-controller", heldWorkGC, notController, true, false, true, false},
		{"garbage-collector-unheld", heldWorkGC, plain, false, false, false, false},
		// Disclosed (safety.md, Rules for every A row): the delete check admits the control
		// plane's own ServiceAccount, whose retention deletes a loop's settled children.
		{"manager", heldWorkManager, plain, true, false, false, false},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			authz := &fakeEstopAuthz{allow: !r.noGrant}
			g := &FleetActionDeleteGuard{APIReader: heldReader(t, r.held, r.terminating), EstopAuthz: authz}
			_, err := g.ValidateDelete(r.ctx, r.action())
			t.Logf("RESULT delete (held=%v terminating=%v): %v", r.held, r.terminating, err)
			switch {
			case r.wantRefused && (err == nil || !apierrors.IsForbidden(err)):
				t.Errorf("want Forbidden, got %v", err)
			case !r.wantRefused && err != nil:
				t.Errorf("want admitted, got %v", err)
			}
		})
	}
}

func TestFleetActionStopFinalizer_AServiceAccountNeverRemovesItFromHeldWork(t *testing.T) {
	rows := []struct {
		name        string
		ctx         context.Context
		held        bool
		terminating bool
		wantRefused bool
		noGrant     bool
	}{
		// The controls: the work is held, and the authorizer decides for a person.
		{"operator-without-estop-clear", heldWorkOperator, true, false, true, true},
		{"operator-with-estop-clear", heldWorkOperator, true, false, false, false},
		{"serviceaccount", heldWorkSA, true, false, true, false},
		{"serviceaccount-by-group", heldWorkSAByGroup, true, false, true, false},
		{"serviceaccount-by-name", heldWorkSAByName, true, false, true, false},
		// The garbage collector's exception is a delete that cascades from a FleetTask, not this.
		{"garbage-collector", heldWorkGC, true, false, true, false},
		{"garbage-collector-as-kcm", heldWorkGCAsKCM, true, false, true, false},
		// The control plane releases the finalizer when the hold ends, not while it stands.
		{"manager", heldWorkManager, true, false, true, false},
		{"manager-unheld", heldWorkManager, false, false, false, false},
		{"manager-namespace-terminating", heldWorkManager, true, true, false, false},
		{"serviceaccount-unheld", heldWorkSA, false, false, false, false},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			rd := heldReader(t, r.held, r.terminating)
			v := &FleetActionValidator{CancelAuthz: &fakeEstopAuthz{allow: !r.noGrant}, Reader: rd, APIReader: rd}
			oldAction := ownedAction()
			controllerutil.AddFinalizer(oldAction, estopscope.StopFinalizer)
			_, err := v.ValidateUpdate(r.ctx, oldAction, ownedAction())
			t.Logf("RESULT finalizer removal (held=%v terminating=%v): %v", r.held, r.terminating, err)
			switch {
			case r.wantRefused && (err == nil || !apierrors.IsForbidden(err)):
				t.Errorf("want Forbidden, got %v", err)
			case !r.wantRefused && err != nil:
				t.Errorf("want admitted, got %v", err)
			}
		})
	}
}
