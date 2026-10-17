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

// Package slapdexporter is ADR-033 Surface 3: a generic slapd exporter that
// translates the cn=Monitor tree into OpenMetrics, collected on scrape.
//
// It knows slapd, never slaptain (ADR-033 amendment 2026-10-08): it imports
// nothing from the API package or the controllers, reads no CR, and puts no
// site, cluster or pod label on anything. That context belongs to the scrape
// target's labels.
//
// It maps each meaningful numeric cn=Monitor value to a named, correctly
// typed family (ADR-033 decision 4, "Curated families"): counters end in
// _total, everything else is a gauge, and a label is used only where summing
// over it means something (op, direction, suffix). The mapping is data — the
// monitorFamilies and mdbStatFamilies tables. The exporter's own families are
// openldap_up and openldap_exporter_{dials,binds}_total{result}.
//
// A reading that could not be taken is absent from that scrape — never a 0 and
// never the last value (ADR-033 decision 5).
package slapdexporter

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// BindMode is how the exporter authenticates to slapd (--bind).
type BindMode string

const (
	// BindExternal is SASL EXTERNAL — over ldapi, the peer credentials of the
	// socket. The sidecar's mode: no password, no Secret.
	BindExternal BindMode = "external"
	// BindSimple is a simple bind as --bind-dn with the password read from
	// --bind-password-file.
	BindSimple BindMode = "simple"
	// BindNone performs no bind at all: the anonymous view.
	BindNone BindMode = "none"
)

const (
	// DefaultURI is --uri's default: slapd's ldapi socket in a slaptain pod.
	DefaultURI = "ldapi:///run/openldap/slapd.ldapi"
	// DefaultListen is --listen's default; the container port is named "metrics".
	DefaultListen = ":9330"
	// DefaultTimeout is --timeout's default: below Prometheus's default 10s
	// scrape timeout, so a hung slapd costs one scrape its cn=Monitor series,
	// not the whole scrape.
	DefaultTimeout = 5 * time.Second
	// MetricsPath is where the exposition is served.
	MetricsPath = "/metrics"
)

// Options is the validated configuration of one exporter process.
type Options struct {
	// URI is the slapd URI in a form ldap.DialURL accepts (see NormalizeURI).
	URI string
	// Bind is the authentication mode.
	Bind BindMode
	// BindDN is the simple-bind DN; empty unless Bind is BindSimple.
	BindDN string
	// BindPassword is the content of --bind-password-file; empty unless Bind is
	// BindSimple.
	BindPassword string
	// Listen is the HTTP listen address.
	Listen string
	// Timeout bounds one scrape's dial + bind + search together (--timeout).
	Timeout time.Duration
}

