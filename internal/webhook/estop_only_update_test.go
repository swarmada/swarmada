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

// ─────────────────────────────────────────────────────────────────────────────
// A ZONE OR NAMESPACE ESTOP IS NEVER REFUSED FOR SOMETHING UNRELATED TO THE STOP.
//
// The FleetZone validator re-checks the parent chain and FZ-R7 on every update, against
// the zone's current ancestors. FZ-R8 admits an ancestor tightening onto an existing
// descendant, so a descendant can come to contradict its ancestor with no write to it,
// and before this rule its next write of any kind was refused, the estop included. The
// SwarmadaConfig validator re-checks its spec rules on every update, so a spec stored
// before a rule was added refused the namespace estop the same way.
//
// Each carrier's estop-only write is exempt, and the estop checks still run on it: the
// marker guard, the estop verb, and (FleetZone) the composite refusal. Every refusal
// below is asserted by what it says, so a test cannot pass on the structural error the
// exemption removed.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// tightenedTree is the admitted-then-contradicted pair: bay allows camera Limited 3 under
// an unrestricted site, then site is tightened to camera Denied (FZ-R8 admits it).
func tightenedTree(t *testing.T, authz *fakeEstopAuthz) (*FleetZoneValidator, *fleetv1.FleetZone) {
	t.Helper()
	bay := fzrZone("bay", "site", &fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{
		op("camera", fleetv1.ZoneRestrictionLimited, "3", nil)}})
	site := fzrZone("site", "", &fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{
		op("camera", fleetv1.ZoneRestrictionDenied, "", nil)}})
	c := zoneClient(t, site, bay)
	return &FleetZoneValidator{Client: c, APIReader: c, EstopAuthz: authz}, bay
}

// withAnn returns a copy of z with the annotations set (an empty value deletes the key).
func withAnn(z *fleetv1.FleetZone, kv ...string) *fleetv1.FleetZone {
	c := z.DeepCopy()
	if c.Annotations == nil {
		c.Annotations = map[string]string{}
	}
	for i := 0; i < len(kv); i += 2 {
		if kv[i+1] == "" {
			delete(c.Annotations, kv[i])
		} else {
			c.Annotations[kv[i]] = kv[i+1]
		}
	}
	return c
}

// refusedSaying fails unless err is non-nil and says every one of want.
func refusedSaying(t *testing.T, what string, err error, want ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s was ADMITTED", what)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("%s was refused, but not for the expected reason (want %q): %v", what, w, err)
		}
	}
}

const fzr7Says = "more permissive than the rule declared by ancestor zone"

func TestFleetZoneEstopIsNotRefusedByAnUnrelatedInvariant(t *testing.T) {
	v, bay := tightenedTree(t, &fakeEstopAuthz{allow: true})

	// The control: bay contradicts its ancestor, and an ordinary write says so.
	labelled := bay.DeepCopy()
	labelled.Labels = map[string]string{"team": "night"}
	_, err := v.ValidateUpdate(ctxAsUser("admin"), bay, labelled)
	refusedSaying(t, "a label write on bay after site tightened", err, fzr7Says)

	// The trigger, with the actor the mutating stamper adds to the same request.
	triggered := withAnn(bay, estopTriggeredAnnotation, "true", AnnEstopActor, "admin")
	if _, err := v.ValidateUpdate(ctxAsUser("admin"), bay, triggered); err != nil {
		t.Fatalf("the zone estop on bay was REFUSED because its ancestor tightened after it was "+
			"admitted, a reason unrelated to the stop: %v", err)
	}

	// The controller's seal: the processed marker recorded and the actor consumed, in one
	// patch. Refused, the fan-out could not record that it ran.
	sealed := withAnn(triggered, estopProcessedAnnotation, "true", AnnEstopActor, "")
	if _, err := v.ValidateUpdate(ctxAsUser(controllerUser), triggered, sealed); err != nil {
		t.Fatalf("the controller's processed marker on bay was refused: %v", err)
	}

	// The clear.
	cleared := withAnn(sealed, estopTriggeredAnnotation, "")
	if _, err := v.ValidateUpdate(ctxAsUser("admin"), sealed, cleared); err != nil {
		t.Fatalf("the zone estop CLEAR on bay was refused because of an unrelated invariant: %v", err)
	}

	// Anything more than the estop annotations is validated as usual.
	for name, mutate := range map[string]func(*fleetv1.FleetZone){
		"trigger plus a label":            func(z *fleetv1.FleetZone) { z.Labels = map[string]string{"team": "night"} },
		"trigger plus another annotation": func(z *fleetv1.FleetZone) { z.Annotations["note"] = "x" },
		"trigger plus a spec change":      func(z *fleetv1.FleetZone) { z.Spec.DisplayName = "Bay" },
	} {
		t.Run(name, func(t *testing.T) {
			n := withAnn(bay, estopTriggeredAnnotation, "true")
			mutate(n)
			_, err := v.ValidateUpdate(ctxAsUser("admin"), bay, n)
			refusedSaying(t, name, err, fzr7Says)
		})
	}
}

