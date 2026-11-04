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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// The FleetZone delete check refuses a delete that would leave something referring to a zone
// that no longer exists (RFC-0001 #fleetzone-deleting-a-zone). These tests cover the rule in
// isolation; fleetzone_delete_guard_envtest_test.go covers it over the wire.

func dgScheme(t *testing.T, withCore bool) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	adds := []func(*runtime.Scheme) error{fleetv1.AddToScheme}
	if withCore {
		adds = append(adds, corev1.AddToScheme)
	}
	for _, add := range adds {
		if err := add(scheme); err != nil {
			t.Fatalf("add scheme: %v", err)
		}
	}
	return scheme
}

func dgNamespace(terminating bool) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: zoneNS}}
	if terminating {
		now := metav1.Now()
		ns.DeletionTimestamp = &now
		ns.Finalizers = []string{"kubernetes"}
	}
	return ns
}

func dgRobot(name, declared, current string) *fleetv1.Robot {
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Namespace: zoneNS, Name: name},
		Spec:       fleetv1.RobotSpec{Zone: declared},
		Status:     fleetv1.RobotStatus{CurrentZone: current},
	}
}

func dgAction(name, zone string, phase fleetv1.ActionPhase) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Namespace: zoneNS, Name: name},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Zone: zone},
		Status:     fleetv1.FleetActionStatus{Phase: phase},
	}
}

// dgValidator builds a validator whose APIReader holds objs and whose cached Client holds
// nothing, so a read that went to the cache would find no reference at all.
func dgValidator(t *testing.T, objs ...client.Object) *FleetZoneValidator {
	t.Helper()
	scheme := dgScheme(t, true)
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	emptyCache := fake.NewClientBuilder().WithScheme(scheme).Build()
	return &FleetZoneValidator{Client: emptyCache, APIReader: reader}
}

func dgDelete(t *testing.T, v *FleetZoneValidator, name string) error {
	t.Helper()
	_, err := v.ValidateDelete(context.Background(), zoneWithParent(name, ""))
	return err
}

// Each kind of reference refuses the delete on its own, as Invalid, and the message names the
// kind of reference and the object that holds it. Every reference lives only in the
// APIReader, so this also shows the check does not read the cache.
func TestFleetZoneDelete_EachReferenceRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		objs []client.Object
		want []string
	}{
		{"child", []client.Object{zoneWithParent("dock", ""), zoneWithParent("dock-bay-1", "dock")},
			[]string{`FleetZone "dock" has child zones [dock-bay-1] (spec.parentZone)`}},
		{"declared", []client.Object{dgRobot("amr-1", "dock", "")},
			[]string{`FleetZone "dock" is declared by robots [amr-1] (spec.zone)`, "re-home or delete"}},
		{"present", []client.Object{dgRobot("amr-2", "aisle", "dock")},
			[]string{`FleetZone "dock" has robots present in it [amr-2] (status.currentZone)`}},
		{"targeted", []client.Object{dgAction("fetch-1", "dock", fleetv1.ActionPhaseInProgress)},
			[]string{`FleetZone "dock" is targeted by FleetActions that can still run [fetch-1] (spec.zone)`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := dgValidator(t, append(tc.objs, dgNamespace(false))...)
			err := dgDelete(t, v, "dock")
			if err == nil {
				t.Fatalf("the delete of zone dock was admitted while a %s reference holds it", tc.name)
			}
			if !apierrors.IsInvalid(err) {
				t.Errorf("want Invalid, got %T: %v", err, err)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("the refusal does not say %q; got: %v", w, err)
				}
			}
		})
	}
}

// The present check is not implied by the declared check. A robot declared in aisle can be
// inside dock's polygon, so its currentZone names dock while its spec.zone does not. Only the
// present check sees it; a guard with the present check removed admits this delete.
func TestFleetZoneDelete_PresentRobotDeclaredElsewhereRefuses(t *testing.T) {
	v := dgValidator(t, dgNamespace(false), dgRobot("amr-2", "aisle", "dock"))
	err := dgDelete(t, v, "dock")
	if err == nil {
		t.Fatal("zone dock was deletable while robot amr-2 is placed in it (status.currentZone: dock). " +
			"Its spec.zone names aisle, so only the present check can refuse this delete.")
	}
	if !strings.Contains(err.Error(), "present") || !strings.Contains(err.Error(), "amr-2") {
		t.Errorf("the refusal should name the present reference and robot amr-2, got: %v", err)
	}
	if strings.Contains(err.Error(), "declared by") {
		t.Errorf("amr-2 does not declare dock, but the refusal says it does: %v", err)
	}
}

