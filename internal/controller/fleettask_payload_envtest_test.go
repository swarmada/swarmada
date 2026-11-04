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

// Does a payload a manifest author writes reach the adapter?
//
// Every earlier payload test built the payload in Go as ActionPayload{Raw: ...}, the one
// spelling no manifest uses. These start from YAML, applied as unstructured so the Go type
// plays no part in writing it, and follow the payload through the API server, the FleetTask
// member copy and the assign_action push (RFC-0001 #fleetaction-the-payload-field).

import (
	"context"
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

const payloadTaskManifest = `
apiVersion: swarmada.io/v1
kind: FleetTask
metadata:
  name: route-patrol
spec:
  completionPolicy: All
  failurePolicy: FailFast
  actions:
  - name: patrol-route
    trigger: Auto
    action:
      type: Patrol
      payload:
        route:
          frame: map
          corridorHalfWidthM: 0.9
          loopClosed: true
          onCorridorExit: Report
          waypoints:
          - {x: 1.0, "y": 2.0}
          - {x: 4.5, "y": 2.0}
`

// rawFormTaskManifest carries {"x":1.5} in the RFC-0001 spelling, payload.raw as base64.
const rawFormTaskManifest = `
apiVersion: swarmada.io/v1
kind: FleetTask
metadata:
  name: raw-form
spec:
  completionPolicy: All
  failurePolicy: FailFast
  actions:
  - name: go
    trigger: Auto
    action:
      type: Navigate
      payload:
        raw: eyJ4IjoxLjV9
`

// applyManifest creates the YAML object in ns as unstructured.
func applyManifest(t *testing.T, ns, manifest string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(manifest), &u.Object); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	u.SetNamespace(ns)
	if err := envK8s.Create(context.Background(), u); err != nil {
		t.Fatalf("apply manifest: %v", err)
	}
	return u
}

// childPayload reads the one child FleetAction of task as unstructured, so what is
// compared is what the API server stores, and returns its spec.payload.
func childPayload(t *testing.T, ns, task string) (string, map[string]any) {
	t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "swarmada.io", Version: "v1", Kind: "FleetActionList"})
	if err := envK8s.List(context.Background(), list, client.InNamespace(ns),
		client.MatchingLabels{ownedByLabel: task}); err != nil {
		t.Fatalf("list children: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("children of %s = %d, want 1", task, len(list.Items))
	}
	p, _, err := unstructured.NestedMap(list.Items[0].Object, "spec", "payload")
	if err != nil {
		t.Fatalf("child spec.payload: %v", err)
	}
	return list.Items[0].GetName(), p
}

// assignedPayload pushes the stored child through the assignment path and returns the
// payload_json bytes the adapter would receive.
func assignedPayload(t *testing.T, ns, name string) []byte {
	t.Helper()
	child := &fleetv1.FleetAction{}
	if err := envK8s.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, child); err != nil {
		t.Fatalf("get child: %v", err)
	}
	pending := &fleetv1.FleetAction{
		ObjectMeta: child.ObjectMeta,
		Spec:       child.Spec,
		Status:     fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhasePending},
	}
	pending.Namespace, pending.ResourceVersion, pending.UID = actionNS, "", ""
	cmd := &fakeCommander{}
	r, _ := newActionReconciler(t, pending, robotInPhase("r1", fleetv1.RobotPhaseIdle, ""))
	r.Commander = cmd
	reconcileAction(t, r, name)
	if len(cmd.assigns) != 1 {
		t.Fatalf("assign pushes = %d, want 1", len(cmd.assigns))
	}
	return cmd.assigns[0].Payload
}

func TestEnvtest_ManifestPayloadReachesTheChildAndTheAdapter(t *testing.T) {
	requireEnvtest(t)
	e := newFTEnv(t)
	task := applyManifest(t, e.ns, payloadTaskManifest)
	written, _, err := unstructured.NestedSlice(task.Object, "spec", "actions")
	if err != nil || len(written) != 1 {
		t.Fatalf("manifest actions: %v", err)
	}
	wantPayload := written[0].(map[string]any)["action"].(map[string]any)["payload"].(map[string]any)

	e.reconcile("route-patrol")

	name, got := childPayload(t, e.ns, "route-patrol")
	if !jsonEqualAny(t, got, wantPayload) {
		t.Fatalf("child %s spec.payload = %v, want the member's payload %v", name, got, wantPayload)
	}
	wire := assignedPayload(t, e.ns, name)
	var onWire map[string]any
	if err := json.Unmarshal(wire, &onWire); err != nil {
		t.Fatalf("payload_json %q is not JSON: %v", wire, err)
	}
	if !jsonEqualAny(t, onWire, wantPayload) {
		t.Errorf("payload_json = %s, want the manifest's payload %v", wire, wantPayload)
	}
}

func TestEnvtest_RawFormPayloadKeepsItsMeaning(t *testing.T) {
	requireEnvtest(t)
	e := newFTEnv(t)
	applyManifest(t, e.ns, rawFormTaskManifest)
	e.reconcile("raw-form")

	name, got := childPayload(t, e.ns, "raw-form")
	if !jsonEqualAny(t, got, map[string]any{"raw": "eyJ4IjoxLjV9"}) {
		t.Errorf("child %s spec.payload = %v, want the raw form as written", name, got)
	}
	if wire := assignedPayload(t, e.ns, name); string(wire) != `{"x":1.5}` {
		t.Errorf("payload_json = %q, want the decoded document {\"x\":1.5}", wire)
	}
}

func jsonEqualAny(t *testing.T, a, b any) bool {
	t.Helper()
	ab, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	bb, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(ab) == string(bb)
}
