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
	"os"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/swarmada/swarmada/internal/leadership"
)

// leaderStateT is this process's hold on leadership; main.go names it through this alias.
type leaderStateT = leadership.State

// serviceAccountNamespaceFile is where a pod reads its own namespace, as controller-runtime
// does to find the election Lease when no namespace is configured.
const serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// setupLeadership adds the leadership runnable to mgr and returns the State it writes. The
// stream server serves adapter streams, and the command dispatcher sends commands stamped with
// the leader term, only while State leads (RFC-0001 #safety-robot-stop-table,
// #fleet-adapter-protocol-the-stop-latch).
//
// The term floor is the swarmada-leader-term Lease in the manager's own namespace
// (POD_NAMESPACE, else the pod's service-account namespace), beside the election Lease and the
// generation store. Outside a pod with neither, it is kept in "default", and the log says so.
// With leader election off the election Lease is not read, and each start of the process takes
// the floor plus one.
func setupLeadership(mgr ctrl.Manager, leaderElection bool, electionID string) (*leaderStateT, error) {
	log := ctrl.Log.WithName("leadership")
	ns := os.Getenv("POD_NAMESPACE")
	if ns == "" {
		if b, err := os.ReadFile(serviceAccountNamespaceFile); err == nil {
			ns = strings.TrimSpace(string(b))
		}
	}
	if ns == "" {
		ns = "default"
		log.Info("POD_NAMESPACE is not set and this process runs outside a pod: the leader-term floor is kept " +
			"in namespace default; set POD_NAMESPACE to the manager's namespace")
	}
	election := ""
	if leaderElection {
		election = electionID
	}
	state := &leadership.State{}
	err := mgr.Add(&leadership.Runnable{
		State: state,
		Terms: &leadership.Floor{
			Reader:        mgr.GetAPIReader(),
			Writer:        mgr.GetClient(),
			Namespace:     ns,
			ElectionLease: election,
		},
		Log: log,
	})
	return state, err
}
