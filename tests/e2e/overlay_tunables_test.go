package e2e_test

import (
	"fmt"
	"strings"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The two overlay tunables the operator converges rather than writes once:
// olcAccessLogPurge on the accesslog overlay and olcSpCheckpoint on the data
// DB's syncprov overlay (ADR-024 R4, with an R5 default for the purge window).
//
// Both used to be written ONLY into the overlay-creation addReq, so a spec edit
// — or a newly introduced operator default — never reached a pod whose overlay
// already existed. That is invisible on a healthy fixture: replication works
// fine with no purge policy right up to the day the journal hits its map
// ceiling and, because the accesslog overlay sits in the DATA write path, takes
// data writes down with it. An assertion on cn=config is the only standing
// guard.
//
// cn=config is node-local (ADR-002), so every RW pod is checked independently:
// a purge window converged on two pods out of three leaves the third growing.
//
// Both attributes were verified live-modifiable on a running slapd before being
// converged at all (replace AND delete, no crash, no hang) — the precondition
// ADR-024's 2026-09-12 and 2026-09-13 amendments made mandatory for this class.
//
// Ungated: a bind and two searches per pod per database.
var _ = Describe("overlay tunables: purge window and checkpoint",
	Label("tunables"), Label("accesslog"), Ordered, ContinueOnFailure, func() {

		var (
			rwPods []string
			dbs    []ldapv1alpha1.SlapdDatabase
			// rotating, slotLength and replicas describe the ADR-037 grid the
			// cluster declares. replicas is spec.replicas — DECLARED, never
			// readiness — because it is what the operator derives the purge
			// interval T = n·t from.
			rotating   bool
			slotLength time.Duration
			replicas   int32
		)

		BeforeAll(func(ctx SpecContext) {
			By("reading the SlapdCluster")
			sc := &ldapv1alpha1.SlapdCluster{}
			Expect(crdClient.Get(ctx, nsName(namespace, "slapd"), sc)).To(Succeed())
			replicas = sc.Spec.Replicas
			if replicas == 0 {
				replicas = 1
			}
			if r := sc.Spec.Rotation; r.Enabled != nil && *r.Enabled && r.SlotLength != nil {
				slotLength = r.SlotLength.Duration
				rotating = slotLength > 0 && slotLength%time.Second == 0
			}
			if !sc.Spec.Replication.Enabled || sc.IsConsumerOnly() {
				Skip("cluster is not a syncrepl provider, so it carries neither " +
					"an accesslog overlay nor a data-DB syncprov overlay")
			}

			By("listing the SlapdDatabase CRs in the namespace")
			list := &ldapv1alpha1.SlapdDatabaseList{}
			Expect(crdClient.List(ctx, list)).To(Succeed())
			for _, sd := range list.Items {
				if sd.Namespace == namespace && sd.Spec.ClusterRef == "slapd" {
					dbs = append(dbs, sd)
				}
			}
			Expect(dbs).NotTo(BeEmpty(), "no SlapdDatabase CRs found for cluster slapd")

			for _, sd := range dbs {
				name := sd.Name
				Eventually(ctx, func() bool {
					return slapdDatabaseRunning(crdClient, namespace, name)
				}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(BeTrue(),
					"SlapdDatabase %s never reached Running", name)
			}

			By("reading the RW StatefulSet")
			sts, err := k8sClient.AppsV1().StatefulSets(namespace).Get(ctx, "slapd", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			n := int32(1)
			if sts.Spec.Replicas != nil {
				n = *sts.Spec.Replicas
			}
			for i := int32(0); i < n; i++ {
				rwPods = append(rwPods, fmt.Sprintf("slapd-%d", i))
			}
		}, NodeTimeout(8*time.Minute))

		It("converges the accesslog purge window on every RW pod", func(ctx SpecContext) {
			if rotating {
				Skip("spec.rotation is on, so olcAccessLogPurge carries the cluster's " +
					"interval and this pod's phase — see the rotation spec below")
			}
			for _, pod := range rwPods {
				By("inspecting cn=config on " + pod)
				conn := dialPodConfigEventually(ctx, pod)

				for _, sd := range dbs {
					if !(&sd).DeltaSyncEnabled() {
						continue
					}
					want := wantAccesslogPurge(sd)

					dataDB, ok := findMdb(observedMdbDatabases(conn), sd.Spec.Suffix)
					Expect(ok).To(BeTrue(), "pod %s has no data database %q", pod, sd.Spec.Suffix)

					dn, values := overlayAttrValues(conn, dataDB.dn,
						"(objectClass=olcAccessLogConfig)", "olcAccessLogPurge")
					Expect(dn).NotTo(BeEmpty(),
						"pod %s: data database %q has no accesslog overlay", pod, sd.Spec.Suffix)

					if want == "" {
						Expect(values).To(BeEmpty(),
							"pod %s: %s (db %s) has accesslogPurge \"none\", so it must carry "+
								"no olcAccessLogPurge", pod, dn, sd.Name)
						continue
					}
					Expect(values).To(ConsistOf(want),
						"pod %s: %s (db %s) must carry olcAccessLogPurge %q — an unpurged "+
							"journal grows to its map ceiling and then stops data writes (ADR-024 R4/R5)",
						pod, dn, sd.Name, want)
				}
				conn.Close()
			}
		}, NodeTimeout(6*time.Minute))

		// ADR-037: with spec.rotation on, olcAccessLogPurge gains a third value
		// and the operator composes it per (pod, database) — the age from the
		// SlapdDatabase, the interval T = spec.replicas × slotLength and the
		// phase k × slotLength from the SlapdCluster.
		//
		// This spec exists because of ADR-037's third amendment, which measured
		// a stagger that had never happened: a driver assuming the SERVER
		// multiplies the value by the pod's ordinal would write ONE phase for
		// every pod, the providers would stay synchronised, and the arm would
		// report no difference — reading as "the patch does nothing" rather
		// than "the stagger never happened". Nothing outside cn=config can tell
		// those two apart, so the distinctness is asserted here, directly, per
		// pod. Staggering alone changes no client-visible number by design; the
		// write endpoint moving is what moves the number, and that is a
		// different milestone.
		It("gives every RW pod its own purge phase on the cluster's grid",
			Label("purge-rotation"), func(ctx SpecContext) {
				if !rotating {
					Skip("spec.rotation.enabled is not set (or its slotLength is not a usable " +
						"whole number of seconds), so no pod carries a purge phase")
				}

				wantInterval := logAge(time.Duration(replicas) * slotLength)
				// phase → the pod that carries it, so a collision names both.
				seen := map[string]string{}

				for i, pod := range rwPods {
					By("inspecting cn=config on " + pod)
					conn := dialPodConfigEventually(ctx, pod)

					wantPhase := logAge(time.Duration(i) * slotLength)
					podPhase := ""

					for _, sd := range dbs {
						if !(&sd).DeltaSyncEnabled() {
							continue
						}
						if purgeDisabled(sd) {
							continue
						}
						if declaresPurgeInterval(sd) {
							// ADR-024 R4: the operator REFUSES such a database
							// rather than overriding it, and reports a
							// condition. Nothing is written, so there is no
							// per-pod value to assert.
							continue
						}

						dataDB, ok := findMdb(observedMdbDatabases(conn), sd.Spec.Suffix)
						Expect(ok).To(BeTrue(), "pod %s has no data database %q", pod, sd.Spec.Suffix)

						dn, values := overlayAttrValues(conn, dataDB.dn,
							"(objectClass=olcAccessLogConfig)", "olcAccessLogPurge")
						Expect(dn).NotTo(BeEmpty(),
							"pod %s: data database %q has no accesslog overlay", pod, sd.Spec.Suffix)
						Expect(values).To(HaveLen(1),
							"pod %s: %s carries %d olcAccessLogPurge values, want exactly one",
							pod, dn, len(values))

						fields := strings.Fields(values[0])
						Expect(fields).To(HaveLen(3),
							"pod %s: %s (db %s) carries olcAccessLogPurge %q — with spec.rotation "+
								"on it must carry THREE fields <age> <interval> <phase>. Two fields "+
								"means this pod purges on its own uncoordinated schedule, which is "+
								"the synchronised purge ADR-037 exists to remove; check the "+
								"AccesslogPurgeConverged condition on the SlapdDatabase and whether "+
								"this slapd image carries the accesslog-purge-phase patch",
							pod, dn, sd.Name, values[0])

						Expect(fields[1]).To(Equal(wantInterval),
							"pod %s: %s (db %s) has purge interval %q, want %q "+
								"(spec.replicas=%d × slotLength=%s)",
							pod, dn, sd.Name, fields[1], wantInterval, replicas, slotLength)

						Expect(fields[2]).To(Equal(wantPhase),
							"pod %s: %s (db %s) has purge phase %q, want %q (ordinal %d × "+
								"slotLength %s). A phase that does not track the ordinal means "+
								"the fleet is not staggered",
							pod, dn, sd.Name, fields[2], wantPhase, i, slotLength)

						// Databases ALIGN, they do not spread: one busy window
						// per pod, in the slot where it is not the writer.
						if podPhase == "" {
							podPhase = fields[2]
						}
						Expect(fields[2]).To(Equal(podPhase),
							"pod %s: database %s purges at %q while another of this pod's "+
								"databases purges at %q. Databases must align on their pod's "+
								"phase, not spread across the period (ADR-037)",
							pod, sd.Name, fields[2], podPhase)
					}

					if podPhase != "" {
						Expect(seen).NotTo(HaveKey(podPhase),
							"pods %s and %s both purge at phase %q — the stagger never happened, "+
								"and every provider will stall its own writes at the same instant",
							seen[podPhase], pod, podPhase)
						seen[podPhase] = pod
					}
					conn.Close()
				}

				Expect(seen).To(HaveLen(len(rwPods)),
					"expected one distinct purge phase per RW pod, got %d for %d pods: %v",
					len(seen), len(rwPods), seen)
			}, NodeTimeout(6*time.Minute))

		It("converges the syncprov checkpoint on every RW pod's data database", func(ctx SpecContext) {
			for _, pod := range rwPods {
				By("inspecting cn=config on " + pod)
				conn := dialPodConfigEventually(ctx, pod)

				for _, sd := range dbs {
					want := strings.TrimSpace(sd.Spec.Replication.SyncprovCheckpoint)
					if strings.EqualFold(want, "none") {
						want = ""
					}

					dataDB, ok := findMdb(observedMdbDatabases(conn), sd.Spec.Suffix)
					Expect(ok).To(BeTrue(), "pod %s has no data database %q", pod, sd.Spec.Suffix)

					dn, values := overlayAttrValues(conn, dataDB.dn,
						"(objectClass=olcSyncProvConfig)", "olcSpCheckpoint")
					Expect(dn).NotTo(BeEmpty(),
						"pod %s: data database %q has no syncprov overlay", pod, sd.Spec.Suffix)

					if want == "" {
						Expect(values).To(BeEmpty(),
							"pod %s: %s (db %s) has no syncprovCheckpoint, so it must carry "+
								"no olcSpCheckpoint", pod, dn, sd.Name)
						continue
					}
					Expect(values).To(ConsistOf(want),
						"pod %s: %s (db %s) must carry olcSpCheckpoint %q, converged on every "+
							"reconcile and not only at overlay creation (ADR-024 R4)",
						pod, dn, sd.Name, want)
				}
				conn.Close()
			}
		}, NodeTimeout(6*time.Minute))
	})

// wantAccesslogPurge mirrors the operator's resolution of
// spec.replication.accesslogPurge: unset → the operator default, "none" → no
// attribute, anything else verbatim. Spelled out here rather than imported so
// the e2e states the contract instead of restating the code.
func wantAccesslogPurge(sd ldapv1alpha1.SlapdDatabase) string {
	v := strings.TrimSpace(sd.Spec.Replication.AccesslogPurge)
	switch {
	case v == "":
		return "7+00:00 1+00:00"
	case strings.EqualFold(v, "none"):
		return ""
	default:
		return v
	}
}

// purgeDisabled reports the explicit "none" opt-out, which survives rotation —
// a schedule must not resurrect a purge the user turned off.
func purgeDisabled(sd ldapv1alpha1.SlapdDatabase) bool {
	return strings.EqualFold(strings.TrimSpace(sd.Spec.Replication.AccesslogPurge), "none")
}

// declaresPurgeInterval reports whether the DATABASE's own field carries an
// interval. With rotation on the cluster owns the schedule, so such a database
// is refused with a condition and nothing is written for it (ADR-024 R4) — the
// operator's default is not a declaration and does not count.
func declaresPurgeInterval(sd ldapv1alpha1.SlapdDatabase) bool {
	v := strings.TrimSpace(sd.Spec.Replication.AccesslogPurge)
	if v == "" || strings.EqualFold(v, "none") {
		return false
	}
	return len(strings.Fields(v)) > 1
}

// logAge renders a duration the way slapd's log_age_parse accepts it:
// "hh:mm:ss", or "ddd+hh:mm:ss" from a day up. Spelled out here rather than
// imported, so the e2e states the wire format instead of restating the code —
// if the operator's formatter drifts, this is what catches it.
func logAge(d time.Duration) string {
	secs := int64(d / time.Second)
	days, rest := secs/86400, secs%86400
	hh, mm, ss := rest/3600, rest%3600/60, rest%60
	if days > 0 {
		return fmt.Sprintf("%d+%02d:%02d:%02d", days, hh, mm, ss)
	}
	return fmt.Sprintf("%02d:%02d:%02d", hh, mm, ss)
}

// overlayAttrValues returns the DN of the overlay under dbDN matching filter and
// that overlay's values for attr (trimmed). An empty DN means no such overlay.
func overlayAttrValues(conn *ldap.Conn, dbDN, filter, attr string) (string, []string) {
	res, err := conn.Search(ldap.NewSearchRequest(
		dbDN, ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
		0, 0, false, filter, []string{attr}, nil))
	Expect(err).NotTo(HaveOccurred(), "search %s under %s", filter, dbDN)
	if len(res.Entries) == 0 {
		return "", nil
	}
	var out []string
	for _, v := range res.Entries[0].GetEqualFoldAttributeValues(attr) {
		out = append(out, strings.TrimSpace(v))
	}
	return res.Entries[0].DN, out
}
