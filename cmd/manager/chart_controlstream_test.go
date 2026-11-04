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
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// The Helm chart must install a manager that serves ControlStream with mTLS
// (RFC-0001 #fleet-adapter-protocol-transport-security, ADR-0025). Without the three
// TLS flags the manager does not serve it at all (controlStreamShouldServe), so a
// chart that renders only the bind address installs a control plane no adapter can
// reach. These tests render the chart the way `helm install` does and read the result.

const chartPath = "../../deploy/swarmada"

// renderChart runs `helm template` on the chart. helm is required, not optional:
// `make test` already needs it for helm-verify-sync, and a chart check that skips when
// the renderer is missing would pass without having read the chart.
func renderChart(t *testing.T, args ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Fatalf("helm is not on PATH; these tests render deploy/swarmada and cannot run without it (install helm)")
	}
	cmd := exec.Command("helm", append([]string{"template", "verify-rel", chartPath, "--namespace", "verify-ns"}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		return stderr.String(), err
	}
	return stdout.String(), nil
}

func mustRender(t *testing.T, args ...string) string {
	t.Helper()
	out, err := renderChart(t, args...)
	if err != nil {
		t.Fatalf("helm template %v failed: %v\n%s", args, err, out)
	}
	return out
}

// managerDeployment returns the rendered controller-manager Deployment.
func managerDeployment(t *testing.T, rendered string) *appsv1.Deployment {
	t.Helper()
	for _, doc := range strings.Split(rendered, "\n---") {
		var d appsv1.Deployment
		if yaml.Unmarshal([]byte(doc), &d) != nil || d.Kind != "Deployment" ||
			!strings.HasSuffix(d.Name, "-controller-manager") {
			continue
		}
		return &d
	}
	t.Fatalf("the rendered chart has no controller-manager Deployment")
	return nil
}

func managerContainer(t *testing.T, d *appsv1.Deployment) corev1.Container {
	t.Helper()
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == "manager" {
			return c
		}
	}
	t.Fatalf("Deployment %s has no manager container", d.Name)
	return corev1.Container{}
}

func argValue(args []string, flag string) (string, bool) {
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, flag+"="); ok {
			return v, true
		}
	}
	return "", false
}

// With default values the manager gets the bind address and all three TLS flags, each
// file lies under a mount of the Secret the server Certificate fills, and the dev-only
// authorization bypass is absent.
func TestChartServesControlStreamWithMTLS(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		wantSecret string
	}{
		{"cert-manager (default)", nil, "verify-rel-swarmada-controlstream-server-tls"},
		{"existingSecret", []string{"--set", "controlStream.tls.existingSecret=my-cs-tls"}, "my-cs-tls"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rendered := mustRender(t, tc.args...)
			c := managerContainer(t, managerDeployment(t, rendered))

			if v, ok := argValue(c.Args, "--fleet-adapter-bind-address"); !ok || v == "" {
				t.Fatalf("--fleet-adapter-bind-address missing or empty in %v", c.Args)
			}
			mounts := map[string]string{} // volume name -> mount path
			for _, m := range c.VolumeMounts {
				mounts[m.Name] = m.MountPath
			}
			secrets := map[string]string{} // volume name -> secret name
			for _, v := range managerDeployment(t, rendered).Spec.Template.Spec.Volumes {
				if v.Secret != nil {
					secrets[v.Name] = v.Secret.SecretName
				}
			}
			for _, flag := range []string{
				"--fleet-adapter-tls-cert-file", "--fleet-adapter-tls-key-file", "--fleet-adapter-client-ca-file",
			} {
				path, ok := argValue(c.Args, flag)
				if !ok || path == "" {
					t.Errorf("%s is not rendered; without it the manager does not serve ControlStream", flag)
					continue
				}
				var from string
				for vol, mp := range mounts {
					if strings.HasPrefix(path, mp+"/") {
						from = secrets[vol]
					}
				}
				if from != tc.wantSecret {
					t.Errorf("%s=%s is served from Secret %q, want %q", flag, path, from, tc.wantSecret)
				}
			}
			for _, a := range c.Args {
				if strings.HasPrefix(a, "--fleet-adapter-insecure-authz") {
					t.Errorf("the chart renders %q; the manager refuses it together with mTLS", a)
				}
			}
			hasCMServer := strings.Contains(rendered, "name: verify-rel-swarmada-controlstream-server\n")
			if wantCM := tc.args == nil; hasCMServer != wantCM {
				t.Errorf("cert-manager server Certificate rendered = %v, want %v", hasCMServer, wantCM)
			}
		})
	}
}

