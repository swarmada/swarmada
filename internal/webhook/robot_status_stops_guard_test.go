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
	"encoding/json"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// status.stops is the control plane's record of the stops it sent (RFC-0001
// #safety-robot-stop-table, the term recorded stop): a robots/status write that changes it is
// refused to every identity but the controller's ServiceAccount.
func TestRobotStatusEstopGuard_StopsAreTheControlPlanes(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	g := &RobotStatusEstopGuard{decoder: admission.NewDecoder(scheme)}
	const sa = "system:serviceaccount:swarmada-system:swarmada-controller-manager"
	prev := controllerPrincipal.Load()
	t.Cleanup(func() { controllerPrincipal.Store(prev) })
	if err := SetControllerPrincipal(sa); err != nil {
		t.Fatal(err)
	}

	robot := func(stops []fleetv1.RobotStop) []byte {
		r := &fleetv1.Robot{
			TypeMeta:   metav1.TypeMeta{APIVersion: fleetv1.GroupVersion.String(), Kind: "Robot"},
			ObjectMeta: metav1.ObjectMeta{Name: "amr-1", Namespace: "warehouse-a"},
			Status:     fleetv1.RobotStatus{EstopState: fleetv1.RobotEstopStopped, Stops: stops},
		}
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	entry := fleetv1.RobotStop{ID: "6f1c2b1e-9a43-4f3e-8d55-0f3f1c2d9e10", Scope: fleetv1.StopScopeRobot,
		Carrier: &fleetv1.StopCarrier{Kind: fleetv1.StopCarrierRobot, Name: "amr-1"}, SentAt: metav1.Now()}
	cleared := entry
	now := metav1.Now()
	cleared.ClearedAt = &now
	for _, tc := range []struct {
		name       string
		user       string
		old, new   []fleetv1.RobotStop
		wantAllow  bool
		wantPhrase string
	}{
		{"operator-removes-an-entry", "alice", []fleetv1.RobotStop{entry}, nil, false, "may not change status.stops"},
		{"operator-adds-a-clearedAt", "alice", []fleetv1.RobotStop{entry}, []fleetv1.RobotStop{cleared}, false, "may not change status.stops"},
		{"operator-adds-an-entry", "alice", nil, []fleetv1.RobotStop{entry}, false, "may not change status.stops"},
		{"controller-records-a-stop", sa, nil, []fleetv1.RobotStop{entry}, true, ""},
		{"operator-leaves-it-alone", "alice", []fleetv1.RobotStop{entry}, []fleetv1.RobotStop{entry}, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := g.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
				Operation: admissionv1.Update, Name: "amr-1", Namespace: "warehouse-a", SubResource: "status",
				UserInfo:  authenticationv1.UserInfo{Username: tc.user},
				OldObject: runtime.RawExtension{Raw: robot(tc.old)}, Object: runtime.RawExtension{Raw: robot(tc.new)},
			}})
			if resp.Allowed != tc.wantAllow {
				t.Fatalf("allowed = %v, want %v (%v)", resp.Allowed, tc.wantAllow, resp.Result)
			}
			if !tc.wantAllow && !strings.Contains(resp.Result.Message, tc.wantPhrase) {
				t.Errorf("refusal does not say %q: %s", tc.wantPhrase, resp.Result.Message)
			}
		})
	}
}
