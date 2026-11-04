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

// Package egress is the reference shape of the ONE rule the compute design
// exist to enforce: an executor writes artifact and result bytes DIRECTLY to
// the declared ArtifactStore/ResultSink, on the data path — the control plane
// never proxies them. WriteArtifact is what a conforming executor's write
// path looks like; it is deliberately NOT reachable from
// internal/controller, because nothing in the control plane is meant to call
// it — that absence is the point, not an oversight.
package egress

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
)

// WriteArtifact PUTs payload directly to an ArtifactStore's (ObjectStore- or
// VolumeClaim/EdgeLocal-backed, over whatever protocol that store's endpoint
// serves — HTTP here, standing in for any of them) address and returns its
// content digest. The control plane is not a party to this call: it resolved
// the destination and any credential in advance and is not on
// this code path at all.
func WriteArtifact(ctx context.Context, storeEndpoint string, payload []byte) (digestHex string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, storeEndpoint, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("building artifact PUT: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("writing artifact to store: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return "", fmt.Errorf("draining artifact store response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("artifact store returned %s", resp.Status)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}
