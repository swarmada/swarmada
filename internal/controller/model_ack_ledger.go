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

import "sync"

// ModelAckLedger records, per robot and model, that the latest model_update pushed to the
// robot was acknowledged. The ModelRollout controller writes it and the UpdateProgress
// ingestor reads it, so a FAILED install report can be told from a declined push.
//
// Why that needs a ledger: an adapter reports FAILED both when an accepted install fails and
// when it declines an artifact, having installed nothing. A decline must not fail the robot,
// so a FAILED report is correlated only to a robot the control plane entered into a batch.
// The batch is written by the rollout status patch at the end of the reconcile, after every
// push in it; a failure reported between the acknowledgement and that write found no batch
// and was dropped. A ControlStream's messages are handled in order, so a decline's FAILED
// report, which the adapter sends before its reply, is ingested before the push returns and
// can never find an entry here; the controller clears the entry before each push and sets
// it on the acknowledgement.
//
// In memory, deliberately: it covers the seconds between an acknowledgement and the batch
// write. A manager that restarts inside that window loses the entry and the report is
// dropped as before. A nil ledger records nothing.
type ModelAckLedger struct {
	mu    sync.Mutex
	acked map[modelAckKey]struct{}
}

type modelAckKey struct{ namespace, robot, model string }

// NewModelAckLedger returns an empty ledger.
func NewModelAckLedger() *ModelAckLedger {
	return &ModelAckLedger{acked: map[modelAckKey]struct{}{}}
}

// forget clears the entry before a push, so a decline of this push is never taken for a
// failure of an earlier, acknowledged one.
func (l *ModelAckLedger) forget(namespace, robot, model string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.acked, modelAckKey{namespace, robot, model})
}

// ack records that the push just made was acknowledged.
func (l *ModelAckLedger) ack(namespace, robot, model string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.acked[modelAckKey{namespace, robot, model}] = struct{}{}
}

// acknowledgedModel returns the model a FAILED report from robot is about when the latest
// push of that model was acknowledged: reported when the report names it, otherwise the
// robot's one acknowledged model. It returns "" when there is none, or more than one and
// the report does not say which.
func (l *ModelAckLedger) acknowledgedModel(namespace, robot, reported string) string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if reported != "" {
		if _, ok := l.acked[modelAckKey{namespace, robot, reported}]; ok {
			return reported
		}
		return ""
	}
	found := ""
	for k := range l.acked {
		if k.namespace != namespace || k.robot != robot {
			continue
		}
		if found != "" {
			return ""
		}
		found = k.model
	}
	return found
}
