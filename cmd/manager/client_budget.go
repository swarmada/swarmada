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
	"fmt"
	"math"
	"time"

	"k8s.io/client-go/rest"
)

// The manager's Kubernetes API client budget: the client-side rate limit on its requests to
// the API server. The defaults are controller-runtime's (pkg/client/config, 20 QPS and burst
// 30), which the manager used before the budget was configurable.
//
// The budget is not one limit for the whole manager. controller-runtime builds one REST client,
// with a token bucket of its own, per client and per resource kind: the manager's client and
// its uncached API reader each hold one bucket for Robots, another for RobotLists, another for
// FleetZones, and so on (unstructured objects get buckets of their own). The manager's client
// reads from the informer cache, so its buckets carry only its writes; a kind's status and
// other subresource writes share the kind's bucket. Each informer has one REST client for its
// list and its watch; a watch is not rate-limited except when it retries.
//
// A few buckets are shared across kinds: each event recorder provider holds one for core/v1
// Events, used by every controller; the discovery client behind the REST mapper holds one,
// shared by the cache, the client and the API reader (it keeps the configured burst, since
// discovery raises the burst only when none is set); the metadata-only client holds one for
// every kind (the manager does not use it). Leader election holds its own, used by nothing
// else. --kube-api-qps and --kube-api-burst set every one of those buckets. A request waits
// only on the bucket of its own client and kind, so the uncached reads of one kind compete only
// with each other. rest_client_rate_limiter_wait_seconds (internal/kubeclientmetrics) shows the
// waits.
const (
	defaultKubeAPIQPS   = 20.0
	defaultKubeAPIBurst = 30
	// minKubeAPIQPS keeps leader election alive with a wide margin. Leader election has a
	// bucket of its own of the configured size, used by nothing else. Each renewal is a Get and
	// an Update of the Lease, and both must be admitted within the renew deadline (3 s by
	// default), so renewals start to fail below about 0.7 QPS: two tokens in three seconds. A
	// shorter --leader-election-renew-deadline needs proportionally more; checkRenewalBudget
	// refuses a QPS too low for the deadline in use.
	minKubeAPIQPS = 5.0

	// leaseRenewalRequests is what one renewal asks of the election's bucket: a Get and an
	// Update of the Lease. renewalMargin is how many renewals' worth of tokens the bucket must
	// refill within one renew deadline.
	leaseRenewalRequests = 2
	renewalMargin        = 3
)

// applyClientBudget sets the client budget on cfg. It refuses a QPS below minKubeAPIQPS or not
// finite, and a burst below 1: client-go reads a negative QPS as "no limit" and a zero burst
// as its default of 10, so neither would mean what the flag says.
func applyClientBudget(cfg *rest.Config, qps float64, burst int) error {
	if math.IsNaN(qps) || math.IsInf(qps, 0) || qps < minKubeAPIQPS {
		return fmt.Errorf("--kube-api-qps must be at least %v, got %v: leader election renews on the same budget",
			minKubeAPIQPS, qps)
	}
	if qps > math.MaxFloat32 {
		return fmt.Errorf("--kube-api-qps %v is out of range", qps)
	}
	if burst < 1 {
		return fmt.Errorf("--kube-api-burst must be at least 1, got %d", burst)
	}
	cfg.QPS = float32(qps)
	cfg.Burst = burst
	return nil
}

// minQPSForRenewDeadline is the lowest QPS that refills three renewals' worth of tokens within
// one renew deadline: 6 divided by the deadline in seconds, 2 at the default 3 s.
func minQPSForRenewDeadline(renewDeadline time.Duration) float64 {
	return leaseRenewalRequests * renewalMargin / renewDeadline.Seconds()
}

// checkRenewalBudget refuses a QPS too low for leader election to renew within renewDeadline.
// At the default 3 s deadline minKubeAPIQPS is the stricter floor; the check binds only when
// the deadline is under 1.2 s. A deadline that is not positive is left for leader election to
// refuse.
func checkRenewalBudget(qps float64, renewDeadline time.Duration) error {
	if renewDeadline <= 0 {
		return nil
	}
	if need := minQPSForRenewDeadline(renewDeadline); qps < need {
		return fmt.Errorf("--kube-api-qps %v is too low for --leader-election-renew-deadline %v: "+
			"each renewal needs %d requests admitted within the deadline, so with leader election on "+
			"the QPS must be at least %d divided by the deadline in seconds, here %v",
			qps, renewDeadline, leaseRenewalRequests, leaseRenewalRequests*renewalMargin, need)
	}
	return nil
}
