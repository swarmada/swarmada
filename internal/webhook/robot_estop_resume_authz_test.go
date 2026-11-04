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
	"strings"
	"testing"
)

// The operator's request to release a robot from the post-clear estop window
// (RFC-0001 #safety-estop-state-machine) is the release decision itself, so writing
// it takes the admin-only estop-clear verb, exactly as the clear does. The key is
// spelled out: it is what an operator writes with kubectl.
const resumeRequestAnnotation = "swarmada.io/estop-resume"

func robotWithResumeRequest(value string) map[string]string {
	if value == "" {
		return nil
	}
	return map[string]string{resumeRequestAnnotation: value}
}

func TestRobotEstopResume_RequestRequiresClearVerb(t *testing.T) {
	for _, tc := range []struct{ name, old, new string }{
		{"added", "", "inspected"},
		{"re-valued", "first", "second"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			authz := &fakeEstopAuthz{allow: true}
			g := estopGate(t, authz)
			oldR, newR := robotForEstop("amr-1", ""), robotForEstop("amr-1", "")
			oldR.Annotations, newR.Annotations = robotWithResumeRequest(tc.old), robotWithResumeRequest(tc.new)
			if _, err := g.ValidateUpdate(ctxAsUser("admin"), oldR, newR); err != nil {
				t.Fatalf("authorized resume request should be admitted, got: %v", err)
			}
			if authz.calls != 1 || authz.gotVerb != estopClearVerb {
				t.Fatalf("authorizer calls = %d, verb = %q; want one %q check", authz.calls, authz.gotVerb, estopClearVerb)
			}
		})
	}
}

func TestRobotEstopResume_UnauthorizedRequestDenied(t *testing.T) {
	g := estopGate(t, &fakeEstopAuthz{allow: false})
	oldR, newR := robotForEstop("amr-1", ""), robotForEstop("amr-1", "")
	newR.Annotations = robotWithResumeRequest("inspected")
	_, err := g.ValidateUpdate(ctxAsUser("mallory"), oldR, newR)
	if err == nil || !strings.Contains(err.Error(), `"estop-clear"`) {
		t.Fatalf("a resume request from a user without estop-clear must be denied for estop-clear, got: %v", err)
	}
}

func TestRobotEstopResume_RequestOnCreateRequiresClearVerb(t *testing.T) {
	g := estopGate(t, &fakeEstopAuthz{allow: false})
	r := robotForEstop("amr-1", "")
	r.Annotations = robotWithResumeRequest("inspected")
	_, err := g.ValidateCreate(ctxAsUser("mallory"), r)
	if err == nil || !strings.Contains(err.Error(), `"estop-clear"`) {
		t.Fatalf("a Robot created carrying a resume request by a user without estop-clear must be denied "+
			"for estop-clear, got: %v", err)
	}
}

// Removing a request releases nothing, so it is not gated: the controller's own
// bookkeeping and an operator withdrawing a request both go through.
func TestRobotEstopResume_RemovingARequestIsNotGated(t *testing.T) {
	authz := &fakeEstopAuthz{allow: false}
	g := estopGate(t, authz)
	oldR, newR := robotForEstop("amr-1", ""), robotForEstop("amr-1", "")
	oldR.Annotations = robotWithResumeRequest("inspected")
	if _, err := g.ValidateUpdate(ctxAsUser("alice"), oldR, newR); err != nil {
		t.Fatalf("removing a resume request must not be gated, got: %v", err)
	}
	if authz.calls != 0 {
		t.Errorf("authorizer called %d times on a removal, want 0", authz.calls)
	}
}

// The Robot defaulter stamps the operator who wrote a resume request, so
// ESTOP_RESUME_CONFIRMED names the person who released the robot.
func TestRobotEstopResume_DefaulterStampsTheRequester(t *testing.T) {
	d := newRobotIDDefaulter(t)
	robot := robotForEstop("amr-1", "")
	robot.Annotations = robotWithResumeRequest("inspected")
	if err := d.Default(ctxCreate("alice"), robot); err != nil {
		t.Fatalf("Default: %v", err)
	}
	if got := robot.Annotations[AnnEstopActor]; got != "alice" {
		t.Fatalf("estop-actor = %q on a resume request, want alice", got)
	}
}
