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

package compute

// flexVolume and inline CSI are refused beside hostPath (RFC-0001
// #computeworkloadtemplate-normative-requirements). Both hand the pod whatever an
// administrator-installed driver exposes, which on some drivers is a node path, so
// either one walks around the hostPath refusal without it firing. Pod Security
// Standards Restricted refuses flexVolume and permits inline CSI, so neither profile
// closes the CSI path; the deny-list does.

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestValidatePodTemplate_RefusesFlexVolumeAndInlineCSI(t *testing.T) {
	for _, tc := range []struct {
		name  string
		src   corev1.VolumeSource
		names string
	}{
		{"flexVolume", corev1.VolumeSource{FlexVolume: &corev1.FlexVolumeSource{Driver: "example/flex"}}, "flexVolume"},
		{"inline csi", corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{Driver: "example.csi.k8s.io"}}, "inline CSI"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := basePod()
			pod.Volumes = append(pod.Volumes, corev1.Volume{Name: "driver", VolumeSource: tc.src})
			err := ValidatePodTemplate(&pod)
			if err == nil {
				t.Fatalf("a template mounting a volume of kind %s was admitted; a node-path-exposing driver "+
					"reaches what the hostPath refusal exists to keep out", tc.name)
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("refused, but the message does not name %q: %v", tc.names, err)
			}
			if !strings.Contains(err.Error(), PolicyRule) {
				t.Errorf("refused without quoting the rule it enforces: %v", err)
			}
		})
	}

	// The control: the claim-backed sources that stay permitted are still admitted.
	pod := basePod()
	pod.Volumes = append(pod.Volumes,
		corev1.Volume{Name: "pvc", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}}},
		corev1.Volume{Name: "eph", VolumeSource: corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{}}})
	if err := ValidatePodTemplate(&pod); err != nil {
		t.Errorf("a PVC or a generic ephemeral volume was refused; only flexVolume and inline CSI are: %v", err)
	}
}
