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

package webhook

import (
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// rolloutSpecUnchanged reports whether an update to a rollout leaves its spec as it was. The
// signing check is about the artifact the spec names, so a write that changes only metadata
// changes nothing the check is about: the controller recording that it applied a resume, a
// label, an annotation. Re-running the check on such a write would refuse it whenever the policy
// cannot be read or now requires a reference the stored rollout lacks, and a rollout that cannot
// take the controller's own bookkeeping cannot be resumed or brought to a terminal phase. The rule
// is about the write, not the writer: any principal's spec change is still checked.
func rolloutSpecUnchanged(oldObj, newObj runtime.Object) bool {
	switch n := newObj.(type) {
	case *fleetv1.FirmwareRollout:
		o, ok := oldObj.(*fleetv1.FirmwareRollout)
		return ok && equality.Semantic.DeepEqual(o.Spec, n.Spec)
	case *fleetv1.ModelRollout:
		o, ok := oldObj.(*fleetv1.ModelRollout)
		return ok && equality.Semantic.DeepEqual(o.Spec, n.Spec)
	}
	return false
}
