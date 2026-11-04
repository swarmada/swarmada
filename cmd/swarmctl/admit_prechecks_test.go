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
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/admission"
	"github.com/swarmada/swarmada/internal/cli"
)

// `swarmctl admit` reports success once the admission is recorded, and the control plane
// creates the Robot afterwards. So anything the create is certain to be refused for has to be
// refused HERE: past this command, the operator has been told it worked.

func fleetAdapter(name, ns string, serves ...string) *fleetv1.FleetAdapter {
	return &fleetv1.FleetAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       fleetv1.FleetAdapterSpec{ServesRobotClasses: serves},
	}
}

// assertNotMarked fails when the DiscoveredRobot carries an admission mark: a refused admission
// must leave nothing for the controller to act on.
func assertNotMarked(t *testing.T, c client.Client, ns, name string) {
	t.Helper()
	var dr fleetv1.DiscoveredRobot
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &dr); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, marked := dr.Annotations[admission.AdmitAnnotation]; marked {
		t.Error("a refused admission must not mark the object")
	}
}

func TestAdmitRefusesAReportedVersionTheRobotCRDRefuses(t *testing.T) {
	ns := "warehouse-a"
	dr := discoveredRobot("sim-robot-001", ns)
	dr.Status.AdapterVersion = "0.1.0-sim" // what the simulation adapter reports
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(dr, fleetAdapter("sim-fleet-adapter", ns)).Build()
	var out bytes.Buffer
	o := newTestOptions(&out, cli.OutputTable)

	err := o.admit(context.Background(), c, authorizer(true), ns, "sim-robot-001",
		&admitOptions{zone: ns, adapter: "sim-fleet-adapter"})
	if err == nil {
		t.Fatal("an admission whose Robot the CRD must refuse was recorded as a success")
	}
	for _, want := range []string{"spec.adapter.version", `^\d+\.\d+\.\d+$`, "--class"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}
	assertNotMarked(t, c, ns, "sim-robot-001")
}

func TestAdmitWithoutClassIsRefusedByAnAdapterThatServesClasses(t *testing.T) {
	ns := "warehouse-a"
	dr := discoveredRobot("sim-robot-001", ns)
	dr.Status.AdapterVersion = "0.1.0"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(dr, fleetAdapter("sim-fleet-adapter", ns, "simbot-v1")).Build()
	var out bytes.Buffer
	o := newTestOptions(&out, cli.OutputTable)

	err := o.admit(context.Background(), c, authorizer(true), ns, "sim-robot-001",
		&admitOptions{zone: ns, adapter: "sim-fleet-adapter"})
	if err == nil || !strings.Contains(err.Error(), "--class is required") || !strings.Contains(err.Error(), "--class simbot-v1") {
		t.Fatalf("want a refusal naming --class and the one class the adapter serves, got %v", err)
	}
	assertNotMarked(t, c, ns, "sim-robot-001")
}

func TestAdmitClassTheAdapterDoesNotServeIsRefused(t *testing.T) {
	ns := "warehouse-a"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(discoveredRobot("dr-1", ns), robotClass("acme-class", ns),
			fleetAdapter("acme-adapter", ns, "other-class")).Build()
	var out bytes.Buffer
	o := newTestOptions(&out, cli.OutputTable)

	err := o.admit(context.Background(), c, authorizer(true), ns, "dr-1",
		&admitOptions{zone: "z", robotClass: "acme-class"})
	if err == nil || !strings.Contains(err.Error(), `does not serve RobotClass "acme-class"`) {
		t.Fatalf("want a refusal naming the unserved class, got %v", err)
	}
	assertNotMarked(t, c, ns, "dr-1")
}

