package e2e_test

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The degrades-at-scale and hygiene tunables (ADR-024, production-config review
// findings 6-16).
//
// Unlike the many-entries fixture in scale_test.go, none of these needs volume
// to be observable: they are attributes in cn=config, present or absent. What
// they have in common with it is that their ABSENCE has no symptom on a healthy
// fixture — a cluster with no connection timeouts, no checkpoint, no TLS floor
// and no monitor backend passes every other spec in this suite — so an assertion
// on cn=config is the only thing standing between "we decided this" and "we
// stopped setting it two releases ago".
//
// cn=config is node-local (ADR-002), so every assertion runs against every pod
// independently, read-only pods included: a tunable converged on two pods out of
// three is a pod that behaves differently from its peers under exactly the
// conditions nobody is watching.
//
// Ungated on purpose. These cost one bind and a handful of base searches per
// pod, which buys a standing guard on twelve decisions.
var _ = Describe("scale and operations tunables", Label("tunables"), Ordered, ContinueOnFailure, func() {

	var (
		pods   []string
		rwPods []string
	)

	BeforeAll(func(ctx SpecContext) {
		sts, err := k8sClient.AppsV1().StatefulSets(namespace).Get(ctx, "slapd", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		replicas := int32(1)
		if sts.Spec.Replicas != nil {
			replicas = *sts.Spec.Replicas
		}
		for i := int32(0); i < replicas; i++ {
			rwPods = append(rwPods, fmt.Sprintf("slapd-%d", i))
		}
		pods = append(pods, rwPods...)

		// Read-only replicas are separate pods with their own cn=config. They
		// serve clients, so the connection lifetimes and the TLS floor matter
		// there for exactly the same reasons.
		if roSts, err := k8sClient.AppsV1().StatefulSets(namespace).
			Get(ctx, "slapd-readonly", metav1.GetOptions{}); err == nil {
			ro := int32(0)
			if roSts.Spec.Replicas != nil {
				ro = *roSts.Spec.Replicas
			}
			for i := int32(0); i < ro; i++ {
				pods = append(pods, fmt.Sprintf("slapd-readonly-%d", i))
			}
		}
		Expect(pods).NotTo(BeEmpty())
	})

	// ── Server-global: connection lifetimes, tool threads, TLS, password hash ─

	It("pins the server-global tunables on every pod", func(ctx SpecContext) {
		for _, pod := range pods {
			By("inspecting cn=config on " + pod)
			conn := dialPodConfigEventually(ctx, pod)

			// NOT asserted: olcIdleTimeout and olcWriteTimeout. The
			// production-config review wanted both converged; writing either on
			// a running slapd 2.7.1 hangs the process (see
			// ensureGlobalTunables for the captured evidence), so the operator
			// does not write them and there is nothing here to guard. They stay
			// in docs/BACKLOG.md as a bootstrap-time item.

			// slapadd reads this during a restore, with the cluster at zero
			// replicas for the whole window (finding 12).
			Expect(atoi(singleConfigValue(conn, "cn=config", "olcToolThreads"))).
				To(BeNumerically(">=", 1), "pod %s: olcToolThreads", pod)

			// Without a floor, the minimum protocol version is whatever the
			// runtime image's OpenSSL permits — a policy that changes silently
			// on a base-image bump (finding 15). 3.3 is TLS 1.2 in slapd's
			// <major>.<minor> spelling.
			min := singleConfigValue(conn, "cn=config", "olcTLSProtocolMin")
			Expect(min).NotTo(Equal("0.0"),
				"pod %s: olcTLSProtocolMin is unpinned (0.0)", pod)
			Expect(min).To(BeElementOf("3.3", "3.4"),
				"pod %s: the TLS floor must be TLS 1.2 or better, got %q", pod, min)

			// The scheme slapd uses when IT hashes a password for a client
			// (finding 16) — and WHERE it is set, which slapd has an opinion
			// about:
			//
			//   olcPasswordHash: value #0: setting password scheme in the
			//   global entry is deprecated. The server may refuse to start if
			//   it is provided by a loadable module, please move it to the
			//   frontend database instead
			//
			// Harmless while the scheme is built in ({SSHA}); a startup
			// failure the day slaptain ships pw-argon2 and someone asks for
			// {ARGON2}. Both halves are asserted, because writing it to the
			// frontend while leaving the old copy behind fixes nothing: the
			// warning survives and two entries disagree about policy.
			Expect(singleConfigValue(conn, "olcDatabase={-1}frontend,cn=config", "olcPasswordHash")).
				NotTo(BeEmpty(), "pod %s: olcPasswordHash is unset on the frontend database", pod)
			Expect(configValues(conn, "cn=config", "olcPasswordHash")).
				To(BeEmpty(), "pod %s: olcPasswordHash still present in the DEPRECATED global entry", pod)

			conn.Close()
		}
	}, NodeTimeout(8*time.Minute))

	// ── Per-database: durability and read-transaction bounds ────────────────

	It("converges the per-database durability tunables on every pod", func(ctx SpecContext) {
		for _, pod := range rwPods {
			By("inspecting the databases on " + pod)
			conn := dialPodConfigEventually(ctx, pod)

			dataDN := configDBDN(conn, baseDN)
			Expect(dataDN).NotTo(BeEmpty(), "pod %s: no data DB for %s", pod, baseDN)

			// A checkpoint is what bounds the write window noSync opens. It is
			// written unconditionally so that turning noSync on later is a
			// one-field change that is already safe (findings 6 and 7).
			ckpt := singleConfigValue(conn, dataDN, "olcDbCheckpoint")
			Expect(ckpt).To(MatchRegexp(`^\d+ \d+$`),
				"pod %s: data DB olcDbCheckpoint = %q, want \"<kbyte> <min>\"", pod, ckpt)

			// noSync is converged, not create-only: the attribute must be
			// present with an explicit value, which is what proves the operator
			// writes it rather than leaving whatever creation happened to set.
			Expect(strings.ToUpper(singleConfigValue(conn, dataDN, "olcDbNoSync"))).
				To(BeElementOf("TRUE", "FALSE"),
					"pod %s: data DB olcDbNoSync must be explicitly set", pod)

			// A long read transaction — a syncrepl full refresh, a bulk export —
			// pins free pages and grows the map (finding 9).
			Expect(atoi(singleConfigValue(conn, dataDN, "olcDbRtxnSize"))).
				To(BeNumerically(">", 0), "pod %s: data DB olcDbRtxnSize", pod)

			// The journal is written on the same hot path as the data, so it
			// carries the same posture on its own, longer interval.
			logDN := configDBDN(conn, accesslogSuffixFor(dbCRName))
			if logDN != "" {
				logCkpt := singleConfigValue(conn, logDN, "olcDbCheckpoint")
				Expect(logCkpt).To(MatchRegexp(`^\d+ \d+$`),
					"pod %s: accesslog olcDbCheckpoint = %q", pod, logCkpt)
			}

			conn.Close()
		}
	}, NodeTimeout(8*time.Minute))

	// ── Syncrepl stanza hardening (finding 11) ──────────────────────────────

	It("hardens every syncrepl stanza with timeouts and a keepalive", func(ctx SpecContext) {
		for _, pod := range rwPods {
			conn := dialPodConfigEventually(ctx, pod)
			dataDN := configDBDN(conn, baseDN)
			Expect(dataDN).NotTo(BeEmpty())

			stanzas := ldapSearchAttr(conn, dataDN, "olcSyncRepl")
			if len(stanzas) == 0 {
				conn.Close()
				Skip("this cluster has no syncrepl stanzas (single replica, no external peers)")
			}
			for _, s := range stanzas {
				// Assert on the raw stanza; print the redacted one. A failure
				// message goes into CI logs and gets pasted into tickets, and a
				// stanza carries the replication password in the clear (a real
				// one was captured in an e2e log this way — ADR-027 follow-up).
				red := redactCredentials(s)
				Expect(s).To(ContainSubstring("network-timeout="),
					"pod %s: a stanza with no network-timeout notices a dead provider "+
						"only when TCP gives up:\n%s", pod, red)
				Expect(s).To(ContainSubstring("keepalive="),
					"pod %s: a refreshAndPersist connection is idle by design between "+
						"writes; without keepalive a silently dropped flow leaves a "+
						"consumer that has stopped consuming and still reports Synced:\n%s",
					pod, red)
				// timeout= (LDAP_OPT_TIMEOUT) must never appear: in the refresh
				// phase it is a BLOCKING ldap_result wait on a threadpool
				// thread, and a cn=config write pauses the whole pool — so any
				// refresh-phase consumer froze the entire server for up to the
				// timeout on every config MOD (measured: three ~300s
				// total-silence freezes per operator convergence pass). The
				// leading space keeps this from matching network-timeout=.
				Expect(s).NotTo(ContainSubstring(" timeout="),
					"pod %s: timeout= turns every cn=config write into a "+
						"server-wide freeze while a consumer is in refresh phase:\n%s",
					pod, red)
				// timelimit= is a server-side search time limit and WOULD abort
				// the persistent search.
				Expect(s).NotTo(ContainSubstring("timelimit="),
					"pod %s: timelimit= would kill the persistent search:\n%s", pod, red)
			}
			conn.Close()
		}
	}, NodeTimeout(8*time.Minute))

	// ── cn=monitor (ADR-033 amendment 2026-10-08) ───────────────────────────
	//
	// The monitor backend is operator-converged R1 and always present: no spec
	// field, no switch. These pin the result on every pod — read-only pods
	// included, because reconcileTunables visits them too and a pod without a
	// monitor tree is one no scraper can see.
	//
	// Problems are collected across every pod and asserted once at the end, so
	// a red run names EVERY pod that is missing something rather than stopping
	// at the first.

	It("runs the monitor backend on every pod, readable only by the node-local identities",
		Label("monitor"), func(ctx SpecContext) {
			readers := monitorReaders(ctx)
			var problems []string

			for _, pod := range pods {
				By("checking the monitor database on " + pod)
				conn := dialAnyPodEventually(ctx, pod, "cn=admin,cn=config", rootPW)
				res, err := conn.Search(ldap.NewSearchRequest(
					"cn=config", ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
					0, 0, false, "(objectClass=olcMonitorConfig)",
					[]string{"olcRootDN"}, nil))
				_ = conn.Close()
				Expect(err).NotTo(HaveOccurred(), "pod %s: search for the monitor database", pod)
				if len(res.Entries) != 1 {
					problems = append(problems, fmt.Sprintf(
						"pod %s: %d monitor databases, want exactly 1 — the operator loads "+
							"back_monitor and adds it on every pod (ADR-033 amendment)",
						pod, len(res.Entries)))
					continue // nothing to read on this pod; the rest would be noise
				}
				if got := res.Entries[0].GetEqualFoldAttributeValue("olcRootDN"); got != "cn=admin,cn=config" {
					problems = append(problems, fmt.Sprintf(
						"pod %s: monitor database olcRootDN = %q, want cn=admin,cn=config", pod, got))
				}

				// Every node-local replication identity can read it (ADR-027,
				// ADR-008 reuse) — not just the first database's: the ACL grants
				// the auth suffix's children, so a second database's identity
				// is the case that tells a constant ACL from an enumerating one.
				for _, rd := range readers {
					mon, err := tryDialAnyPod(pod, rd.dn, rd.pw)
					if err != nil {
						problems = append(problems, fmt.Sprintf("pod %s: %v", pod, err))
						continue
					}
					counters, err := mon.Search(ldap.NewSearchRequest(
						"cn=Monitor", ldap.ScopeBaseObject, ldap.NeverDerefAliases,
						0, 10, false, "(objectClass=*)", []string{"monitoredInfo"}, nil))
					_ = mon.Close()
					if err != nil || len(counters.Entries) != 1 {
						problems = append(problems, fmt.Sprintf(
							"pod %s: %s must be able to read cn=Monitor (err=%v)", pod, rd.dn, err))
					}
				}

				// And nobody else. cn=Monitor lists the DN of every current bind
				// and every connection peer, so it is at least as closed as the
				// databases it reflects (ADR-020's rule, a different tree).
				// `by * none` answers No such object, which is what the
				// constant ACL was measured to produce. Only meaningful AFTER
				// presence is established above: a pod with no monitor database
				// answers the same thing.
				anon, err := tryDialAnyPod(pod, "", "")
				Expect(err).NotTo(HaveOccurred())
				_, err = anon.Search(ldap.NewSearchRequest(
					"cn=Monitor", ldap.ScopeBaseObject, ldap.NeverDerefAliases,
					0, 10, false, "(objectClass=*)", []string{"monitoredInfo"}, nil))
				_ = anon.Close()
				if !ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject) {
					problems = append(problems, fmt.Sprintf(
						"pod %s: anonymous read of cn=Monitor must answer No such object (32), got err=%v",
						pod, err))
				}
			}
			Expect(problems).To(BeEmpty(), "\n%s", strings.Join(problems, "\n"))
		}, NodeTimeout(8*time.Minute))

	// The property the milestone exists for: at rest, the operator writes
	// NOTHING to the monitor database. Every cn=config write is one the
	// timeout= freeze class turned into a server-wide pause (48f688d), and the
	// withdrawn implementation rewrote this ACL on every database add/remove.
	// modifyTimestamp and entryCSN move on any write to the entry, so both are
	// read, a window longer than one reconcile tick is waited out, and both
	// are read again.
	It("writes nothing to the monitor database at rest, on every pod",
		Label("monitor", "monitor-at-rest"), func(ctx SpecContext) {
			type stamp struct{ modified, csn string }
			read := func() map[string]stamp {
				out := map[string]stamp{}
				for _, pod := range pods {
					conn := dialAnyPodEventually(ctx, pod, "cn=admin,cn=config", rootPW)
					res, err := conn.Search(ldap.NewSearchRequest(
						"cn=config", ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
						0, 0, false, "(objectClass=olcMonitorConfig)",
						[]string{"modifyTimestamp", "entryCSN"}, nil))
					_ = conn.Close()
					Expect(err).NotTo(HaveOccurred(), "pod %s: search for the monitor database", pod)
					Expect(res.Entries).To(HaveLen(1), "pod %s: no monitor database to watch", pod)
					e := res.Entries[0]
					out[pod] = stamp{
						modified: e.GetEqualFoldAttributeValue("modifyTimestamp"),
						csn:      e.GetEqualFoldAttributeValue("entryCSN"),
					}
				}
				return out
			}

			before := read()
			By(fmt.Sprintf("waiting %s — longer than one SlapdCluster reconcile tick", monitorAtRestWindow))
			select {
			case <-ctx.Done():
				Fail("interrupted while waiting out the at-rest window")
			case <-time.After(monitorAtRestWindow):
			}
			after := read()

			var problems []string
			for _, pod := range pods {
				if before[pod] != after[pod] {
					problems = append(problems, fmt.Sprintf(
						"pod %s: a write happened at rest — the monitor database changed within %s "+
							"with nothing to converge (modifyTimestamp %s → %s, entryCSN %s → %s). "+
							"Either something hand-edited it, or the operator is rewriting it every "+
							"pass (ACL comparison broken?)",
						pod, monitorAtRestWindow, before[pod].modified, after[pod].modified,
						before[pod].csn, after[pod].csn))
				}
			}
			Expect(problems).To(BeEmpty(), "\n%s", strings.Join(problems, "\n"))
		}, NodeTimeout(8*time.Minute))

	// olmMDB* (map size, pages, readers, entries) appear under a database's
	// cn=Database N,cn=Databases,cn=Monitor entry only if the monitor database
	// existed when that mdb database was OPENED. olcMonitoring is not the knob:
	// it is on by default for mdb and setting it live does nothing (measured).
	// So the property that matters is the statistics actually being there.
	It("exposes olmMDB* statistics for every mdb database the operator manages, on every pod",
		Label("monitor"), func(ctx SpecContext) {
			dbs := clusterDatabases(ctx)
			readers := monitorReaders(ctx)
			Expect(readers).NotTo(BeEmpty())
			var problems []string

			for _, pod := range pods {
				By("reading cn=Databases,cn=Monitor on " + pod)

				// Which mdb suffixes this pod should expose: the auth database,
				// every data database, and — where the pod has one (RW pods) —
				// each data database's journal. cn=config tells us which
				// journals exist; their existence is asserted elsewhere.
				cfg := dialAnyPodEventually(ctx, pod, "cn=admin,cn=config", rootPW)
				suffixes := []string{ldapv1alpha1.AuthSuffix}
				for _, sd := range dbs {
					suffixes = append(suffixes, sd.Spec.Suffix)
					if configDBDN(cfg, accesslogSuffixFor(sd.Name)) != "" {
						suffixes = append(suffixes, accesslogSuffixFor(sd.Name))
					}
				}
				_ = cfg.Close()

				mon := dialAnyPodEventually(ctx, pod, readers[0].dn, readers[0].pw)
				res, err := mon.Search(ldap.NewSearchRequest(
					"cn=Databases,cn=Monitor", ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
					0, 30, false, "(objectClass=*)", []string{"*", "+"}, nil))
				_ = mon.Close()
				if err != nil {
					problems = append(problems, fmt.Sprintf(
						"pod %s: read cn=Databases,cn=Monitor as %s: %v", pod, readers[0].dn, err))
					continue
				}

				for _, suffix := range suffixes {
					var matches []*ldap.Entry
					for _, e := range res.Entries {
						for _, nc := range e.GetEqualFoldAttributeValues("namingContexts") {
							if strings.EqualFold(nc, suffix) {
								matches = append(matches, e)
								break
							}
						}
					}
					if len(matches) > 1 {
						var dns []string
						for _, m := range matches {
							dns = append(dns, m.DN)
						}
						problems = append(problems, fmt.Sprintf(
							"pod %s: %s: %d cn=Databases,cn=Monitor entries list this suffix (%s) — "+
								"two databases serve one suffix, or the match is wrong",
							pod, suffix, len(matches), strings.Join(dns, ", ")))
						continue
					}
					if len(matches) == 0 {
						problems = append(problems, fmt.Sprintf(
							"pod %s: %s: no cn=Databases,cn=Monitor entry with namingContexts=%s",
							pod, suffix, suffix))
						continue
					}
					entry := matches[0]
					var olm []string
					for _, a := range entry.Attributes {
						if strings.HasPrefix(strings.ToLower(a.Name), "olmmdb") {
							olm = append(olm, a.Name)
						}
					}
					if len(olm) == 0 {
						problems = append(problems, fmt.Sprintf(
							"pod %s: %s (%s) carries no olmMDB* attributes — the database was opened "+
								"before the monitor database existed; it needs a slapd restart, or the "+
								"create-order rule was broken (ADR-033 amendment 2026-10-08)",
							pod, suffix, entry.DN))
					}
				}
			}
			Expect(problems).To(BeEmpty(), "\n%s", strings.Join(problems, "\n"))
		}, NodeTimeout(8*time.Minute))
})

// ── helpers ──────────────────────────────────────────────────────────────────

// singleConfigValue reads one attribute expected to carry exactly one value,
// returning "" when the attribute is absent. Absence is a legitimate assertion
// target here — most of these findings ARE an absent attribute — so it is
// returned rather than failed on.
func singleConfigValue(conn *ldap.Conn, dn, attr string) string {
	res, err := conn.Search(ldap.NewSearchRequest(
		dn, ldap.ScopeBaseObject, ldap.NeverDerefAliases,
		1, 0, false, "(objectClass=*)", []string{attr}, nil))
	Expect(err).NotTo(HaveOccurred(), "read %s on %s", attr, dn)
	Expect(res.Entries).To(HaveLen(1))
	vals := res.Entries[0].GetEqualFoldAttributeValues(attr)
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}

// configValues reads every value of an attribute, so a spec can assert that an
// attribute is ABSENT — which singleConfigValue cannot distinguish from an
// attribute present with an empty value.
func configValues(conn *ldap.Conn, dn, attr string) []string {
	res, err := conn.Search(ldap.NewSearchRequest(
		dn, ldap.ScopeBaseObject, ldap.NeverDerefAliases,
		1, 0, false, "(objectClass=*)", []string{attr}, nil))
	Expect(err).NotTo(HaveOccurred(), "read %s on %s", attr, dn)
	Expect(res.Entries).To(HaveLen(1))
	return res.Entries[0].GetEqualFoldAttributeValues(attr)
}

func atoi(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	Expect(err).NotTo(HaveOccurred(), "not an integer: %q", s)
	return n
}

func accesslogSuffixFor(db string) string { return "cn=accesslog-" + db }

// ── cn=monitor helpers ───────────────────────────────────────────────────────

// monitorAtRestWindow outlasts one SlapdCluster reconcile tick
// (csnCheckInterval, 60 s, is the periodic trigger for a Running cluster —
// ADR-002 amendment), so a pass that rewrites the monitor database at rest
// lands inside it.
const monitorAtRestWindow = 90 * time.Second

// clusterDatabases lists the SlapdDatabase CRs of cluster "slapd" in the test
// namespace — every database the operator converges, not just DB_CR_NAME.
func clusterDatabases(ctx SpecContext) []ldapv1alpha1.SlapdDatabase {
	list := &ldapv1alpha1.SlapdDatabaseList{}
	Expect(crdClient.List(ctx, list)).To(Succeed())
	var dbs []ldapv1alpha1.SlapdDatabase
	for _, sd := range list.Items {
		if sd.Namespace == namespace && sd.Spec.ClusterRef == "slapd" {
			dbs = append(dbs, sd)
		}
	}
	Expect(dbs).NotTo(BeEmpty(), "no SlapdDatabase CRs found for cluster slapd")
	return dbs
}

type monitorReader struct{ dn, pw string }

// monitorReaders is one node-local replication identity per database
// (cn=repl-<db>,cn=slaptain-auth, ADR-027) with that database's
// replication-password.
func monitorReaders(ctx SpecContext) []monitorReader {
	var out []monitorReader
	for _, sd := range clusterDatabases(ctx) {
		secret := sd.Name + "-credentials"
		if sd.Spec.Credentials.SecretName != "" {
			secret = sd.Spec.Credentials.SecretName
		}
		out = append(out, monitorReader{
			dn: ldapv1alpha1.AuthIdentityDN(sd.Name),
			pw: readSecretKey(ctx, secret, "replication-password"),
		})
	}
	return out
}

// anyPodAddr is podNodePortAddr with the NodePort base chosen by pod KIND.
// The suite's tryDialPodConfig always uses E2E_POD_NODEPORT_BASE, so a
// "slapd-readonly-0" dialled through it reaches the RW pod slapd-0 (same
// ordinal) — the read-only pod is never actually inspected.
func anyPodAddr(pod string) string {
	if strings.Contains(pod, "-readonly-") {
		return podNodePortAddr(pod, "E2E_RO_POD_NODEPORT_BASE")
	}
	return podNodePortAddr(pod, "E2E_POD_NODEPORT_BASE")
}

// tryDialAnyPod dials one pod (RW or RO) and binds as bindDN; an empty bindDN
// means no bind at all (anonymous). Errors are returned, never asserted, so it
// is safe inside a retry (see tryDialPodConfig for why that matters).
func tryDialAnyPod(pod, bindDN, password string) (*ldap.Conn, error) {
	addr := anyPodAddr(pod)
	if addr == "" {
		return nil, fmt.Errorf("E2E_NODE_IP and the pod's NodePort base must be set to dial %s", pod)
	}
	conn, err := ldap.DialURL("ldap://"+addr, ldap.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}))
	if err != nil {
		return nil, fmt.Errorf("dial %s (%s): %w", pod, addr, err)
	}
	conn.SetTimeout(10 * time.Second)
	if bindDN != "" {
		if err := conn.Bind(bindDN, password); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("bind %s on %s (%s): %w", bindDN, pod, addr, err)
		}
	}
	return conn, nil
}

func dialAnyPodEventually(ctx SpecContext, pod, bindDN, password string) *ldap.Conn {
	var conn *ldap.Conn
	Eventually(ctx, func() error {
		c, err := tryDialAnyPod(pod, bindDN, password)
		if err != nil {
			return err
		}
		conn = c
		return nil
	}).WithTimeout(2*time.Minute).WithPolling(5*time.Second).Should(Succeed(),
		"%s on %s must become reachable", bindDN, pod)
	return conn
}

// effectiveLogLevel resolves what slapd is actually running at: spec.logLevel
// when set, the operator's default otherwise. The field is a pointer precisely
// so that an explicit 0 — "log nothing" — is expressible, which means a reader
// cannot treat nil and 0 as the same thing (finding 14).
//
// Kept in step with the operator's own default by TestEffectiveLogLevelDefault
// in the operator module; duplicated here rather than imported because the
// constant is unexported.
func effectiveLogLevel(sc *ldapv1alpha1.SlapdCluster) int32 {
	if sc == nil || sc.Spec.LogLevel == nil {
		return 16640
	}
	return *sc.Spec.LogLevel
}
