// Copyright 2026 The Swarmada Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package k8sclient

import (
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	swarmadav1 "github.com/swarmada/swarmada/api/v1"
)

// This file is the only place that reads the Swarmada CRD types. Each mapper is
// a pure function (CRD object -> view), so they are exhaustively unit-testable
// without a cluster, an informer, or a running UI. Keeping the projection pure
// here is what lets store.go stay a thin cache wrapper and lets a later option-B
// event channel reuse these mappers verbatim.

// mapRobot projects a Robot into its display view.
func mapRobot(r *swarmadav1.Robot) RobotView {
	v := RobotView{
		Name:         r.Name,
		Phase:        string(r.Status.Phase),
		Estop:        estopOrNormal(r.Status.EstopState),
		SpecZone:     r.Spec.Zone,
		CurrentZone:  r.Status.CurrentZone,
		AssignedAction: r.Status.AssignedAction,
		AdapterName:  r.Spec.Adapter.Name,
	}

	if r.Status.BatteryPercent != nil {
		b := *r.Status.BatteryPercent
		v.BatteryPercent = &b
	}

	// ZoneDrift: status.specZoneMatchesCurrent is a *bool. Treat nil (not yet
	// computed) as "no drift" so a freshly-registered robot doesn't render a
	// spurious drift warning.
	if m := r.Status.SpecZoneMatchesCurrent; m != nil && !*m {
		v.ZoneDrift = true
	}

	v.Caps, v.Capabilities = mapCapabilities(r.Status.Capabilities)
	v.Hardware = mapHardware(r.Status.Hardware)

	if p := r.Status.Position; p != nil {
		v.HasPosition = true
		v.Position = PositionView{X: p.X, Y: p.Y, Floor: copyInt32(p.Floor)}
	}

	if c := r.Status.Connectivity; c != nil {
		if c.LastSeenAt != nil {
			v.LastTelemetry = c.LastSeenAt.Time
		} else {
			v.TelemetryUnknown = true
		}
		v.LatencyMs = copyInt32(c.LatencyMs)
	} else {
		v.TelemetryUnknown = true
	}

	if h := r.Status.Health; h != nil {
		v.HealthStatus = string(h.Status)
		v.HealthMessage = h.Message
	}

	v.FirmwareVersion = r.Status.FirmwareVersion
	v.PreviousFirmwareVersion = r.Status.PreviousFirmwareVersion
	v.Conditions = mapConditions(r.Status.Conditions)
	v.InstalledModels = mapInstalledModels(r.Status.InstalledModels)
	v.ModelGrantedCaps = mapModelGranted(r.Status.ModelGrantedCapabilities)

	return v
}

func mapConditions(in []metav1.Condition) []ConditionView {
	out := make([]ConditionView, 0, len(in))
	for i := range in {
		c := in[i]
		out = append(out, ConditionView{
			Type:           c.Type,
			Status:         string(c.Status),
			Reason:         c.Reason,
			Message:        c.Message,
			LastTransition: c.LastTransitionTime.Time,
		})
	}
	return out
}

func mapInstalledModels(in []swarmadav1.InstalledModelStatusEntry) []InstalledModelView {
	out := make([]InstalledModelView, 0, len(in))
	for i := range in {
		m := in[i]
		out = append(out, InstalledModelView{
			Name:           m.Name,
			Status:         string(m.Status),
			RunningVersion: m.RunningVersion,
			FailureReason:  m.FailureReason,
		})
	}
	return out
}

func mapModelGranted(in []swarmadav1.ModelGrantedCapabilityEntry) []ModelGrantedView {
	out := make([]ModelGrantedView, 0, len(in))
	for i := range in {
		e := in[i]
		caps := make([]string, len(e.Capabilities))
		copy(caps, e.Capabilities)
		out = append(out, ModelGrantedView{ModelName: e.ModelName, GrantedBy: e.GrantedBy, Capabilities: caps})
	}
	return out
}

// mapEvent projects a core/v1 Event into an EventView, choosing the best
// available timestamp (LastTimestamp, then EventTime, then creation).
func mapEvent(e *corev1.Event) EventView {
	ts := e.LastTimestamp.Time
	if ts.IsZero() && !e.EventTime.IsZero() {
		ts = e.EventTime.Time
	}
	if ts.IsZero() {
		ts = e.CreationTimestamp.Time
	}
	count := e.Count
	if e.Series != nil && e.Series.Count > count {
		count = e.Series.Count
	}
	return EventView{Time: ts, Type: e.Type, Reason: e.Reason, Message: e.Message, Count: count}
}

// bucketRobotEvents groups Events whose involvedObject is a Robot by robot
// name, newest-first within each bucket.
func bucketRobotEvents(items []corev1.Event) map[string][]EventView {
	out := make(map[string][]EventView)
	for i := range items {
		e := &items[i]
		if e.InvolvedObject.Kind != "Robot" {
			continue
		}
		out[e.InvolvedObject.Name] = append(out[e.InvolvedObject.Name], mapEvent(e))
	}
	for name := range out {
		evs := out[name]
		sort.Slice(evs, func(a, b int) bool { return evs[a].Time.After(evs[b].Time) })
	}
	return out
}

