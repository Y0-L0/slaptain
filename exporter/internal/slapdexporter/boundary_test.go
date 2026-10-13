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
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// forbiddenImports are the prefixes that would make this exporter know
// slaptain rather than slapd (ADR-033 amendment 2026-10-08): the API package,
// the controllers, any other slaptain-internal package, and the Kubernetes
// client stack a CR read would need. The exporter is a translator from
// cn=Monitor to OpenMetrics and must stay usable against any slapd.
var forbiddenImports = []string{
	"github.com/chuck-chuck-chuck-net/slaptain/",
	"k8s.io/",
	"sigs.k8s.io/",
}

// TestPackageImportsNothingFromSlaptain parses this package's production files
// and fails on any forbidden import. A test cannot read intent; it can make
// the boundary a decision instead of a drift.
func TestPackageImportsNothingFromSlaptain(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var checked int
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		checked++
		for _, imp := range af.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			for _, bad := range forbiddenImports {
				if strings.HasPrefix(path, bad) {
					t.Errorf("%s imports %s: the exporter knows slapd, never slaptain (ADR-033 amendment 2026-10-08)",
						f, path)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no production files found; the boundary check checked nothing")
	}
}