// The three estop checks run on the estop-only path, in order: the marker guard, the
// estop verb, then the composite refusal. Every case is built on the contradicted bay,
// so without the exemption each would be refused by FZ-R7 instead, and the assertion on
// what the refusal says tells the two apart.
func TestFleetZoneEstopOnlyPathStillRunsTheEstopChecks(t *testing.T) {
	const (
		markerSays    = "the estop idempotence marker is written only by the control plane"
		deniedSays    = "is not permitted to"
		compositeSays = "is a composite zone"
	)

	t.Run("a forged processed marker is refused", func(t *testing.T) {
		v, bay := tightenedTree(t, &fakeEstopAuthz{allow: true})
		_, err := v.ValidateUpdate(ctxAsUser("alice"), bay, withAnn(bay, estopProcessedAnnotation, "true"))
		refusedSaying(t, "a user writing the processed marker", err, markerSays)
		if !apierrors.IsForbidden(err) {
			t.Fatalf("want Forbidden, got %v", err)
		}
	})

	t.Run("a forged marker is named first, ahead of the verb", func(t *testing.T) {
		authz := &fakeEstopAuthz{allow: false}
		v, bay := tightenedTree(t, authz)
		_, err := v.ValidateUpdate(ctxAsUser("alice"), bay,
			withAnn(bay, estopTriggeredAnnotation, "true", estopProcessedAnnotation, "true"))
		refusedSaying(t, "a trigger carrying a forged marker", err, markerSays)
		if authz.calls != 0 {
			t.Fatalf("the verb was consulted before the marker guard refused (%d calls)", authz.calls)
		}
	})

	t.Run("an unauthorized trigger is refused", func(t *testing.T) {
		authz := &fakeEstopAuthz{allow: false}
		v, bay := tightenedTree(t, authz)
		_, err := v.ValidateUpdate(ctxAsUser("viewer"), bay, withAnn(bay, estopTriggeredAnnotation, "true"))
		refusedSaying(t, "a trigger without estop-trigger", err, deniedSays, estopTriggerVerb)
		if authz.gotVerb != estopTriggerVerb {
			t.Fatalf("verb checked = %q, want %q", authz.gotVerb, estopTriggerVerb)
		}
	})

	t.Run("an unauthorized clear is refused", func(t *testing.T) {
		authz := &fakeEstopAuthz{allow: false}
		v, bay := tightenedTree(t, authz)
		stopped := withAnn(bay, estopTriggeredAnnotation, "true")
		_, err := v.ValidateUpdate(ctxAsUser("viewer"), stopped, withAnn(stopped, estopTriggeredAnnotation, ""))
		refusedSaying(t, "a clear without estop-clear", err, deniedSays, estopClearVerb)
		if authz.gotVerb != estopClearVerb {
			t.Fatalf("verb checked = %q, want %q", authz.gotVerb, estopClearVerb)
		}
	})

	// ward is a composite that also contradicts site, so without the exemption the
	// refusal would be FZ-R7's.
	compositeUnderSite := func(t *testing.T, authz *fakeEstopAuthz) (*FleetZoneValidator, *fleetv1.FleetZone) {
		t.Helper()
		ward := fzrZone("ward", "site", &fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{
			op("camera", fleetv1.ZoneRestrictionLimited, "3", nil)}})
		ward.Spec.MemberZones = []string{"bay"}
		site := fzrZone("site", "", &fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{
			op("camera", fleetv1.ZoneRestrictionDenied, "", nil)}})
		c := zoneClient(t, site, ward)
		return &FleetZoneValidator{Client: c, APIReader: c, EstopAuthz: authz}, ward
	}

	t.Run("a trigger on a composite is refused", func(t *testing.T) {
		v, ward := compositeUnderSite(t, &fakeEstopAuthz{allow: true})
		_, err := v.ValidateUpdate(ctxAsUser("admin"), ward, withAnn(ward, estopTriggeredAnnotation, "true"))
		refusedSaying(t, "a trigger on composite ward", err, compositeSays)
		if !apierrors.IsForbidden(err) {
			t.Fatalf("want Forbidden, got %v", err)
		}
	})

	t.Run("the verb is checked ahead of the composite refusal", func(t *testing.T) {
		v, ward := compositeUnderSite(t, &fakeEstopAuthz{allow: false})
		_, err := v.ValidateUpdate(ctxAsUser("viewer"), ward, withAnn(ward, estopTriggeredAnnotation, "true"))
		refusedSaying(t, "an unauthorized trigger on composite ward", err, deniedSays)
	})

	t.Run("a clear on a composite is admitted", func(t *testing.T) {
		v, ward := compositeUnderSite(t, &fakeEstopAuthz{allow: true})
		stopped := withAnn(ward, estopTriggeredAnnotation, "true")
		if _, err := v.ValidateUpdate(ctxAsUser("admin"), stopped, withAnn(stopped, estopTriggeredAnnotation, "")); err != nil {
			t.Fatalf("an estop annotation on a composite must stay removable: %v", err)
		}
	})
}

