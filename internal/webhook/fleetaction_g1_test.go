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

package webhook

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// `FleetActionSpec.Executor.Sinks`' doc comment promised since the field
// was added that "Each MUST exist in this namespace and accept the endpoint's
// declared results — admission checks this (G1)". No such check existed. These
// tests are that check.
//
// A note on what these tests are NOT proving: G1 is a correctness and
// operator-experience gate, not a credential guard. Nothing in the tree resolves a
// ResultSink's credentialRef or issues a grant — see the trace recorded on
// — so an unresolvable sink name could not previously reach a credential.
// If a grant path is added later, G1 becomes load-bearing for it and these tests
// become security tests; they are written to stay correct either way.

const g1NS = "warehouse-a"

func computeEndpoint(name string, results ...string) *fleetv1.ComputeEndpoint {
	rs := make([]fleetv1.ComputeEndpointResultField, 0, len(results))
	for _, r := range results {
		rs = append(rs, fleetv1.ComputeEndpointResultField{Name: r, Kind: fleetv1.ValueKindString})
	}
	return &fleetv1.ComputeEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: g1NS},
		Spec: fleetv1.ComputeEndpointSpec{
			Binding:  fleetv1.ExecutorBindingHttp,
			Contract: fleetv1.ExecutorContractApp,
			Results:  rs,
		},
	}
}

// resultSink with no declared columns accepts anything; with columns, only those.
func resultSink(name string, columns ...string) *fleetv1.ResultSink {
	sink := &fleetv1.ResultSink{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: g1NS},
		Spec: fleetv1.ResultSinkSpec{
			Kind:     fleetv1.ResultSinkKindDatabase,
			Endpoint: "postgres://evidence.internal:5432/detections",
		},
	}
	if len(columns) > 0 {
		sink.Spec.Schema = &fleetv1.ResultSinkSchema{Table: "detections", Columns: columns}
	}
	return sink
}

func externalAction(endpointRef string, sinks ...string) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "act", Namespace: g1NS},
		Spec: fleetv1.FleetActionSpec{
			Type: fleetv1.ActionTypeExternal,
			Executor: &fleetv1.ActionExecutor{
				EndpointRef: endpointRef,
				Sinks:       sinks,
			},
		},
	}
}

func TestG1_NonexistentResultSinkIsRejected(t *testing.T) {
	v := &FleetActionValidator{Reader: newSupportedReader(t, computeEndpoint("plate-reader"))}

	_, err := v.ValidateCreate(context.Background(), externalAction("plate-reader", "evidence-db"))
	if err == nil {
		t.Fatal("a FleetAction naming a ResultSink that does not exist was admitted; G1 must reject it")
	}
	if !strings.Contains(err.Error(), ReasonExternalEndpointNotFound) {
		t.Errorf("error must carry the closed-vocabulary reason %q; got: %v",
			ReasonExternalEndpointNotFound, err)
	}
	if !strings.Contains(err.Error(), "evidence-db") {
		t.Errorf("error must name the missing sink so an operator can act on it; got: %v", err)
	}
}

func TestG1_NonexistentComputeEndpointIsRejected(t *testing.T) {
	v := &FleetActionValidator{Reader: newSupportedReader(t, resultSink("evidence-db"))}

	_, err := v.ValidateCreate(context.Background(), externalAction("plate-reader", "evidence-db"))
	if err == nil {
		t.Fatal("a FleetAction naming a ComputeEndpoint that does not exist was admitted; G1 must reject it")
	}
	if !strings.Contains(err.Error(), ReasonExternalEndpointNotFound) {
		t.Errorf("error must carry reason %q; got: %v", ReasonExternalEndpointNotFound, err)
	}
	if !strings.Contains(err.Error(), "plate-reader") {
		t.Errorf("error must name the missing endpoint; got: %v", err)
	}
}

