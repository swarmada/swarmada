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

package command

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controlstream"
	"github.com/swarmada/swarmada/internal/probe"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

const (
	cmdNS      = "warehouse-a"
	cmdRobotID = "amr-07"
	cmdAdapter = "acme-adapter"
)

// fakeSender captures pushed Commands and lets a test drive the correlated
// reply. respond=nil means "never reply" (exercises the timeout path).
type fakeSender struct {
	mu       sync.Mutex
	sent     []*fav1.Command
	sendErr  error
	dispatch *Dispatcher // to feed the reply back through RouteResult
	respond  func(cmd *fav1.Command) *fav1.CommandResult
}

func (f *fakeSender) Send(msg *fav1.ControlPlaneMessage) error {
	f.mu.Lock()
	if f.sendErr != nil {
		f.mu.Unlock()
		return f.sendErr
	}
	cmd := msg.GetCommand()
	f.sent = append(f.sent, cmd)
	respond := f.respond
	f.mu.Unlock()
	if respond != nil {
		// Reply asynchronously, exactly as a real adapter would over its stream.
		go f.dispatch.RouteResult(respond(cmd))
	}
	return nil
}

func newDispatcher(t *testing.T, robotAdapter string) (*Dispatcher, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objs := []client.Object{}
	if robotAdapter != "" {
		objs = append(objs, &fleetv1.Robot{
			ObjectMeta: metav1.ObjectMeta{Namespace: cmdNS, Name: cmdRobotID},
			Spec:       fleetv1.RobotSpec{Adapter: fleetv1.AdapterRef{Name: robotAdapter}},
		})
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	d := New(c)
	return d, c
}

func verifiedIdentity() controlstream.TLSIdentity {
	return controlstream.TLSIdentity{Verified: true, Namespace: cmdNS, AdapterName: cmdAdapter}
}

func TestVerify_HealthyRoundTrip(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	sender := &fakeSender{dispatch: d, respond: func(cmd *fav1.Command) *fav1.CommandResult {
		return &fav1.CommandResult{
			CommandId: cmd.GetCommandId(),
			RobotId:   cmd.GetRobotId(),
			Result: &fav1.CommandResult_Verify{Verify: &fav1.VerifyResult{
				Status:        fav1.ProbeStatus_PROBE_STATUS_HEALTHY,
				ActualMetrics: map[string]float64{"range_m": 12.0},
				Message:       "ok",
			}},
		}
	}}
	d.RegisterStream(verifiedIdentity(), sender)

	res, err := d.Verify(context.Background(), cmdNS, cmdRobotID, probe.VerifyRequest{
		ProbeType: fleetv1.ProbeTypeHardware, Target: "front-lidar",
		Expected: map[string]float64{"range_m": 10.0}})
	if err != nil {
		t.Fatalf("Verify error: %v", err)
	}
	if res.Status != probe.StatusHealthy {
		t.Errorf("status = %q, want Healthy", res.Status)
	}
	if res.ActualMetrics["range_m"] != 12.0 {
		t.Errorf("actual metrics = %v", res.ActualMetrics)
	}
	// The right verify arm was pushed with the target/expected populated.
	if len(sender.sent) != 1 {
		t.Fatalf("sent %d commands, want 1", len(sender.sent))
	}
	vh := sender.sent[0].GetVerifyHardware()
	if vh == nil || vh.GetComponentName() != "front-lidar" || vh.GetExpectedMetrics()["range_m"] != 10.0 {
		t.Errorf("pushed verify_hardware = %+v", vh)
	}
	if sender.sent[0].GetRobotId() != cmdRobotID {
		t.Errorf("command robot_id = %q", sender.sent[0].GetRobotId())
	}
}

func TestVerify_ProbeTypeArms(t *testing.T) {
	cases := []struct {
		probeType fleetv1.ProbeType
		check     func(*fav1.Command) bool
	}{
		{fleetv1.ProbeTypeHardware, func(c *fav1.Command) bool { return c.GetVerifyHardware().GetComponentName() == "t" }},
		{fleetv1.ProbeTypeCapability, func(c *fav1.Command) bool { return c.GetVerifyCapability().GetCapabilityName() == "t" }},
		{fleetv1.ProbeTypeModel, func(c *fav1.Command) bool { return c.GetVerifyModel().GetModelName() == "t" }},
	}
	for _, tc := range cases {
		d, _ := newDispatcher(t, cmdAdapter)
		sender := &fakeSender{dispatch: d, respond: func(cmd *fav1.Command) *fav1.CommandResult {
			return &fav1.CommandResult{CommandId: cmd.GetCommandId(),
				Result: &fav1.CommandResult_Verify{Verify: &fav1.VerifyResult{Status: fav1.ProbeStatus_PROBE_STATUS_HEALTHY}}}
		}}
		d.RegisterStream(verifiedIdentity(), sender)
		if _, err := d.Verify(context.Background(), cmdNS, cmdRobotID, probe.VerifyRequest{ProbeType: tc.probeType, Target: "t"}); err != nil {
			t.Fatalf("%s: %v", tc.probeType, err)
		}
		if !tc.check(sender.sent[0]) {
			t.Errorf("%s: wrong command arm pushed: %+v", tc.probeType, sender.sent[0])
		}
	}
}

// A model probe carries spec.syntheticInput to the adapter as
// VerifyModel.synthetic_input (ADR-0024).
func TestVerify_ModelCarriesSyntheticInput(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	sender := &fakeSender{dispatch: d, respond: func(cmd *fav1.Command) *fav1.CommandResult {
		return &fav1.CommandResult{CommandId: cmd.GetCommandId(),
			Result: &fav1.CommandResult_Verify{Verify: &fav1.VerifyResult{Status: fav1.ProbeStatus_PROBE_STATUS_HEALTHY}}}
	}}
	d.RegisterStream(verifiedIdentity(), sender)

	input := []byte("synthetic-frame-bytes")
	if _, err := d.Verify(context.Background(), cmdNS, cmdRobotID, probe.VerifyRequest{
		ProbeType: fleetv1.ProbeTypeModel, Target: "pick-net", SyntheticInput: input,
	}); err != nil {
		t.Fatalf("verify: %v", err)
	}
	vm := sender.sent[0].GetVerifyModel()
	if vm == nil || vm.GetModelName() != "pick-net" || string(vm.GetSyntheticInput()) != string(input) {
		t.Errorf("pushed verify_model = %+v, want synthetic_input carried", vm)
	}
}

func TestVerify_Unsupported(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	sender := &fakeSender{dispatch: d, respond: func(cmd *fav1.Command) *fav1.CommandResult {
		return &fav1.CommandResult{CommandId: cmd.GetCommandId(), Unsupported: true}
	}}
	d.RegisterStream(verifiedIdentity(), sender)

	res, err := d.Verify(context.Background(), cmdNS, cmdRobotID, probe.VerifyRequest{ProbeType: fleetv1.ProbeTypeModel, Target: "pick-net"})
	if err != nil {
		t.Fatalf("unsupported must not error (it is a valid outcome): %v", err)
	}
	if !res.Unsupported {
		t.Errorf("res.Unsupported = false, want true")
	}
}

func TestVerify_NoStreamIsUnreachable(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter) // robot exists, but no stream registered
	_, err := d.Verify(context.Background(), cmdNS, cmdRobotID, probe.VerifyRequest{ProbeType: fleetv1.ProbeTypeHardware, Target: "x"})
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
}

