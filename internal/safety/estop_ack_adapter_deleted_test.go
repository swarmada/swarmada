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

package safety

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// The admission gate MUST NOT block stopping (RFC-0001 #fleetadapter-admission-gating), and the
// FleetAdapter's existence is one of its conditions. An operator who deletes adapter-a while
// amr-1 still names it still gets a confirmed stop from amr-1's own adapter: the ack was once
// dropped as a failed adapter read, and the stop timed out unconfirmed. This runs the real
// SafetyStream, streamauth.Authorizer and Dispatcher (newAckFixture).
func TestEstopAck_AdapterDeletedStillConfirms(t *testing.T) {
	f := newAckFixture(t)
	gone := &fleetv1.FleetAdapter{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "adapter-a"}}
	if err := f.c.Delete(context.Background(), gone); err != nil {
		t.Fatalf("delete adapter-a: %v", err)
	}

	res := f.triggerAndAck(t, "adapter-a", "amr-1")
	if !res.Confirmed || res.State != fleetv1.RobotEstopStopped {
		t.Fatalf("result = %+v, want a confirmed stop from amr-1's own adapter after its FleetAdapter was deleted", res)
	}
	if got := f.robotEstopState(t, "amr-1"); got != fleetv1.RobotEstopStopped {
		t.Errorf("amr-1 estopState = %s, want Stopped", got)
	}
}