// ParseFlags parses the `slapd-exporter` subcommand's arguments (everything
// after the subcommand name) into validated Options.
//
// Flags: --uri (default DefaultURI, normalised by NormalizeURI), --bind
// (external|simple|none, default external), --bind-dn and --bind-password-file
// (simple only), --listen (default DefaultListen), --timeout (default
// DefaultTimeout).
//
// The password file is read once, here. Exactly one trailing line ending ("\n"
// or "\r\n") is removed — what an editor or `echo` leaves — and nothing else:
// a password may legitimately end in a space.
//
// It refuses — returns an error, so the process does not start — on an unknown
// --bind, on --bind=simple without a DN or without a readable password file, on
// credential flags given with a non-simple bind, on an empty --uri or one
// NormalizeURI rejects, and on a --timeout that is not positive. It never
// exits the process itself.
func ParseFlags(args []string) (Options, error) {
	fs := flag.NewFlagSet("slapd-exporter", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		uri     = fs.String("uri", DefaultURI, "slapd URI: ldapi:///<path>, ldapi://<%2f-encoded path>, ldap:// or ldaps://")
		bind    = fs.String("bind", string(BindExternal), "bind mode: external, simple or none")
		bindDN  = fs.String("bind-dn", "", "simple-bind DN (--bind=simple only)")
		pwFile  = fs.String("bind-password-file", "", "file holding the simple-bind password (--bind=simple only)")
		listen  = fs.String("listen", DefaultListen, "HTTP listen address; metrics at "+MetricsPath)
		timeout = fs.Duration("timeout", DefaultTimeout, "bound on one scrape's dial + bind + search")
	)
	if err := fs.Parse(args); err != nil {
		var usage bytes.Buffer
		fs.SetOutput(&usage)
		fs.PrintDefaults()
		return Options{}, fmt.Errorf("%w\nusage of slapd-exporter:\n%s", err, usage.String())
	}
	if fs.NArg() > 0 {
		return Options{}, fmt.Errorf("unexpected arguments %q", fs.Args())
	}

	if *uri == "" {
		return Options{}, errors.New("--uri must not be empty")
	}
	norm, err := NormalizeURI(*uri)
	if err != nil {
		return Options{}, err
	}
	if *timeout <= 0 {
		return Options{}, fmt.Errorf("--timeout must be positive, got %v", *timeout)
	}
	opts := Options{URI: norm, Bind: BindMode(*bind), Listen: *listen, Timeout: *timeout}

	switch opts.Bind {
	case BindSimple:
		if *bindDN == "" {
			return Options{}, errors.New("--bind=simple requires --bind-dn")
		}
		if *pwFile == "" {
			return Options{}, errors.New("--bind=simple requires --bind-password-file")
		}
		raw, err := os.ReadFile(*pwFile)
		if err != nil {
			return Options{}, fmt.Errorf("--bind-password-file: %w", err)
		}
		pw := string(raw)
		if p, ok := strings.CutSuffix(pw, "\r\n"); ok {
			pw = p
		} else {
			pw = strings.TrimSuffix(pw, "\n")
		}
		opts.BindDN, opts.BindPassword = *bindDN, pw
	case BindExternal, BindNone:
		if *bindDN != "" || *pwFile != "" {
			return Options{}, fmt.Errorf("--bind-dn and --bind-password-file apply to --bind=simple only, not --bind=%s",
				opts.Bind)
		}
	default:
		return Options{}, fmt.Errorf("--bind must be one of external, simple, none; got %q", *bind)
	}
	return opts, nil
}

// NormalizeURI returns raw in a form ldap.DialURL can dial.
//
// slapd's own -h spelling of an ldapi socket percent-encodes the path into the
// host part (ldapi://%2frun%2fopenldap%2fslapd.ldapi), which net/url rejects,
// and go-ldap with it before v3.4.15. That form, and only that form, is
// rewritten to ldapi:///<path>; an escape that does not decode is an error —
// at flag parsing, so a malformed --uri stops the exporter at startup instead
// of failing every scrape. Everything else
// passes through unchanged: validating a URI is ldap.DialURL's job, and a
// second validator here could only disagree with it.
func NormalizeURI(raw string) (string, error) {
	const ldapi = "ldapi://"
	if len(raw) <= len(ldapi) || !strings.EqualFold(raw[:len(ldapi)], ldapi) || raw[len(ldapi)] == '/' {
		return raw, nil
	}
	path, err := url.PathUnescape(raw[len(ldapi):])
	if err != nil {
		return "", fmt.Errorf("--uri %q: %w", raw, err)
	}
	return "ldapi://" + path, nil
}

// Sample is one metric sample derived from cn=Monitor.
type Sample struct {
	// Name is the full metric name, e.g. openldap_connections_total.
	Name string
	// Labels is the sample's complete label set: empty, or exactly one label.
	Labels map[string]string
	// Value is the sample value.
	Value float64
	// Type is the family's type: prometheus.CounterValue or GaugeValue.
	Type prometheus.ValueType
}

// SamplesFromEntries maps the entries of one cn=Monitor subtree search (all
// user and operational attributes) to samples. It is pure.
//
// Each monitorFamilies row selects one entry by DN (compared without case) —
// or, for a "cn=*,<parent>" row, every child of parent, labelled with the
// child's lowercased cn — and reads the first value of one attribute. The
// openldap_mdb_* rows read a database entry's olmMDB* attributes, labelled
// suffix=<the entry's namingContexts>.
//
// A value that is missing or does not parse — a number, or for
// monitorTimestamp a generalizedTime — yields no sample: absent, never 0
// (olmMDBPageSize does not exist on 2.6; a database opened before the monitor
// database carries no olmMDB* at all).
func SamplesFromEntries(entries []*ldap.Entry) []Sample {
	var out []Sample
	add := func(f family, labels map[string]string, raw string) {
		if v, ok := parseValue(f.attr, raw); ok {
			out = append(out, Sample{Name: f.name, Labels: labels, Value: v, Type: f.typ})
		}
	}
	for _, e := range entries {
		for _, f := range monitorFamilies {
			if lv, ok := matchDN(f.dn, e.DN); ok {
				labels := map[string]string{}
				if f.label != "" {
					labels[f.label] = lv
				}
				add(f, labels, e.GetAttributeValue(f.attr))
			}
		}
		if sfx := e.GetAttributeValue("namingContexts"); sfx != "" {
			for _, f := range mdbStatFamilies {
				add(f, map[string]string{labelSuffix: sfx}, e.GetAttributeValue(f.attr))
			}
		}
	}
	return out
}

