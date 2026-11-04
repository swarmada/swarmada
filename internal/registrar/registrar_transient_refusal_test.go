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

package registrar

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// A refusal with no rejection code tells a conforming adapter that the robot is not its own, and
// it never announces the robot again (RFC-0001 #fleet-adapter-protocol-connection-lifecycle). A
// register whose Robot lookup fails to read is not that: it is refused NOT_ADMITTED, which the
// adapter retries through the provisioning loop.
func TestRegister_TransientLookupFailureIsRefusedWithACode(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	robot := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Namespace: regNS, Name: regRobotID}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(robot).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*fleetv1.RobotList); ok {
					return errors.New("etcdserver: request timed out")
				}
				return cl.List(ctx, list, opts...)
			},
		}).Build()
	r := &Registrar{Client: c, Now: func() time.Time { return time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC) }}

	ack := r.Register(context.Background(), adapterID(), &fav1.RegisterRobot{RobotId: regRobotID})
	if ack.GetAccepted() {
		t.Fatalf("a register whose lookup failed was accepted: %+v", ack)
	}
	if ack.GetRejection() != fav1.RegistrationRejection_REGISTRATION_REJECTION_NOT_ADMITTED {
		t.Errorf("rejection = %v, want NOT_ADMITTED: a code-less refusal tells the adapter the robot is not its own; message %q",
			ack.GetRejection(), ack.GetMessage())
	}
}

// The control: an admitted robot registers, and a robot_id that names two Robots is still refused
// with no code (the lookup read the Robots; they do not say which is meant).
func TestRegister_LookupThatReadsKeepsItsAnswer(t *testing.T) {
	admitted := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Namespace: regNS, Name: regRobotID}}
	r, _ := newRegistrar(t, admitted)
	if ack := r.Register(context.Background(), adapterID(), &fav1.RegisterRobot{RobotId: regRobotID}); !ack.GetAccepted() {
		t.Fatalf("control: the admitted robot was refused: %+v", ack)
	}

	twin := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Namespace: regNS, Name: "other",
		Annotations: map[string]string{fleetv1.RobotIDAnnotation: regRobotID}}}
	plain := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Namespace: regNS, Name: regRobotID}}
	r, _ = newRegistrar(t, twin, plain)
	ack := r.Register(context.Background(), adapterID(), &fav1.RegisterRobot{RobotId: regRobotID})
	if ack.GetAccepted() || ack.GetRejection() != fav1.RegistrationRejection_REGISTRATION_REJECTION_UNSPECIFIED {
		t.Errorf("an ambiguous robot_id: accepted=%v rejection=%v, want a refusal with no code", ack.GetAccepted(), ack.GetRejection())
	}
}
