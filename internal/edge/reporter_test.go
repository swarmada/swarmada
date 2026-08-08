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

package edge

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func reporterFixtures(t *testing.T) (*FeedReporter, *Node, func(name string) *fleetv1.FleetZone, *time.Time) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	dock := &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: "dock", Namespace: "ns"},
		Spec:       fleetv1.FleetZoneSpec{EdgeNode: &fleetv1.EdgeNodeConfig{}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dock).
		WithStatusSubresource(&fleetv1.FleetZone{}).Build()

	clk := time.Now()
	n := feedNode(&clk)
	get := func(name string) *fleetv1.FleetZone {
		fz := &fleetv1.FleetZone{}
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: name}, fz); err != nil {
			t.Fatalf("get zone %s: %v", name, err)
		}
		return fz
	}
	return NewFeedReporter(n, c, "ns", time.Second, logr.Discard()), n, get, &clk
}

// The reporter writes the never-seen-beyond-grace robots into status, then clears
// the field once their feeds arrive.
func TestFeedReporter_WritesAndClears(t *testing.T) {
	r, n, getZone, clk := reporterFixtures(t)

	n.MissingFeeds()                // seed the grace window
	*clk = clk.Add(2 * time.Minute) // beyond grace → both robots flagged

	r.reportOnce(context.Background())
	if got := getZone("dock").Status.EdgeFeedUnavailable; !reflect.DeepEqual(got, []string{"amr-1", "amr-2"}) {
		t.Fatalf("status.edgeFeedUnavailable = %v, want [amr-1 amr-2]", got)
	}

	// Feeds arrive; the next report clears the warning.
	n.markSeen("amr-1")
	n.markSeen("amr-2")
	r.reportOnce(context.Background())
	if got := getZone("dock").Status.EdgeFeedUnavailable; len(got) != 0 {
		t.Fatalf("status.edgeFeedUnavailable = %v, want cleared", got)
	}
}

// A healthy fleet (all feeds live within grace) never writes to status.
func TestFeedReporter_NoWriteWhenHealthy(t *testing.T) {
	r, n, getZone, clk := reporterFixtures(t)

	n.MissingFeeds()
	n.markSeen("amr-1")
	n.markSeen("amr-2")
	*clk = clk.Add(2 * time.Minute)

	r.reportOnce(context.Background())
	if got := getZone("dock").Status.EdgeFeedUnavailable; len(got) != 0 {
		t.Fatalf("healthy fleet should leave status empty, got %v", got)
	}
}