const (
	labelSuffix = "suffix"
	counter     = prometheus.CounterValue
	gauge       = prometheus.GaugeValue
	// attrTimestamp holds a generalizedTime, exported as Unix seconds.
	attrTimestamp = "monitorTimestamp"
	// The cn=Monitor attributes most families read their value from.
	attrMonitorCounter = "monitorCounter"
	attrMonitoredInfo  = "monitoredInfo"
	attrMDBPageSize    = "olmMDBPageSize"
	// Label names shared by several families.
	labelDirection = "direction"
	labelResult    = "result"
)

// family maps one cn=Monitor attribute to one metric family.
type family struct {
	name, help string
	typ        prometheus.ValueType
	// dn selects the entry; "cn=*,<parent>" selects every child of parent and
	// puts its lowercased cn value in label. Empty for mdbStatFamilies.
	dn, label string
	attr      string
}

// monitorFamilies is ADR-033 decision 4's table, one row per family. What it
// leaves out on purpose: the cn=Operations aggregate (the sum over op),
// cn=Uptime (time() - openldap_start_time_seconds), cn=Current,cn=Time, and
// the string-valued entries. The coverage test holds every recorded numeric
// value to this table or to that list.
var monitorFamilies = []family{
	{name: "openldap_operations_initiated_total", typ: counter, dn: "cn=*,cn=Operations,cn=Monitor", label: "op",
		attr: "monitorOpInitiated", help: "Operations initiated, by type (cn=<Op>,cn=Operations monitorOpInitiated)."},
	{name: "openldap_operations_completed_total", typ: counter, dn: "cn=*,cn=Operations,cn=Monitor", label: "op",
		attr: "monitorOpCompleted", help: "Operations completed, by type (cn=<Op>,cn=Operations monitorOpCompleted)."},
	{name: "openldap_connections_total", typ: counter, dn: "cn=Total,cn=Connections,cn=Monitor",
		attr: attrMonitorCounter, help: "Connections accepted since start (cn=Total,cn=Connections)."},
	{name: "openldap_connections_current", typ: gauge, dn: "cn=Current,cn=Connections,cn=Monitor",
		attr: attrMonitorCounter, help: "Connections open now (cn=Current,cn=Connections)."},
	{name: "openldap_max_file_descriptors", typ: gauge, dn: "cn=Max File Descriptors,cn=Connections,cn=Monitor",
		attr: attrMonitorCounter, help: "File descriptor limit (cn=Max File Descriptors,cn=Connections)."},
	{name: "openldap_sent_bytes_total", typ: counter, dn: "cn=Bytes,cn=Statistics,cn=Monitor",
		attr: attrMonitorCounter, help: "Bytes sent (cn=Bytes,cn=Statistics)."},
	{name: "openldap_sent_pdus_total", typ: counter, dn: "cn=PDU,cn=Statistics,cn=Monitor",
		attr: attrMonitorCounter, help: "PDUs sent (cn=PDU,cn=Statistics)."},
	{name: "openldap_sent_entries_total", typ: counter, dn: "cn=Entries,cn=Statistics,cn=Monitor",
		attr: attrMonitorCounter, help: "Entries sent (cn=Entries,cn=Statistics)."},
	{name: "openldap_sent_referrals_total", typ: counter, dn: "cn=Referrals,cn=Statistics,cn=Monitor",
		attr: attrMonitorCounter, help: "Referrals sent (cn=Referrals,cn=Statistics)."},
	{name: "openldap_threads_open", typ: gauge, dn: "cn=Open,cn=Threads,cn=Monitor",
		attr: attrMonitoredInfo, help: "Worker threads open (cn=Open,cn=Threads)."},
	{name: "openldap_threads_active", typ: gauge, dn: "cn=Active,cn=Threads,cn=Monitor",
		attr: attrMonitoredInfo, help: "Worker threads active (cn=Active,cn=Threads)."},
	{name: "openldap_threads_pending", typ: gauge, dn: "cn=Pending,cn=Threads,cn=Monitor",
		attr: attrMonitoredInfo, help: "Operations pending a thread (cn=Pending,cn=Threads)."},
	{name: "openldap_threads_starting", typ: gauge, dn: "cn=Starting,cn=Threads,cn=Monitor",
		attr: attrMonitoredInfo, help: "Worker threads starting (cn=Starting,cn=Threads)."},
	{name: "openldap_threads_backload", typ: gauge, dn: "cn=Backload,cn=Threads,cn=Monitor",
		attr: attrMonitoredInfo, help: "Active plus pending (cn=Backload,cn=Threads)."},
	{name: "openldap_threads_max", typ: gauge, dn: "cn=Max,cn=Threads,cn=Monitor",
		attr: attrMonitoredInfo, help: "Worker thread limit (cn=Max,cn=Threads)."},
	{name: "openldap_threads_max_pending", typ: gauge, dn: "cn=Max Pending,cn=Threads,cn=Monitor",
		attr: attrMonitoredInfo, help: "Pending operation limit (cn=Max Pending,cn=Threads)."},
	{name: "openldap_waiters", typ: gauge, dn: "cn=*,cn=Waiters,cn=Monitor", label: labelDirection,
		attr: attrMonitorCounter, help: "Descriptors waiting, by direction (cn=Read|Write,cn=Waiters)."},
	{name: "openldap_start_time_seconds", typ: gauge, dn: "cn=Start,cn=Time,cn=Monitor",
		attr: attrTimestamp, help: "slapd start time, Unix seconds (cn=Start,cn=Time monitorTimestamp)."},
}

