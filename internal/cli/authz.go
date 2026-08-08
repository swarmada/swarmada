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

package cli

import (
	"context"
	"fmt"

	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// swarmadaGroup is the API group every Swarmada custom verb is authorized under.
const swarmadaGroup = "swarmada.io"

// RequireVerb enforces a Swarmada custom verb (admit, reject, cancel,
// estop-trigger, estop-clear — RFC-0001 §9.5.3) before its action runs. It posts
// a SelfSubjectAccessReview for the verb against the target resource and returns
// a structured error unless the API server explicitly allows it.
//
// This is the RBAC gate that makes a custom verb meaningful on a CRD: the action
// is performed only after the server confirms the caller holds the *verb* grant,
// never as a bare patch that would ride on a broader update permission. It fails
// closed (§9.5.3): any error contacting the authorizer, or a non-allow verdict,
// denies the action.
func RequireVerb(ctx context.Context, cs kubernetes.Interface, verb, resource, namespace, name string) error {
	ssar := &authzv1.SelfSubjectAccessReview{
		Spec: authzv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authzv1.ResourceAttributes{
				Group:     swarmadaGroup,
				Resource:  resource,
				Verb:      verb,
				Namespace: namespace,
				Name:      name,
			},
		},
	}
	res, err := cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, ssar, metav1.CreateOptions{})
	if err != nil {
		// Fail closed: an authorizer we cannot reach is treated as a denial.
		return fmt.Errorf("authorization check for verb %q on %s failed: %w", verb, resource, err)
	}
	if res.Status.Allowed {
		return nil
	}
	msg := fmt.Sprintf("not authorized to %s %s/%s in namespace %q: requires the %q verb on %s.%s (RFC-0001 §9.5.3)",
		verb, singularOf(resource), name, namespace, verb, resource, swarmadaGroup)
	if res.Status.Reason != "" {
		msg += " — " + res.Status.Reason
	}
	if res.Status.EvaluationError != "" {
		msg += " (" + res.Status.EvaluationError + ")"
	}
	return fmt.Errorf("%s", msg)
}

// singularOf gives a readable singular for an error message; it is presentation
// only and falls back to the plural if it does not recognize the suffix.
func singularOf(resource string) string {
	switch resource {
	case "discoveredrobots":
		return "discoveredrobot"
	case "fleetactions":
		return "fleetaction"
	case "fleetzones":
		return "fleetzone"
	case "robots":
		return "robot"
	default:
		return resource
	}
}
