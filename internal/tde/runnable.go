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
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// recoveryConfigName is the fixed name of the per-namespace SwarmadaConfig singleton
// (mirrors controller.SwarmadaConfigName; inlined to avoid a controller→tde import).
const recoveryConfigName = "swarmada-config"

// RecoveryRunnable rebuilds TDE reservation state from FleetZone.status each time
// this replica acquires leadership (§9.4.7 startup recovery + leader-failover
// re-recovery). It is a leader-elected manager.Runnable: a standby replica holds no
// reservation state, and the moment it wins leadership the engine is rebuilt from
// durable FleetZone.status BEFORE it serves any grant.
//
// The engine's grant gate fails closed until recovery completes — while not
// recovered, RequestReservation denies with tde_unavailable and the caller requeues
// — so a freshly promoted leader can never over-grant against empty or stale
// in-memory state. On loss of leadership the fail-closed latch is re-armed, so a
// later re-promotion recovers afresh rather than trusting state that may have
// drifted while this replica was demoted.
type RecoveryRunnable struct {
	// Engine is the shared deconfliction engine whose state is rebuilt.
	Engine *Engine
	// Client reads FleetZone.status (the manager cached client; the cache is synced
	// before leader-elected runnables start).
	Client client.Client
	// Mode selects the §9.4.7 recovery policy when the Zone Controller is ready
	// (RecoverValidate in production).
	Mode RecoveryMode
	// Log receives lifecycle messages.
	Log logr.Logger
	// RetryInterval is the backoff between failed recovery attempts; the gate stays
	// fail-closed across retries. Zero uses a 2s default.
	RetryInterval time.Duration

	// Ready, when non-nil, reports whether the Zone Controller has loaded zone
	// geometry. Recovery waits for it (up to ReadyTimeout) before validating Occupied
	// reservations against Robot.status.currentZone; on timeout it applies Fallback
	// (§9.4.7). Nil disables the readiness gate (Mode is used unconditionally).
	Ready func() bool
	// ReadyTimeout bounds the wait for Ready. Zero uses the §9.1.11.10 default (30s).
	ReadyTimeout time.Duration
	// Fallback is the conservative recovery mode when the Zone Controller is not ready
	// in time. Empty defaults to RecoverReleaseAll (§9.4.7).
	Fallback RecoveryMode
	// ReadyPollInterval is how often Ready is polled while waiting. Zero uses 200ms.
	ReadyPollInterval time.Duration

	// ConfigNamespace is the namespace whose SwarmadaConfig supplies the recovery
	// tunables (spec.trafficDeconfliction.recovery.*). Because recovery is a
	// cluster-wide action, these are sourced from a single designated namespace — the
	// manager's own (POD_NAMESPACE) — not per workload namespace (ADR-0015). Empty
	// disables the lookup and uses the static ReadyTimeout/Fallback defaults.
	ConfigNamespace string
}

// NeedLeaderElection reports true so the manager starts this runnable only on the
// leader and re-runs it on every leadership acquisition (the failover hook). With
// leader election disabled the manager runs it once at startup, preserving the
// single-replica recovery behaviour.
func (r *RecoveryRunnable) NeedLeaderElection() bool { return true }

// Start recovers reservation state, opens the grant gate, then blocks until
// leadership is lost (ctx cancellation), re-arming the fail-closed latch on exit.
func (r *RecoveryRunnable) Start(ctx context.Context) error {
	// Fail closed until THIS leader has rebuilt state from FleetZone.status. The gate
	// stays closed through the readiness wait AND the recovery retries below, so no
	// grant is served against unrebuilt state.
	r.Engine.SetRecovered(false)

	// Resolve the recovery tunables from the manager-namespace SwarmadaConfig
	// (ADR-0015), fail-safe to the static defaults. Choose the recovery mode: validate
	// against currentZone once the Zone Controller is ready, else the conservative
	// fallback on timeout (§9.4.7).
	readyTimeout, fallback := r.resolveRecoveryConfig(ctx)
	mode := r.recoveryMode(ctx, readyTimeout, fallback)

	interval := r.RetryInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	// Retry until recovery succeeds or leadership is lost. The gate stays fail-closed
	// throughout, so we deny (never over-grant) while retrying — Recover opens it.
	for {
		if err := r.Engine.Recover(ctx, r.Client, mode); err == nil {
			break
		} else if ctx.Err() != nil {
			return nil // leadership lost during recovery; remain fail-closed
		} else {
			r.Log.Error(err, "TDE leader recovery failed; grant gate stays fail-closed, retrying")
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(interval):
			}
		}
	}
	r.Log.Info("TDE reservation state recovered on leadership acquisition", "mode", mode)

	<-ctx.Done()
	// Leadership lost (or shutdown): re-arm fail-closed so a re-promotion re-recovers.
	r.Engine.SetRecovered(false)
	return nil
}

// resolveRecoveryConfig returns the ready-timeout and conservative fallback for this
// recovery, sourced from the manager-namespace SwarmadaConfig
// (spec.trafficDeconfliction.recovery.*) when ConfigNamespace is set, else the static
// defaults (ADR-0015). It FAILS SAFE: an unreadable/absent config or a non-positive /
// empty value keeps the built-in default, so recovery never weakens on a config gap.
func (r *RecoveryRunnable) resolveRecoveryConfig(ctx context.Context) (time.Duration, RecoveryMode) {
	timeout := r.ReadyTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second // §9.1.11.10 default
	}
	fallback := r.Fallback
	if fallback == "" {
		fallback = RecoverReleaseAll // §9.4.7 default conservative fallback
	}
	if r.ConfigNamespace == "" {
		return timeout, fallback
	}

	var cfg fleetv1.SwarmadaConfig
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: r.ConfigNamespace, Name: recoveryConfigName}, &cfg); err != nil {
		return timeout, fallback // fail-safe: keep defaults
	}
	rec := cfg.Spec.TrafficDeconfliction.Recovery
	if rec.ZoneControllerWaitTimeoutSeconds > 0 {
		timeout = time.Duration(rec.ZoneControllerWaitTimeoutSeconds) * time.Second
	}
	if rec.ConservativeRecoveryFallback != "" {
		fallback = RecoveryMode(rec.ConservativeRecoveryFallback)
	}
	return timeout, fallback
}

// recoveryMode waits for the Zone Controller to become ready (up to readyTimeout)
// and returns Mode (RecoverValidate) on success, or the conservative fallback on
// timeout (§9.4.7). With no Ready gate configured it uses Mode unconditionally.
func (r *RecoveryRunnable) recoveryMode(ctx context.Context, readyTimeout time.Duration, fallback RecoveryMode) RecoveryMode {
	if r.Ready == nil {
		return r.Mode
	}
	if r.waitReady(ctx, readyTimeout) || ctx.Err() != nil {
		return r.Mode
	}
	r.Log.Info("Zone Controller not ready within timeout; conservative TDE recovery", "fallback", fallback)
	return fallback
}

// waitReady blocks until Ready() is true, the timeout elapses, or ctx is cancelled.
func (r *RecoveryRunnable) waitReady(ctx context.Context, timeout time.Duration) bool {
	if r.Ready() {
		return true
	}
	if timeout <= 0 {
		timeout = 30 * time.Second // §9.1.11.10 default
	}
	interval := r.ReadyPollInterval
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return false
		case <-tick.C:
			if r.Ready() {
				return true
			}
		}
	}
}
