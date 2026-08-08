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

package safety

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func dispatcherWith(t *testing.T, objs ...client.Object) *Dispatcher {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return New(c, record.NewFakeRecorder(1))
}

func estopConfig(perAdapterMs int32) *fleetv1.SwarmadaConfig {
	return &fleetv1.SwarmadaConfig{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "swarmada-config"},
		Spec: fleetv1.SwarmadaConfigSpec{
			Estop: fleetv1.SwarmadaEstopConfig{
				Delivery: fleetv1.EstopDeliveryConfig{PerAdapterTimeoutMs: perAdapterMs},
			},
		},
	}
}

// deliveryTimeoutFor sources the per-adapter ACK wait from
// spec.estop.delivery.perAdapterTimeoutMs, failing safe to the dispatcher default
// (ADR-0016).
func TestDeliveryTimeoutFor(t *testing.T) {
	t.Run("no config → default", func(t *testing.T) {
		d := dispatcherWith(t)
		if got := d.deliveryTimeoutFor(context.Background(), ns); got != defaultDeliveryTimeout {
			t.Errorf("got %v, want %v", got, defaultDeliveryTimeout)
		}
	})
	t.Run("config honored", func(t *testing.T) {
		d := dispatcherWith(t, estopConfig(750))
		if got := d.deliveryTimeoutFor(context.Background(), ns); got != 750*time.Millisecond {
			t.Errorf("got %v, want 750ms", got)
		}
	})
	t.Run("zero → fail-safe default", func(t *testing.T) {
		d := dispatcherWith(t, estopConfig(0))
		if got := d.deliveryTimeoutFor(context.Background(), ns); got != defaultDeliveryTimeout {
			t.Errorf("got %v, want %v", got, defaultDeliveryTimeout)
		}
	})
}
