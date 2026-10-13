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
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The exporter is its own Go module (ADR-033 decision 4, "Packaging"), so the
// compiler already refuses an import of the operator's internal packages. What
// it does not refuse is a require of the operator module for its PUBLIC API
// package, or of the Kubernetes client stack. Since go 1.17 a module's go.mod
// lists every module that provides a package in its build, indirect ones
// included, so reading go.mod catches the dependency however deep it enters.

// forbiddenRequires returns the module paths among go.mod's requirements that
// would make the exporter know slaptain or Kubernetes rather than slapd.
// It reuses forbiddenImports (boundary_test.go): a module path is forbidden
// exactly when a package path under it would be.
func forbiddenRequires(gomod string) []string {
	var bad []string
	inBlock := false
	for line := range strings.Lines(gomod) {
		f := strings.Fields(line)
		switch {
		case len(f) == 0:
			continue
		case f[0] == "require" && len(f) > 1 && f[1] == "(":
			inBlock = true
			continue
		case inBlock && f[0] == ")":
			inBlock = false
			continue
		case f[0] == "require" && len(f) > 1:
			f = f[1:]
		case !inBlock:
			continue
		}
		for _, prefix := range forbiddenImports {
			if strings.HasPrefix(f[0], prefix) {
				bad = append(bad, f[0])
			}
		}
	}
	return bad
}

func TestForbiddenRequires(t *testing.T) {
	cases := []struct {
		name  string
		gomod string
		want  []string
	}{
		{"clean", "module m\n\ngo 1.25.3\n\nrequire (\n\tgithub.com/go-ldap/ldap/v3 v3.4.12\n)\n", nil},
		{"operator in a require block, indirect",
			"module m\n\nrequire (\n\tgithub.com/go-ldap/ldap/v3 v3.4.12\n" +
				"\tgithub.com/chuck-chuck-chuck-net/slaptain/operator v0.3.1 // indirect\n)\n",
			[]string{"github.com/chuck-chuck-chuck-net/slaptain/operator"}},
		{"single-line require of the k8s stack",
			"module m\n\nrequire k8s.io/client-go v0.35.0\nrequire sigs.k8s.io/controller-runtime v0.23.1\n",
			[]string{"k8s.io/client-go", "sigs.k8s.io/controller-runtime"}},
		{"the module's own path is not a requirement",
			"module github.com/chuck-chuck-chuck-net/slaptain/exporter\n", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := forbiddenRequires(tc.gomod); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("forbiddenRequires = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestModuleRequiresNothingFromSlaptain holds the real go.mod to the rule.
func TestModuleRequiresNothingFromSlaptain(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if bad := forbiddenRequires(string(b)); len(bad) > 0 {
		t.Errorf("the exporter module requires %q: it knows slapd, never slaptain (ADR-033 decision 4)", bad)
	}
}
