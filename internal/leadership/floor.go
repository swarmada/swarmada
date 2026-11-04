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

package leadership

import (
	"context"
	"fmt"
	"strconv"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// FloorLeaseName is the coordination.k8s.io Lease that holds the leader-term floor, in the
	// control plane's own namespace (Floor.Namespace). It holds no lock: like the generation
	// store's Leases, it is a small object the control plane owns, written with the
	// resourceVersion it was read at.
	FloorLeaseName = "swarmada-leader-term"
	// AnnotationTermFloor holds the highest leader term issued, in decimal.
	AnnotationTermFloor = "swarmada.io/leader-term-floor"
	// maxFloorAttempts bounds the read-compute-write loop when other writers race it.
	maxFloorAttempts = 8
)

// Floor issues leader terms that never decrease. Each Next reads the highest term issued so
// far (the floor, kept in the FloorLeaseName Lease) and the election Lease's transition count,
// takes the larger plus one, and writes it back as the new floor with the resourceVersion it
// read, so two processes cannot issue one term. The transition count rises with each change
// of holder; the floor is what keeps the term from falling when the election Lease is deleted
// and its count starts again at 0. Deleting the floor Lease as well lowers the next term to the
// election Lease's count plus one; only a writer of Leases in the control plane's namespace can.
type Floor struct {
	// Reader reads both Leases from the API server, never from a cache: a cached floor can be
	// behind the one another process wrote.
	Reader client.Reader
	// Writer creates and updates the floor Lease.
	Writer client.Client
	// Namespace is the control plane's own namespace: the floor Lease's, and the election
	// Lease's.
	Namespace string
	// ElectionLease names the leader-election Lease. Empty when leader election is off: the
	// count is then 0 and the floor alone decides.
	ElectionLease string
}

// Next issues the next leader term and records it as the floor.
func (f *Floor) Next(ctx context.Context) (uint64, error) {
	for attempt := 0; attempt < maxFloorAttempts; attempt++ {
		transitions, err := f.transitions(ctx)
		if err != nil {
			return 0, err
		}
		lease := &coordinationv1.Lease{}
		key := client.ObjectKey{Namespace: f.Namespace, Name: FloorLeaseName}
		found := true
		if err := f.Reader.Get(ctx, key, lease); err != nil {
			if !apierrors.IsNotFound(err) {
				return 0, fmt.Errorf("reading the leader-term floor %s: %w", key, err)
			}
			found = false
		}
		floor, err := floorOf(lease)
		if err != nil {
			return 0, err
		}
		term := max(floor, transitions) + 1
		if !found {
			lease = &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
				Namespace:   f.Namespace,
				Name:        FloorLeaseName,
				Annotations: map[string]string{AnnotationTermFloor: strconv.FormatUint(term, 10)},
			}}
			if err := f.Writer.Create(ctx, lease); err != nil {
				if apierrors.IsAlreadyExists(err) {
					continue
				}
				return 0, fmt.Errorf("creating the leader-term floor %s: %w", key, err)
			}
			return term, nil
		}
		if lease.Annotations == nil {
			lease.Annotations = map[string]string{}
		}
		lease.Annotations[AnnotationTermFloor] = strconv.FormatUint(term, 10)
		// Update carries the resourceVersion read above: a floor another process raised in
		// between conflicts, and the term is computed again on what it wrote.
		if err := f.Writer.Update(ctx, lease); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return 0, fmt.Errorf("writing the leader-term floor %s: %w", key, err)
		}
		return term, nil
	}
	return 0, fmt.Errorf("the leader-term floor %s/%s kept changing under %d attempts to raise it",
		f.Namespace, FloorLeaseName, maxFloorAttempts)
}

// transitions is the election Lease's leaseTransitions, or 0 when there is no such Lease.
func (f *Floor) transitions(ctx context.Context) (uint64, error) {
	if f.ElectionLease == "" {
		return 0, nil
	}
	lease := &coordinationv1.Lease{}
	key := client.ObjectKey{Namespace: f.Namespace, Name: f.ElectionLease}
	if err := f.Reader.Get(ctx, key, lease); err != nil {
		if apierrors.IsNotFound(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading the election Lease %s: %w", key, err)
	}
	if n := lease.Spec.LeaseTransitions; n != nil && *n > 0 {
		return uint64(*n), nil
	}
	return 0, nil
}

// floorOf reads the floor annotation. Absent is 0. A value that is not a number is refused
// rather than read as 0: reading it as 0 would let the term fall.
func floorOf(lease *coordinationv1.Lease) (uint64, error) {
	v, ok := lease.Annotations[AnnotationTermFloor]
	if !ok {
		return 0, nil
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("the leader-term floor %s/%s holds %s=%q, which is not a term; "+
			"set it to the highest term issued, or delete the annotation only if no higher term was ever issued",
			lease.Namespace, lease.Name, AnnotationTermFloor, v)
	}
	return n, nil
}
