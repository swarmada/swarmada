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

// The refusal of a write to status.estopHeldBy tells the writer every way a record is
// removed (RFC-0001 #safety-estop-scopes): a record names its carrier by kind and name, so
// the clear of a carrier created again under that name removes it too, and a record whose
// carrier is gone is removed by a robot-scope stop and clear. A message that said only the
// stop's own clear removes it would send an operator after a clear that can no longer run.
func TestRobotStatusEstopGuard_HeldByRefusalNamesEveryWayARecordIsRemoved(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	g := &RobotStatusEstopGuard{decoder: admission.NewDecoder(scheme)}

	robot := func(held []fleetv1.EstopHold) []byte {
		r := &fleetv1.Robot{
			TypeMeta:   metav1.TypeMeta{APIVersion: fleetv1.GroupVersion.String(), Kind: "Robot"},
			ObjectMeta: metav1.ObjectMeta{Name: "amr-1", Namespace: "warehouse-a"},
			Status:     fleetv1.RobotStatus{EstopState: fleetv1.RobotEstopStopped, EstopHeldBy: held},
		}
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	stored := robot([]fleetv1.EstopHold{{Kind: fleetv1.EstopHoldFleetZone, Name: "bay-4", UID: "u1"}})
	written := robot(nil)

	resp := g.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation:   admissionv1.Update,
		Name:        "amr-1",
		Namespace:   "warehouse-a",
		SubResource: "status",
		UserInfo:    authenticationv1.UserInfo{Username: "alice"},
		OldObject:   runtime.RawExtension{Raw: stored},
		Object:      runtime.RawExtension{Raw: written},
	}})
	if resp.Allowed {
		t.Fatal("removing a status.estopHeldBy record by hand must be refused")
	}
	msg := resp.Result.Message
	for _, want := range []string{
		"may not change status.estopHeldBy",
		"the clear of a stop on a carrier of the same kind and name",
		"the stop of a carrier created again under that name",
		"robot-scope stop and clear",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal does not say %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "only that stop's clear") {
		t.Errorf("refusal still says only the stop's own clear removes the record:\n%s", msg)
	}
}
