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

// The API server refuses a flexVolume or an inline CSI volume in a template, beside
// hostPath (RFC-0001 #computeworkloadtemplate-the-cel-half). The rule is CEL, so it
// holds on an install that serves no webhook; this proves it against the real API
// server, where a fake client would admit anything.

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestEnvtest_ComputeWorkloadTemplateRefusesFlexVolumeAndInlineCSI(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	for _, tc := range []struct {
		name, obj, names string
		src              corev1.VolumeSource
	}{
		{"flexVolume", "via-flex", "flexVolume", corev1.VolumeSource{FlexVolume: &corev1.FlexVolumeSource{Driver: "example/flex"}}},
		{"inline csi", "via-csi", "inline csi", corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{Driver: "example.csi.k8s.io"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := mkComputeWorkloadTemplate(tc.obj, ns, "")
			tmpl.Spec.PodTemplate.Spec.Volumes = []corev1.Volume{{Name: "driver", VolumeSource: tc.src}}
			err := envK8s.Create(ctx, tmpl)
			if err == nil {
				t.Fatalf("the API server ACCEPTED a template mounting a volume of kind %s; with a node-path-exposing "+
					"driver installed, that reaches what the hostPath refusal keeps out", tc.name)
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("rejected, but not by the volume-source rule (want %q): %v", tc.names, err)
			}
		})
	}

	// The control: a claim-backed volume is still admitted.
	ok := mkComputeWorkloadTemplate("via-pvc", ns, "")
	ok.Spec.PodTemplate.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}}}}
	if err := envK8s.Create(ctx, ok); err != nil {
		t.Errorf("a PVC-backed template was refused; only flexVolume and inline csi are: %v", err)
	}
}
