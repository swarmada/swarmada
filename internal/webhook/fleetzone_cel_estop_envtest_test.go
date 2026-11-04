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
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// celCase is a FleetZone spec that breaks exactly one of the CRD's CEL rules, and a
// fragment of that rule's message.
type celCase struct {
	name string
	says string
	spec fleetv1.FleetZoneSpec
}

func fleetZoneCELCases() []celCase {
	ops := func(o ...fleetv1.ZoneOperatingRestriction) fleetv1.FleetZoneSpec {
		return fleetv1.FleetZoneSpec{Restrictions: &fleetv1.ZoneRestrictions{Operating: o}}
	}
	return []celCase{
		{"composite-shared-resources", "spec.sharedResources is not permitted on a composite zone",
			fleetv1.FleetZoneSpec{MemberZones: []string{"room-a"},
				SharedResources: []fleetv1.SharedResource{{Name: "dock-1", Type: fleetv1.SharedResourceType("ChargingDock")}}}},
		{"composite-capacity", "spec.maxConcurrentRobots must be 0 on a composite zone",
			fleetv1.FleetZoneSpec{MemberZones: []string{"room-a"}, MaxConcurrentRobots: 3}},
		{"capability-mode", "spec.restrictions.capabilities[].mode must be Denied",
			fleetv1.FleetZoneSpec{Restrictions: &fleetv1.ZoneRestrictions{Capabilities: []fleetv1.ZoneCapabilityRestriction{
				{Name: "lidar", Mode: fleetv1.ZoneRestrictionLimited}}}}},
		{"entry-mode", "spec.restrictions.entry.mode must be Denied",
			fleetv1.FleetZoneSpec{Restrictions: &fleetv1.ZoneRestrictions{Entry: &fleetv1.ZoneEntryRestriction{
				Mode: fleetv1.ZoneRestrictionLimited}}}},
		{"operating-limits-only-when-limited", "max and allowed are meaningful only when mode is Limited",
			ops(fleetv1.ZoneOperatingRestriction{Subject: "camera", Mode: fleetv1.ZoneRestrictionDenied, Max: "3"})},
		{"operating-limited-needs-one-limit", "set exactly one of max or allowed on a Limited rule",
			ops(fleetv1.ZoneOperatingRestriction{Subject: "camera", Mode: fleetv1.ZoneRestrictionLimited})},
		{"operating-subject", "spec.restrictions.operating[].subject must be one",
			ops(fleetv1.ZoneOperatingRestriction{Subject: "lasers", Mode: fleetv1.ZoneRestrictionDenied})},
	}
}

// stripCEL removes every x-kubernetes-validations rule from a schema, in place.
func stripCEL(s *apiextensionsv1.JSONSchemaProps) {
	if s == nil {
		return
	}
	s.XValidations = nil
	for k, p := range s.Properties {
		stripCEL(&p)
		s.Properties[k] = p
	}
	if s.Items != nil {
		stripCEL(s.Items.Schema)
	}
}

