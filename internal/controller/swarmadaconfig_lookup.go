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
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// namespaceConfig returns the namespace's SwarmadaConfig singleton, or
// (nil, false) when none is readable (a list error or no config present).
//
// Callers MUST fail safe to their own built-in defaults on a false result: an
// unreadable or absent SwarmadaConfig never blocks reconciliation. This is the
// shared read path for controllers that source per-namespace tunables from
// spec.* (mirrors the fail-safe pattern in the registrar and robot controllers).
func namespaceConfig(ctx context.Context, c client.Client, namespace string) (*fleetv1.SwarmadaConfig, bool) {
	var list fleetv1.SwarmadaConfigList
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil || len(list.Items) == 0 {
		return nil, false
	}
	return &list.Items[0], true
}
