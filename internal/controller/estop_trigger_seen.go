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
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// triggerSeen is when this process first saw each carrier's stop trigger, from the watch event
// that delivered it, before its pass was queued. swarmada_estop_fanout_duration_seconds runs
// "from the operator's trigger" (RFC-0001 #control-plane-observability-prometheus-metrics-contract,
// #safety-timing-requirements), and a pass can wait in the queue, so the episode is timed from
// here rather than from the pass. It is still not the operator's trigger: admission and the
// watch's own latency come before it, and a process that starts or gains leadership with a
// trigger already written has no stamp, and times that episode from its pass (ADR-0042's
// addendum).
//
// One entry per carrier: a trigger is stamped when it is present and the processed marker does
// not already record it, and a stamp is never moved later. It is dropped when the marker is
// written, when the carrier's clear is applied, and when the carrier is deleted.
type triggerSeen struct {
	mu sync.Mutex
	m  map[types.NamespacedName]time.Time
}

func (s *triggerSeen) note(obj client.Object, at time.Time) {
	ann := obj.GetAnnotations()
	trigger, firing := ann[annEstopTriggered]
	if !firing {
		return
	}
	if processed, marked := ann[annEstopProcessed]; marked && processed == trigger {
		return
	}
	key := client.ObjectKeyFromObject(obj)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[types.NamespacedName]time.Time{}
	}
	if _, ok := s.m[key]; !ok {
		s.m[key] = at
	}
}

// since is when the carrier's trigger was first seen, or fallback when it was not seen, or was
// seen after it.
func (s *triggerSeen) since(key types.NamespacedName, fallback time.Time) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if at, ok := s.m[key]; ok && at.Before(fallback) {
		return at
	}
	return fallback
}

func (s *triggerSeen) forget(key types.NamespacedName) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
}

// predicate stamps on every create and update of a carrier and forgets on its delete. It
// filters nothing out: every event passes, as without it.
func (s *triggerSeen) predicate() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { s.note(e.Object, time.Now()); return true },
		UpdateFunc: func(e event.UpdateEvent) bool { s.note(e.ObjectNew, time.Now()); return true },
		DeleteFunc: func(e event.DeleteEvent) bool {
			s.forget(client.ObjectKeyFromObject(e.Object))
			return true
		},
		GenericFunc: func(event.GenericEvent) bool { return true },
	}
}
