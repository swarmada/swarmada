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

package tde

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func recoveryConfig(ns string, waitSeconds int32, fallback fleetv1.TDERecoveryFallback) *fleetv1.SwarmadaConfig {
	return &fleetv1.SwarmadaConfig{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: recoveryConfigName},
		Spec: fleetv1.SwarmadaConfigSpec{
			TrafficDeconfliction: fleetv1.SwarmadaTrafficDeconflictionConfig{
				Recovery: fleetv1.TDERecoveryConfig{
					ZoneControllerWaitTimeoutSeconds: waitSeconds,
					ConservativeRecoveryFallback:     fallback,
				},
			},
		},
	}
}

// resolveRecoveryConfig sources the ready-timeout and fallback from the manager
// namespace config, failing safe to the static defaults (ADR-0015).
func TestResolveRecoveryConfig(t *testing.T) {
	const mgrNS = "swarmada-system"

	t.Run("no ConfigNamespace → static defaults", func(t *testing.T) {
		r := &RecoveryRunnable{
			Client: recoveryClient(t), ReadyTimeout: 30 * time.Second,
			Fallback: RecoverReleaseAll, Log: logr.Discard(),
		}
		to, fb := r.resolveRecoveryConfig(context.Background())
		if to != 30*time.Second || fb != RecoverReleaseAll {
			t.Fatalf("got (%v, %v), want (30s, ReleaseAll)", to, fb)
		}
	})

	t.Run("config overrides both", func(t *testing.T) {
		cfg := recoveryConfig(mgrNS, 90, fleetv1.TDEReleaseReservedOnly)
		r := &RecoveryRunnable{
			Client: recoveryClient(t, cfg), ReadyTimeout: 30 * time.Second,
			Fallback: RecoverReleaseAll, ConfigNamespace: mgrNS, Log: logr.Discard(),
		}
		to, fb := r.resolveRecoveryConfig(context.Background())
		if to != 90*time.Second || fb != RecoverReleaseReservedOnly {
			t.Fatalf("got (%v, %v), want (90s, ReleaseReservedOnly)", to, fb)
		}
	})

	t.Run("absent config → fail-safe defaults", func(t *testing.T) {
		r := &RecoveryRunnable{
			Client: recoveryClient(t), ReadyTimeout: 30 * time.Second,
			Fallback: RecoverReleaseAll, ConfigNamespace: mgrNS, Log: logr.Discard(),
		}
		to, fb := r.resolveRecoveryConfig(context.Background())
		if to != 30*time.Second || fb != RecoverReleaseAll {
			t.Fatalf("got (%v, %v), want (30s, ReleaseAll) on absent config", to, fb)
		}
	})
}
