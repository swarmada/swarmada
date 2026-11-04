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

package artifact

import (
	"strings"
)

// Binding a credential to the hosts it is for.
//
// The registry credentials Secret names the hosts its credentials belong to (key
// `registries`). Every other source of a host — a signature reference, an artifact URI, a
// watched registry — is written by whoever writes the rollout or the policy, so the Secret's
// own list is the only one that can say where its credentials may go. A credential with no
// list is presented nowhere: the fetch is anonymous.

// RegistriesKey is the registry credentials Secret key that lists the hosts its
// credentials may be presented to.
const RegistriesKey = "registries"

// ParseRegistries splits a `registries` value into host[:port] entries. Entries are
// separated by commas or newlines; surrounding space is ignored, and hosts compare
// case-insensitively, so each entry is lower-cased.
func ParseRegistries(v string) []string {
	var hosts []string
	for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		if h := strings.ToLower(strings.TrimSpace(f)); h != "" {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

// URLShapedEntries returns the entries of hosts written as a URL (they contain "://").
// Entries are host[:port] and are matched exactly, so such an entry matches no host.
func URLShapedEntries(hosts []string) []string {
	var out []string
	for _, h := range hosts {
		if strings.Contains(h, "://") {
			out = append(out, h)
		}
	}
	return out
}

// HostListed reports whether host (a URL's host[:port]) is one of hosts. The match is
// exact apart from case: a port must match as written, and a subdomain of a listed
// host is a different host.
func HostListed(hosts []string, host string) bool {
	host = strings.ToLower(host)
	for _, h := range hosts {
		if h == host {
			return true
		}
	}
	return false
}

// IsLoopbackHost reports whether hostname (no port, no brackets) is the local machine:
// the development registry case, where plain http is accepted.
func IsLoopbackHost(hostname string) bool {
	switch strings.ToLower(hostname) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// CredentialTransport reports whether a credential may travel over scheme to hostname:
// https, or plain http to the loopback development registry.
func CredentialTransport(scheme, hostname string) bool {
	return scheme == "https" || (scheme == "http" && IsLoopbackHost(hostname))
}

// Bound reports whether the credential names any host it may be presented to.
func (c *Credential) Bound() bool {
	return c != nil && len(c.Registries) > 0
}

// For returns the credential to present to host, or nil (anonymous) when host is not
// one the credential lists. A credential that lists no host is presented to none.
func (c *Credential) For(host string) *Credential {
	if c == nil || !HostListed(c.Registries, host) {
		return nil
	}
	return c
}
