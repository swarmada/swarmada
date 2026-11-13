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
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controlstream"
	"github.com/swarmada/swarmada/internal/robotid"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// The IDs of removed entries (RFC-0001 #safety-robot-stop-table, Removing a recorded stop; Reported
// stop IDs and local-latch reports).
//
// The control plane keeps, for each robot, the ID of every entry it removes from status.stops: on a
// connection below 1.4.0 at the clear, and otherwise on the adapter's EstopClearAck. An entry that
// already has its clearedAt is remembered before the write that removes it, and forgotten if that
// write does not land, so no report finds the entry gone and its ID not yet remembered. A later report
// naming such an ID, for that robot, is answered as a cleared stop: with the stop's fence on the
// ControlStream and its EstopClear on the SafetyStream. It is how an ID an adapter latched while its
// connection was at 1.3.x, which is sent no EstopClear, is lifted on a later 1.4.0 connection
// without a restart of the adapter. The memory is this process's own: after a change of leader it
// is gone, as the text allows.

// maxRemembered is how many removed IDs are kept per robot; the oldest goes first. It is
// status.stops' cap, so a robot whose every entry was removed is still covered.
const maxRemembered = maxRecordedStops

// removedStop is one remembered removed entry.
type removedStop struct {
	id        string
	uid       types.UID // the robot it was removed from; a robot created again under the name is another
	clearedAt time.Time
}

// rememberedKey keys the memory by namespace and wire robot_id, the ID a report names.
func rememberedKey(namespace, wireID string) string { return namespace + "/" + wireID }

// keptReport is a report of a remembered ID that arrived while the adapter's connection was not
// known here to be at 1.4.0: an adapter reports its latched IDs as its SafetyStream opens, which can
// be before the new ControlStream's hello. It is answered once the adapter reports 1.4.0.
type keptReport struct {
	wireID string
	rm     removedStop
}

// ClearedFenceSender is satisfied by a FenceSender that can send a cleared stop's fence: its Estop
// on the robot's ControlStream, in order with the AssignActions, without adding the stop to the
// robot's stops in force. Reached by type assertion, so a FenceSender that lacks it keeps compiling;
// the answer then goes without its fence.
type ClearedFenceSender interface {
	SendClearedFence(robot *fleetv1.Robot, stopID string, estop *fav1.Estop) error
}

// rememberRemoved records that robot's entries ids were removed, with the clearedAt each carried
// (or now, for one with none).
func (d *Dispatcher) rememberRemoved(robot *fleetv1.Robot, stops []fleetv1.RobotStop) {
	if len(stops) == 0 {
		return
	}
	key := rememberedKey(robot.Namespace, robotid.Wire(robot))
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.removed == nil {
		d.removed = map[string][]removedStop{}
	}
	list := d.removed[key]
	for _, st := range stops {
		// A removed stop holds nothing: whose it was no longer matters to a takeover (takeover.go).
		delete(d.issued, st.ID)
		at := d.clock()
		if st.ClearedAt != nil {
			at = st.ClearedAt.Time
		}
		kept := list[:0]
		for _, r := range list {
			if r.id != st.ID {
				kept = append(kept, r)
			}
		}
		list = append(kept, removedStop{id: st.ID, uid: robot.UID, clearedAt: at})
	}
	if over := len(list) - maxRemembered; over > 0 {
		list = append([]removedStop(nil), list[over:]...)
	}
	d.removed[key] = list
}

// forgetRemoved undoes rememberRemoved for robot's entries stops, whose removal did not land: an
// entry that stays on the robot is not a removed one. Only entries with a clearedAt are remembered
// ahead of their removal: their EstopClear is already due, and is sent again on each SafetyStream
// that opens while they stand (resendClears), so a report answered in between was answered rightly.
func (d *Dispatcher) forgetRemoved(robot *fleetv1.Robot, stops []fleetv1.RobotStop) {
	if len(stops) == 0 {
		return
	}
	key := rememberedKey(robot.Namespace, robotid.Wire(robot))
	d.mu.Lock()
	defer d.mu.Unlock()
	gone := make(map[string]bool, len(stops))
	for _, st := range stops {
		gone[st.ID] = true
	}
	var kept []removedStop
	for _, r := range d.removed[key] {
		if !gone[r.id] || r.uid != robot.UID {
			kept = append(kept, r)
		}
	}
	if len(kept) == 0 {
		delete(d.removed, key)
		return
	}
	d.removed[key] = kept
}

// rememberedLocked is the remembered removed entry id of the robot with wire robot_id wireID in
// namespace. The caller holds d.mu.
func (d *Dispatcher) rememberedLocked(namespace, wireID, id string) (removedStop, bool) {
	for _, r := range d.removed[rememberedKey(namespace, wireID)] {
		if r.id == id {
			return r, true
		}
	}
	return removedStop{}, false
}