// An adapter that lists no classes serves any class, including none: a self-described robot
// is admissible there, and the CLI must not be stricter than the gate.
func TestAdmitWithoutClassProceedsForAServesAnyAdapter(t *testing.T) {
	ns := "warehouse-a"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(discoveredRobot("dr-1", ns), fleetAdapter("acme-adapter", ns)).Build()
	var out bytes.Buffer
	o := newTestOptions(&out, cli.OutputTable)

	if err := o.admit(context.Background(), c, authorizer(true), ns, "dr-1",
		&admitOptions{zone: "z", adapter: "acme-adapter"}); err != nil {
		t.Fatalf("admit: %v", err)
	}
	var dr fleetv1.DiscoveredRobot
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "dr-1"}, &dr)
	if _, marked := dr.Annotations[admission.AdmitAnnotation]; !marked {
		t.Error("an admission a serves-any adapter accepts must be recorded")
	}
}

// Admission is level-triggered: an adapter that is not installed yet is not a refusal, because
// the controller completes the admission once it is. The operator is told, not stopped.
func TestAdmitProceedsWhenTheAdapterIsNotInstalledYet(t *testing.T) {
	ns := "warehouse-a"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(discoveredRobot("dr-1", ns)).Build()
	var out bytes.Buffer
	o := newTestOptions(&out, cli.OutputTable)

	if err := o.admit(context.Background(), c, authorizer(true), ns, "dr-1",
		&admitOptions{zone: "z", adapter: "acme-adapter"}); err != nil {
		t.Fatalf("admit: %v", err)
	}
	if !strings.Contains(out.String(), "fleetadapter.swarmada.io/acme-adapter not found") {
		t.Errorf("the operator must be told the adapter is not installed yet, got %q", out.String())
	}
	var dr fleetv1.DiscoveredRobot
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "dr-1"}, &dr)
	if _, marked := dr.Annotations[admission.AdmitAnnotation]; !marked {
		t.Error("the admission must still be recorded")
	}
}

// --- the documented command, against the objects it documents --------------------------------

// readmeAdmitCommands returns every `swarmctl admit robot …` in a fenced block of
// config/samples/README.md, with continuation lines joined. Fenced only, as docs-cli-check
// reads them: a fenced block is what a reader copies and runs.
func readmeAdmitCommands(t *testing.T, root string) []string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, "config", "samples", "README.md"))
	if err != nil {
		t.Fatalf("reading config/samples/README.md: %v", err)
	}
	var cmds []string
	inFence, cur := false, ""
	for _, line := range strings.Split(string(body), "\n") {
		if fenceLine.MatchString(line) {
			inFence = !inFence
			continue
		}
		if !inFence {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if cur == "" && !strings.HasPrefix(trimmed, "swarmctl admit robot ") {
			continue
		}
		cur += " " + strings.TrimSuffix(trimmed, "\\")
		if !strings.HasSuffix(trimmed, "\\") {
			cmds = append(cmds, strings.Join(strings.Fields(cur), " "))
			cur = ""
		}
	}
	return cmds
}

// simAdapterVersion is the adapter_version the in-tree simulation adapter reports in its hello,
// read from its source so the test admits the robot the adapter actually announces.
func simAdapterVersion(t *testing.T, root string) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(root, "adapters", "simulation", "sim_adapter.py"))
	if err != nil {
		t.Fatalf("reading the simulation adapter: %v", err)
	}
	m := regexp.MustCompile(`adapter_version="([^"]+)"`).FindSubmatch(src)
	if m == nil {
		t.Fatal("the simulation adapter's hello names no adapter_version")
	}
	return string(m[1])
}