func TestVerify_WrongAdapterIsUnreachable(t *testing.T) {
	// The robot's adapter differs from the one whose stream is registered.
	d, _ := newDispatcher(t, "other-adapter")
	d.RegisterStream(verifiedIdentity(), &fakeSender{dispatch: d})
	_, err := d.Verify(context.Background(), cmdNS, cmdRobotID, probe.VerifyRequest{ProbeType: fleetv1.ProbeTypeHardware, Target: "x"})
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable (no stream for the robot's adapter)", err)
	}
}

func TestVerify_SendErrorIsUnreachable(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	d.RegisterStream(verifiedIdentity(), &fakeSender{dispatch: d, sendErr: errors.New("stream broken")})
	_, err := d.Verify(context.Background(), cmdNS, cmdRobotID, probe.VerifyRequest{ProbeType: fleetv1.ProbeTypeHardware, Target: "x"})
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable on send failure", err)
	}
}

func TestVerify_TimeoutIsUnreachable(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	d.timeout = 50 * time.Millisecond
	d.RegisterStream(verifiedIdentity(), &fakeSender{dispatch: d}) // never responds
	_, err := d.Verify(context.Background(), cmdNS, cmdRobotID, probe.VerifyRequest{ProbeType: fleetv1.ProbeTypeHardware, Target: "x"})
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable on timeout", err)
	}
}

func TestVerify_ContextCancelled(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	d.timeout = 10 * time.Second
	d.RegisterStream(verifiedIdentity(), &fakeSender{dispatch: d}) // never responds
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	_, err := d.Verify(ctx, cmdNS, cmdRobotID, probe.VerifyRequest{ProbeType: fleetv1.ProbeTypeHardware, Target: "x"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestRouteResult_StaleCommandIDDropped(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	// A result for a command_id nobody is waiting on must be a harmless no-op.
	d.RouteResult(&fav1.CommandResult{CommandId: 9999})
	d.RouteResult(nil)
}

func TestRegisterStream_UnverifiedNotRegistered(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	dereg := d.RegisterStream(controlstream.TLSIdentity{Verified: false, Namespace: cmdNS, AdapterName: cmdAdapter},
		&fakeSender{dispatch: d})
	dereg() // must be safe to call
	_, err := d.Verify(context.Background(), cmdNS, cmdRobotID, probe.VerifyRequest{ProbeType: fleetv1.ProbeTypeHardware, Target: "x"})
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable (unverified identity was not registered)", err)
	}
}

func TestVerify_ConcurrentCorrelation(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	// Echo each command back with its own id — many in flight at once must each
	// get exactly their own result (correlation by command_id, not FIFO).
	sender := &fakeSender{dispatch: d, respond: func(cmd *fav1.Command) *fav1.CommandResult {
		return &fav1.CommandResult{CommandId: cmd.GetCommandId(),
			Result: &fav1.CommandResult_Verify{Verify: &fav1.VerifyResult{
				Status:        fav1.ProbeStatus_PROBE_STATUS_HEALTHY,
				ActualMetrics: map[string]float64{"id": float64(cmd.GetCommandId())},
			}}}
	}}
	d.RegisterStream(verifiedIdentity(), sender)

	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := d.Verify(context.Background(), cmdNS, cmdRobotID, probe.VerifyRequest{ProbeType: fleetv1.ProbeTypeHardware, Target: "x"})
			if err != nil {
				t.Errorf("concurrent Verify: %v", err)
				return
			}
			// The echoed id metric must match a real command id (never zero/empty).
			if res.ActualMetrics["id"] == 0 {
				t.Errorf("correlation lost: %v", res.ActualMetrics)
			}
		}()
	}
	wg.Wait()
}