// mdbStatFamilies are back-mdb's per-database statistics, labelled by suffix.
var mdbStatFamilies = []family{
	{name: "openldap_mdb_pages_max", typ: gauge, attr: "olmMDBPagesMax", help: "olmMDBPagesMax: pages in the LMDB map"},
	{name: "openldap_mdb_pages_used", typ: gauge, attr: "olmMDBPagesUsed", help: "olmMDBPagesUsed: pages in use"},
	{name: "openldap_mdb_pages_free", typ: gauge, attr: "olmMDBPagesFree", help: "olmMDBPagesFree: free pages"},
	{name: "openldap_mdb_readers_max", typ: gauge, attr: "olmMDBReadersMax", help: "olmMDBReadersMax: reader slots"},
	{name: "openldap_mdb_readers_used", typ: gauge, attr: "olmMDBReadersUsed",
		help: "olmMDBReadersUsed: reader slots in use"},
	{name: "openldap_mdb_entries", typ: gauge, attr: "olmMDBEntries", help: "olmMDBEntries: entries in the database"},
	{name: "openldap_mdb_page_size_bytes", typ: gauge, attr: attrMDBPageSize,
		help: "olmMDBPageSize: LMDB page size (2.7+)"},
}

// matchDN reports whether dn is the entry pattern names, comparing without
// case. For a "cn=*,<parent>" pattern it matches any direct cn= child of
// parent and returns that child's cn value, lowercased.
func matchDN(pattern, dn string) (string, bool) {
	parent, wild := strings.CutPrefix(pattern, "cn=*,")
	if !wild {
		return "", strings.EqualFold(pattern, dn)
	}
	rdn, rest, ok := strings.Cut(dn, ",")
	if !ok || len(rdn) <= 3 || !strings.EqualFold(rdn[:3], "cn=") || !strings.EqualFold(rest, parent) {
		return "", false
	}
	return strings.ToLower(rdn[3:]), true
}

