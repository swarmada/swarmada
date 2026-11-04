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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// The estop idempotence marker guard.
//
// Zone and namespace estop fan-out is made idempotent by swarmada.io/estop-processed,
// which records the estop-trigger value the controller has already acted on. The
// estop-trigger and estop-clear verbs are SubjectAccessReview-gated; the marker was
// not, so any principal holding ordinary `update` on the carrier could write the
// marker ahead of a trigger. The trigger was then read as already handled and the
// fan-out never ran — silently, with no estop delivered and no audit entry, because
// the suppression happens before the path that would record one.
//
// The marker is a control-plane bookkeeping field, not an operator-facing one: only
// the controller has any business writing it. This guard refuses every other write
// of it, which is what takes estop-trigger blast radius back out of `update`.
//
// It belongs in the VALIDATING webhooks (failurePolicy=Fail), not the estop-actor
// mutators (failurePolicy=Ignore): a guard that an unreachable webhook server
// silently disables is not a guard. Refusing an admission request is a decision, not
// a failure, so failurePolicy does not weaken it.

const (
	// estopProcessedAnnotation mirrors the controllers' annEstopProcessed
	// (internal/controller/zoneestop_controller.go). It is the write this guards.
	estopProcessedAnnotation = "swarmada.io/estop-processed"

	// serviceAccountUserPrefix is the username form the API server gives a
	// ServiceAccount principal.
	serviceAccountUserPrefix = "system:serviceaccount:"

	// InClusterTokenPath is where the kubelet mounts the pod's own ServiceAccount token.
	InClusterTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
)

// controllerPrincipal is the one username the guard admits a marker write from: the
// exact principal the API server authenticates this manager as,
// system:serviceaccount:<the namespace it runs in>:<the ServiceAccount it runs as>.
//
// It is read from the manager's own credentials rather than written down here, because
// a name written down is right for one install. A fixed "swarmada-manager" in any
// namespace was both too wide and too narrow: anyone able to create a ServiceAccount of
// that name in any namespace could forge the marker, and a Helm release not named
// "swarmada" renders "<fullname>-manager", which was refused, so the controller's own
// estop-clear marker delete was refused and the clear stalled.
//
// Unset, nothing matches and every marker write is refused. That is the closed direction,
// and cmd/manager logs it at startup, because it also refuses the controller.
var controllerPrincipal atomic.Pointer[string]

// SetControllerPrincipal records the manager's own ServiceAccount username. It refuses
// anything that is not a well-formed system:serviceaccount:<ns>:<name>.
func SetControllerPrincipal(username string) error {
	if !isServiceAccountUsername(username) {
		return fmt.Errorf("%q is not a ServiceAccount username (want %s<namespace>:<name>)",
			username, serviceAccountUserPrefix)
	}
	controllerPrincipal.Store(&username)
	return nil
}

// ControllerPrincipalFromToken returns the subject of the ServiceAccount token at path,
// which is the username the API server authenticates the bearer as. The token is this
// process's own mounted credential; only its subject is read, and nothing is authorized
// on the strength of the token's contents.
func ControllerPrincipalFromToken(path string) (string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the kubelet's fixed token mount, or a test fixture; never request input
	if err != nil {
		return "", fmt.Errorf("reading the manager's ServiceAccount token: %w", err)
	}
	parts := strings.Split(strings.TrimSpace(string(raw)), ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("the token at %s is not a JWT", path)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decoding the token payload: %w", err)
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("parsing the token payload: %w", err)
	}
	if !isServiceAccountUsername(claims.Sub) {
		return "", fmt.Errorf("the token subject %q is not a ServiceAccount username", claims.Sub)
	}
	return claims.Sub, nil
}

// GuardEstopProcessedMarker refuses any write of swarmada.io/estop-processed that
// does not come from the control-plane controller. oldAnn is nil on a create.
//
// It is exported because its call sites are the three carriers' validating
// webhooks — FleetZoneValidator, RobotAdmissionGate and SwarmadaConfigValidator —
// which are outside this change's file ownership and are wired up separately; see
// the wiring report. Nothing calls it until those three lines land, and until
// they do the Security Model's disclosure of this gap stands.
//
// It fails closed in every direction: an admission request that carries no user
// (a route the webhook cannot attribute) is refused, as is any principal that is
// not the controller's ServiceAccount. Refusing the CONTROLLER's own write would
// not lose an estop — the fan-out would simply re-run and re-deliver it — so the
// closed direction is the safe one here as well.
//
// A write that leaves the marker unchanged is not a marker write and is admitted:
// the guard must not turn every unrelated update of an estopped zone into a denial.
func GuardEstopProcessedMarker(ctx context.Context, gr schema.GroupResource, namespace, name string, oldAnn, newAnn map[string]string) error {
	oldVal, had := oldAnn[estopProcessedAnnotation]
	newVal, has := newAnn[estopProcessedAnnotation]
	if had == has && oldVal == newVal {
		return nil // the marker is untouched by this request
	}

	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return apierrors.NewForbidden(gr, name,
			fmt.Errorf("cannot attribute a write of %q: no admission request in context", estopProcessedAnnotation))
	}
	if isControllerServiceAccount(req.UserInfo.Username) {
		return nil
	}
	return apierrors.NewForbidden(gr, name,
		fmt.Errorf("user %q may not write %q on %s %q in namespace %q: the estop idempotence marker is written only by "+
			"the control plane, and a client write of it suppresses an estop fan-out (RFC-0001 Security Model)",
			req.UserInfo.Username, estopProcessedAnnotation, gr.Resource, name, namespace))
}

// isControllerServiceAccount reports whether username is exactly the principal this
// manager runs as. Same name in another namespace, or a similar name, is not.
func isControllerServiceAccount(username string) bool {
	p := controllerPrincipal.Load()
	return p != nil && username == *p
}

// isServiceAccountUsername reports whether username has the form
// system:serviceaccount:<namespace>:<name> with both parts non-empty.
func isServiceAccountUsername(username string) bool {
	rest, ok := strings.CutPrefix(username, serviceAccountUserPrefix)
	if !ok {
		return false
	}
	ns, sa, ok := strings.Cut(rest, ":")
	return ok && ns != "" && sa != "" && !strings.Contains(sa, ":")
}
