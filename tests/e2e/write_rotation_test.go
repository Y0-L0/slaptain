package e2e_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The rotating write endpoint (ADR-037 decision 2, PURGE-ROTATION-PLAN Phase 4).
//
// This is the spec the whole feature exists to satisfy. The per-pod purge
// phases that landed first changed no client-visible number by design, and
// ADR-037's third amendment measured exactly that: a staggered fleet lands on a
// synchronised fleet, and "the only variable that matters is whether the write
// target purges". So the assertion that counts is the second one below — the
// pod the write Service names is never the pod whose purge phase is current.
//
// The grid is absolute and epoch-anchored, which is what makes it assertable
// from outside the operator: with n read-write pods and a slot length t, the
// slot current at wall-clock T is floor(unix(T)/t) mod n, and pod k purges at
// phase k·t. This spec recomputes that from time.Now() and the CR, so an
// operator that silently stopped rotating — or rotated onto the wrong pod —
// fails here rather than looking healthy.
//
// The cluster's own spec.rotation is turned ON by this spec and restored
// afterwards, with a slot length short enough that a boundary is reachable
// inside a test. Nothing else in the suite declares one, and a production-shaped
// t (ADR-037 expects 6h or 12h) would make the rotation unobservable.
var _ = Describe("ADR-037: the write endpoint rotates",
	Label("purge-rotation"), Label("write-rotation"), Ordered, ContinueOnFailure, func() {

		const (
			// slotLength is deliberately absurd for a real deployment and
			// deliberately not tiny: it has to be a whole number of seconds (the
			// rotation gate refuses anything else), long enough that a sample
			// taken away from the boundaries is unambiguous, and short enough
			// that a spec can sit through two of them.
			slotLength = 20 * time.Second
			// boundaryGuard is how far from a slot boundary a sample must sit.
			// The operator repoints the selector AT the boundary, so a reading
			// taken either side of one legitimately names the other slot's
			// writer. The lockstep ADR-037 describes is exact on the grid and
			// approximate in wall-clock terms, and this is that approximation
			// made explicit rather than raced against.
			boundaryGuard = 5 * time.Second
			writeSvcName  = "slapd-write"
			probeSvcName  = "slapd-write-probe"
		)

		var (
			replicas    int32
			rwPods      []string
			sidOrdinals map[int]int32
		)

		// slotAt is the operator's own grid, recomputed here: floor(unix/t) mod n.
		slotAt := func(at time.Time) int32 {
			step := int64(slotLength / time.Second)
			slots := at.Unix() / step
			return int32((slots%int64(replicas) + int64(replicas)) % int64(replicas))
		}

		// awayFromBoundary sleeps until the current instant is at least
		// boundaryGuard from both the previous and the next slot boundary.
		awayFromBoundary := func(ctx SpecContext) {
			step := slotLength
			for {
				now := time.Now()
				into := time.Duration(now.Unix()%int64(step/time.Second))*time.Second +
					time.Duration(now.Nanosecond())
				if into >= boundaryGuard && step-into >= boundaryGuard {
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(500 * time.Millisecond):
				}
			}
		}

		// writeSelector returns the pod the write Service currently names, or ""
		// when the Service carries no pod-name entry at all.
		writeSelector := func(ctx context.Context) string {
			svc, err := k8sClient.CoreV1().Services(namespace).Get(ctx, writeSvcName, metav1.GetOptions{})
			if err != nil {
				return ""
			}
			return svc.Spec.Selector["statefulset.kubernetes.io/pod-name"]
		}

		// setRotation patches spec.rotation on the fixture cluster.
		setRotation := func(ctx context.Context, on bool, t time.Duration) {
			sc := &ldapv1alpha1.SlapdCluster{}
			Expect(crdClient.Get(ctx, nsName(namespace, "slapd"), sc)).To(Succeed())
			enabled := on
			sc.Spec.Rotation.Enabled = &enabled
			if on {
				d := metav1.Duration{Duration: t}
				sc.Spec.Rotation.SlotLength = &d
			}
			Expect(crdClient.Update(ctx, sc)).To(Succeed())
		}

		BeforeAll(func(ctx SpecContext) {
			By("reading the SlapdCluster")
			sc := &ldapv1alpha1.SlapdCluster{}
			Expect(crdClient.Get(ctx, nsName(namespace, "slapd"), sc)).To(Succeed())
			replicas = sc.Spec.Replicas
			if replicas == 0 {
				replicas = 1
			}
			if replicas < 2 {
				Skip("a one-pod cluster has nowhere to rotate to: the single pod is both " +
					"purger and writer, which is ADR-037's degenerate case and not a rotation")
			}
			rwPods = rwPodNames(ctx, "slapd")

			By("turning spec.rotation on with a slot length a test can sit through")
			original := sc.Spec.Rotation
			setRotation(ctx, true, slotLength)
			DeferCleanup(func(ctx SpecContext) {
				By("restoring the cluster's original spec.rotation")
				cur := &ldapv1alpha1.SlapdCluster{}
				if err := crdClient.Get(ctx, nsName(namespace, "slapd"), cur); err == nil {
					cur.Spec.Rotation = original
					Expect(crdClient.Update(ctx, cur)).To(Succeed())
				}
			})

			// The cluster was on the chart's production-shaped grid until the
			// line above (6h, next rotation at midnight), and the operator
			// adopts the 20s one on its next pass. Until it has, the Service
			// still names whichever pod the OLD grid chose — which is a
			// perfectly valid selector for a grid that is no longer in force,
			// and sampling it reads as a rotation pointing at the wrong pod.
			//
			// Measured: the operator repointed to slapd-2 for slot 1 at
			// 21:02:29.0 and the first sample read slapd-0 — the 6h grid's
			// answer — at 21:02:29.022.
			//
			// So wait for ONE coherent reading before anything samples. This
			// is a precondition, not an assertion weakened: every strict
			// per-sample check below still runs, and still runs four times.
			By("waiting for the operator to adopt the new grid before anything samples it")
			Eventually(ctx, func() string {
				now := time.Now()
				want := fmt.Sprintf("slapd-%d", (slotAt(now)+1)%replicas)
				if got := writeSelector(ctx); got == want {
					return want
				}
				return fmt.Sprintf("selector=%s grid wants %s", writeSelector(ctx), want)
			}).WithTimeout(4*slotLength).WithPolling(time.Second).Should(HavePrefix("slapd-"),
				"the write endpoint never agreed with the %s grid. Either the operator has not "+
					"adopted the new slotLength (it requeues at every boundary, so one pass is "+
					"all it should take), or the writer displacement is wrong: with "+
					"slotsFromWriteToPurge unset the default is +1, so the endpoint must name "+
					"the pod that purges in the NEXT slot", slotLength)

			By("mapping each pod's olcServerID to its ordinal, so a CSN names a pod")
			// An entryCSN carries the sid of the server that accepted the write
			// (#sid# field), and that is the only in-band way to tell which pod a
			// write actually landed on — which is the question the pooled-client
			// measurement below exists to answer.
			sidOrdinals = map[int]int32{}
			for i, pod := range rwPods {
				cfg := dialAnyPodEventually(ctx, pod, "cn=admin,cn=config", rootPW)
				sr, err := cfg.Search(ldap.NewSearchRequest(
					"cn=config", ldap.ScopeBaseObject, ldap.NeverDerefAliases,
					0, 0, false, "(objectClass=*)", []string{"olcServerID"}, nil))
				Expect(err).NotTo(HaveOccurred(), "read olcServerID on %s", pod)
				Expect(sr.Entries).NotTo(BeEmpty())
				for _, v := range sr.Entries[0].GetAttributeValues("olcServerID") {
					var sid int
					if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &sid); err == nil {
						sidOrdinals[sid] = int32(i)
					}
				}
				_ = cfg.Close()
			}
			Expect(sidOrdinals).To(HaveLen(len(rwPods)),
				"every RW pod must carry a distinct olcServerID (ADR-017); got %v", sidOrdinals)
		})

		// The endpoint exists, and it names ONE pod. Both halves matter: a
		// Service with no pod-name entry selects the whole fleet and the
		// rotation does nothing at all, and a Service selecting nothing is a
		// total write outage — strictly worse than an endpoint that stopped
		// moving.
		It("publishes <cluster>-write selecting exactly one read-write pod",
			func(ctx SpecContext) {
				var svc *corev1.Service
				Eventually(ctx, func() error {
					s, err := k8sClient.CoreV1().Services(namespace).Get(ctx, writeSvcName, metav1.GetOptions{})
					if err != nil {
						return err
					}
					svc = s
					if svc.Spec.Selector["statefulset.kubernetes.io/pod-name"] == "" {
						return fmt.Errorf("selector %v carries no pod-name entry, so it names the "+
							"whole fleet and no rotation is observable", svc.Spec.Selector)
					}
					return nil
				}).WithTimeout(2*time.Minute).WithPolling(2*time.Second).Should(Succeed(),
					"the write endpoint must appear once spec.rotation is on")

				Expect(svc.Spec.Selector["app.kubernetes.io/instance"]).To(Equal("slapd"),
					"the cluster labels ride along as the failure floor: losing the pod-name entry "+
						"must degrade to this cluster's fleet, never to every pod in the namespace")
				Expect(svc.Spec.Selector["statefulset.kubernetes.io/pod-name"]).To(BeElementOf(rwPods),
					"the endpoint must name a read-write pod of this cluster")

				By("checking the endpoint resolves to exactly one ready address")
				Eventually(ctx, func() (int, error) {
					eps, err := k8sClient.CoreV1().Endpoints(namespace).Get(ctx, writeSvcName, metav1.GetOptions{})
					if err != nil {
						return -1, err
					}
					n := 0
					for _, s := range eps.Subsets {
						n += len(s.Addresses)
					}
					return n, nil
				}).WithTimeout(90*time.Second).WithPolling(2*time.Second).Should(Equal(1),
					"the write endpoint must resolve to one pod: zero is a write outage, more than "+
						"one means the selector is not naming a pod at all")
			})

		// The feature's entire claim.
		//
		// purgerotation_test.go and writeservice_test.go pin it on the pure
		// functions; this pins that the Service a client is actually steered by
		// carries it, against the same wall clock the purge daemon fires on.
		// Every health signal in the cluster stays green when this breaks.
		It("never names the pod whose purge phase is current", func(ctx SpecContext) {
			// Four samples across more than two slots, each taken away from a
			// boundary so the reading is unambiguous.
			for i := 0; i < 4; i++ {
				awayFromBoundary(ctx)
				now := time.Now()
				slot := slotAt(now)
				purging := fmt.Sprintf("slapd-%d", slot)
				writer := writeSelector(ctx)

				Expect(writer).NotTo(BeEmpty(),
					"sample %d: the write Service names no pod", i)
				Expect(writer).NotTo(Equal(purging),
					"sample %d at %s: the write Service selects %s, which is the pod purging this "+
						"slot (slot %d of %d, t=%s). ADR-037's third amendment measured the write "+
						"target's own purge as the ENTIRE client-visible quantity — a rotation that "+
						"lands here is measurably the same as no rotation",
					i, now.UTC().Format(time.RFC3339), writer, slot, replicas, slotLength)

				// THE SIGN. slotsFromWriteToPurge is unset here, so it takes
				// the operator's default of 1: "purge the slot AFTER writing".
				// The writer is therefore the pod that purges NEXT, ordinal
				// (slot+1) mod n — the opposite end of the walk from the
				// superseded writerOffset, whose unset default named the pod
				// that had purged LAST. Both satisfy "never the purging pod"
				// on a healthy fleet, so the assertion above cannot tell them
				// apart and this one has to.
				wantWriter := fmt.Sprintf("slapd-%d", (slot+1)%replicas)
				Expect(writer).To(Equal(wantWriter),
					"sample %d at %s: the write Service selects %s, want %s. With "+
						"slotsFromWriteToPurge unset the default is +1 — the writer purges one "+
						"slot from now, having just stopped taking writes (slot %d of %d, t=%s)",
					i, now.UTC().Format(time.RFC3339), writer, wantWriter, slot, replicas, slotLength)

				GinkgoWriter.Printf("sample %d  %s  slot=%d purging=%s writer=%s\n",
					i, now.UTC().Format(time.RFC3339), slot, purging, writer)

				select {
				case <-ctx.Done():
					return
				case <-time.After(slotLength / 2):
				}
			}
		})

		// The selector MOVES. A rotation that is arithmetically correct and
		// never applied reads identically to one that works, right up to the
		// moment somebody measures the stall.
		It("repoints the endpoint across a slot boundary", func(ctx SpecContext) {
			awayFromBoundary(ctx)
			before := writeSelector(ctx)
			Expect(before).NotTo(BeEmpty())
			GinkgoWriter.Printf("before boundary: %s (slot %d)\n", before, slotAt(time.Now()))

			Eventually(ctx, func() string {
				return writeSelector(ctx)
			}).WithTimeout(3*slotLength).WithPolling(time.Second).ShouldNot(Equal(before),
				"the write endpoint still selects %s after more than two slots of t=%s — the "+
					"rotation is not firing. The likeliest cause is a requeue that never happens: "+
					"the controller's steady-state RequeueAfter is gated on "+
					"spec.replication.enabled, so a rotation requeue returned inside that branch "+
					"silently never rotates the clusters outside it", before, slotLength)

			after := writeSelector(ctx)
			GinkgoWriter.Printf("after boundary:  %s (slot %d)\n", after, slotAt(time.Now()))
			Expect(after).To(BeElementOf(rwPods))
		})

		// The feature's claim, and its precondition, measured together.
		//
		// THE CLAIM, asserted: a NEW connection opened after the endpoint moves
		// writes to the NEW target. That is what the rotation promises and it
		// must be green — an endpoint new connections do not follow is an
		// endpoint that does nothing.
		//
		// THE PRECONDITION, recorded: a Kubernetes Service does not re-wire
		// established connections — kube-proxy steers new flows while existing
		// conntrack entries keep going where they were sent. So a client holding
		// one connection keeps writing to the pod it first reached. That is
		// expected, it is written down (PURGE-ROTATION-PLAN, "Known risks": the
		// rotation is for short-lived connections by design), and it is a
		// property of the DEPLOYMENT rather than a defect here. The rotation
		// benefits clients that open short-lived connections, or pool with a
		// maximum connection lifetime well below the slot length; a client that
		// holds a connection for hours does not benefit, and nothing in this
		// operator could make it.
		//
		// So that half is observed and reported, never asserted, and this spec
		// is not a characterisation harness for pooled clients.
		//
		// The measurement needs a path through kube-proxy, which the write
		// Service's ClusterIP is not from outside the cluster, so the probe is a
		// throwaway NodePort Service carrying the SAME selector, kept in step
		// with the real one. kube-proxy does not care who wrote the selector:
		// the flow behaviour is identical.
		//
		// Which pod took a write is read from the entry's own entryCSN — the
		// #sid# field names the server that accepted it (ADR-017 makes that a
		// bare per-pod integer), so this is in-band evidence rather than an
		// inference from where the client thought it was connected.
		It("sends a new connection to the new write target when the endpoint moves",
			Label("write-rotation-pooled"), func(ctx SpecContext) {
				// A per-run suffix: an interrupted run cannot leave an entry that
				// makes the next one fail on "already exists".
				run := strconv.FormatInt(time.Now().Unix()%100000, 10)

				nodeIP := os.Getenv("E2E_NODE_IP")
				if nodeIP == "" {
					Skip("E2E_NODE_IP is not set, so there is no path through kube-proxy to probe")
				}

				awayFromBoundary(ctx)
				first := writeSelector(ctx)
				Expect(first).NotTo(BeEmpty())

				By("standing up a throwaway NodePort mirror of the write endpoint")
				mirror := func(ctx context.Context, podName string) *corev1.Service {
					svc := &corev1.Service{
						ObjectMeta: metav1.ObjectMeta{Name: probeSvcName, Namespace: namespace},
						Spec: corev1.ServiceSpec{
							Type: corev1.ServiceTypeNodePort,
							Selector: map[string]string{
								"app.kubernetes.io/name":             "slapd",
								"app.kubernetes.io/instance":         "slapd",
								"statefulset.kubernetes.io/pod-name": podName,
							},
							Ports: []corev1.ServicePort{{
								Name: "ldap", Port: 389, TargetPort: intstr.FromInt32(1024),
								Protocol: corev1.ProtocolTCP,
							}},
						},
					}
					cur, err := k8sClient.CoreV1().Services(namespace).Get(ctx, probeSvcName, metav1.GetOptions{})
					if apierrors.IsNotFound(err) {
						out, cerr := k8sClient.CoreV1().Services(namespace).Create(ctx, svc, metav1.CreateOptions{})
						Expect(cerr).NotTo(HaveOccurred())
						return out
					}
					Expect(err).NotTo(HaveOccurred())
					cur.Spec.Selector = svc.Spec.Selector
					out, uerr := k8sClient.CoreV1().Services(namespace).Update(ctx, cur, metav1.UpdateOptions{})
					Expect(uerr).NotTo(HaveOccurred())
					return out
				}

				probe := mirror(ctx, first)
				DeferCleanup(func(ctx SpecContext) {
					_ = k8sClient.CoreV1().Services(namespace).Delete(ctx, probeSvcName, metav1.DeleteOptions{})
				})
				addr := fmt.Sprintf("%s:%d", nodeIP, probe.Spec.Ports[0].NodePort)
				GinkgoWriter.Printf("probe endpoint %s mirrors %s\n", addr, first)

				// whereDidItLand writes one entry over conn and reports the
				// ordinal of the pod whose serverID stamped its entryCSN.
				whereDidItLand := func(conn *ldap.Conn, uid string) (int32, string) {
					dn := fmt.Sprintf("uid=%s,ou=People,%s", uid, baseDN)
					req := ldap.NewAddRequest(dn, nil)
					req.Attribute("objectClass", []string{"posixAccount", "shadowAccount", "inetOrgPerson"})
					req.Attribute("cn", []string{"Rotation Probe"})
					req.Attribute("sn", []string{"Probe"})
					req.Attribute("uid", []string{uid})
					req.Attribute("uidNumber", []string{"76001"})
					req.Attribute("gidNumber", []string{"76001"})
					req.Attribute("homeDirectory", []string{"/home/" + uid})
					Expect(conn.Add(req)).To(Succeed(), "write %s through the probe endpoint", dn)
					// The probe entry is written to ONE pod and deleted through the
					// cluster Service, which load-balances: a delete issued
					// milliseconds later can reach a pod the entry has not
					// replicated to yet and come back "no such object". Retry
					// until it is gone, so a run does not leave an entry behind
					// that makes the NEXT run fail on "already exists" — which is
					// how this spec failed once, looking like a product defect.
					DeferCleanup(func(ctx SpecContext) {
						Eventually(ctx, func() bool {
							_ = ldapConn.Del(ldap.NewDelRequest(dn, nil))
							return ldapExists(ldapConn, dn)
						}).WithTimeout(60*time.Second).WithPolling(2*time.Second).Should(BeFalse(),
							"probe entry %s must not survive the spec", dn)
					})

					sr, err := conn.Search(ldap.NewSearchRequest(dn, ldap.ScopeBaseObject,
						ldap.NeverDerefAliases, 0, 0, false, "(objectClass=*)",
						[]string{"entryCSN"}, nil))
					Expect(err).NotTo(HaveOccurred())
					Expect(sr.Entries).NotTo(BeEmpty())
					csn := sr.Entries[0].GetAttributeValue("entryCSN")
					parts := strings.Split(csn, "#")
					Expect(len(parts)).To(BeNumerically(">=", 3), "entryCSN %q has no sid field", csn)
					sid, err := strconv.ParseInt(strings.TrimSpace(parts[2]), 16, 32)
					Expect(err).NotTo(HaveOccurred(), "sid field of %q", csn)
					ord, ok := sidOrdinals[int(sid)]
					Expect(ok).To(BeTrue(), "sid %d (from %q) matches no pod's olcServerID %v",
						sid, csn, sidOrdinals)
					return ord, csn
				}

				By("opening one connection and writing through it, before any rotation")
				pooled := retryConnectLDAP(ctx, addr, baseDN, adminPW)
				DeferCleanup(func() { _ = pooled.Close() })
				landedA, csnA := whereDidItLand(pooled, "rotprobe-a-"+run)
				Expect(fmt.Sprintf("slapd-%d", landedA)).To(Equal(first),
					"the first write did not land on the pod the endpoint names, so the probe is "+
						"not measuring the endpoint at all (entryCSN %s)", csnA)

				By("waiting for the write endpoint to move, then repointing the probe with it")
				Eventually(ctx, func() string { return writeSelector(ctx) }).
					WithTimeout(3 * slotLength).WithPolling(time.Second).ShouldNot(Equal(first))
				awayFromBoundary(ctx)
				second := writeSelector(ctx)
				Expect(second).NotTo(Equal(first))
				mirror(ctx, second)
				GinkgoWriter.Printf("endpoint moved %s → %s\n", first, second)

				By("waiting for the endpoint to RESOLVE to the new target before dialling it")
				// Not merely "does it answer": a Service whose selector has just
				// been rewritten keeps answering from the old pod until the
				// EndpointSlice updates and kube-proxy reprograms. A fresh
				// connection opened inside that window reaches the OLD target —
				// measured, 16 ms after the patch — and that is the propagation
				// delay of the endpoint, not a client failing to follow it. What
				// a real client does is dial once the endpoint has converged.
				newPod, err := k8sClient.CoreV1().Pods(namespace).Get(ctx, second, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(newPod.Status.PodIP).NotTo(BeEmpty())
				Eventually(ctx, func() []string {
					eps, err := k8sClient.CoreV1().Endpoints(namespace).Get(ctx, probeSvcName, metav1.GetOptions{})
					if err != nil {
						return nil
					}
					var ips []string
					for _, s := range eps.Subsets {
						for _, a := range s.Addresses {
							ips = append(ips, a.IP)
						}
					}
					return ips
				}).WithTimeout(60*time.Second).WithPolling(time.Second).
					Should(ConsistOf(newPod.Status.PodIP),
						"the probe endpoint must resolve to %s (%s) before a fresh connection can "+
							"be said to have followed or not followed it", second, newPod.Status.PodIP)

				By("writing again on the SAME connection")
				landedB, csnB := whereDidItLand(pooled, "rotprobe-b-"+run)
				followed := fmt.Sprintf("slapd-%d", landedB) == second

				By("writing on a FRESH connection")
				var landedC int32
				var csnC string
				freshConn := retryConnectLDAP(ctx, addr, baseDN, adminPW)
				DeferCleanup(func() { _ = freshConn.Close() })
				landedC, csnC = whereDidItLand(freshConn, "rotprobe-c-"+run)

				AddReportEntry("write endpoint: who followed the move", fmt.Sprintf(
					"endpoint moved %s → %s\n"+
						"  before the move, the first write landed on slapd-%d (entryCSN %s)\n"+
						"  FRESH connection wrote to slapd-%d (entryCSN %s) — this is the claim\n"+
						"  established connection wrote to slapd-%d (entryCSN %s) — moved: %v; a "+
						"Service does not re-wire an open flow, which is why the rotation is for "+
						"short-lived connections by design",
					first, second, landedA, csnA, landedC, csnC, landedB, csnB, followed))
				GinkgoWriter.Printf(
					"established connection moved with the endpoint: %v "+
						"(wrote to slapd-%d, endpoint names %s)\n", followed, landedB, second)

				Expect(fmt.Sprintf("slapd-%d", landedC)).To(Equal(second),
					"a FRESH connection to the moved endpoint wrote to slapd-%d while the endpoint "+
						"names %s — if new connections do not reach the new writer the endpoint "+
						"does not work at all (entryCSN %s)", landedC, second, csnC)
			})

		// Rotation off is not "point the endpoint nowhere" and not "delete it".
		// A Service clients were told to follow must keep answering; what it
		// must stop doing is pinning every write onto one pod whose purge is no
		// longer phased by anybody — which is the measured stall arriving at the
		// moment the user believes they returned to the old behaviour.
		It("stops naming one pod when rotation is turned off", func(ctx SpecContext) {
			setRotation(ctx, false, 0)

			Eventually(ctx, func() (string, error) {
				svc, err := k8sClient.CoreV1().Services(namespace).Get(ctx, writeSvcName, metav1.GetOptions{})
				if err != nil {
					return "", err
				}
				return svc.Spec.Selector["statefulset.kubernetes.io/pod-name"], nil
			}).WithTimeout(2*time.Minute).WithPolling(2*time.Second).Should(BeEmpty(),
				"with the rotation off the write endpoint must stop naming one pod: nothing phases "+
					"that pod's purge any more, so pinning every write to it is the stall ADR-037 "+
					"measured, arriving when the feature is switched OFF")

			svc, err := k8sClient.CoreV1().Services(namespace).Get(ctx, writeSvcName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(),
				"the endpoint must not be DELETED: clients were told to follow this name, and "+
					"removing it turns an operator-side configuration change into a write outage")
			Expect(svc.Spec.Selector).To(HaveKeyWithValue("app.kubernetes.io/instance", "slapd"),
				"the retired endpoint is an alias of the client Service, selecting the whole fleet")

			By("checking it still resolves to the fleet")
			Eventually(ctx, func() (int, error) {
				eps, err := k8sClient.CoreV1().Endpoints(namespace).Get(ctx, writeSvcName, metav1.GetOptions{})
				if err != nil {
					return -1, err
				}
				n := 0
				for _, s := range eps.Subsets {
					n += len(s.Addresses)
				}
				return n, nil
			}).WithTimeout(60*time.Second).WithPolling(2*time.Second).Should(BeNumerically(">", 1),
				"a retired write endpoint that resolves to one pod or none has not been retired")
		})
	})
