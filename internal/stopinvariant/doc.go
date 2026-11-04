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

// Package stopinvariant holds the generated stop-invariant test: a model of a small world
// (robots, a zone tree, stops, actions) driven through random operation sequences from a fixed
// seed, with the two guards (internal/estopscope, internal/executorguard) deciding every
// admission, fan-out and dispatch, and an independent oracle checking after every step that
// no covered robot is unheld, that no action has two executors with overlapping lease windows,
// and that no admitted edit took a covered robot out of a stop's reach. It has no non-test
// code; this file names the package.
package stopinvariant
