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
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

// Every test in this package that plays the controller does so as controllerUser, so
// that is the principal this test binary runs as. Tests that need another restore it.
func init() {
	if err := SetControllerPrincipal(controllerUser); err != nil {
		panic(err)
	}
}

func withPrincipal(t *testing.T, username string) {
	t.Helper()
	if err := SetControllerPrincipal(username); err != nil {
		t.Fatalf("SetControllerPrincipal(%q): %v", username, err)
	}
	t.Cleanup(func() { _ = SetControllerPrincipal(controllerUser) })
}

// A Helm release not named "swarmada" renders its manager ServiceAccount as
// <fullname>-manager. The guard must admit that manager's own marker writes, or the
// estop-clear marker delete is refused and the clear stalls; and it must still refuse
// the kustomize name, which on that install is somebody else.
func TestEstopMarkerGuard_HelmReleasePrincipal(t *testing.T) {
	const helmManager = "system:serviceaccount:robotics:prod-swarmada-manager"
	withPrincipal(t, helmManager)

	if err := GuardEstopProcessedMarker(ctxAsUser(helmManager), fleetZoneGR, zoneNS, "floor-1",
		markerAnn("e1"), nil); err != nil {
		t.Fatalf("the Helm-rendered manager must be able to drop the marker on an estop clear: %v", err)
	}
	for _, other := range []string{
		"system:serviceaccount:robotics:swarmada-manager",
		"system:serviceaccount:other:prod-swarmada-manager",
	} {
		if err := GuardEstopProcessedMarker(ctxAsUser(other), fleetZoneGR, zoneNS, "floor-1",
			nil, markerAnn("e1")); err == nil {
			t.Errorf("%q is not the manager on this install and must not write the marker", other)
		}
	}
}

// With no principal recorded nothing is the controller, including a principal that
// looks like one.
func TestEstopMarkerGuard_NoPrincipalRefusesEveryone(t *testing.T) {
	controllerPrincipal.Store(nil)
	t.Cleanup(func() { _ = SetControllerPrincipal(controllerUser) })
	if isControllerServiceAccount(controllerUser) {
		t.Fatal("with no principal recorded, no username may be treated as the controller")
	}
}

func TestSetControllerPrincipal_RejectsMalformed(t *testing.T) {
	for _, bad := range []string{
		"", "swarmada-manager", "system:serviceaccount:swarmada-system",
		"system:serviceaccount::swarmada-manager", "system:serviceaccount:ns:",
		"system:serviceaccount:ns:a:b", "system:node:worker-1",
	} {
		if err := SetControllerPrincipal(bad); err == nil {
			_ = SetControllerPrincipal(controllerUser)
			t.Errorf("SetControllerPrincipal(%q) accepted a malformed ServiceAccount username", bad)
		}
	}
}

// The principal comes from the manager's own mounted token: its subject is the username
// the API server authenticates the bearer as.
func TestControllerPrincipalFromToken(t *testing.T) {
	dir := t.TempDir()
	write := func(name, payload string) string {
		p := filepath.Join(dir, name)
		tok := "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".c2ln"
		if err := os.WriteFile(p, []byte(tok+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	good := write("good", `{"sub":"system:serviceaccount:robotics:prod-swarmada-manager"}`)
	got, err := ControllerPrincipalFromToken(good)
	if err != nil || got != "system:serviceaccount:robotics:prod-swarmada-manager" {
		t.Fatalf("ControllerPrincipalFromToken = %q, %v", got, err)
	}

	for name, payload := range map[string]string{
		"user-subject": `{"sub":"alice"}`,
		"no-subject":   `{}`,
		"not-json":     `not json`,
	} {
		if got, err := ControllerPrincipalFromToken(write(name, payload)); err == nil {
			t.Errorf("%s: accepted subject %q", name, got)
		}
	}
	if _, err := ControllerPrincipalFromToken(filepath.Join(dir, "absent")); err == nil {
		t.Error("a missing token must be an error, not an empty principal")
	}
}
