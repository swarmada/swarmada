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

// Package rbac holds the conformance test for the five built-in swarmada:* RBAC
// ClusterRoles (RFC-0001 §9.5.3). The test asserts the permission-matrix
// invariants — most importantly that the estop-clear safety verb is granted to
// swarmada:admin and to no other built-in role — so a future edit to the manifests
// cannot silently over-grant a safety-critical verb.
package rbac
