package e2e_test

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// ADR-037's seventeenth amendment splits the rotation into two independently
// settable halves. The coupled default is covered by write_rotation_test.go and
// overlay_tunables_test.go; these two specs cover the SPLIT modes, and each
// asserts the same two things from opposite sides:
//
//	the half that is ON did its work, and the half that is OFF did NOT.
//
// The second half is the point. A mode that silently ran both would pass any
// spec that only checked the enabled half, and the whole reason for the
// decomposition is that somebody can run one without the other.
//
// Tier 2 (ADR-039): what is owed here is "functionally valid and honestly
// reported", not the end-to-end measurement the coupled form carries. These
// specs discharge exactly that and claim nothing about whether either mode is
// a good idea.
//
// ORDERED, and the order is load-bearing in BOTH halves.
//
// The write half: WriteOnly runs first so slapd-write exists and names a pod,
// and PurgeOnly can then assert it was RETIRED to the fleet rather than merely
// never created. Run the other way round, that assertion would pass on a
// cluster where the endpoint had never existed — true, and about the wrong
// thing.
//
// The purge half: each spec asserts the arity the OTHER one does not leave
// behind, so every assertion here is a transition and not a state. Measured on
// a three-site mesh 2026-10-22, with the arity logged per poll:
//
//	WriteOnly  poll 1 [3 3 3] FAILS (the fixture's coupled form) → poll 2 [2 2 2]
//	PurgeOnly  poll 1 [2 2 2] FAILS (what WriteOnly left)        → poll 2 [3 3 3]
//
// That first-poll failure is the teeth: the assertion only passes because the
// operator converged, not because the cluster was already there.
//
// The corollary is a warning. Run ONE of these in isolation — a --focus, a
// bisect — and its incoming state may already satisfy it, so Eventually
// succeeds on the first poll and the spec proves nothing. They are a pair.
// (Learned by mutating the two expectations against each other, which passed:
// the mutation inverted each spec to precisely the state the other had left,
// so it was the MUTATION that was vacuous, not the specs. Verify the checker.)
var _ = Describe("ADR-037: the rotation's halves split",
	Label("purge-rotation", "rotation-modes"), Ordered, func() {

		const slot = 20 * time.Second

		var (
			rwPods       []string
			origRotation ldapv1alpha1.SlapdRotationConfig
			origPurge    *ldapv1alpha1.SlapdAccesslogPurgeConfig
		)

		// writeSelector returns the single pod <cluster>-write names, or "" when
		// it names none — which is what "the writer half is off" looks like from
		// outside: the Service widens to the whole fleet rather than vanishing.
		// Returns (exists, podName). The two must stay distinguishable: an
		// absent Service and a widened one both name no pod, and a spec that
		// collapsed them would pass on a cluster where the endpoint had simply
		// never been created — the reading-indistinguishable-from-absence trap.
		writeEndpoint := func(ctx context.Context) (bool, string) {
			svc, err := k8sClient.CoreV1().Services(namespace).Get(ctx, "slapd-write", metav1.GetOptions{})
			if err != nil {
				return false, ""
			}
			return true, svc.Spec.Selector["statefulset.kubernetes.io/pod-name"]
		}

		setMode := func(ctx context.Context, mode string) {
			sc := &ldapv1alpha1.SlapdCluster{}
			Expect(crdClient.Get(ctx, nsName(namespace, "slapd"), sc)).To(Succeed())
			on := true
			sc.Spec.Rotation.Enabled = &on
			sc.Spec.Rotation.Mode = mode
			sc.Spec.Rotation.SlotLength = &metav1.Duration{Duration: slot}
			// Both have no referent in a split mode and would be REPORTED
			// rather than ignored, which would withhold the rotation entirely
			// and make every assertion below fail for the wrong reason.
			sc.Spec.Rotation.LeadTime = nil
			sc.Spec.Rotation.SlotsFromWriteToPurge = nil
			// WriteOnly declares no purge schedule, so form A is legal beside
			// it; the other modes refuse it. Clearing keeps one variable.
			sc.Spec.AccesslogPurge = nil
			Expect(crdClient.Update(ctx, sc)).To(Succeed())
		}

		// purgeArity reports how many space-separated fields olcAccessLogPurge
		// carries on every replicated database of every RW pod: 3 means the pod
		// took a phase, 2 means it did not.
		purgeArity := func(ctx SpecContext) map[string][]int {
			out := map[string][]int{}
			list := &ldapv1alpha1.SlapdDatabaseList{}
			Expect(crdClient.List(ctx, list)).To(Succeed())
			var dbs []ldapv1alpha1.SlapdDatabase
			for _, sd := range list.Items {
				if sd.Namespace == namespace && sd.Spec.ClusterRef == "slapd" && (&sd).DeltaSyncEnabled() {
					dbs = append(dbs, sd)
				}
			}
			for _, pod := range rwPods {
				conn := dialPodConfigEventually(ctx, pod)
				for _, sd := range dbs {
					dataDB, ok := findMdb(observedMdbDatabases(conn), sd.Spec.Suffix)
					if !ok {
						continue
					}
					_, values := overlayAttrValues(conn, dataDB.dn,
						"(objectClass=olcAccessLogConfig)", "olcAccessLogPurge")
					for _, v := range values {
						out[pod] = append(out[pod], len(strings.Fields(v)))
					}
				}
				conn.Close()
			}
			return out
		}

		// phaseRefused reports whether the operator has TRIED to write a phase
		// and been refused by slapd. Capability is read from the attempt, never
		// from the image tag or the version string — a tag is a label somebody
		// chose, and a pure build reports the same `slapd -VV` as a patched one
		// (docs/BACKLOG.md), so neither distinguishes them. ADR-040 decision 6.
		phaseRefused := func(ctx SpecContext) bool {
			sc := &ldapv1alpha1.SlapdCluster{}
			if err := crdClient.Get(ctx, nsName(namespace, "slapd"), sc); err != nil {
				return false
			}
			for _, c := range sc.Status.Conditions {
				if c.Type == "AccesslogPurgeConverged" && c.Reason == "PurgePhaseUnsupported" {
					return true
				}
			}
			return false
		}

		BeforeAll(func(ctx SpecContext) {
			sc := &ldapv1alpha1.SlapdCluster{}
			Expect(crdClient.Get(ctx, nsName(namespace, "slapd"), sc)).To(Succeed())
			if sc.Spec.Replicas < 2 {
				Skip("a one-pod cluster is ADR-037's degenerate case: the single pod both " +
					"writes and purges whatever the mode says")
			}
			rwPods = rwPodNames(ctx, "slapd")
			origRotation = sc.Spec.Rotation
			origPurge = sc.Spec.AccesslogPurge.DeepCopy()

			DeferCleanup(func(ctx SpecContext) {
				By("restoring the cluster's original schedule, and waiting for it to land")
				cur := &ldapv1alpha1.SlapdCluster{}
				if err := crdClient.Get(ctx, nsName(namespace, "slapd"), cur); err != nil {
					return
				}
				cur.Spec.Rotation = origRotation
				cur.Spec.AccesslogPurge = origPurge
				Expect(crdClient.Update(ctx, cur)).To(Succeed())
				// Shared cn=config state: overlay_tunables_test.go asserts a
				// CONVERGED olcAccessLogPurge and Ginkgo randomises containers,
				// so leaving before the operator has rewritten the fleet
				// indicts a cluster that was merely mid-flight.
				waitScheduleConverged(ctx, origRotation, origPurge)
			})
		})

		It("WriteOnly moves the endpoint and leaves the purge schedule flat", func(ctx SpecContext) {
			By("setting spec.rotation.mode to WriteOnly")
			setMode(ctx, ldapv1alpha1.RotationModeWriteOnly)

			By("the write half: <cluster>-write names exactly one RW pod")
			var first string
			Eventually(ctx, func(g Gomega) {
				_, first = writeEndpoint(ctx)
				g.Expect(first).To(BeElementOf(rwPods),
					"WriteOnly must select one read-write pod")
			}).WithTimeout(90 * time.Second).WithPolling(2 * time.Second).Should(Succeed())

			By("the purge half: no pod carries a phase — two fields, not three")
			Eventually(ctx, func(g Gomega) {
				arity := purgeArity(ctx)
				g.Expect(arity).To(HaveLen(len(rwPods)))
				for pod, got := range arity {
					for _, n := range got {
						g.Expect(n).To(Equal(2),
							"pod %s: WriteOnly declares no purge schedule, so the phase "+
								"must be absent — this is the half that is OFF", pod)
					}
				}
			}).WithTimeout(3 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

			By("and the endpoint still moves across a slot boundary")
			Eventually(ctx, func(g Gomega) {
				_, now := writeEndpoint(ctx)
				g.Expect(now).NotTo(Equal(first),
					"the endpoint must rotate; it is the half this mode exists to run")
			}).WithTimeout(3*slot + 30*time.Second).WithPolling(2 * time.Second).Should(Succeed())
		})
		It("PurgeOnly staggers the phases and leaves the write endpoint alone", func(ctx SpecContext) {
			By("setting spec.rotation.mode to PurgeOnly")
			setMode(ctx, ldapv1alpha1.RotationModePurgeOnly)

			// A phase is a third value of olcAccessLogPurge, which only a slapd
			// carrying accesslog-purge-phase.patch accepts. Rather than ask the
			// tag, ATTEMPT it and read the outcome: either every pod takes the
			// phase, or the operator reports that this slapd refused one. The
			// condition cannot be consulted before the attempt, because nothing
			// has tried yet — which is why this is a race between two outcomes
			// and not a precondition check.
			//
			// Measured 2026-10-22 on the pure pair: refused with arity 2, while
			// WriteOnly in the same container PASSED — rotating the endpoint
			// needs no patch at all. That asymmetry is why only this spec skips.
			By("the purge half: the phase is taken, or this slapd says it cannot")
			staggered := false
			Eventually(ctx, func() bool {
				if phaseRefused(ctx) {
					return true
				}
				arity := purgeArity(ctx)
				if len(arity) != len(rwPods) {
					return false
				}
				for _, got := range arity {
					for _, n := range got {
						if n != 3 {
							return false
						}
					}
				}
				staggered = true
				return true
			}).WithTimeout(3*time.Minute).WithPolling(5*time.Second).Should(BeTrue(),
				"neither outcome arrived: no pod took a phase and the operator never "+
					"reported PurgePhaseUnsupported")

			if !staggered {
				Skip("this slapd refuses a three-value olcAccessLogPurge (no " +
					"accesslog-purge-phase patch), so PurgeOnly has no phase to stagger; " +
					"WriteOnly needs no patch and is covered above")
			}

			By("the write half: the endpoint is RETIRED to the fleet, not merely absent")
			// The WriteOnly spec above ran first and left slapd-write naming one
			// pod, so "names nobody" here can only mean retirement. Had this run
			// on a cluster where the Service never existed, the first assertion
			// would fail rather than pass vacuously.
			Eventually(ctx, func(g Gomega) {
				exists, pod := writeEndpoint(ctx)
				g.Expect(exists).To(BeTrue(),
					"slapd-write must still exist — rotation off WIDENS it to the fleet "+
						"rather than deleting it (ADR-037 eighth amendment)")
				g.Expect(pod).To(BeEmpty(),
					"PurgeOnly moves no endpoint, so slapd-write names no single pod")
			}).WithTimeout(90 * time.Second).WithPolling(2 * time.Second).Should(Succeed())

			By("and it stays retired across a slot boundary")
			Consistently(ctx, func(g Gomega) {
				_, pod := writeEndpoint(ctx)
				g.Expect(pod).To(BeEmpty())
			}).WithTimeout(slot + 10*time.Second).WithPolling(4 * time.Second).Should(Succeed())
		})

	})