// TestEnvtest_FleetZoneCELRulesOnAnEstopOnlyUpdate establishes, for each CEL rule on the
// FleetZone CRD, whether it can refuse an update that changes nothing but the estop
// annotation. The API server runs these rules before any webhook, so the webhook's
// estop-only exemption cannot reach them.
//
// None of the rules compares old with new, and the estop write does not touch spec, so
// a rule can refuse it only when the stored spec already breaks it: an object stored
// before the rule was in the CRD. This test builds that state: the CRD is installed with
// its rules stripped, one zone per rule is stored breaking it, and the shipped CRD is
// then installed over it, as an upgrade would. Whether the API server re-evaluates a rule
// on a field the update did not change is its validation ratcheting, which depends on its
// version. Measured: v1.29.0 refuses the estop-only update under every rule; v1.31.0 and
// v1.37.0 admit it. The outcome is asserted for those versions; 1.30 is logged only.
func TestEnvtest_FleetZoneCELRulesOnAnEstopOnlyUpdate(t *testing.T) {
	requireWebhookEnvtest(t)
	ctx := context.Background()

	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting a second API server: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "swarmada.io_fleetzones.yaml"))
	if err != nil {
		t.Fatalf("reading the shipped FleetZone CRD: %v", err)
	}
	shipped := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal(raw, shipped); err != nil {
		t.Fatalf("parsing the shipped FleetZone CRD: %v", err)
	}
	older := shipped.DeepCopy()
	for i := range older.Spec.Versions {
		if s := older.Spec.Versions[i].Schema; s != nil {
			stripCEL(s.OpenAPIV3Schema)
		}
	}
	if _, err := envtest.InstallCRDs(cfg, envtest.CRDInstallOptions{
		CRDs: []*apiextensionsv1.CustomResourceDefinition{older},
	}); err != nil {
		t.Fatalf("installing the FleetZone CRD without its CEL rules: %v", err)
	}

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{fleetv1.AddToScheme, corev1.AddToScheme, apiextensionsv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatalf("scheme: %v", err)
		}
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "cel-"}}
	if err := c.Create(ctx, ns); err != nil {
		t.Fatalf("namespace: %v", err)
	}

	cases := fleetZoneCELCases()
	for _, tc := range cases {
		z := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: tc.name, Namespace: ns.Name}, Spec: tc.spec}
		if err := c.Create(ctx, z); err != nil {
			t.Fatalf("storing %s before its rule is in the CRD: %v", tc.name, err)
		}
	}

	// The upgrade: the shipped CRD, rules and all.
	cur := &apiextensionsv1.CustomResourceDefinition{}
	if err := c.Get(ctx, client.ObjectKey{Name: shipped.Name}, cur); err != nil {
		t.Fatalf("reading the installed CRD: %v", err)
	}
	cur.Spec.Versions = shipped.Spec.Versions
	if err := c.Update(ctx, cur); err != nil {
		t.Fatalf("installing the shipped FleetZone CRD over the stored zones: %v", err)
	}
	probe := 0
	eventually(t, "the API server enforcing the shipped CEL rules", func() (bool, string) {
		probe++
		z := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("probe-%d", probe), Namespace: ns.Name},
			Spec: cases[0].spec}
		err := c.Create(ctx, z)
		if err == nil {
			_ = c.Delete(ctx, z)
			return false, "a zone breaking the first rule was still admitted"
		}
		return strings.Contains(err.Error(), cases[0].says), err.Error()
	})

	info, err := discovery.NewDiscoveryClientForConfigOrDie(cfg).ServerVersion()
	if err != nil {
		t.Fatalf("server version: %v", err)
	}
	minor, _ := strconv.Atoi(strings.TrimRight(info.Minor, "+"))
	t.Logf("API server %s", info.GitVersion)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := patchFleetZone(ctx, c, ns.Name, tc.name, func(z *fleetv1.FleetZone) {
				z.Annotations[estopTriggeredAnnotation] = "true"
			})
			t.Logf("estop-only update of a zone stored before %q: %v", tc.says, err)
			switch {
			case minor <= 29:
				// No ratcheting: the rule is evaluated on the whole stored spec and refuses
				// the stop. The webhook's exemption cannot reach it.
				if err == nil || !strings.Contains(err.Error(), tc.says) {
					t.Fatalf("on %s the estop-only update was expected to be refused by the rule "+
						"(%q); got %v", info.GitVersion, tc.says, err)
				}
			case minor >= 31:
				// Ratcheting: a field the update did not change is not re-checked.
				if err != nil {
					t.Fatalf("on %s the estop-only update was expected to be admitted (an "+
						"unchanged field is not re-checked); got %v", info.GitVersion, err)
				}
			default:
				t.Logf("%s: not measured when this test was written; outcome logged only", info.GitVersion)
			}
		})
	}
}
