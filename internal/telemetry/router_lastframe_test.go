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

package telemetry

import (
	"context"
	"testing"
	"time"
)

// lastFrameProbe is a status sink that reads the router's LastFrame while the frame
// it is applying is still being ingested.
type lastFrameProbe struct {
	r      *Router
	during []bool
}

func (p *lastFrameProbe) ApplyMaterialUpdate(_ context.Context, upd MaterialUpdate) (bool, error) {
	_, ok := p.r.LastFrame(upd.Namespace, upd.RobotID)
	p.during = append(p.during, ok)
	return true, nil
}

// LastFrame is the estop resume witness. A robot not yet heard from has no frame,
// however long the router has run; and the stamp lands only once the frame has been
// through the plane, so a reader that sees it can also read the status write the
// frame caused.
func TestRouter_LastFrameIsStampedAfterTheStatusWrite(t *testing.T) {
	start := time.Now()
	now := start
	probe := &lastFrameProbe{}
	r := NewRouter(func(string) PlaneConfig { return PlaneConfig{} }, probe, WithClock(func() time.Time { return now }))
	probe.r = r

	now = start.Add(time.Hour)
	if at, ok := r.LastFrame("ns-a", "rid-1"); ok {
		t.Fatalf("unheard robot: LastFrame = %s, true; want no frame", at)
	}

	pct := int32(80)
	now = start.Add(2 * time.Hour)
	if err := r.Ingest(context.Background(), Frame{RobotID: "rid-1", Namespace: "ns-a", BatteryPct: &pct}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if len(probe.during) != 1 || probe.during[0] {
		t.Fatalf("LastFrame during the status write = %v, want [false]: the stamp must follow the write", probe.during)
	}
	if at, ok := r.LastFrame("ns-a", "rid-1"); !ok || !at.Equal(now) {
		t.Fatalf("LastFrame = %s, %v; want %s, true", at, ok, now)
	}
	if _, ok := r.LastFrame("ns-b", "rid-1"); ok {
		t.Fatal("a frame in ns-a stamped the same robot_id in ns-b")
	}
}
