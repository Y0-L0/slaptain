/*
Copyright 2026.

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

package slapdexporter

import (
	"net/url"
	"testing"
)

// TestTCPTarget: the dial address and TLS ServerName for ldap/ldaps. Found in
// re-review: net.SplitHostPort on u.Host turns a port-less IPv6 literal
// "[::1]" into "[[::1]]:389", and hands TLS a ServerName "[::1]" no
// certificate can match. url.URL's Hostname/Port already know the brackets.
func TestTCPTarget(t *testing.T) {
	for _, tc := range []struct{ uri, addr, serverName string }{
		{"ldap://[::1]", "[::1]:389", "::1"},
		{"ldaps://[2001:db8::1]", "[2001:db8::1]:636", "2001:db8::1"},
		{"ldap://[2001:db8::1]:1024", "[2001:db8::1]:1024", "2001:db8::1"},
		{"ldap://h.example", "h.example:389", "h.example"},
		{"ldaps://h.example:1636", "h.example:1636", "h.example"},
		{"ldaps://192.0.2.10", "192.0.2.10:636", "192.0.2.10"},
	} {
		t.Run(tc.uri, func(t *testing.T) {
			u, err := url.Parse(tc.uri)
			if err != nil {
				t.Fatalf("url.Parse(%q): %v", tc.uri, err)
			}
			addr, sn := tcpTarget(u)
			if addr != tc.addr || sn != tc.serverName {
				t.Errorf("tcpTarget(%q) = (%q, %q), want (%q, %q)", tc.uri, addr, sn, tc.addr, tc.serverName)
			}
		})
	}
}
