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

// The garbage collector deletes the members of a FleetTask that was deleted, and removes the
// task's controller reference when it orphans them. Every guard that lets it through asks
// isGarbageCollector, so the guards agree on who it is.

const (
	// garbageCollectorUser is the garbage collector where the controller manager runs each
	// controller under its own ServiceAccount.
	garbageCollectorUser = "system:serviceaccount:kube-system:generic-garbage-collector"
	// kubeControllerManagerUser is the garbage collector where the controller manager does not
	// give each controller its own credentials, and every controller shares this one.
	kubeControllerManagerUser = "system:kube-controller-manager"
)

// isGarbageCollector reports whether username is one the garbage collector runs as.
func isGarbageCollector(username string) bool {
	return username == garbageCollectorUser || username == kubeControllerManagerUser
}