// Every reference is reported, not only the first one found.
func TestFleetZoneDelete_ReportsEveryReference(t *testing.T) {
	v := dgValidator(t, dgNamespace(false),
		zoneWithParent("dock-bay-1", "dock"),
		dgRobot("amr-1", "dock", "dock"),
		dgAction("fetch-1", "dock", fleetv1.ActionPhasePending))
	err := dgDelete(t, v, "dock")
	if err == nil {
		t.Fatal("zone dock was deletable with four references to it")
	}
	for _, w := range []string{"child zones", "declared by", "present in it", "targeted by"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("the refusal leaves out the %q reference: %v", w, err)
		}
	}
}

// A zone nothing refers to is deleted as before. References to OTHER zones do not count.
func TestFleetZoneDelete_UnreferencedZoneAdmitted(t *testing.T) {
	v := dgValidator(t, dgNamespace(false),
		zoneWithParent("aisle-bay-1", "aisle"),
		dgRobot("amr-1", "aisle", "aisle"),
		dgAction("fetch-1", "aisle", fleetv1.ActionPhaseInProgress))
	if err := dgDelete(t, v, "dock"); err != nil {
		t.Fatalf("zone dock has no references and must be deletable: %v", err)
	}
}

// The terminal set comes from the FleetAction state machine. Succeeded and Cancelled have no
// outgoing edge. Failed has one, back to Pending, only while the action can still requeue.
// Every other phase can still run, including the empty phase of an unreconciled action.
func TestFleetZoneDelete_ActionPhases(t *testing.T) {
	scheduled := metav1.Now()
	failed := func(policy fleetv1.ActionFailurePolicy, assigned bool, retries int32, rp *fleetv1.ActionRetryPolicy) *fleetv1.FleetAction {
		a := dgAction("fetch-1", "dock", fleetv1.ActionPhaseFailed)
		a.Spec.OnFailure = policy
		a.Spec.RetryPolicy = rp
		a.Status.RetryCount = retries
		if assigned {
			a.Status.ScheduledAt = &scheduled
		}
		return a
	}
	for _, tc := range []struct {
		name   string
		action *fleetv1.FleetAction
		blocks bool
	}{
		{"unset phase", dgAction("fetch-1", "dock", ""), true},
		{"Pending", dgAction("fetch-1", "dock", fleetv1.ActionPhasePending), true},
		{"Assigned", dgAction("fetch-1", "dock", fleetv1.ActionPhaseAssigned), true},
		{"InProgress", dgAction("fetch-1", "dock", fleetv1.ActionPhaseInProgress), true},
		{"Paused", dgAction("fetch-1", "dock", fleetv1.ActionPhasePaused), true},
		{"Revoking", dgAction("fetch-1", "dock", fleetv1.ActionPhaseRevoking), true},
		{"Preempted", dgAction("fetch-1", "dock", fleetv1.ActionPhasePreempted), true},
		{"Succeeded", dgAction("fetch-1", "dock", fleetv1.ActionPhaseSucceeded), false},
		{"Cancelled", dgAction("fetch-1", "dock", fleetv1.ActionPhaseCancelled), false},
		{"Failed, Requeue, assigned, retries left", failed(fleetv1.ActionFailureRequeue, true, 0, nil), true},
		{"Failed, onFailure unset, assigned, retries left", failed("", true, 2, nil), true},
		{"Failed, Requeue, assigned, default retries used up", failed(fleetv1.ActionFailureRequeue, true, 3, nil), false},
		{"Failed, Requeue, assigned, own retries left", failed(fleetv1.ActionFailureRequeue, true, 3,
			&fleetv1.ActionRetryPolicy{MaxRetries: 5}), true},
		{"Failed, Requeue, assigned, own retries used up", failed(fleetv1.ActionFailureRequeue, true, 1,
			&fleetv1.ActionRetryPolicy{MaxRetries: 1}), false},
		{"Failed, Requeue, never assigned", failed(fleetv1.ActionFailureRequeue, false, 0, nil), false},
		{"Failed, Alert", failed(fleetv1.ActionFailureAlert, true, 0, nil), false},
		{"Failed, Abandon", failed(fleetv1.ActionFailureAbandon, true, 0, nil), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := dgValidator(t, dgNamespace(false), tc.action)
			err := dgDelete(t, v, "dock")
			switch {
			case tc.blocks && err == nil:
				t.Fatal("an action that can still run in zone dock did not block its delete")
			case tc.blocks && !strings.Contains(err.Error(), "fetch-1"):
				t.Errorf("refused, but the refusal does not name fetch-1: %v", err)
			case !tc.blocks && err != nil:
				t.Fatalf("a finished action must not block the delete: %v", err)
			}
		})
	}
}

