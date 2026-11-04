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
	"testing"

	"k8s.io/client-go/tools/record"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// setResumeRecorder wires the Event recorder the resume request answers through.
func setResumeRecorder(r *RobotEstopReconciler, rec record.EventRecorder) { r.Recorder = rec }

// The operator-facing spelling of the resume request pinned in
// estop_resume_operator_envtest_test.go is the API's.
func TestEstopResumeAnnotationSpelling(t *testing.T) {
	if fleetv1.EstopResumeAnnotation != resumeRequestKey || fleetv1.EstopResumeProcessedAnnotation != resumeProcessedKey {
		t.Fatalf("API spells the resume annotations %q / %q; the operator-facing spelling is %q / %q",
			fleetv1.EstopResumeAnnotation, fleetv1.EstopResumeProcessedAnnotation, resumeRequestKey, resumeProcessedKey)
	}
}