// mapCapabilities produces both the list roll-up and the full per-entry
// breakdown in a single pass. FirstProblem follows the input order, which the
// controllers keep stable (listMapKey=name), so the headline problem is
// deterministic.
func mapCapabilities(entries []swarmadav1.CapabilityStatusEntry) (CapSummary, []CapabilityView) {
	sum := CapSummary{Total: len(entries)}
	views := make([]CapabilityView, 0, len(entries))
	for i := range entries {
		e := entries[i]
		status := string(e.Status)
		if e.Status == swarmadav1.CapabilityStatusActive {
			sum.Active++
		} else if sum.FirstProblem == "" {
			sum.FirstProblem = e.Name
			sum.FirstProblemState = status
		}
		views = append(views, CapabilityView{
			Name:   e.Name,
			Status: status,
			Paused: e.Paused,
			Reason: e.Reason,
		})
	}
	return sum, views
}

func mapHardware(entries []swarmadav1.HardwareComponentStatus) []HardwareView {
	views := make([]HardwareView, 0, len(entries))
	for i := range entries {
		e := entries[i]
		views = append(views, HardwareView{
			Name:   e.Name,
			Status: string(e.Status),
			Reason: e.DegradationReason,
		})
	}
	return views
}

// mapFleetAction projects a FleetAction into its view.
func mapFleetAction(t *swarmadav1.FleetAction) FleetActionView {
	v := FleetActionView{
		Name:          t.Name,
		Phase:         string(t.Status.Phase),
		AssignedRobot: t.Status.AssignedRobot,
		Priority:      string(t.Spec.Priority),
		ProgressPct:   t.Status.ProgressPct,
		RetryCount:    t.Status.RetryCount,
		Message:       t.Status.Message,
	}
	if d := t.Spec.Deadline; d != nil {
		dl := d.Time
		v.Deadline = &dl
	}
	return v
}

// mapRobotProbe projects a RobotProbe into its view, summarizing the last
// cycle's per-robot results into coverage/failing counts.
func mapRobotProbe(p *swarmadav1.RobotProbe) RobotProbeView {
	v := RobotProbeView{
		Name:            p.Name,
		ProbeType:       string(p.Spec.ProbeType),
		TargetComponent: p.Spec.TargetComponent,
		LastResult:      string(p.Status.LastProbeResult),
		RobotCount:      len(p.Status.RobotResults),
	}
	if t := p.Status.LastProbeTime; t != nil {
		v.LastProbeTime = t.Time
	}
	for i := range p.Status.RobotResults {
		if p.Status.RobotResults[i].ProbeStatus != swarmadav1.ProbeResultHealthy {
			v.FailingCount++
		}
	}
	return v
}

// mapFleetZone projects a FleetZone into its view.
func mapFleetZone(z *swarmadav1.FleetZone) ZoneView {
	v := ZoneView{
		Name:                z.Name,
		DisplayName:         z.Spec.DisplayName,
		ParentZone:          z.Spec.ParentZone,
		IsLeaf:              z.Status.IsLeaf,
		ChildZones:          append([]string(nil), z.Status.ChildZones...),
		RobotCount:          z.Status.RobotCount,
		CurrentConcurrent:   z.Status.CurrentConcurrentRobots,
		MaxConcurrentRobots: z.Spec.MaxConcurrentRobots,
		EstopStatus:         string(z.Status.EstopStatus),
		EdgeFeedUnavailable: append([]string(nil), z.Status.EdgeFeedUnavailable...),
		HasEdgeNode:         z.Spec.EdgeNode != nil,
		Waypoints:           len(z.Spec.Waypoints),
		LastEstopUnknown:    z.Status.LastEstopAt == nil,
	}
	if z.Status.LastEstopAt != nil {
		v.LastEstopAt = z.Status.LastEstopAt.Time
	}
	// "Empty is treated as Clear" per the API. Normalising here keeps every reader from having to
	// know that, and stops a blank cell reading as "unknown" when it means "not stopped".
	if v.EstopStatus == "" {
		v.EstopStatus = "Clear"
	}
	return v
}

// mapFleetAdapter projects a FleetAdapter into its view.
func mapFleetAdapter(a *swarmadav1.FleetAdapter) AdapterView {
	v := AdapterView{
		Name:            a.Name,
		Phase:           string(a.Status.Phase),
		Conformance:     string(a.Status.Conformance),
		ProtocolVersion: a.Status.NegotiatedProtocolVersion,
		ConnectedRobots: a.Status.ConnectedRobots,
		Message:         a.Status.Message,
	}
	if hb := a.Status.LastHeartbeat; hb != nil {
		v.LastHeartbeat = hb.Time
	} else {
		v.HeartbeatUnknown = true
	}
	return v
}

func estopOrNormal(s swarmadav1.RobotEstopState) string {
	if s == "" {
		return string(swarmadav1.RobotEstopNormal)
	}
	return string(s)
}

func copyInt32(p *int32) *int32 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
