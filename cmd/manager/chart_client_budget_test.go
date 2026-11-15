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
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The chart passes the client budget to the manager, its defaults are the manager's own, and
// main.go registers the two flags the chart sets.
func TestChartPassesTheClientBudget(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "cmd", "manager", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, reg := range []string{
		`flag.Float64Var(&kubeAPIQPS, "kube-api-qps", defaultKubeAPIQPS,`,
		`flag.IntVar(&kubeAPIBurst, "kube-api-burst", defaultKubeAPIBurst,`,
	} {
		if !strings.Contains(string(src), reg) {
			t.Fatalf("main.go does not register %s", reg)
		}
	}

	c := managerContainer(t, managerDeployment(t, mustRender(t)))
	if v, _ := argValue(c.Args, "--kube-api-qps"); v != strconv.FormatFloat(defaultKubeAPIQPS, 'f', -1, 64) {
		t.Errorf("default chart renders --kube-api-qps=%q, want the manager's default %v", v, defaultKubeAPIQPS)
	}
	if v, _ := argValue(c.Args, "--kube-api-burst"); v != strconv.Itoa(defaultKubeAPIBurst) {
		t.Errorf("default chart renders --kube-api-burst=%q, want the manager's default %d", v, defaultKubeAPIBurst)
	}

	c = managerContainer(t, managerDeployment(t, mustRender(t,
		"--set", "manager.kubeAPI.qps=7.5", "--set", "manager.kubeAPI.burst=40")))
	if v, _ := argValue(c.Args, "--kube-api-qps"); v != "7.5" {
		t.Errorf("manager.kubeAPI.qps=7.5 renders --kube-api-qps=%q", v)
	}
	if v, _ := argValue(c.Args, "--kube-api-burst"); v != "40" {
		t.Errorf("manager.kubeAPI.burst=40 renders --kube-api-burst=%q", v)
	}
}

// The chart accepts each boundary the manager accepts: the floor, the largest float32 and a
// burst of 1, and a whole burst written as a float.
func TestChartAcceptsTheClientBudgetBoundaries(t *testing.T) {
	for _, tc := range []struct{ set, flag, want string }{
		{"manager.kubeAPI.qps=5", "--kube-api-qps", "5"},
		{"manager.kubeAPI.qps=3.4028234663852886e+38", "--kube-api-qps", "3.4028234663852886e+38"},
		{"manager.kubeAPI.burst=1", "--kube-api-burst", "1"},
		{"manager.kubeAPI.burst=30.0", "--kube-api-burst", "30"},
	} {
		out, err := renderChart(t, "--set", tc.set)
		if err != nil {
			t.Errorf("%s is refused; the manager accepts it:\n%s", tc.set, out)
			continue
		}
		c := managerContainer(t, managerDeployment(t, out))
		if v, _ := argValue(c.Args, tc.flag); v != tc.want {
			t.Errorf("%s renders %s=%q, want %q", tc.set, tc.flag, v, tc.want)
		}
	}
}

// A budget the manager would refuse at startup is refused at render time, naming the value.
func TestChartRefusesAnInvalidClientBudget(t *testing.T) {
	for _, tc := range []struct{ how, set string }{
		{"--set", "manager.kubeAPI.qps=0"},
		{"--set", "manager.kubeAPI.qps=-1"},
		{"--set", "manager.kubeAPI.qps=4.9"},
		{"--set", "manager.kubeAPI.qps=3.403e+38"},
		{"--set", "manager.kubeAPI.qps=1e39"},
		{"--set-string", "manager.kubeAPI.qps=NaN"},
		{"--set-string", "manager.kubeAPI.qps=Inf"},
		{"--set", "manager.kubeAPI.burst=0"},
		{"--set", "manager.kubeAPI.burst=1.5"},
	} {
		set := tc.set
		out, err := renderChart(t, tc.how, set)
		if err == nil {
			t.Errorf("%s rendered; it must be refused", set)
			continue
		}
		key, _, _ := strings.Cut(set, "=")
		if !strings.Contains(out, key+"=") || !strings.Contains(out, "is refused") {
			t.Errorf("%s: refusal does not name %s:\n%s", set, key, out)
		}
	}
}
