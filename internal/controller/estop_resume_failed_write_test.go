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
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/safety"
	"github.com/swarmada/swarmada/internal/telemetry"
)

// flakyStatusSink fails every status write while fail is set.
type flakyStatusSink struct {
	inner telemetry.StatusSink
	fail  bool
}

func (s *flakyStatusSink) ApplyMaterialUpdate(ctx context.Context, u telemetry.MaterialUpdate) (bool, error) {
	if s.fail {
		return false, errors.New("apiserver unavailable")
	}
	return s.inner.ApplyMaterialUpdate(ctx, u)
}

// A post-clear frame reports the lidar Failed, and its status write fails. An operator
// who confirms before the robot's next frame is refused: the stored status still reads
// the lidar Healthy, and releasing on it would release on a report the robot has
// already contradicted (RFC-0001 #safety-estop-state-machine). The real router, status
// sink and dispatcher; only the API write is made to fail.
func TestEstopResume_RefusedAfterAFailedWriteOfAFailedComponent(t *testing.T) {
	scheme := livenessScheme(t)
	robot := &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: "amr-1", Namespace: "acme", Annotations: map[string]string{RobotIDAnnotation: "amr-1"}},
		Spec:       fleetv1.RobotSpec{Capabilities: []fleetv1.ClassCapability{{Name: "navigation", Type: fleetv1.CapabilityKindHardwareNative, RequiredHardware: []string{"lidar"}}}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(robot).WithStatusSubresource(&fleetv1.Robot{}).Build()
	ctx := context.Background()
	_ = c.Get(ctx, client.ObjectKeyFromObject(robot), robot)
	robot.Status.Phase = fleetv1.RobotPhaseIdle
	robot.Status.EstopState = fleetv1.RobotEstopStopped
	robot.Status.Hardware = []fleetv1.HardwareComponentStatus{{Name: "lidar", Status: fleetv1.HardwareHealthy}}
	robot.Status.Capabilities = []fleetv1.CapabilityStatusEntry{{Name: "navigation", Status: fleetv1.CapabilityStatusActive}}
	if err := c.Status().Update(ctx, robot); err != nil {
		t.Fatal(err)
	}
	sink := &flakyStatusSink{inner: &RobotStatusSink{Client: c}}
	frames := telemetry.NewRouter(func(string) telemetry.PlaneConfig { return telemetry.PlaneConfig{} }, sink)
	healthy := map[string]fleetv1.HardwareStatus{"lidar": fleetv1.HardwareHealthy}
	_ = frames.Ingest(ctx, telemetry.Frame{RobotID: "amr-1", Namespace: "acme", Timestamp: time.Now(), Phase: fleetv1.RobotPhaseIdle, Hardware: healthy})
	d := safety.New(c, nil)
	d.Telemetry = frames
	d.DeriveCapabilities = DeriveCapabilities
	if st, err := d.ClearEstop(ctx, "acme", "amr-1", "alice", safety.Ends{}); err != nil || st != fleetv1.RobotEstopResuming {
		t.Fatalf("clear = %s %v", st, err)
	}
	time.Sleep(2 * time.Millisecond)
	sink.fail = true
	err := frames.Ingest(ctx, telemetry.Frame{RobotID: "amr-1", Namespace: "acme", Timestamp: time.Now(), Phase: fleetv1.RobotPhaseIdle,
		Hardware: map[string]fleetv1.HardwareStatus{"lidar": fleetv1.HardwareFailed}})
	sink.fail = false
	st, why, cerr := d.ConfirmResume(ctx, "acme", "amr-1", "operator")
	if err == nil {
		t.Fatal("the status write was made to fail, and the ingest reported no error")
	}
	if cerr != nil || st != fleetv1.RobotEstopResuming {
		t.Fatalf("ConfirmResume = %s (%q, err %v), want Resuming: the robot's own post-clear frame reported the "+
			"lidar Failed, and its status write failed", st, why, cerr)
	}
}
