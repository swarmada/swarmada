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

	"k8s.io/apimachinery/pkg/types"
)

// writtenMarkers is the processed marker this process last wrote or removed, per carrier. The
// manager's cache can read a carrier one write behind its own marker write. A pass that reads the
// copy from before the marker was written would issue the stop again (row S1 fires once for a
// trigger). A pass that reads the copy from before the marker was removed would clear the stop
// again and write a second ESTOP_CLEARED entry for one clear. A hit never decides anything: it only
// sends the pass to the API server, which holds the answer, so what is remembered cannot withhold
// a stop or a clear. A miss adds no read, so a first stop and a first clear wait on nothing new.
type writtenMarkers struct {
	mu   sync.Mutex
	last map[types.NamespacedName]markerWrite
}

// markerWrite is one carrier's last marker write: the value written, or the value removed.
type markerWrite struct {
	value   string
	removed bool
}

func (w *writtenMarkers) set(key types.NamespacedName, m markerWrite) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.last == nil {
		w.last = map[types.NamespacedName]markerWrite{}
	}
	w.last[key] = m
}

// wrote records that this process wrote the marker of trigger on the carrier.
func (w *writtenMarkers) wrote(key types.NamespacedName, trigger string) {
	w.set(key, markerWrite{value: trigger})
}

// removed records that this process removed the marker, of value processed, from the carrier.
func (w *writtenMarkers) removed(key types.NamespacedName, processed string) {
	w.set(key, markerWrite{value: processed, removed: true})
}

// behind reports whether a pass's copy of the carrier may be one write behind this process's
// last marker write: the trigger is there without the marker this process wrote for it, or the
// trigger is gone and the marker this process removed is still there.
func (w *writtenMarkers) behind(key types.NamespacedName, trigger string, firing bool, processed string, marked bool) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	m, ok := w.last[key]
	if !ok {
		return false
	}
	switch {
	case firing && (!marked || processed != trigger):
		return !m.removed && m.value == trigger
	case !firing && marked:
		return m.removed && m.value == processed
	}
	return false
}

func (w *writtenMarkers) forget(key types.NamespacedName) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.last, key)
}
