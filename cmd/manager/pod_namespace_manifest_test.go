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
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

// The manager reads its TDE recovery settings from the SwarmadaConfig in its own
// namespace, which it learns from POD_NAMESPACE (the RecoveryRunnable's ConfigNamespace
// in main). Unset, both settings are ignored and recovery runs on the built-ins. Both
// shipped deployments must therefore set it from the downward API.

func requirePodNamespaceFromDownwardAPI(t *testing.T, where string, d *appsv1.Deployment) {
	t.Helper()
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name != "manager" {
			continue
		}
		for _, e := range c.Env {
			if e.Name != "POD_NAMESPACE" {
				continue
			}
			if e.ValueFrom == nil || e.ValueFrom.FieldRef == nil || e.ValueFrom.FieldRef.FieldPath != "metadata.namespace" {
				t.Errorf("%s: POD_NAMESPACE is set, but not from fieldRef metadata.namespace: %+v", where, e)
			}
			return
		}
		t.Errorf("%s: the manager container does not set POD_NAMESPACE, so the TDE recovery settings are ignored", where)
		return
	}
	t.Errorf("%s: no container named manager", where)
}

func TestChartSetsPodNamespaceFromDownwardAPI(t *testing.T) {
	requirePodNamespaceFromDownwardAPI(t, "deploy/swarmada", managerDeployment(t, mustRender(t)))
}

func TestKustomizeManagerSetsPodNamespaceFromDownwardAPI(t *testing.T) {
	raw, err := os.ReadFile("../../config/manager/manager.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range strings.Split(string(raw), "\n---") {
		var d appsv1.Deployment
		if yaml.Unmarshal([]byte(doc), &d) != nil || d.Kind != "Deployment" {
			continue
		}
		requirePodNamespaceFromDownwardAPI(t, "config/manager/manager.yaml", &d)
		return
	}
	t.Fatal("config/manager/manager.yaml has no Deployment")
}
