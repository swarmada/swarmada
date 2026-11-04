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
	"testing"
	"time"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controlstream"
)

// status.negotiatedContractVersion is the version AGREED for the connection (RFC-0001
// #fleet-adapter-protocol-package-and-versioning), which candidate filter 9 reads: 1.4.0 for an
// adapter that reported any 1.4 version, and the reported 1.3.x otherwise. It recorded the
// reported version, so a 1.4.7 adapter read 1.4.7, a version no control plane agreed with it.
func TestContractPhase_StatusRecordsTheVersionAgreedForTheConnection(t *testing.T) {
	for _, tc := range []struct{ reported, want string }{
		{"1.4.7", "1.4.0"},
		{"1.4.0", "1.4.0"},
		{"1.3.0", "1.3.0"},
		{"1.3.2", "1.3.2"},
	} {
		t.Run(tc.reported, func(t *testing.T) {
			r, c := newFAReconciler(t, time.Unix(1000, 0), fleetv1.FleetAdapterPhasePending, nil)
			r.AdapterConnected(context.Background(), faIdentity(), controlstream.Negotiation{
				ProtocolVersion: "fleet_adapter.v1", ContractVersion: tc.reported, ContractCompatible: true,
			})
			fa := getFA(t, c)
			if fa.Status.Phase != fleetv1.FleetAdapterPhaseConnected {
				t.Fatalf("phase = %s, want Connected", fa.Status.Phase)
			}
			if got := fa.Status.NegotiatedContractVersion; got != tc.want {
				t.Errorf("negotiatedContractVersion = %q for a reported %q, want %q (the agreed version)",
					got, tc.reported, tc.want)
			}
		})
	}
}
