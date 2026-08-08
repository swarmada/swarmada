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

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/cli"
)

// `swarmctl modelpolicy reset` — the operator recovery path for a FailedRepeatedly suspension
// (§9.1.9.4). Before this existed, clearing a suspension meant hand-editing status, which bypassed
// both the RBAC gate and the audit trail; the CLI is the enforcement point, as for admit/reject.
//
// The property that matters most is the DENIED case: the verb gate must run before anything is
// read or written, so an unauthorised caller cannot clear a suspension and cannot learn whether the
// policy exists.

const mpNS = "warehouse-a"

func suspendedPolicy(name string) *fleetv1.ModelPolicy {
	return &fleetv1.ModelPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: mpNS},
	}
}

// mpOptions builds options with captured streams; --yes is passed in every case so the
// confirmation prompt never blocks.
func mpOptions() *options {
	var out bytes.Buffer
	return &options{streams: cli.IOStreams{In: strings.NewReader(""), Out: &out, Err: &out},
		outputFmt: cli.OutputTable}
}

func mpClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := fleetv1.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func getPolicy(t *testing.T, c client.Client, name string) *fleetv1.ModelPolicy {
	t.Helper()
	var mp fleetv1.ModelPolicy
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: mpNS, Name: name}, &mp); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	return &mp
}

// With the verb granted, the reset request is written for the controller to act on.
func TestModelPolicyReset_AuthorizedWritesRequest(t *testing.T) {
	o := mpOptions()
	c := mpClient(t, suspendedPolicy("nav-gate"))

	if err := o.modelPolicyReset(context.Background(), c, authorizer(true), mpNS,
		"nav-gate", "metrics regression fixed", true); err != nil {
		t.Fatalf("authorized reset must succeed: %v", err)
	}
	got := getPolicy(t, c, "nav-gate").Annotations[policyResetAnnotation]
	if got != "metrics regression fixed" {
		t.Errorf("reset annotation = %q, want the operator's reason", got)
	}
}

// Denied: no write at all. This is the whole point of the SAR gate.
func TestModelPolicyReset_DeniedWritesNothing(t *testing.T) {
	o := mpOptions()
	c := mpClient(t, suspendedPolicy("nav-gate"))

	err := o.modelPolicyReset(context.Background(), c, authorizer(false), mpNS,
		"nav-gate", "let me in", true)
	if err == nil {
		t.Fatal("a caller without the policy-reset verb must be refused")
	}
	if _, present := getPolicy(t, c, "nav-gate").Annotations[policyResetAnnotation]; present {
		t.Error("a denied reset must not write the request annotation")
	}
}

// The gate runs BEFORE the Get, so a denied caller cannot probe for existence: resetting a
// nonexistent policy must fail with the authorization error, not a not-found.
func TestModelPolicyReset_DeniedBeforeLookup(t *testing.T) {
	o := mpOptions()
	c := mpClient(t) // no policies at all

	err := o.modelPolicyReset(context.Background(), c, authorizer(false), mpNS,
		"does-not-exist", "probe", true)
	if err == nil {
		t.Fatal("must be refused")
	}
	if strings.Contains(strings.ToLower(err.Error()), "not found") {
		t.Errorf("denied caller learned the policy does not exist: %v", err)
	}
}

// An empty --reason still produces a request, so the operator is never blocked by a missing
// annotation value; the controller keys on the value CHANGING.
func TestModelPolicyReset_EmptyReasonGetsDefault(t *testing.T) {
	o := mpOptions()
	c := mpClient(t, suspendedPolicy("nav-gate"))

	if err := o.modelPolicyReset(context.Background(), c, authorizer(true), mpNS,
		"nav-gate", "", true); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if got := getPolicy(t, c, "nav-gate").Annotations[policyResetAnnotation]; got == "" {
		t.Error("an empty reason must still write a non-empty request value")
	}
}

// A missing policy is a clear error for an AUTHORIZED caller (contrast with the denied case above).
func TestModelPolicyReset_MissingPolicyReported(t *testing.T) {
	o := mpOptions()
	c := mpClient(t)

	err := o.modelPolicyReset(context.Background(), c, authorizer(true), mpNS, "ghost", "x", true)
	if err == nil {
		t.Fatal("resetting a nonexistent policy must error")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error = %q, want it to name the policy", err.Error())
	}
}