// staleConfig is a SwarmadaConfig whose stored spec fails a rule of this validator: a
// real telemetry sink with no endpoint. Admission refuses that spec today; the object
// stands for one stored before the rule existed.
func staleConfig() *fleetv1.SwarmadaConfig {
	cfg := validConfig()
	cfg.Namespace = zoneNS
	cfg.Spec.Telemetry.Sink.Endpoint = ""
	return cfg
}

func cfgWithAnn(c *fleetv1.SwarmadaConfig, kv ...string) *fleetv1.SwarmadaConfig {
	n := c.DeepCopy()
	if n.Annotations == nil {
		n.Annotations = map[string]string{}
	}
	for i := 0; i < len(kv); i += 2 {
		if kv[i+1] == "" {
			delete(n.Annotations, kv[i])
		} else {
			n.Annotations[kv[i]] = kv[i+1]
		}
	}
	return n
}

func TestNamespaceEstopIsNotRefusedByAnUnrelatedSpecRule(t *testing.T) {
	const endpointSays = "endpoint is required for a real telemetry sink"
	v := &SwarmadaConfigValidator{EstopAuthz: &fakeEstopAuthz{allow: true}}
	cfg := staleConfig()

	labelled := cfg.DeepCopy()
	labelled.Labels = map[string]string{"team": "night"}
	_, err := v.ValidateUpdate(ctxAsUser("admin"), cfg, labelled)
	refusedSaying(t, "a label write on a config whose stored spec fails a rule", err, endpointSays)

	triggered := cfgWithAnn(cfg, estopTriggeredAnnotation, "true", AnnEstopActor, "admin")
	if _, err := v.ValidateUpdate(ctxAsUser("admin"), cfg, triggered); err != nil {
		t.Fatalf("the namespace estop was REFUSED by a spec rule unrelated to the stop: %v", err)
	}
	sealed := cfgWithAnn(triggered, estopProcessedAnnotation, "true", AnnEstopActor, "")
	if _, err := v.ValidateUpdate(ctxAsUser(controllerUser), triggered, sealed); err != nil {
		t.Fatalf("the controller's processed marker on the config was refused: %v", err)
	}
	cleared := cfgWithAnn(sealed, estopTriggeredAnnotation, "")
	if _, err := v.ValidateUpdate(ctxAsUser("admin"), sealed, cleared); err != nil {
		t.Fatalf("the namespace estop CLEAR was refused by an unrelated spec rule: %v", err)
	}

	n := cfgWithAnn(cfg, estopTriggeredAnnotation, "true")
	n.Spec.Telemetry.Sink.Type = fleetv1.TelemetrySinkMimir
	_, err = v.ValidateUpdate(ctxAsUser("admin"), cfg, n)
	refusedSaying(t, "a trigger plus a spec change", err, endpointSays)
}

func TestNamespaceEstopOnlyPathStillRunsTheEstopChecks(t *testing.T) {
	t.Run("a forged processed marker is refused", func(t *testing.T) {
		v := &SwarmadaConfigValidator{EstopAuthz: &fakeEstopAuthz{allow: true}}
		cfg := staleConfig()
		_, err := v.ValidateUpdate(ctxAsUser("alice"), cfg, cfgWithAnn(cfg, estopProcessedAnnotation, "true"))
		refusedSaying(t, "a user writing the processed marker", err,
			"the estop idempotence marker is written only by the control plane")
	})
	t.Run("an unauthorized trigger is refused", func(t *testing.T) {
		v := &SwarmadaConfigValidator{EstopAuthz: &fakeEstopAuthz{allow: false}}
		cfg := staleConfig()
		_, err := v.ValidateUpdate(ctxAsUser("viewer"), cfg, cfgWithAnn(cfg, estopTriggeredAnnotation, "true"))
		refusedSaying(t, "a trigger without estop-trigger", err, "is not permitted to", estopTriggerVerb)
	})
	t.Run("an unauthorized clear is refused", func(t *testing.T) {
		v := &SwarmadaConfigValidator{EstopAuthz: &fakeEstopAuthz{allow: false}}
		stopped := cfgWithAnn(staleConfig(), estopTriggeredAnnotation, "true")
		_, err := v.ValidateUpdate(ctxAsUser("viewer"), stopped, cfgWithAnn(stopped, estopTriggeredAnnotation, ""))
		refusedSaying(t, "a clear without estop-clear", err, "is not permitted to", estopClearVerb)
	})
}