func TestG1_ValidReferencesAreAdmitted(t *testing.T) {
	v := &FleetActionValidator{Reader: newSupportedReader(t,
		computeEndpoint("plate-reader", "plate", "confidence"),
		resultSink("evidence-db", "plate", "confidence", "observed_at"),
	)}

	if _, err := v.ValidateCreate(context.Background(), externalAction("plate-reader", "evidence-db")); err != nil {
		t.Fatalf("a FleetAction whose endpoint and sink both exist and whose results the sink accepts must be admitted; got: %v", err)
	}
}

func TestG1_SinkNotAcceptingADeclaredResultIsRejected(t *testing.T) {
	// The sink declares a column set; the endpoint declares a result field outside
	// it. That is a contract mismatch, not a missing object.
	v := &FleetActionValidator{Reader: newSupportedReader(t,
		computeEndpoint("plate-reader", "plate", "confidence"),
		resultSink("evidence-db", "plate"), // no `confidence` column
	)}

	_, err := v.ValidateCreate(context.Background(), externalAction("plate-reader", "evidence-db"))
	if err == nil {
		t.Fatal("a sink whose declared columns do not accept every endpoint result was admitted; G1 must reject it")
	}
	if !strings.Contains(err.Error(), ReasonExternalContractMismatch) {
		t.Errorf("a mismatch is %q, not %q — the two are different operator actions; got: %v",
			ReasonExternalContractMismatch, ReasonExternalEndpointNotFound, err)
	}
	if !strings.Contains(err.Error(), "confidence") {
		t.Errorf("error must name the offending result field; got: %v", err)
	}
}

func TestG1_SinkDeclaringNoSchemaAcceptsAnything(t *testing.T) {
	// Explicit presence: an undeclared column list means "not stated", never
	// "accepts nothing". A sink nobody has described yet must not reject every
	// action that names it.
	v := &FleetActionValidator{Reader: newSupportedReader(t,
		computeEndpoint("plate-reader", "plate", "confidence"),
		resultSink("evidence-db"), // no schema at all
	)}

	if _, err := v.ValidateCreate(context.Background(), externalAction("plate-reader", "evidence-db")); err != nil {
		t.Fatalf("a sink declaring no schema accepts anything; got: %v", err)
	}
}

func TestG1_IgnoresANonExecutorAction(t *testing.T) {
	// An ordinary robot action carries no executor and must not be touched by G1.
	v := &FleetActionValidator{Reader: newSupportedReader(t)}
	action := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "act", Namespace: g1NS},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
	}
	if _, err := v.ValidateCreate(context.Background(), action); err != nil {
		t.Fatalf("G1 must not apply to an action with no spec.executor; got: %v", err)
	}
}

func TestG1_NilReaderFailsOpen(t *testing.T) {
	// Matches validateSupported's discipline: with no Reader the gate is disabled
	// rather than blocking every write. G2/G3 remain the backstop.
	v := &FleetActionValidator{}
	if _, err := v.ValidateCreate(context.Background(), externalAction("nope", "also-nope")); err != nil {
		t.Fatalf("a validator with no Reader must fail open; got: %v", err)
	}
}

func TestG1_RunsOnUpdateToo(t *testing.T) {
	//  gates create AND update: an executor reference edited to name an object
	// that does not exist must be refused exactly as it would be at create.
	v := &FleetActionValidator{Reader: newSupportedReader(t, computeEndpoint("plate-reader"))}

	old := externalAction("plate-reader")
	updated := externalAction("plate-reader", "evidence-db") // sink added, does not exist

	_, err := v.ValidateUpdate(context.Background(), old, updated)
	if err == nil {
		t.Fatal("an update adding a nonexistent sink was admitted; G1 gates update as well as create")
	}
	if !strings.Contains(err.Error(), ReasonExternalEndpointNotFound) {
		t.Errorf("error must carry reason %q; got: %v", ReasonExternalEndpointNotFound, err)
	}
}
