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

package rbac

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Preflight verifies at BOOT that this process can actually read the resources it
// will need mid-request, and fails loudly if it cannot.
//
// WHY THIS EXISTS, given that a missing permission already produces an error at
// use. Two properties of how the read happens make that error nearly invisible:
//
//   - The reader is `mgr.GetClient()`, which serves typed Gets from the shared
//     informer cache. An informer with no `list`/`watch` cannot sync, so the first
//     read does not fail fast with a clean 403 — it blocks until cache-sync timeout
//     and then errors, inside an admission request.
//   - The G1 gate that performs the read fails OPEN on any non-NotFound error, on
//     purpose (an unreadable dependency must not block an otherwise valid write).
//     A missing grant therefore does not surface as a visible failure at all: it
//     silently disables the gate, and every FleetAction is admitted unchecked.
//
// A permission problem that degrades a validation gate to a no-op without saying so
// is exactly the class of failure this project treats as unacceptable elsewhere — a
// check believed to run that does not. The preflight converts it into a startup
// error naming the missing permission.
//
// It uses an UNCACHED reader (`mgr.GetAPIReader()`): the point is to test the
// permission itself, not to warm a cache, and a cached read here would hit the very
// sync problem being diagnosed.
//
// SCOPE. Only Forbidden is fatal. A missing CRD, a not-yet-registered type, or any
// other transport error is reported and tolerated — this is an RBAC preflight, not
// an installation check, and a deployment that has not installed the optional
// compute CRDs is a legitimate configuration, not a misconfigured one.
func Preflight(ctx context.Context, r client.Reader, namespace string) error {
	// Exactly the resources the manager reads of the compute configuration set, and
	// exactly the verb it reads them with. computeworkloadtemplates and
	// artifactstores are deliberately ABSENT: no code reachable from any shipped
	// binary reads them (their consumers in internal/compute and internal/egress are
	// libraries no cmd/ target imports), so granting them would be permission for a
	// read that never happens.
	checks := []struct {
		name string
		list client.ObjectList
	}{
		{"computeendpoints.swarmada.io", &fleetv1.ComputeEndpointList{}},
		{"resultsinks.swarmada.io", &fleetv1.ResultSinkList{}},
	}

	var forbidden []string
	for _, c := range checks {
		err := r.List(ctx, c.list, client.InNamespace(namespace), client.Limit(1))
		switch {
		case err == nil:
		case apierrors.IsForbidden(err):
			forbidden = append(forbidden, c.name)
		case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
			// CRD not installed. Not this function's business.
		default:
			// Transport or server-side problem. Not a permission verdict, so it is
			// not grounds to refuse to start.
		}
	}

	if len(forbidden) > 0 {
		return fmt.Errorf(
			"RBAC preflight failed: this ServiceAccount cannot list %v. "+
				"The FleetAction admission gate reads these to validate "+
				"spec.executor.endpointRef and spec.executor.sinks[]; without the grant it "+
				"fails open and admits external actions unchecked. "+
				"Grant get;list;watch on these resources (config/rbac/role.yaml)",
			forbidden)
	}
	return nil
}
