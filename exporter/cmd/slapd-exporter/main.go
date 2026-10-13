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

// Command slapd-exporter is ADR-033 Surface 3: the sidecar that serves slapd's
// cn=Monitor as openldap_* metrics. It is its own module, image and version
// line (ADR-033 decision 4): the slapd-exporter image runs it as its
// entrypoint, and the operator passes no arguments, so the flag defaults
// (ldapi:///run/openldap/slapd.ldapi, SASL EXTERNAL, :9330) are the sidecar
// contract and stay stable across releases.
//
// Flag errors exit 2 before anything listens; a Serve error (e.g. the listen
// address is taken) exits 1. An unreachable slapd is not an error.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/chuck-chuck-chuck-net/slaptain/exporter/internal/slapdexporter"
)

func main() {
	opts, err := slapdexporter.ParseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "slapd-exporter: %v\n", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	err = slapdexporter.Serve(ctx, opts)
	stop()
	if err != nil {
		fmt.Fprintf(os.Stderr, "slapd-exporter: %v\n", err)
		os.Exit(1)
	}
}
