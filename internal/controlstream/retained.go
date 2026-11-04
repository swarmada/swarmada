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

package controlstream

import (
	"context"

	"github.com/swarmada/swarmada/internal/contract"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// outcomeAckMinContract is the first contract version that carries ActionOutcomeAck.
const outcomeAckMinContract = "1.4.0"

// RetainedOutcomeIngestor reads an ActionStatusUpdate the adapter sent with retained = true:
// the terminal outcome of the robot's last action, retained from before this connection
// (RFC-0001 #fleet-adapter-protocol-reconnect-action-state-reconciliation, The retained
// outcome; row A13 of #safety-action-table).
//
// ack reports whether the control plane has recorded the outcome or decided it: it wrote it,
// found it already recorded, decided it moves nothing (a stop holds the work), or found the
// action gone from the API server. The server then answers with an ActionOutcomeAck naming the
// action and the outcome's report token, and the adapter stops retaining it. ack is false for
// an outcome left undecided: a report token that does not match the action's assignment (which
// a cache behind the API server can also produce), a stream that may not act on the action's
// robot, a report that is not terminal, or a write that did not land. The adapter reports
// such an outcome again on its next reconnect, so a refusal never makes it drop a real one.
//
// The same authorization contract as ActionStatusIngestor applies to authorize.
type RetainedOutcomeIngestor interface {
	IngestRetainedOutcome(ctx context.Context, namespace string, authorize RobotAuthorization,
		u *fav1.ActionStatusUpdate) (ack bool, err error)
}

// ingestRetained hands a retained outcome to the ingestor and acknowledges it on this stream
// when the ingestor recorded or decided it. An ingestor that does not read retained outcomes
// leaves it unacknowledged, so the adapter keeps it. Only a failure to send the
// acknowledgement is returned: it is a connection-level fault.
func (s *Server) ingestRetained(ctx context.Context, w *streamWriter, id AdapterIdentity, tlsID TLSIdentity,
	u *fav1.ActionStatusUpdate) error {
	ri, ok := s.ActionStatus.(RetainedOutcomeIngestor)
	if !ok {
		s.logger().Info("retained outcome not read: no ingestor reads retained outcomes; not acknowledged",
			"namespace", id.Namespace, "actionID", u.GetActionId())
		return nil
	}
	authorize := func(ctx context.Context, robotID string) bool {
		allowed, _ := s.authorizeRobot(ctx, tlsID, robotID)
		return allowed
	}
	ack, err := ri.IngestRetainedOutcome(ctx, id.Namespace, authorize, u)
	if err != nil {
		s.logger().Error(err, "ingesting a retained outcome; not acknowledged, so the adapter reports it again",
			"namespace", id.Namespace, "actionID", u.GetActionId())
		return nil
	}
	if !ack {
		return nil
	}
	if below, _ := contract.Below(id.NegotiatedContractVersion, outcomeAckMinContract); below {
		// ActionOutcomeAck is a 1.4.0 addition: a connection held to 1.3.x is sent none (RFC-0001
		// #fleet-adapter-protocol-package-and-versioning, Current version).
		s.logger().Info("retained outcome read; no ActionOutcomeAck on a connection below 1.4.0",
			"namespace", id.Namespace, "actionID", u.GetActionId(), "negotiated", id.NegotiatedContractVersion)
		return nil
	}
	return w.send(&fav1.ControlPlaneMessage{Payload: &fav1.ControlPlaneMessage_OutcomeAck{
		OutcomeAck: &fav1.ActionOutcomeAck{ActionId: u.GetActionId(), FencingToken: u.GetFencingToken()},
	}})
}