// robotCRDAdapterVersionPattern is the pattern the generated Robot CRD applies to
// spec.adapter.version: what the API server enforces, rather than a copy of it.
func robotCRDAdapterVersionPattern(t *testing.T, root string) *regexp.Regexp {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "config", "crd", "bases", "swarmada.io_robots.yaml"))
	if err != nil {
		t.Fatalf("reading the Robot CRD: %v", err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema struct {
						Properties struct {
							Spec struct {
								Properties struct {
									Adapter struct {
										Properties struct {
											Version struct {
												Pattern string `json:"pattern"`
											} `json:"version"`
										} `json:"properties"`
									} `json:"adapter"`
								} `json:"properties"`
							} `json:"spec"`
						} `json:"properties"`
					} `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &crd); err != nil || len(crd.Spec.Versions) == 0 {
		t.Fatalf("parsing the Robot CRD: %v", err)
	}
	pattern := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties.Spec.Properties.Adapter.Properties.Version.Pattern
	if pattern == "" {
		t.Fatal("the Robot CRD declares no pattern for spec.adapter.version")
	}
	return regexp.MustCompile(pattern)
}

func decodeSample(t *testing.T, root, rel string, into client.Object) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	if err := yaml.Unmarshal(raw, into); err != nil {
		t.Fatalf("decoding %s: %v", rel, err)
	}
}

// config/samples/README.md's admit commands, run as written against the first-run objects and
// the robot the simulation adapter announces, must yield a Robot the API server accepts: an
// adapter version the CRD pattern admits and a class the adapter serves. Recording an
// admission that cannot complete is the failure this pins — the command reported success and
// no Robot ever appeared.
func TestSamplesREADMEAdmitCommandsCreateARobotAgainstTheSimAdapter(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	cmds := readmeAdmitCommands(t, root)
	if len(cmds) == 0 {
		t.Fatal("config/samples/README.md prints no fenced `swarmctl admit robot` command")
	}
	crdVersion := robotCRDAdapterVersionPattern(t, root)
	version := simAdapterVersion(t, root)

	for _, line := range cmds {
		t.Run(line, func(t *testing.T) {
			class := &fleetv1.RobotClass{}
			decodeSample(t, root, "config/samples/first-run/robotclass.yaml", class)
			adapter := &fleetv1.FleetAdapter{}
			decodeSample(t, root, "config/samples/first-run/fleetadapter.yaml", adapter)

			var out bytes.Buffer
			o := newTestOptions(&out, cli.OutputTable)
			cmd := newAdmitCommand(o)
			ns := cmd.Flags().StringP("namespace", "n", "", "")
			// "swarmctl admit robot <name> …": the flags follow the kind token and the name.
			fields := strings.Fields(line)[3:]
			name := strings.NewReplacer("<discovered-name>", "sim-robot-001", "<name>", "sim-robot-001").Replace(fields[0])
			if err := cmd.ParseFlags(fields[1:]); err != nil {
				t.Fatalf("the documented flags do not parse: %v", err)
			}
			get := func(f string) string { v, _ := cmd.Flags().GetString(f); return v }
			a := &admitOptions{zone: get("zone"), robotClass: get("class"), adapter: get("adapter"), name: get("name")}

			class.Namespace, adapter.Namespace = *ns, *ns
			dr := discoveredRobot(name, *ns)
			dr.Status.AdapterVersion = version
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(dr, class, adapter).Build()

			if err := o.admit(context.Background(), c, authorizer(true), *ns, name, a); err != nil {
				t.Fatalf("the documented command is refused: %v", err)
			}
			var marked fleetv1.DiscoveredRobot
			if err := c.Get(context.Background(), client.ObjectKey{Namespace: *ns, Name: name}, &marked); err != nil {
				t.Fatalf("get: %v", err)
			}
			p, err := admission.DecodeParams(marked.Annotations[admission.AdmitAnnotation])
			if err != nil {
				t.Fatalf("the documented command recorded no usable admission: %v", err)
			}
			var cls *fleetv1.RobotClass
			if p.RobotClass != "" {
				cls = class
			}
			// Built the way the controller builds it, then held to what the API server checks.
			robot, err := admission.BuildRobot(dr, p, cls, *ns)
			if err != nil {
				t.Fatalf("the recorded admission does not build: %v", err)
			}
			if !crdVersion.MatchString(robot.Spec.Adapter.Version) {
				t.Errorf("spec.adapter.version %q is refused by the Robot CRD", robot.Spec.Adapter.Version)
			}
			if served := adapter.Spec.ServesRobotClasses; len(served) > 0 && !slices.Contains(served, robot.Spec.RobotClass) {
				t.Errorf("the Robot admission gate refuses it: %s serves %v, the Robot's class is %q",
					adapter.Name, served, robot.Spec.RobotClass)
			}
		})
	}
}
