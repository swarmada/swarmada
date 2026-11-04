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

package safety

import (
	"context"
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// atomicWrites makes a fake client's writes atomic, as an API server's are. The fake applies a
// patch by a read, an apply and an update that are not one step, so two concurrent status patches
// (the stop's record, written beside its estopState) can lose one of them on the fake where an API
// server would apply both. Every write takes one lock.
type atomicWrites struct {
	client.Client
	mu *sync.Mutex
}

func atomicFake(c client.Client) client.Client { return &atomicWrites{Client: c, mu: &sync.Mutex{}} }

func (a *atomicWrites) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Client.Update(ctx, obj, opts...)
}

func (a *atomicWrites) Patch(ctx context.Context, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Client.Patch(ctx, obj, p, opts...)
}

func (a *atomicWrites) Status() client.SubResourceWriter {
	return &atomicStatus{SubResourceWriter: a.Client.Status(), mu: a.mu}
}

type atomicStatus struct {
	client.SubResourceWriter
	mu *sync.Mutex
}

func (s *atomicStatus) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.SubResourceWriter.Update(ctx, obj, opts...)
}

func (s *atomicStatus) Patch(ctx context.Context, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.SubResourceWriter.Patch(ctx, obj, p, opts...)
}
