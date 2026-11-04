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

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/cli"
	"github.com/swarmada/swarmada/internal/estopscope"
)

// `swarmctl estop-clear` on a zone that carries no stop says there is nothing to clear,
// changes nothing and fails: removing an annotation that is not there writes nothing, so
// "cleared", "Resuming" and a recorded reason would all be untrue.
func TestEstopClear_AZoneWithNoStopHasNothingToClear(t *testing.T) {
	ns := "warehouse-a"
	for _, tc := range []struct {
		name string
		ann  map[string]string
		want string
	}{
		{name: "never stopped", want: "has no emergency stop to clear (it carries no swarmada.io/estop-triggered annotation); nothing was changed"},
		{name: "its clear already in flight", ann: map[string]string{estopscope.ProcessedAnnotation: "x"},
			want: "its stop was already cleared and the control plane is applying that clear"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			zone := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "bay-4", Namespace: ns, Annotations: tc.ann}}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(zone).Build()
			out := &bytes.Buffer{}
			o := newTestOptions(out, cli.OutputTable)
			err := o.estopClear(context.Background(), c, authorizer(true), ns, "bay-4", "aisle clear", true)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("estop-clear err = %v, want %q", err, tc.want)
			}
			if strings.Contains(out.String(), "cleared") {
				t.Fatalf("estop-clear printed that the stop was cleared: %q", out.String())
			}
			got := &fleetv1.FleetZone{}
			_ = c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "bay-4"}, got)
			if len(got.Annotations) != len(tc.ann) {
				t.Fatalf("estop-clear changed the zone's annotations: %v", got.Annotations)
			}
		})
	}
}