// parseValue reads one cn=Monitor value: a number, or for monitorTimestamp a
// generalizedTime as Unix seconds.
func parseValue(attr, raw string) (float64, bool) {
	if raw == "" {
		return 0, false
	}
	if attr == attrTimestamp {
		t, err := time.Parse("20060102150405Z", raw)
		return float64(t.Unix()), err == nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	return v, err == nil
}

// Conn is the slice of *ldap.Conn the collector uses; *ldap.Conn satisfies it.
type Conn interface {
	Bind(username, password string) error
	ExternalBind() error
	Search(req *ldap.SearchRequest) (*ldap.SearchResult, error)
	Close() error
}

// DialFunc opens a transport to the (normalised) URI. The collector calls it
// once per scrape, with that scrape's deadline: now + Options.Timeout.
//
// The deadline bounds the dial and every operation on the returned Conn — the
// production DialFunc sets it on the socket (net.Conn.SetDeadline before
// ldap.NewConn/Start), so a slapd that stops answering fails the pending
// operation at the deadline instead of holding the scrape. A DialFunc that
// cannot complete by the deadline returns an error and no Conn.
type DialFunc func(uri string, deadline time.Time) (Conn, error)

const schemeLDAPS = "ldaps"

// DialLDAP is the production DialFunc. It dials the socket itself — unix for
// ldapi, tcp for ldap, tcp + TLS for ldaps, with go-ldap's default socket and
// ports — sets deadline on it, and only then hands it to ldap.NewConn, so the
// dial, the TLS handshake and every later operation fail at the deadline.
func DialLDAP(uri string, deadline time.Time) (Conn, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}
	d := &net.Dialer{Deadline: deadline}
	var (
		nc    net.Conn
		isTLS bool
	)
	switch u.Scheme {
	case "ldapi":
		path := u.Path
		if path == "" || path == "/" {
			path = "/var/run/slapd/ldapi" // go-ldap's DialURL default
		}
		nc, err = d.Dial("unix", path)
	case "ldap", schemeLDAPS:
		addr, serverName := tcpTarget(u)
		nc, err = d.Dial("tcp", addr)
		if err == nil && u.Scheme == schemeLDAPS {
			isTLS = true
			nc = tls.Client(nc, &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12})
		}
	default:
		return nil, fmt.Errorf("unsupported scheme %q in %q (want ldapi, ldap or ldaps)", u.Scheme, uri)
	}
	if err != nil {
		return nil, err
	}
	if err := nc.SetDeadline(deadline); err != nil {
		_ = nc.Close()
		return nil, err
	}
	if tc, ok := nc.(*tls.Conn); ok {
		if err := tc.Handshake(); err != nil {
			_ = nc.Close()
			return nil, err
		}
	}
	conn := ldap.NewConn(nc, isTLS)
	conn.Start()
	return conn, nil
}

// tcpTarget derives, from an ldap:// or ldaps:// URL, the TCP address to dial
// and the TLS ServerName to verify. The port defaults to 389 (ldap) or 636
// (ldaps). An IPv6 literal is bracketed in the address ([::1]:389) and bare in
// the ServerName (::1) — the URL's brackets are syntax, not part of the host.
func tcpTarget(u *url.URL) (addr, serverName string) {
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = ldap.DefaultLdapPort
		if u.Scheme == schemeLDAPS {
			port = ldap.DefaultLdapsPort
		}
	}
	return net.JoinHostPort(host, port), host
}

// Collector is a prometheus.Collector that reads cn=Monitor on every Collect —
// never on a background timer.
//
// Each Collect dials, binds as Options.Bind says (no bind for BindNone), issues
// one subtree search of cn=Monitor requesting user and operational attributes,
// and closes the connection. openldap_exporter_dials_total and
// openldap_exporter_binds_total count the attempts by result="ok"|"fail" (the
// binds counter is never touched under BindNone). openldap_up is 1 when this
// scrape's read succeeded and 0 when it did not. If the dial, the bind or the
// search fails — including a search that returns entries together with an
// error — no cn=Monitor-derived sample is emitted for that scrape: absent,
// never the previous value.
//
// Options.Timeout bounds dial + bind + search of one scrape together: the
// collector hands the DialFunc the scrape's deadline, which the production
// DialFunc sets on the socket. A call still blocked at the deadline fails as
// its step (a hung dial counts as a failed dial, a hung bind as a failed
// bind), and the connection is closed. A hung slapd (ADR-030's freeze accepts
// connections and never answers) costs one scrape its cn=Monitor series and
// piles up nothing.
type Collector struct {
	opts  Options
	dial  DialFunc
	dials *prometheus.CounterVec
	binds *prometheus.CounterVec
	up    *prometheus.Desc
	descs map[string]*prometheus.Desc
}

const (
	resultOK   = "ok"
	resultFail = "fail"
)

