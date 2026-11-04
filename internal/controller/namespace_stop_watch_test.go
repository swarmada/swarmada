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
	"errors"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// A Robot's create reconciles its namespace's SwarmadaConfig, and only its create: a robot
// cannot change namespace, so no update brings it under a namespace's stop.
func TestNamespaceEstop_ARobotCreateReconcilesItsNamespacesConfig(t *testing.T) {
	r, _ := newNamespaceEstop(t, &fakeEstopper{}, nsConfig("stop", "stop"))
	got := r.configsOfRobot(context.Background(), zeRobot("late", ""))
	if len(got) != 1 || got[0].Namespace != zeNS || got[0].Name != "swarmada" {
		t.Fatalf("a Robot create enqueued %v, want its namespace's SwarmadaConfig", got)
	}
	if !robotCreated.Create(event.CreateEvent{Object: zeRobot("late", "")}) {
		t.Error("a Robot create must reach the namespace-estop controller")
	}
	if robotCreated.Update(event.UpdateEvent{ObjectOld: zeRobot("late", ""), ObjectNew: zeRobot("late", "z1")}) {
		t.Error("a Robot update should not reconcile the namespace's SwarmadaConfig")
	}
}

// A SwarmadaConfig list that fails does not drop the create: it falls back to the
// singleton's fixed name.
func TestNamespaceEstop_ARobotCreateIsNotLostToAnUnreadableConfigList(t *testing.T) {
	r, _ := newNamespaceEstop(t, &fakeEstopper{})
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("cache not synced")
		},
	})
	got := r.configsOfRobot(context.Background(), zeRobot("late", ""))
	if len(got) != 1 || got[0].Namespace != zeNS || got[0].Name != SwarmadaConfigName {
		t.Fatalf("a Robot create under a failing list enqueued %v, want %s/%s", got, zeNS, SwarmadaConfigName)
	}
}
