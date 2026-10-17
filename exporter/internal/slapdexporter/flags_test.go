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
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-ldap/ldap/v3"
)

// The flags and their defaults are the exporter's interface. The operator passes
// no arguments, so the defaults ARE the sidecar's contract (ADR-033 decision 4,
// "Packaging"): names, defaults and refusals are pinned here.

func passwordFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestParseFlagsDefaults: no flags is the sidecar's configuration — ldapi,
// EXTERNAL, :9330.
func TestParseFlagsDefaults(t *testing.T) {
	got, err := ParseFlags(nil)
	if err != nil {
		t.Fatalf("ParseFlags(nil): %v", err)
	}
	want := Options{URI: "ldapi:///run/openldap/slapd.ldapi", Bind: BindExternal, Listen: ":9330", Timeout: 5 * time.Second}
	if got != want {
		t.Errorf("defaults = %+v, want %+v", got, want)
	}
}

// TestParseFlagsSidecarInvocation: what earlier operators passed in the pod
// template — --uri in slapd's own -h spelling and --bind, nothing else — still
// comes out dialable, with the default timeout, so an older operator's
// arguments keep working against a newer exporter.
func TestParseFlagsSidecarInvocation(t *testing.T) {
	got, err := ParseFlags([]string{"--uri=ldapi://%2frun%2fopenldap%2fslapd.ldapi", "--bind=external"})
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	want := Options{URI: DefaultURI, Bind: BindExternal, Listen: DefaultListen, Timeout: DefaultTimeout}
	if got != want || DefaultTimeout != 5*time.Second {
		t.Errorf("got %+v (DefaultTimeout %v), want %+v with a 5s timeout", got, DefaultTimeout, want)
	}
}

// TestParseFlagsSimpleBindReadsThePasswordFile: the password comes from the
// file's content, never from argv.
func TestParseFlagsSimpleBindReadsThePasswordFile(t *testing.T) {
	pw := passwordFile(t, "s3cret")
	got, err := ParseFlags([]string{"--uri=ldap://ldap.example.org:1024", "--bind=simple",
		"--bind-dn=cn=monitor,dc=example,dc=org", "--bind-password-file=" + pw, "--listen=127.0.0.1:19330", "--timeout=2s"})
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	want := Options{URI: "ldap://ldap.example.org:1024", Bind: BindSimple, BindDN: "cn=monitor,dc=example,dc=org",
		BindPassword: "s3cret", Listen: "127.0.0.1:19330", Timeout: 2 * time.Second}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestParseFlagsPasswordFileTrimsOneLineEnding: the file is read once, at
// startup (structural: Options carries the string, not the path); exactly one trailing "\n" or "\r\n" — what an editor or echo
// leaves, and the classic err=49 nobody can see — is removed, and nothing else.
// A password may end in a space or in a deliberate second newline.
func TestParseFlagsPasswordFileTrimsOneLineEnding(t *testing.T) {
	for _, tc := range []struct{ name, content, want string }{
		{"no line ending", "s3cret", "s3cret"},
		{"one LF", "s3cret\n", "s3cret"},
		{"one CRLF", "s3cret\r\n", "s3cret"},
		{"trailing space survives", "s3cret ", "s3cret "},
		{"trailing space before LF survives", "s3cret \n", "s3cret "},
		{"only one LF is removed", "s3cret\n\n", "s3cret\n"},
		{"a lone CR is not a line ending", "s3cret\r", "s3cret\r"},
		{"leading space survives", " s3cret\n", " s3cret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pw := passwordFile(t, tc.content)
			got, err := ParseFlags([]string{"--bind=simple", "--bind-dn=cn=monitor,dc=example,dc=org",
				"--bind-password-file=" + pw})
			if err != nil {
				t.Fatalf("ParseFlags: %v", err)
			}
			if got.BindPassword != tc.want {
				t.Errorf("file %q → password %q, want %q", tc.content, got.BindPassword, tc.want)
			}
		})
	}
}

// TestParseFlagsBindNone: no bind, no credentials.
func TestParseFlagsBindNone(t *testing.T) {
	got, err := ParseFlags([]string{"--bind=none"})
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if got.Bind != BindNone || got.BindDN != "" || got.BindPassword != "" {
		t.Errorf("got %+v, want bind none with no credentials", got)
	}
}