// Namespace teardown is exempt: every kind of reference is present, and the delete is
// admitted because the namespace is being deleted.
func TestFleetZoneDelete_NamespaceTeardownExempt(t *testing.T) {
	refs := []client.Object{
		zoneWithParent("dock-bay-1", "dock"),
		dgRobot("amr-1", "dock", "dock"),
		dgAction("fetch-1", "dock", fleetv1.ActionPhaseInProgress),
	}
	if err := dgDelete(t, dgValidator(t, append(refs, dgNamespace(false))...), "dock"); err == nil {
		t.Fatal("control failed: the references did not refuse the delete in a namespace that is staying")
	}
	if err := dgDelete(t, dgValidator(t, append(refs, dgNamespace(true))...), "dock"); err != nil {
		t.Fatalf("the delete was refused during namespace teardown, which would hold the namespace "+
			"in Terminating: %v", err)
	}
}

// A namespace that cannot be read fails closed.
func TestFleetZoneDelete_UnreadableNamespaceFailsClosed(t *testing.T) {
	// No core types in the scheme, so reading the Namespace errors.
	reader := fake.NewClientBuilder().WithScheme(dgScheme(t, false)).
		WithObjects(dgRobot("amr-1", "dock", "")).Build()
	v := &FleetZoneValidator{Client: reader, APIReader: reader}
	if err := dgDelete(t, v, "dock"); err == nil {
		t.Fatal("the delete was admitted although the teardown check could not read the namespace")
	}
}

// With no APIReader the check cannot run, and a delete it cannot check is refused.
func TestFleetZoneDelete_NoAPIReaderFailsClosed(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(dgScheme(t, true)).WithObjects(dgNamespace(false)).Build()
	v := &FleetZoneValidator{Client: c}
	err := dgDelete(t, v, "dock")
	if err == nil {
		t.Fatal("a validator with no APIReader admitted a delete it could not check")
	}
	if !apierrors.IsInternalError(err) {
		t.Errorf("want InternalError, got %T: %v", err, err)
	}
}

// A refusal names at most maxNamedReferences objects and counts the rest.
func TestFleetZoneDelete_NamedReferencesCapped(t *testing.T) {
	objs := []client.Object{dgNamespace(false)}
	for i := 0; i < maxNamedReferences+3; i++ {
		objs = append(objs, dgRobot(fmt.Sprintf("amr-%02d", i), "dock", ""))
	}
	err := dgDelete(t, dgValidator(t, objs...), "dock")
	if err == nil {
		t.Fatal("zone dock was deletable with thirteen robots declaring it")
	}
	if !strings.Contains(err.Error(), "and 3 more") || strings.Contains(err.Error(), "amr-12") {
		t.Errorf("want the first %d names and a count of the rest, got: %v", maxNamedReferences, err)
	}
}

// defaultActionMaxRetries must equal the RetryPolicy.maxRetries default in the generated CRD.
// If the default moves and this constant does not, a Failed action that will still requeue
// stops blocking the delete of its zone.
func TestFleetZoneDelete_DefaultMaxRetriesMatchesCRD(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "swarmada.io_fleetactions.yaml"))
	if err != nil {
		t.Fatalf("reading the FleetAction CRD: %v", err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema struct {
						Properties struct {
							Spec struct {
								Properties struct {
									RetryPolicy struct {
										Properties struct {
											MaxRetries struct {
												Default *int32 `json:"default"`
											} `json:"maxRetries"`
										} `json:"properties"`
									} `json:"retryPolicy"`
								} `json:"properties"`
							} `json:"spec"`
						} `json:"properties"`
					} `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := utilyaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("parsing the FleetAction CRD: %v", err)
	}
	if len(crd.Spec.Versions) == 0 {
		t.Fatal("the FleetAction CRD has no versions")
	}
	got := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties.Spec.Properties.RetryPolicy.Properties.MaxRetries.Default
	if got == nil {
		t.Fatal("the FleetAction CRD declares no spec.retryPolicy.maxRetries default")
	}
	if *got != defaultActionMaxRetries {
		t.Fatalf("the CRD defaults spec.retryPolicy.maxRetries to %d; defaultActionMaxRetries is %d",
			*got, defaultActionMaxRetries)
	}
}
