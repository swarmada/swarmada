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

package estopscope

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A zone's stop reaches the robots its fan-out finds in the zone and, when it propagates, in
// the zones below it (RFC-0001 #safety-estop-scopes). The fan-out reads the zone tree and the
// robots' zones when it runs, not when the stop is issued. A stop is pending from its trigger
// until its processed marker is written: in that window, a zone or a robot that leaves the
// stopped zone's subtree is never found by the fan-out, and nothing records the stop on it.
// Once the marker is written, every robot the stop reached records it (status.estopHeldBy) and
// is held wherever it goes. The readers below let the admission checks and the Zone
// Controller keep a zone or a robot under a pending stop until then.

// Pending reports whether a zone's stop is issued and not yet applied: it carries the trigger
// and no processed marker.
func Pending(z *fleetv1.FleetZone) bool {
	return pendingOn(z.Annotations)
}

func pendingOn(annotations map[string]string) bool {
	if _, triggered := annotations[TriggerAnnotation]; !triggered {
		return false
	}
	_, processed := annotations[ProcessedAnnotation]
	return !processed
}

// PendingStopsOver returns the zones whose pending stop reaches what sits under start: StopsOver
// with Pending.
func PendingStopsOver(ctx context.Context, rd client.Reader, namespace, start string, self bool) ([]string, error) {
	return StopsOver(ctx, rd, namespace, start, self, Pending)
}

// Carries reports whether a zone carries a stop at all: issued (the trigger) or applied and not
// yet cleared (the processed marker, which stands until the clear has been applied).
func Carries(z *fleetv1.FleetZone) bool {
	_, triggered := z.Annotations[TriggerAnnotation]
	_, processed := z.Annotations[ProcessedAnnotation]
	return triggered || processed
}

// StopsOver returns the zones whose stop, as stopped selects it, reaches what sits under start. With
// self, start is a robot's own zone and its stop counts whatever its policy; without self,
// start is the parent of a zone being moved, and counts only if its stop propagates down. A
// zone that cannot be found ends the walk, as it ends the fan-out's. The walk reaches as far as
// the fan-out does: start and walkCap ancestors above it (the fan-out finds a robot whose zone
// has walkCap ancestors, and admission admits such a zone); a chain still climbing past that is
// an error, so every caller fails closed on it. rd must read the API server, not a cache: a
// cached read can miss a trigger written a moment earlier.
func StopsOver(ctx context.Context, rd client.Reader, namespace, start string, self bool,
	stopped func(*fleetv1.FleetZone) bool) ([]string, error) {
	if rd == nil {
		return nil, fmt.Errorf("no uncached reader is configured to check zone %q for a stop still being applied", start)
	}
	var out []string
	name := start
	for depth := 0; name != ""; depth++ {
		if depth > walkCap {
			return nil, fmt.Errorf("the parentZone chain above zone %q is deeper than %d zones; it cannot be checked "+
				"for a stop still being applied", start, walkCap)
		}
		z := &fleetv1.FleetZone{}
		if err := rd.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, z); err != nil {
			if apierrors.IsNotFound(err) {
				break
			}
			return nil, fmt.Errorf("reading zone %q to check for a stop still being applied: %w", name, err)
		}
		if stopped(z) && ((self && depth == 0) || propagatesDown(z)) {
			out = append(out, z.Name)
		}
		name = z.Spec.ParentZone
	}
	return out, nil
}

// PendingStopsLeft returns the pending stops that reach a robot in zone from and do not reach
// one in zone to: the stops a move from one to the other would take the robot out of.
func PendingStopsLeft(ctx context.Context, rd client.Reader, namespace, from, to string) ([]string, error) {
	if from == "" || from == to {
		return nil, nil
	}
	before, err := PendingStopsOver(ctx, rd, namespace, from, true)
	if err != nil || len(before) == 0 {
		return nil, err
	}
	after, err := PendingStopsOver(ctx, rd, namespace, to, true)
	if err != nil {
		return nil, err
	}
	return Missing(before, after), nil
}

// PendingNamespaceStops returns the SwarmadaConfigs in namespace whose stop is issued and not
// yet applied. The namespace stop reaches every robot in the namespace when its fan-out runs.
func PendingNamespaceStops(ctx context.Context, rd client.Reader, namespace string) ([]string, error) {
	if rd == nil {
		return nil, fmt.Errorf("no uncached reader is configured to check namespace %q for a stop still being applied", namespace)
	}
	var configs fleetv1.SwarmadaConfigList
	if err := rd.List(ctx, &configs, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing SwarmadaConfigs in %q to check for a stop still being applied: %w", namespace, err)
	}
	var out []string
	for i := range configs.Items {
		if pendingOn(configs.Items[i].Annotations) {
			out = append(out, configs.Items[i].Name)
		}
	}
	return out, nil
}

// Missing returns the names in before that are not in after.
func Missing(before, after []string) []string {
	keep := make(map[string]bool, len(after))
	for _, n := range after {
		keep[n] = true
	}
	var out []string
	for _, n := range before {
		if !keep[n] {
			out = append(out, n)
		}
	}
	return out
}
