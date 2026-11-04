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

package webhook

import (
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/swarmada/swarmada/internal/controller"
)

// wireEstopReaders gives the zone- and namespace-estop controllers the uncached reader, as
// cmd/manager does: the hold records are read and written through it.
func wireEstopReaders(ze *controller.ZoneEstopReconciler, ne *controller.NamespaceEstopReconciler, rd client.Reader) {
	ze.APIReader = rd
	ne.APIReader = rd
}
