// Copyright 2026 The Swarmada Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Command swarmtop is a read-only, live-updating terminal fleet inspector for
// Swarmada — a Swarmada-native alternative to generic Kubernetes UIs, purpose
// built for Robot/FleetAction/RobotProbe/FleetAdapter state.
//
// A snapshot-backed Store watches Robots/FleetActions/FleetAdapters through a
// controller-runtime cache, and a Bubble Tea UI renders each as a list with the
// same selectable master-detail experience: a split pane (`s`) and a full-screen
// detail view (`enter`). Robot detail shows capabilities/hardware/events; action
// and adapter detail show the lifecycle/health fields plus the live phase and
// battery of the robots they touch.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/swarmada/swarmtop/internal/k8sclient"
	"github.com/swarmada/swarmtop/internal/ui"
)

func defaultKubeconfig() string {
	if v := os.Getenv("KUBECONFIG"); v != "" {
		return v
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".kube", "config")
	}
	return ""
}

func main() {
	kubeconfig := flag.String("kubeconfig", defaultKubeconfig(), "path to a kubeconfig file")
	namespace := flag.String("namespace", "", "namespace to watch (empty = all namespaces)")
	flag.StringVar(namespace, "n", "", "namespace to watch (shorthand)")
	robot := flag.String("robot", "", "open directly to this robot's detail view")
	flag.Parse()

	if err := run(*kubeconfig, *namespace, *robot); err != nil {
		fmt.Fprintln(os.Stderr, "swarmtop:", err)
		os.Exit(1)
	}
}

func run(kubeconfig, namespace, robot string) error {
	// Cancel the informer context on Ctrl-C / SIGTERM so the cache goroutines
	// exit cleanly when the TUI quits.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return fmt.Errorf("load kubeconfig %q: %w", kubeconfig, err)
	}

	store, err := k8sclient.NewStore(cfg, namespace)
	if err != nil {
		return err
	}
	if err := store.Start(ctx); err != nil {
		return fmt.Errorf("start fleet watch: %w", err)
	}

	p := tea.NewProgram(ui.NewFocused(store, robot), tea.WithContext(ctx), tea.WithAltScreen())
	_, err = p.Run()
	return err
}
