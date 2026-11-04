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

// Package compute holds the control-plane-side mechanics of the Grpc/Service
// binding that are NOT the wire contract itself (that is proto/compute/v1):
// endpoint pinning and workload provisioning.
package compute

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"

	discoveryv1 "k8s.io/api/discovery/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ErrNoReadyEndpoint is returned when a Service has no ready backend address.
var ErrNoReadyEndpoint = fmt.Errorf("no ready endpoint")

// ResolvePinnedEndpoint resolves ONE ready backend address behind a
// Kubernetes Service, for a contract: Service ComputeEndpoint. It NEVER returns the Service's own address (ClusterIP or DNS
// name) — only a specific backend address drawn from the Service's
// EndpointSlices — because a Service load-balances per connection/request,
// and handing an adapter the VIP would let replicas each see a fraction of
// one robot's frame stream, degrading tracking and pre-roll silently.
//
// Selection among multiple ready addresses is deterministic (lowest address,
// byte order) rather than random, so a given call is reproducible in tests
// and logs. Returns "host:port".
func ResolvePinnedEndpoint(ctx context.Context, c client.Client, namespace, service, port string) (string, error) {
	candidates, err := readyCandidates(ctx, c, namespace, service, port)
	if err != nil {
		return "", err
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("%w: service %q port %q in namespace %q", ErrNoReadyEndpoint, service, port, namespace)
	}
	sort.Strings(candidates)
	return candidates[0], nil
}

// Pin returns the address a Service-contract run should use for its
// lifetime ("MUST resolve a specific endpoint... and MUST pin
// the stream to it for the run's lifetime"). If `current` is already set and
// is still among the Service's ready addresses, it is returned unchanged —
// re-resolving on every reconcile would defeat pinning. Otherwise a fresh
// address is resolved. `changed` reports whether the returned address differs
// from `current`, which is what a caller uses to decide whether to raise the
// discontinuity event and re-Activate against the new address.
func Pin(ctx context.Context, c client.Client, namespace, service, port, current string) (address string, changed bool, err error) {
	candidates, err := readyCandidates(ctx, c, namespace, service, port)
	if err != nil {
		return "", false, err
	}
	if current != "" {
		for _, cand := range candidates {
			if cand == current {
				return current, false, nil
			}
		}
	}
	if len(candidates) == 0 {
		return "", false, fmt.Errorf("%w: service %q port %q in namespace %q", ErrNoReadyEndpoint, service, port, namespace)
	}
	sort.Strings(candidates)
	return candidates[0], candidates[0] != current, nil
}

func readyCandidates(ctx context.Context, c client.Client, namespace, service, port string) ([]string, error) {
	var slices discoveryv1.EndpointSliceList
	if err := c.List(ctx, &slices,
		client.InNamespace(namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: service},
	); err != nil {
		return nil, fmt.Errorf("listing endpointslices for service %q: %w", service, err)
	}

	var candidates []string
	for _, sl := range slices.Items {
		portNum, ok := resolvePort(sl.Ports, port)
		if !ok {
			continue
		}
		for _, ep := range sl.Endpoints {
			if !endpointReady(ep) {
				continue
			}
			for _, addr := range ep.Addresses {
				candidates = append(candidates, net.JoinHostPort(addr, strconv.Itoa(int(portNum))))
			}
		}
	}
	return candidates, nil
}

// endpointReady applies the EndpointSlice API's own explicit-presence rule:
// Conditions.Ready unset means "assume ready" (the upstream contract for a
// nil *bool here), so only an EXPLICIT false excludes a candidate — never a
// defaulted zero value standing in for "unknown" (docs/api-principles.md).
func endpointReady(ep discoveryv1.Endpoint) bool {
	return ep.Conditions.Ready == nil || *ep.Conditions.Ready
}

func resolvePort(ports []discoveryv1.EndpointPort, name string) (int32, bool) {
	for _, p := range ports {
		if p.Name != nil && *p.Name == name && p.Port != nil {
			return *p.Port, true
		}
	}
	return 0, false
}