// answerRemoved answers a report, on from, of rm, an ID removed from the robot with wire robot_id
// wireID: the stop's fence and its EstopClear. Only on the adapter's current SafetyStream, while
// this process leads, for the robot the entry was removed from and the adapter that serves it, and
// on a connection that negotiated 1.4.0: one below it is sent no EstopClear. It runs in a goroutine
// of its own, off the SafetyStream receive loop.
func (d *Dispatcher) answerRemoved(identity controlstream.TLSIdentity, from controlstream.SafetySender, wireID string,
	rm removedStop) {
	logger := log.Log.WithName("estop-dispatcher").WithValues("robot", wireID, "namespace", identity.Namespace,
		"stopID", rm.id)
	d.mu.Lock()
	current := d.streams[adapterKey(identity.Namespace, identity.AdapterName)] == from
	d.mu.Unlock()
	if !current {
		logger.Info("report of a removed stop on a SafetyStream that has been replaced; not answered")
		return
	}
	term, leading := d.leads()
	if !leading {
		return
	}
	var reader client.Reader = d.Client
	if d.APIReader != nil {
		reader = d.APIReader
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultConfirmTimeout)
	defer cancel()
	robot, err := robotid.Lookup(ctx, reader, identity.Namespace, wireID)
	switch {
	case err != nil || robot == nil:
		logger.Info("report of a removed stop for a robot that could not be read; not answered", "err", fmt.Sprint(err))
		return
	case robot.UID != rm.uid:
		logger.Info("report of a stop removed from another robot of the same name; not answered")
		return
	case robot.Spec.Adapter.Name != identity.AdapterName:
		logger.Info("report of a removed stop from an adapter that does not serve the robot; not answered",
			"adapter", identity.AdapterName)
		return
	case d.adapterClearPath(robot) != clearSend:
		logger.Info("report of a removed stop on a connection not known here to be at 1.4.0; kept, and answered " +
			"once the adapter reports 1.4.0 (a connection below it is sent no EstopClear)")
		d.keepReport(identity, wireID, rm)
		return
	}
	estop := &fav1.Estop{EstopId: rm.id, Reason: "a cleared stop the adapter reports latched",
		IssuedBy: heldStopIssuer, IssuedAtMs: d.clock().UnixMilli()}
	if f, ok := d.Fence.(ClearedFenceSender); ok {
		if err := f.SendClearedFence(robot, rm.id, estop); err != nil {
			logger.Info("the fence of a reported cleared stop was not sent; its EstopClear waits for it at the "+
				"adapter, and the next report is answered again", "err", err.Error())
		}
	}
	d.mu.Lock()
	d.idCounter++
	seq := d.idCounter
	d.mu.Unlock()
	if err := from.Send(&fav1.ControlPlaneSafetyMessage{Seq: seq, RobotId: wireID,
		Payload: &fav1.ControlPlaneSafetyMessage_EstopClear{EstopClear: &fav1.EstopClear{
			EstopId: rm.id, ClearedBy: heldStopIssuer, ClearedAtMs: rm.clearedAt.UnixMilli(), LeaderTerm: term}}}); err != nil {
		logger.Error(err, "sending the EstopClear of a reported cleared stop; the next report is answered again")
		return
	}
	logger.Info("reported stop answered as cleared: its fence and its EstopClear sent")
}

// keepReport keeps a report of a remembered ID until the adapter reports 1.4.0 (answerKeptReports),
// once per robot and ID, at most maxRemembered per adapter.
func (d *Dispatcher) keepReport(identity controlstream.TLSIdentity, wireID string, rm removedStop) {
	key := adapterKey(identity.Namespace, identity.AdapterName)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.kept == nil {
		d.kept = map[string][]keptReport{}
	}
	for _, k := range d.kept[key] {
		if k.wireID == wireID && k.rm.id == rm.id {
			return
		}
	}
	list := append(d.kept[key], keptReport{wireID: wireID, rm: rm})
	if over := len(list) - maxRemembered; over > 0 {
		list = append([]keptReport(nil), list[over:]...)
	}
	d.kept[key] = list
}

// answerKeptReports answers, on the adapter's current SafetyStream, the reports kept until it
// reported 1.4.0. With no current stream they stay kept; the adapter reports again when one opens.
func (d *Dispatcher) answerKeptReports(identity controlstream.TLSIdentity) {
	key := adapterKey(identity.Namespace, identity.AdapterName)
	d.mu.Lock()
	sender := d.streams[key]
	reports := d.kept[key]
	if sender != nil {
		delete(d.kept, key)
	}
	d.mu.Unlock()
	if sender == nil {
		return
	}
	for _, k := range reports {
		d.answerRemoved(identity, sender, k.wireID, k.rm)
	}
}

// removedEntries is the entries of before that after no longer holds.
func removedEntries(before, after []fleetv1.RobotStop) []fleetv1.RobotStop {
	kept := make(map[string]bool, len(after))
	for _, st := range after {
		kept[st.ID] = true
	}
	var out []fleetv1.RobotStop
	for _, st := range before {
		if !kept[st.ID] {
			out = append(out, st)
		}
	}
	return out
}
