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
	"bufio"
	"crypto/tls"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoShippedManifestPairsInsecureAuthzWithMTLS reads every manifest under config/
// and deploy/ and refuses one that sets --fleet-adapter-insecure-authz=true beside the
// ControlStream TLS flags. The manager refuses that pair at startup, so a manifest that
// ships it is a deployment that does not start; this finds it before a cluster does.
// Comment lines are ignored, so a manifest may still explain the flag.
func TestNoShippedManifestPairsInsecureAuthzWithMTLS(t *testing.T) {
	var scanned int
	for _, root := range []string{filepath.Join("..", "..", "config"), filepath.Join("..", "..", "deploy")} {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || (filepath.Ext(path) != ".yaml" && filepath.Ext(path) != ".yml") {
				return nil
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			var insecure, tls bool
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if strings.HasPrefix(line, "#") {
					continue
				}
				insecure = insecure || strings.Contains(line, "--fleet-adapter-insecure-authz=true")
				tls = tls || strings.Contains(line, "--fleet-adapter-tls-cert-file") ||
					strings.Contains(line, "--fleet-adapter-client-ca-file")
			}
			if err := sc.Err(); err != nil {
				return err
			}
			scanned++
			if insecure && tls {
				t.Errorf("%s sets --fleet-adapter-insecure-authz=true together with the ControlStream TLS "+
					"flags; the manager refuses that pair at startup", path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}
	if scanned == 0 {
		t.Fatal("no manifests were scanned, so this test proved nothing")
	}
}

// TestControlStreamPostureRefusesInsecureAuthzWithMTLS: the flag is the dev posture's
// single switch, so it may stand alone and mTLS may stand alone, but the pair is
// refused, and the refusal names the flag and what to do instead.
func TestControlStreamPostureRefusesInsecureAuthzWithMTLS(t *testing.T) {
	cfg := &tls.Config{}
	for _, tc := range []struct {
		name     string
		tlsCfg   *tls.Config
		insecure bool
		refused  bool
	}{
		{"neither", nil, false, false},
		{"insecure-authz alone (plaintext dev posture)", nil, true, false},
		{"mTLS alone", cfg, false, false},
		{"insecure-authz with mTLS", cfg, true, true},
	} {
		err := controlStreamPosture(tc.tlsCfg, tc.insecure)
		if (err != nil) != tc.refused {
			t.Errorf("%s: controlStreamPosture error = %v, want refused=%v", tc.name, err, tc.refused)
			continue
		}
		if err != nil {
			for _, want := range []string{"--fleet-adapter-insecure-authz", "mTLS", "servesRobotClasses"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s: the refusal does not say %q: %v", tc.name, want, err)
				}
			}
		}
	}
}
