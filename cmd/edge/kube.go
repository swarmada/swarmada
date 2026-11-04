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
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/edge"
)

// startConfigSync builds a Kubernetes reader and runs a config Syncer against node in
// a background goroutine, keeping the node's zones/robots current from the FleetZone
// and Robot resources in namespace. It is best-effort: if no cluster client can be
// built (no --kubeconfig and not in-cluster) it logs and returns, leaving the node on
// its bootstrap --config-file. A returned error means a client was requested but could
// not be built; sync failures at runtime are handled inside the Syncer (fail safe).
func startConfigSync(ctx context.Context, node *edge.Node, namespace, kubeconfig string,
	interval time.Duration, log logr.Logger) error {
	rc, err := restConfig(kubeconfig)
	if err != nil {
		if kubeconfig == "" {
			// No explicit kubeconfig and not in a cluster: run with static config only.
			log.Info("no Kubernetes client (no --kubeconfig, not in-cluster); " +
				"running on the static --config-file with no live sync")
			return nil
		}
		return fmt.Errorf("build kube rest config: %w", err)
	}

	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("register scheme: %w", err)
	}
	reader, err := client.New(rc, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("build kube client: %w", err)
	}

	source := edge.NewKubeConfigSource(reader, namespace)
	syncer := edge.NewSyncer(node, source, interval, log)
	go func() { _ = syncer.Run(ctx) }()

	// Report EdgeFeedUnavailable to each edge zone's status so the control-plane Zone
	// Controller can warn on a serving adapter that never established its EdgeStream
	// (RFC-0001 #fleet-adapter-protocol-the-edge-stream). Same client as the read path; requires fleetzones/status patch RBAC.
	reporter := edge.NewFeedReporter(node, reader, namespace, interval, log)
	go func() { _ = reporter.Run(ctx) }()

	log.Info("config sync started", "namespace", namespace, "interval", interval.String())
	return nil
}

// restConfig loads a kubeconfig file when given, else falls back to in-cluster config.
func restConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}