// NewCollector returns a Collector for opts that opens connections with dial.
//
// A zero (or negative) opts.Timeout means DefaultTimeout: the zero Options is
// usable, and no scrape is ever unbounded.
func NewCollector(opts Options, dial DialFunc) *Collector {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	c := &Collector{
		opts: opts,
		dial: dial,
		dials: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "openldap_exporter_dials_total", Help: "Connection attempts to slapd, by result.",
		}, []string{labelResult}),
		binds: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "openldap_exporter_binds_total", Help: "Bind attempts to slapd, by result.",
		}, []string{labelResult}),
		up: prometheus.NewDesc("openldap_up",
			"1 when this scrape read cn=Monitor, 0 when the dial, bind or search failed.", nil, nil),
		descs: map[string]*prometheus.Desc{},
	}
	for _, f := range monitorFamilies {
		var labels []string
		if f.label != "" {
			labels = []string{f.label}
		}
		c.descs[f.name] = prometheus.NewDesc(f.name, f.help, labels, nil)
	}
	for _, f := range mdbStatFamilies {
		c.descs[f.name] = prometheus.NewDesc(f.name, f.help, []string{labelSuffix}, nil)
	}
	return c
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	c.dials.Describe(ch)
	c.binds.Describe(ch)
	ch <- c.up
	for _, d := range c.descs {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	entries, err := c.read()
	if err != nil {
		log.Printf("slapd-exporter: cn=Monitor read failed: %v", err)
	}
	c.dials.Collect(ch)
	c.binds.Collect(ch)
	if err != nil {
		ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 1)
	for _, s := range SamplesFromEntries(entries) {
		labelValues := slices.Collect(maps.Values(s.Labels)) // at most one label per family
		// A label value that is not valid UTF-8 costs this one series.
		if m, err := prometheus.NewConstMetric(c.descs[s.Name], s.Type, s.Value, labelValues...); err == nil {
			ch <- m
		}
	}
}

// monitorFilter selects the classes the families read: cn=Threads and cn=Time
// entries and the per-database entries (plus the olmMDBDatabase auxiliary) are
// monitoredObject, cn=Connections, cn=Statistics and cn=Waiters entries
// monitorCounterObject, cn=Operations children monitorOperation. The
// monitorConnection entries — one per open connection, ~25 attributes each —
// do not match, nor do the monitorContainer entries the table leaves out.
const monitorFilter = "(|(objectClass=monitoredObject)(objectClass=monitorCounterObject)(objectClass=monitorOperation))"

// read takes one cn=Monitor reading: dial, bind, one subtree search, all
// bounded by one deadline on the socket. Any failure is an error and no
// entries.
func (c *Collector) read() ([]*ldap.Entry, error) {
	conn, err := c.dial(c.opts.URI, time.Now().Add(c.opts.Timeout))
	if err != nil {
		c.dials.WithLabelValues(resultFail).Inc()
		return nil, fmt.Errorf("dial %s: %w", c.opts.URI, err)
	}
	c.dials.WithLabelValues(resultOK).Inc()
	defer func() { _ = conn.Close() }()

	if c.opts.Bind != BindNone {
		if c.opts.Bind == BindSimple {
			err = conn.Bind(c.opts.BindDN, c.opts.BindPassword)
		} else {
			err = conn.ExternalBind()
		}
		if err != nil {
			c.binds.WithLabelValues(resultFail).Inc()
			return nil, fmt.Errorf("bind (%s): %w", c.opts.Bind, err)
		}
		c.binds.WithLabelValues(resultOK).Inc()
	}

	res, err := conn.Search(ldap.NewSearchRequest("cn=Monitor", ldap.ScopeWholeSubtree, ldap.NeverDerefAliases,
		0, 0, false, monitorFilter, []string{"*", "+"}, nil))
	if err != nil {
		// A partial result is not a reading (decision 5).
		return nil, fmt.Errorf("search cn=Monitor: %w", err)
	}
	return res.Entries, nil
}

// NewHandler serves c at MetricsPath. A scrape whose cn=Monitor read failed is
// still answered 200 — the exporter's own counters are evidence too, and a
// failing slapd must not read as a failing exporter.
func NewHandler(c *Collector) http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	mux := http.NewServeMux()
	mux.Handle(MetricsPath, promhttp.HandlerFor(reg, promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError}))
	return mux
}

// Serve listens on opts.Listen and serves NewHandler(NewCollector(opts,
// DialLDAP)) until ctx is done. An unreachable slapd is never a reason to
// return: a crash-looping sidecar is noise, and the exporter must never take
// its pod NotReady.
func Serve(ctx context.Context, opts Options) error {
	ln, err := net.Listen("tcp", opts.Listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: NewHandler(NewCollector(opts, DialLDAP)), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("slapd-exporter: reading %s (bind %s), serving %s on %s", opts.URI, opts.Bind, MetricsPath, ln.Addr())
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), opts.Timeout+time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	}
}