// Commands are pushed from the leading replica's in-memory stream table, and every
// replica accepts adapter streams, so more than one replica loses commands. The chart
// refuses that combination and says why; with ControlStream off it is allowed.
func TestChartRefusesMultipleReplicasWithControlStream(t *testing.T) {
	out, err := renderChart(t, "--set", "replicaCount=2")
	if err == nil {
		t.Fatalf("replicaCount=2 with ControlStream enabled rendered; it must be refused")
	}
	for _, want := range []string{"replicaCount=2", "controlStream.enabled=true", "receives no commands"} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal does not say %q:\n%s", want, out)
		}
	}
	mustRender(t, "--set", "replicaCount=2", "--set", "controlStream.enabled=false")
}

// No TLS source means a manager that logs "NOT serving ControlStream (fail closed)";
// the chart says so at render time instead.
func TestChartRefusesControlStreamWithoutTLSSource(t *testing.T) {
	out, err := renderChart(t, "--set", "controlStream.tls.certManager.enabled=false")
	if err == nil {
		t.Fatalf("ControlStream with no TLS source rendered; it must be refused")
	}
	if !strings.Contains(out, "controlStream.tls.existingSecret") {
		t.Fatalf("refusal does not name controlStream.tls.existingSecret:\n%s", out)
	}
}

func TestChartRefusesInsecureAuthzInExtraArgs(t *testing.T) {
	out, err := renderChart(t, "--set", "manager.extraArgs[0]=--fleet-adapter-insecure-authz")
	if err == nil {
		t.Fatalf("--fleet-adapter-insecure-authz in manager.extraArgs rendered; it must be refused")
	}
	if !strings.Contains(out, "servesRobotClasses") {
		t.Fatalf("refusal does not say how robots are authorized instead:\n%s", out)
	}
}

// failurePolicy=Ignore opens the estop, marker, Robot-gate and zone-delete validators
// whenever the webhook server is down; it renders only when acknowledged.
func TestChartRefusesFailOpenWebhooksUnlessAcknowledged(t *testing.T) {
	out, err := renderChart(t, "--set", "webhooks.failurePolicy=Ignore")
	if err == nil {
		t.Fatalf("webhooks.failurePolicy=Ignore rendered without the acknowledgement")
	}
	for _, want := range []string{"acknowledgeFailOpenSecurity", "vrobot", "vfleetzone", "vswarmadaconfig"} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal does not name %q:\n%s", want, out)
		}
	}
	mustRender(t, "--set", "webhooks.failurePolicy=Ignore", "--set", "webhooks.acknowledgeFailOpenSecurity=true")
}

// One ControlStream port: the manager's flag default, the chart, the kustomize overlays
// and the simulation adapter's --endpoint default name the same number, and none of
// them is 9443, which the admission webhook server always binds.
func TestControlStreamPortDefaultsAgree(t *testing.T) {
	const want = "9445"
	read := func(rel string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("..", "..", rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		return string(b)
	}
	find := func(rel, pattern string) []string {
		t.Helper()
		var got []string
		for _, m := range regexp.MustCompile(pattern).FindAllStringSubmatch(read(rel), -1) {
			got = append(got, m[1])
		}
		if len(got) == 0 {
			t.Fatalf("%s: nothing matches %q, so this test read nothing there", rel, pattern)
		}
		return got
	}
	checks := []struct{ rel, pattern string }{
		{"cmd/manager/main.go", `"fleet-adapter-bind-address", ":(\d+)"`},
		{"deploy/swarmada/values.yaml", `(?m)^controlStream:\n  enabled: \w+\n  port: (\d+)`},
		{"config/controlstream-tls/manager-mtls-patch.yaml", `--fleet-adapter-bind-address=:(\d+)`},
		{"config/controlstream-tls/controlstream-service.yaml", `(?m)^\s+(?:port|targetPort): (\d+)`},
		{"config/overlays/quickstart-mtls/controlstream-service.yaml", `(?m)^\s+(?:port|targetPort): (\d+)`},
		{"config/overlays/quickstart-dev/controlstream-service.yaml", `(?m)^\s+(?:port|targetPort): (\d+)`},
		{"adapters/simulation/sim_adapter.py", `"--endpoint", default="localhost:(\d+)"`},
	}
	for _, c := range checks {
		for _, got := range find(c.rel, c.pattern) {
			if got != want {
				t.Errorf("%s: ControlStream port %s, want %s", c.rel, got, want)
			}
		}
	}
	rendered := mustRender(t)
	c := managerContainer(t, managerDeployment(t, rendered))
	if v, _ := argValue(c.Args, "--fleet-adapter-bind-address"); v != ":"+want {
		t.Errorf("chart renders --fleet-adapter-bind-address=%s, want :%s", v, want)
	}
}