// TestParseFlagsRefuses: every misconfiguration refuses to start, rather than
// starting and failing every scrape — a sidecar that runs but can never read
// looks healthy (Ready) and exports nothing.
func TestParseFlagsRefuses(t *testing.T) {
	pw := passwordFile(t, "s3cret")
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"unknown --bind", []string{"--bind=sasl"}},
		{"empty --bind", []string{"--bind="}},
		{"simple without a password file", []string{"--bind=simple", "--bind-dn=cn=monitor,dc=example,dc=org"}},
		{"simple with a missing password file",
			[]string{"--bind=simple", "--bind-dn=cn=monitor,dc=example,dc=org", "--bind-password-file=" + missing}},
		{"simple without a DN", []string{"--bind=simple", "--bind-password-file=" + pw}},
		{"credentials with external", []string{"--bind=external", "--bind-password-file=" + pw}},
		{"a DN with none", []string{"--bind=none", "--bind-dn=cn=monitor,dc=example,dc=org"}},
		// An empty --uri is a flag error, not a URI for DialURL to judge every
		// scrape: refused here so the sidecar does not run Ready and read nothing.
		{"empty URI", []string{"--uri="}},
		{"unknown flag", []string{"--interval=30s"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseFlags(tc.args)
			if err == nil {
				t.Errorf("ParseFlags(%q) = %+v, nil; want an error", tc.args, got)
			}
		})
	}
}

// TestNormalizeURI: only slapd's -h spelling (the socket path percent-encoded
// into the host) is rewritten, to ldapi:///<path>. Everything else passes
// through unchanged — validating a URI is ldap.DialURL's job (ADR-033 revision
// after review), and a second validator here could only disagree with it.
func TestNormalizeURI(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"slapd -h form", "ldapi://%2frun%2fopenldap%2fslapd.ldapi", "ldapi:///run/openldap/slapd.ldapi"},
		{"slapd -h form, upper-case escapes", "ldapi://%2Frun%2Fopenldap%2Fslapd.ldapi", "ldapi:///run/openldap/slapd.ldapi"},
		{"another socket path", "ldapi://%2fvar%2frun%2fslapd%2fldapi", "ldapi:///var/run/slapd/ldapi"},
		{"path form unchanged", "ldapi:///run/openldap/slapd.ldapi", "ldapi:///run/openldap/slapd.ldapi"},
		// go-ldap dials its compiled-in default socket for an empty ldapi
		// path; that is DialURL's documented behaviour, not ours to override.
		{"empty-path ldapi unchanged", "ldapi://", "ldapi://"},
		{"ldap unchanged", "ldap://ldap.example.org:1024", "ldap://ldap.example.org:1024"},
		{"ldaps unchanged", "ldaps://ldap.example.org:1025", "ldaps://ldap.example.org:1025"},
		{"other scheme unchanged (DialURL refuses it)", "http://ldap.example.org", "http://ldap.example.org"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeURI(tc.in)
			if err != nil || got != tc.want {
				t.Errorf("NormalizeURI(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
			}
		})
	}
	// The one thing the rewrite itself can get wrong: an -h form whose escape
	// does not decode. Passing it through would hand DialURL the same string it
	// already refuses, with a less useful error.
	if got, err := NormalizeURI("ldapi://%zz"); err == nil {
		t.Errorf("NormalizeURI(%q) = %q, nil; want an error", "ldapi://%zz", got)
	}
}

// TestNormalizeURIResultIsDialableByGoLDAP: the point of normalising is that
// go-ldap can dial the result. A real unix socket, named the way slapd -h
// names it, must accept a connection from ldap.DialURL(NormalizeURI(...)).
// (Before v3.4.15, go-ldap rejected the -h form outright: net/url refuses "%2f"
// in a host.)
func TestNormalizeURIResultIsDialableByGoLDAP(t *testing.T) {
	// A short directory: unix socket paths are limited to ~108 bytes.
	dir, err := os.MkdirTemp("", "sx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "slapd.ldapi")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen %s: %v", sock, err)
	}
	defer func() { _ = ln.Close() }()
	accepted := make(chan struct{}, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- struct{}{}
			_ = c.Close()
		}
	}()

	// go-ldap v3.4.15 learned to dial this form unnormalised (parseLDAPURL
	// percent-decodes an ldapi host), so the rewrite is no longer what makes the
	// socket reachable. NormalizeURI stays for its other job — refusing a
	// malformed escape at flag parsing rather than at every scrape — and this
	// test keeps pinning that what it returns still reaches the socket.
	hForm := "ldapi://" + strings.ReplaceAll(sock, "/", "%2f")
	norm, err := NormalizeURI(hForm)
	if err != nil {
		t.Fatalf("NormalizeURI(%q): %v", hForm, err)
	}
	conn, err := ldap.DialURL(norm)
	if err != nil {
		t.Fatalf("ldap.DialURL(%q) (normalised from %q): %v", norm, hForm, err)
	}
	defer func() { _ = conn.Close() }()
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("socket never accepted a connection")
	}
}
