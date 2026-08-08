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

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Single-object admission rules from RFC-0001 §9.3.1, enforced by the CRD schema rather
// than by a controller. These run against a real API server (envtest) with the generated
// CRDs installed, because that is the only thing that proves a CEL rule or a pattern is
// actually in effect: a marker in a Go comment that failed to regenerate, or a rule the
// API server rejects as uncompilable, both look identical to a correct one when read.
//
// Each rule gets a rejection case AND an acceptance case. A rule that rejects everything
// passes a rejection-only test while making the resource unusable.

func mustReject(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("object was ACCEPTED; expected rejection mentioning %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("rejected, but not for the expected reason.\n  want substring: %s\n  got: %v", want, err)
	}
}

// ── Robot: a capability's providingModel must name an installed model ──────────────────

func TestRobotSchema_ProvidingModelMustBeInstalled(t *testing.T) {
	requireEnvtest(t)
	ns := envtestNamespace(t)
	ctx := context.Background()

	robot := func(model, provides string) *fleetv1.Robot {
		r := &fleetv1.Robot{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "amr-", Namespace: ns},
			Spec: fleetv1.RobotSpec{
				Zone:         "zone-a",
				Manufacturer: "acme",
				Model:        "amr-100",
				Adapter:      fleetv1.AdapterRef{Name: "acme-adapter", Version: "1.0.0"},
				Capabilities: []fleetv1.ClassCapability{{
					Name: "item-pick.ai-guided", Type: "model-driven", ProvidingModel: provides,
				}},
			},
		}
		if model != "" {
			r.Spec.InstalledModels = []fleetv1.ClassModel{{
				Name: model, Version: "1.0.0", ModelURI: "oci://example.test/models/" + model,
			}}
		}
		return r
	}

	// A capability pointing at a model this robot does not have would derive as permanently
	// inactive with no operator-visible reason — the model it names simply never appears.
	err := envK8s.Create(ctx, robot("item-recognition-v3", "item-recognition-v4"))
	mustReject(t, err, "providingModel must reference a model declared in spec.installedModels")

	// Same, with no installedModels at all — the empty-list path must not be a loophole.
	err = envK8s.Create(ctx, robot("", "item-recognition-v3"))
	mustReject(t, err, "providingModel must reference a model declared in spec.installedModels")

	// The matching case must be accepted, or the rule is just a ban on model-driven caps.
	if err := envK8s.Create(ctx, robot("item-recognition-v3", "item-recognition-v3")); err != nil {
		t.Fatalf("a capability naming an installed model must be accepted: %v", err)
	}
}

// ── FirmwareRollout: newVersion must be an orderable semver ────────────────────────────

func TestFirmwareRolloutSchema_NewVersionIsSemver(t *testing.T) {
	requireEnvtest(t)
	ns := envtestNamespace(t)
	ctx := context.Background()

	rollout := func(version string) *fleetv1.FirmwareRollout {
		return &fleetv1.FirmwareRollout{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "fw-", Namespace: ns},
			Spec: fleetv1.FirmwareRolloutSpec{
				TargetSelector:   metav1.LabelSelector{MatchLabels: map[string]string{"fleet": "a"}},
				NewVersion:       version,
				FirmwareURI:      "https://example.test/fw.bin",
				FirmwareChecksum: "sha256:" + strings.Repeat("a", 64),
			},
		}
	}

	// Each of these reads as a version to a human and orders as nothing to a comparator, so
	// admitting them defers the failure to batch selection — after the operator believes the
	// rollout is scheduled.
	for _, bad := range []string{"latest", "v2.1.0", "2.1", "2.1.0-rc1", "2.1.0+build7", ""} {
		if err := envK8s.Create(ctx, rollout(bad)); err == nil {
			t.Fatalf("newVersion %q was accepted; it is not an orderable major.minor.patch", bad)
		}
	}
	if err := envK8s.Create(ctx, rollout("2.1.0")); err != nil {
		t.Fatalf("a plain semver must be accepted: %v", err)
	}
	// Multi-digit components are ordinary, not an edge case to exclude.
	if err := envK8s.Create(ctx, rollout("10.20.30")); err != nil {
		t.Fatalf("multi-digit semver must be accepted: %v", err)
	}
}

// ── ModelRollout: grants and revokes must be disjoint ──────────────────────────────────

func TestModelRolloutSchema_GrantsAndRevokesAreDisjoint(t *testing.T) {
	requireEnvtest(t)
	ns := envtestNamespace(t)
	ctx := context.Background()

	rollout := func(grants, revokes []string) *fleetv1.ModelRollout {
		return &fleetv1.ModelRollout{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "mr-", Namespace: ns},
			Spec: fleetv1.ModelRolloutSpec{
				TargetSelector:      metav1.LabelSelector{MatchLabels: map[string]string{"fleet": "a"}},
				ModelName:           "item-recognition",
				NewVersion:          "4.0.0",
				ModelURI:            "oci://example.test/models/item-recognition",
				ModelChecksum:       "sha256:" + strings.Repeat("b", 64),
				GrantsCapabilities:  grants,
				RevokesCapabilities: revokes,
			},
		}
	}

	const msg = "must not appear in both grantsCapabilities and revokesCapabilities"

	// The overlap is the whole point: whichever list the controller applied second would
	// decide the outcome, making it an artefact of iteration order.
	err := envK8s.Create(ctx, rollout([]string{"item-pick.ai-guided"}, []string{"item-pick.ai-guided"}))
	mustReject(t, err, msg)

	// A single shared entry among several must fail too — the rule is "any overlap", and a
	// naive whole-list comparison would pass this while the hazard is identical.
	err = envK8s.Create(ctx,
		rollout([]string{"a.one", "b.two", "c.three"}, []string{"x.nine", "b.two"}))
	mustReject(t, err, msg)

	// Disjoint lists are the normal case and must be accepted.
	if err := envK8s.Create(ctx, rollout([]string{"a.one"}, []string{"b.two"})); err != nil {
		t.Fatalf("disjoint grants/revokes must be accepted: %v", err)
	}
	// Either list absent must be accepted — "not both" is not "not either".
	if err := envK8s.Create(ctx, rollout([]string{"a.one"}, nil)); err != nil {
		t.Fatalf("grants-only must be accepted: %v", err)
	}
	if err := envK8s.Create(ctx, rollout(nil, []string{"b.two"})); err != nil {
		t.Fatalf("revokes-only must be accepted: %v", err)
	}
}
