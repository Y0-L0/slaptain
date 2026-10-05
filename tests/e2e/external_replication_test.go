package e2e_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// External replication tests verify cross-cluster delta-syncrepl between two
// independent SlapdClusters. Gated by E2E_EXTERNAL_REPL=1.
//
// Prerequisites — both clusters must be fully deployed BEFORE running tests:
//   - Operator installed on both siteA and siteB
//   - SlapdCluster deployed on both with externalPeers pointing at each other
//   - SlapdDatabase + SlapdSchema deployed on both (data replicates automatically)
//   - Both sites' LDAP services reachable from the test machine
//
// Required env vars:
//
//	E2E_EXTERNAL_REPL=1         — enable these tests
//	E2E_REMOTE_LDAP_ADDR        — LDAP address of siteB (e.g. "10.0.1.50:30389")
//	E2E_REMOTE_ADMIN_PW         — plaintext admin password for siteB
//
// Optional env vars:
//
//	NAMESPACE_TESTING            — siteA namespace (default: slaptain-testing)
//
// The siteA cluster uses the default kubeconfig (or KUBECONFIG env var).
// See tests/README.md § "Cross-cluster replication tests" for full setup instructions.

var _ = Describe("external replication", Label("external-replication"), Ordered, func() {

	var (
		remoteAddr    string
		remoteAdminPW string
		remoteRootPW  string // siteB's OWN cn=config admin password — see BeforeEach
		replicas      int32

		// The RID coordinate system in force for this run (ADR-031). A RID is
		// not a literal to match any more, it is a coordinate to decode:
		//
		//	rid = 1 + segment·(P+S) + offset
		//	offset <  P  →  a provider at MY OWN site, at pod ordinal `offset`
		//	offset >= P  →  MY LINK to peer site `offset − P`
		//
		// All three terms are read off the live cluster rather than assumed:
		// the segment is declared on the SlapdDatabase, and P and S come from
		// the mesh's spec.ridLayout (8 and 12 when it declares none), because
		// the layout is mesh-scoped and a mesh is free to narrow it.
		ridSegment int32
		ridP, ridS int32
		// peerSiteIndex maps a peer's name to the site index its remote slot is
		// keyed on — the site's declared serverIDIndex under meshRef, or the
		// peer's own spec.siteIndex off-mesh. It is the ONLY thing that decides
		// a cross-site stanza's RID, which is why the removal spec below no
		// longer has to drop the peer at the end of the list.
		peerSiteIndex map[string]int32
	)

	BeforeEach(func(ctx SpecContext) {
		if os.Getenv("E2E_EXTERNAL_REPL") != "1" {
			Skip("E2E_EXTERNAL_REPL not set; skipping external replication tests")
		}

		remoteAddr = os.Getenv("E2E_REMOTE_LDAP_ADDR")
		Expect(remoteAddr).NotTo(BeEmpty(), "E2E_REMOTE_LDAP_ADDR must be set")

		remoteAdminPW = os.Getenv("E2E_REMOTE_ADMIN_PW")
		if remoteAdminPW == "" {
			// Fallback: if both clusters share the same credentials secret
			// (e.g. pre-seeded via credentialsSecretName), read from the local
			// cluster's slapd-passwords.
			remoteAdminPW = adminPW
		}
		Expect(remoteAdminPW).NotTo(BeEmpty(), "E2E_REMOTE_ADMIN_PW must be set (or siteA admin password is used as fallback)")

		// The cn=config admin password is per-cluster. The database credentials
		// ARE shared across sites (e2e.sh pre-creates the same secret
		// everywhere), which is why remoteAdminPW can fall back to the local
		// one — but each SlapdCluster auto-generates its own
		// <name>-config-password, so the suite-wide rootPW is siteA's and only
		// siteA's. Anything binding cn=admin,cn=config against siteB must use
		// this. e2e.sh reads it off the remote cluster and exports it; when it
		// is empty, cross-site cn=config reads are skipped rather than attempted
		// with a password that cannot work.
		remoteRootPW = os.Getenv("E2E_REMOTE_ROOT_PW")

		sts, err := k8sClient.AppsV1().StatefulSets(namespace).Get(ctx, "slapd", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		replicas = 1
		if sts.Spec.Replicas != nil {
			replicas = *sts.Spec.Replicas
		}

		// Read the coordinate system off the live objects. Nothing here is a
		// constant: the segment is the deployer's declaration, and the layout is
		// the mesh's, so a mesh that narrows podsPerSite or widens maxSites
		// moves every RID in the cluster and these specs follow it.
		sd := &ldapv1alpha1.SlapdDatabase{}
		Expect(crdClient.Get(ctx, types.NamespacedName{Name: dbCRName, Namespace: namespace}, sd)).To(Succeed())
		Expect(sd.Spec.Replication.RIDSegment).NotTo(BeNil(), "test fixture must set spec.replication.ridSegment")
		ridSegment = *sd.Spec.Replication.RIDSegment

		ridP, ridS = ridLayoutInForce(ctx, "slapd")
		peerSiteIndex = peerSiteIndices(ctx, "slapd")
	}, NodeTimeout(30*time.Second))

	// ── 1. Every stanza's RID decodes to the provider it names ───────────────

	It("every syncrepl stanza's RID decodes to the provider its URI names", func(ctx SpecContext) {
		// What this replaces, and why the replacement is stronger.
		//
		// Until ADR-031 this spec matched one literal — rid=151, from the
		// pre-ADR-031 formula ridSegment*100 + 50 + j + 1, where j was a peer's
		// POSITION in the peer list. A literal can only ever say "some stanza
		// carries this number"; it cannot say the number belongs to the peer it
		// is written next to, which is the property that actually matters.
		// slapd does not reject a duplicate RID (ADR-031, "What slapd actually
		// requires"): two stanzas sharing a number both load and share a
		// connid, so a misattributed RID is silent at every layer below this
		// assertion.
		//
		// So: decode every stanza and check it against who the stanza says it
		// talks to. A remote stanza must decode to the remote block (offset >=
		// P) and name the peer site whose serverIDIndex equals offset − P; the
		// stanza's own tls_cacert=…/peers/<site>/ca.crt is the second,
		// independent witness of which site that is, so the decode is checked
		// against the operator's OTHER attribution of the same peer rather than
		// against a number this spec computed for itself. An in-cluster stanza
		// must decode to the local block and name the pod at that ordinal.
		//
		// Robustness: the RID is read from the live objects every pass (segment
		// from the SlapdDatabase, P and S from the mesh), and the whole check is
		// retried, because the suite adds and removes databases and sites around
		// it and a pod mid-roll yields a transiently short or stale set.
		Expect(peerSiteIndex).NotTo(BeEmpty(),
			"external-replication specs require a cluster with external peers, and this one resolved none")

		Eventually(ctx, func() error {
			return checkRWPodStanzas(replicas, func(pod string, stanzas []string) error {
				return checkStanzaCoordinates(pod, stanzas, ridSegment, ridP, ridS, peerSiteIndex)
			})
		}).WithTimeout(3*time.Minute).WithPolling(5*time.Second).Should(Succeed(),
			"every olcSyncRepl value on every read-write pod must decode to the provider it points at")
	}, NodeTimeout(5*time.Minute))

	// ── 1a. Every declared peer actually has its stanza ──────────────────────

	It("every external peer the operator resolved has a stanza at its own site coordinate", func(ctx SpecContext) {
		// The complement of the decode check above, and it is a separate
		// assertion because the two fail for opposite reasons. The decode check
		// is satisfied by a pod carrying NO external stanzas at all — which is
		// precisely what a short set under the atomic Replace looks like
		// (RID-PLAN hazard 2, and the failure mode M4b exists for). This one
		// asserts presence, peer by peer, at the exact coordinate that peer's
		// declared site index gives it.
		Expect(peerSiteIndex).NotTo(BeEmpty(),
			"external-replication specs require a cluster with external peers, and this one resolved none")

		Eventually(ctx, func() error {
			return checkRWPodStanzas(replicas, func(pod string, stanzas []string) error {
				for name, idx := range peerSiteIndex {
					want := remoteRIDFor(ridSegment, idx, ridP, ridS)
					if !hasRID(stanzas, want) {
						return fmt.Errorf("%s has no stanza for peer %q at rid=%03d "+
							"(segment %d, site index %d, layout P=%d S=%d); stanzas: %v",
							pod, name, want, ridSegment, idx, ridP, ridS, stanzas)
					}
				}
				return nil
			})
		}).WithTimeout(3*time.Minute).WithPolling(5*time.Second).Should(Succeed(),
			"each resolved external peer must have its syncrepl stanza on every read-write pod")
	}, NodeTimeout(5*time.Minute))

	// ── 1b. The remote site's cn=config is actually readable ─────────────────
	//
	// Regression guard for a diagnostic that could never authenticate: the
	// cross-site dump bound cn=admin,cn=config with the LOCAL cluster's
	// password, so it always failed with "Invalid Credentials" and never printed
	// siteB's syncrepl stanzas — precisely the state you need when cross-site
	// replication stalls. Asserting the bind here keeps the credential wiring
	// honest: if e2e.sh stops exporting E2E_REMOTE_ROOT_PW, or exports the wrong
	// site's, this spec fails instead of a diagnostic silently going blank.

	It("can read the remote site's cn=config with the remote config password", func(ctx SpecContext) {
		Expect(remoteRootPW).NotTo(BeEmpty(),
			"E2E_REMOTE_ROOT_PW must be exported for multi-site runs — without it the "+
				"cross-site diagnostics cannot read siteB's syncrepl stanzas")
		Expect(remoteRootPW).NotTo(Equal(rootPW),
			"E2E_REMOTE_ROOT_PW looks like the LOCAL config password; each SlapdCluster "+
				"auto-generates its own, so this would be the same bug in a new disguise")

		// Dial and bind with explicit timeouts, and retry. A bare
		// ldap.DialURL + Bind has no deadline at all: a NodePort connection can
		// establish against a backend that is gone (stale conntrack, a pod
		// replaced moments earlier by the resilience specs) and then wait for a
		// bind response forever. That is exactly what happened on the first run
		// of this spec — the goroutine sat in readPacket for the full 90 s node
		// timeout. Every other cross-pod path in the suite retries for the same
		// reason (retryConnectLDAP); this one must too.
		var stanzas []string
		Eventually(ctx, func() error {
			c, err := ldap.DialURL("ldap://"+remoteAddr, ldap.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}))
			if err != nil {
				return fmt.Errorf("dial %s: %w", remoteAddr, err)
			}
			defer c.Close()
			c.SetTimeout(10 * time.Second)

			if err := c.Bind("cn=admin,cn=config", remoteRootPW); err != nil {
				return fmt.Errorf("bind cn=admin,cn=config on %s: %w", remoteAddr, err)
			}
			sr, err := c.Search(ldap.NewSearchRequest(
				"cn=config", ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
				0, 0, false, fmt.Sprintf("(olcSuffix=%s)", baseDN),
				[]string{"olcSyncRepl"}, nil))
			if err != nil {
				return fmt.Errorf("search cn=config on %s: %w", remoteAddr, err)
			}
			if len(sr.Entries) == 0 {
				return fmt.Errorf("remote data DB entry not found in cn=config")
			}
			stanzas = sr.Entries[0].GetEqualFoldAttributeValues("olcSyncRepl")
			return nil
		}).WithTimeout(90*time.Second).WithPolling(5*time.Second).Should(Succeed(),
			"the remote site's cn=config must be readable with E2E_REMOTE_ROOT_PW")
		Expect(stanzas).NotTo(BeEmpty(),
			"the remote site should have syncrepl stanzas for the data DB")

		// Prove the fixed diagnostic actually renders them — retried, because it
		// opens its own connections and a single unlucky cross-site dial made
		// this assertion fail on an otherwise-healthy mesh ("bind error:
		// connection timed out"). What is being asserted is the credential
		// wiring, not the reachability of one TCP connection.
		Eventually(ctx, func() error {
			dump := dumpReplDiagnostics(remoteAddr, remoteAdminPW, remoteRootPW)
			if !strings.Contains(dump, "olcSyncRepl:") {
				return fmt.Errorf("diagnostic did not dump the remote syncrepl stanzas:\n%s", dump)
			}
			if strings.Contains(dump, "config bind error") {
				return fmt.Errorf("diagnostic failed its config bind:\n%s", dump)
			}
			return nil
		}).WithTimeout(90*time.Second).WithPolling(5*time.Second).Should(Succeed(),
			"the cross-site diagnostic must render the remote site's syncrepl stanzas")
	}, NodeTimeout(4*time.Minute))

	// ── 2. Write on siteA propagates to siteB ────────────────────────────────

	It("a write on siteA propagates to siteB", func(ctx SpecContext) {
		uid := fmt.Sprintf("ext-repl-a2b-%d", GinkgoRandomSeed())
		dn := addReplTestUser(ldapConn, uid, 65600)
		defer ldapConn.Del(ldap.NewDelRequest(dn, nil)) //nolint:errcheck

		remoteConn := connectLDAP(remoteAddr, baseDN, remoteAdminPW)
		defer remoteConn.Close()

		GinkgoLogr.Info("wrote entry on siteA, waiting for siteB", "dn", dn)
		Eventually(ctx, func() bool {
			return ldapExists(remoteConn, dn)
		}).WithTimeout(60*time.Second).WithPolling(3*time.Second).Should(BeTrue(),
			"entry %s should replicate from siteA to siteB within 60 s\n%s", dn,
			dumpReplDiagnostics(remoteAddr, remoteAdminPW, remoteRootPW))
	}, NodeTimeout(3*time.Minute))

	// ── 3. Write on siteB propagates to siteA ────────────────────────────────

	It("a write on siteB propagates to siteA", func(ctx SpecContext) {
		remoteConn := connectLDAP(remoteAddr, baseDN, remoteAdminPW)
		defer remoteConn.Close()

		uid := fmt.Sprintf("ext-repl-b2a-%d", GinkgoRandomSeed())
		dn := addReplTestUser(remoteConn, uid, 65601)
		defer remoteConn.Del(ldap.NewDelRequest(dn, nil)) //nolint:errcheck

		GinkgoLogr.Info("wrote entry on siteB, waiting for siteA", "dn", dn)
		Eventually(ctx, func() bool {
			return ldapExists(ldapConn, dn)
		}).WithTimeout(60*time.Second).WithPolling(3*time.Second).Should(BeTrue(),
			"entry %s should replicate from siteB to siteA within 60 s\n%s", dn,
			dumpReplDiagnostics(localLDAPAddr, adminPW, rootPW))
	}, NodeTimeout(3*time.Minute))

	// ── 4. Removing a site from the mesh removes that peer's stanza ──────────

	It("removing a site from the SlapdMesh removes that peer's syncrepl stanza",
		Label("mesh-peer-removal"), func(ctx SpecContext) {
			// Peer removal, expressed against the object that owns the peer set.
			//
			// Until ADR-028 this spec patched spec.replication.externalPeers empty
			// and watched the stanza go. That premise is inexpressible on a
			// mesh-driven cluster, and not by accident: there the peers are DERIVED
			// from the SlapdMesh plus the operator's own siteName, the spec field is
			// empty, and setting it alongside meshRef is refused outright with
			// MeshResolved=False — deliberately, because resolving that ambiguity
			// silently is how a serverID collision gets in (ADR-028 §4). Removing a
			// peer on a mesh means editing the SlapdMesh, which is a different act
			// on a different object.
			//
			// Re-expressed that way it covers strictly more than the spec it
			// replaces: the whole derivation path runs (mesh → peer set → stanza),
			// and the operator's WATCH on the SlapdMesh is exercised — the only
			// thing that makes a mesh edit reach cn=config before the five-minute
			// SlapdDatabase resync floor, and nothing else in the suite touches it.
			sc := &ldapv1alpha1.SlapdCluster{}
			Expect(crdClient.Get(ctx, types.NamespacedName{Name: "slapd", Namespace: namespace}, sc)).To(Succeed())
			Expect(sc.Spec.MeshRef).NotTo(BeEmpty(),
				"this spec removes a peer by editing the SlapdMesh, so the fixture cluster must be "+
					"mesh-driven; a cluster with hand-written spec.replication.externalPeers has no mesh to edit")

			mesh := &ldapv1alpha1.SlapdMesh{}
			Expect(crdClient.Get(ctx, types.NamespacedName{Name: sc.Spec.MeshRef, Namespace: namespace}, mesh)).
				To(Succeed(), "cluster references mesh %q", sc.Spec.MeshRef)

			// The RESOLVED peer set, in the operator's own order — never the raw
			// spec (see clusterExternalPeerNames for why reading that field is the
			// trap this suite already fell into once).
			peerNames := clusterExternalPeerNames(ctx, "slapd")
			Expect(peerNames).NotTo(BeEmpty(),
				"external-replication specs require a cluster with external peers, and this one reports none")

			// Drop the last peer. Until ADR-031 that was a REQUIREMENT, because a
			// peer's RID was its position in the list (ridBase+50+j+1, ADR-003):
			// removing a site in the middle renumbered every peer after it, so
			// "rid X is gone" could be reading a number that still existed under a
			// different peer. Under ADR-031 the slot is keyed on the peer's
			// declared site index, so removing any site renumbers nothing at all
			// (decision 2, "fan-out is not a dimension") — the tail is now just
			// the simplest victim to name, and the survivors' RIDs being
			// unchanged is a property of the scheme rather than of this choice.
			//
			// That makes the control assertion below sharper: it no longer says
			// "the other stanzas are still somewhere", it says each surviving peer
			// still holds the exact coordinate its own site index gives it.
			victim := peerNames[len(peerNames)-1]
			survivors := peerNames[:len(peerNames)-1]
			victimIdx, known := peerSiteIndex[victim]
			Expect(known).To(BeTrue(),
				"peer %q has no resolved site index, so its RID coordinate is unknowable; "+
					"under meshRef that index is the site's serverIDIndex (ADR-028 amendment), "+
					"off-mesh it is externalPeers[].siteIndex (ADR-031 decision 4)", victim)
			victimRID := remoteRIDFor(ridSegment, victimIdx, ridP, ridS)

			var victimSite *ldapv1alpha1.MeshSite
			remaining := make([]ldapv1alpha1.MeshSite, 0, len(mesh.Spec.Sites))
			for i := range mesh.Spec.Sites {
				if mesh.Spec.Sites[i].Name == victim {
					victimSite = mesh.Spec.Sites[i].DeepCopy()
					continue
				}
				remaining = append(remaining, mesh.Spec.Sites[i])
			}
			Expect(victimSite).NotTo(BeNil(),
				"the operator resolved a peer %q that the mesh does not declare as a site — peer names ARE "+
					"mesh site names (ADR-028 §4), so a mismatch here means the derivation and the object disagree",
				victim)

			originalSites := make([]ldapv1alpha1.MeshSite, len(mesh.Spec.Sites))
			copy(originalSites, mesh.Spec.Sites)

			// Leave the mesh as we found it even when an assertion below fails:
			// this is the ONE object every site's cross-site wiring is derived
			// from, and a suite that abandoned it a site short would silently
			// un-wire the rest of the run.
			restored := false
			DeferCleanup(func(ctx SpecContext) {
				if restored {
					return
				}
				cur := &ldapv1alpha1.SlapdMesh{}
				Expect(crdClient.Get(ctx, types.NamespacedName{Name: mesh.Name, Namespace: namespace}, cur)).To(Succeed())
				p := client.MergeFrom(cur.DeepCopy())
				cur.Spec.Sites = originalSites
				Expect(crdClient.Patch(ctx, cur, p)).To(Succeed())
				GinkgoLogr.Info("restored the mesh site list after a failed assertion", "site", victim)
			}, NodeTimeout(time.Minute))

			GinkgoLogr.Info("dropping a site from the mesh",
				"mesh", mesh.Name, "site", victim, "rid", victimRID, "survivors", survivors)

			patch := client.MergeFrom(mesh.DeepCopy())
			mesh.Spec.Sites = remaining
			Expect(crdClient.Patch(ctx, mesh, patch)).To(Succeed())

			// The dropped peer's stanza goes; every survivor's stays. The second
			// half is the control: "all external stanzas vanished" would satisfy a
			// removed-peer assertion just as well, and would mean the derivation
			// had collapsed to an empty peer set — which is exactly the outcome
			// ADR-028 §4 refuses to reach silently.
			Eventually(ctx, func() error {
				return checkRWPodStanzas(replicas, func(pod string, stanzas []string) error {
					if hasRID(stanzas, victimRID) {
						return fmt.Errorf("%s still carries the dropped peer's stanza (rid=%03d)", pod, victimRID)
					}
					if namesPeerCAPath(stanzas, victim) {
						return fmt.Errorf("%s still names the dropped peer in a tls_cacert path (peers/%s/)", pod, victim)
					}
					for j := range survivors {
						idx, known := peerSiteIndex[survivors[j]]
						if !known {
							return fmt.Errorf("%s: surviving peer %q has no resolved site index",
								pod, survivors[j])
						}
						rid := remoteRIDFor(ridSegment, idx, ridP, ridS)
						if !hasRID(stanzas, rid) {
							return fmt.Errorf("%s lost surviving peer %q (rid=%03d) as well — the derivation "+
								"collapsed rather than dropping one site, OR it renumbered the survivors, "+
								"which ADR-031 decision 2 says a topology change must never do",
								pod, survivors[j], rid)
						}
					}
					return nil
				})
			}).WithTimeout(5*time.Minute).WithPolling(5*time.Second).Should(Succeed(),
				"dropping site %q from mesh %q should remove exactly its syncrepl stanza from every RW pod",
				victim, mesh.Name)
			// 5 minutes, not the 60 s its spec-patching predecessor used: removing
			// a peer also removes its CA volume from the pod template, so the
			// StatefulSet rolls while the stanza is being rewritten (ADR-013's
			// accepted wart). The stanza edit itself lands on the mesh watch within
			// seconds; the budget is for the roll happening underneath it, and for
			// the cn=config pause freeze (reconcile-loop-fixes.md 2026-09-16) not
			// turning one slow pod into a red run.

			// Put it back and watch the stanza return — removal is only half the
			// behaviour, and a one-way test would pass just as well against an
			// operator that had stopped writing external stanzas altogether.
			cur := &ldapv1alpha1.SlapdMesh{}
			Expect(crdClient.Get(ctx, types.NamespacedName{Name: mesh.Name, Namespace: namespace}, cur)).To(Succeed())
			patch = client.MergeFrom(cur.DeepCopy())
			cur.Spec.Sites = originalSites
			Expect(crdClient.Patch(ctx, cur, patch)).To(Succeed())
			restored = true

			Eventually(ctx, func() error {
				return checkRWPodStanzas(replicas, func(pod string, stanzas []string) error {
					if !hasRID(stanzas, victimRID) {
						return fmt.Errorf("%s has not regained the restored peer's stanza (rid=%03d)", pod, victimRID)
					}
					return nil
				})
			}).WithTimeout(8*time.Minute).WithPolling(5*time.Second).Should(Succeed(),
				"restoring site %q to mesh %q should bring its syncrepl stanza back on every RW pod",
				victim, mesh.Name)
			// 8 minutes: re-adding reconciles on the watch immediately, but the
			// stanza cannot be written until the peer's ADDRESSES are known, and
			// discovery rides the 60 s CSN-monitoring tick (ADR-016 amendment
			// 2026-08-26). Worst case is a full tick, the follow-up SlapdDatabase
			// reconcile on every pod, and the pod template rolling back underneath
			// all of it.
		}, NodeTimeout(20*time.Minute))

	// ── 5. External peer status reported in CR ───────────────────────────────

	It("external peer status is reported in the SlapdCluster status", func(ctx SpecContext) {
		sc := &ldapv1alpha1.SlapdCluster{}
		Expect(crdClient.Get(ctx, types.NamespacedName{Name: "slapd", Namespace: namespace}, sc)).To(Succeed())

		// Every branch below is conditional on a peer MODE, so a spec that finds
		// no peers at all asserts nothing and passes quietly — which is exactly
		// what happened on the first mesh run, where spec.replication.externalPeers
		// is empty by design (ADR-028 §4). Establish the precondition loudly
		// first: this file only runs under E2E_EXTERNAL_REPL=1, so a cluster with
		// no external peers is a broken fixture, never a legitimate shape.
		peerNames := clusterExternalPeerNames(ctx, "slapd")
		Expect(peerNames).NotTo(BeEmpty(),
			"external-replication specs require a cluster with external peers, and this one reports "+
				"none; on a mesh-driven cluster the peers are derived and spec.replication.externalPeers "+
				"is empty, so read them through clusterExternalPeerNames rather than off the spec")

		// Classify peers by mode. A mesh derives discovery peers and nothing
		// else (externalPeersForSite never emits uri or podAddresses), so the
		// mode is known without re-deriving the peer list.
		var hasURIPeers, hasDiscoveryPeers, hasPodAddrPeers bool
		if sc.Spec.MeshRef != "" {
			hasDiscoveryPeers = true
		}
		for _, ep := range sc.Spec.Replication.ExternalPeers {
			if ep.Discovery != nil {
				hasDiscoveryPeers = true
			} else if len(ep.PodAddresses) > 0 {
				hasPodAddrPeers = true
			} else if ep.URI != "" {
				hasURIPeers = true
			}
		}
		Expect(hasURIPeers || hasDiscoveryPeers || hasPodAddrPeers).To(BeTrue(),
			"the cluster reports peers %v but none could be classified by mode, so every assertion "+
				"below would be skipped and this spec would pass without checking anything", peerNames)

		if hasURIPeers {
			Expect(sc.Status.ExternalPeerStatuses).NotTo(BeEmpty(),
				"status.externalPeerStatuses should be populated for URI-based peers")
			for _, ps := range sc.Status.ExternalPeerStatuses {
				Expect(ps.Name).NotTo(BeEmpty(), "peer status should have a name")
				Expect(ps.Connected).To(BeTrue(),
					"external peer %q should be connected (got lastError: %s)", ps.Name, ps.LastError)
			}
		} else if hasDiscoveryPeers {
			// Discovery peers appear in ExternalPeerStatuses with DiscoveredProviders.
			Expect(sc.Status.ExternalPeerStatuses).NotTo(BeEmpty(),
				"status.externalPeerStatuses should be populated for discovery-mode peers")
			for _, ps := range sc.Status.ExternalPeerStatuses {
				Expect(ps.Name).NotTo(BeEmpty(), "peer status should have a name")
				Expect(ps.DiscoveredProviders).NotTo(BeEmpty(),
					"discovery peer %q should have discovered providers", ps.Name)

				// ADR-031: discovery returns provider IDENTITY, not bare
				// addresses, and orders the result by that identity. Before
				// M1 this list was sorted with sort.Strings over IPs, so its
				// position carried nothing — an unrelated remote pod blipping
				// permuted it and every consumer that indexed into it pointed
				// at a different provider than it had the pass before.
				//
				// Only a live mesh can show this. A unit test proves the
				// reduction sorts a slice it was handed; it cannot prove that
				// discovery against three real clusters hands it providers
				// whose ordinals are the remote pods' real ones.
				seen := map[int32]string{}
				prev := int32(-1)
				for _, dp := range ps.DiscoveredProviders {
					Expect(dp.Ordinal).To(BeNumerically(">", prev),
						"peer %q returned providers out of ordinal order (%v): ordinal %d follows %d, "+
							"so list position is not provider identity and ADR-031's RID coordinate "+
							"would move whenever a remote pod blipped",
						ps.Name, ps.DiscoveredProviders, dp.Ordinal, prev)
					Expect(seen).NotTo(HaveKey(dp.Ordinal),
						"peer %q returned two providers claiming ordinal %d (%s and %s), so the pod "+
							"list spans more than one fleet and the provider is ambiguous",
						ps.Name, dp.Ordinal, seen[dp.Ordinal], dp.Address)
					seen[dp.Ordinal] = dp.Address
					prev = dp.Ordinal
				}
			}
		} else if hasPodAddrPeers {
			// Static podAddresses peers are omitted from ExternalPeerStatuses —
			// the operator can't reach the replication network.
			Expect(sc.Status.ExternalPeerStatuses).To(BeEmpty(),
				"podAddresses peers should not appear in externalPeerStatuses")
		}
	}, NodeTimeout(30*time.Second))
})

// hasRID reports whether any stanza carries the given RID, compared as a
// NUMBER.
//
// It replaces a pair of string helpers (containsRID / containsAnyRID) that
// matched "rid=" + the caller's decimal text, and that is a trap worth
// recording rather than quietly deleting: the operator renders the value with
// %03d, so a stanza reads `rid=030`, while a caller formatting the same number
// with %d produces "30" and the prefix match silently fails. Under the
// pre-ADR-031 scheme every external RID was >= 100 and already three digits, so
// the two agreed by accident for as long as the old formula lasted. ADR-031's
// coordinates start at 1, which is exactly the range where the accident stops.
func hasRID(stanzas []string, rid int32) bool {
	for _, st := range stanzas {
		if got, err := stanzaRID(st); err == nil && got == rid {
			return true
		}
	}
	return false
}

// namesPeerCAPath reports whether any stanza points its tls_cacert at the named
// peer's CA mount directory.
//
// A second, independent witness for "this peer is wired here", and deliberately
// a name-based one: peer.Name is the directory component of the CA mount path
// (/etc/openldap/tls/peers/<name>/ca.crt) and therefore ends up verbatim in the
// stanza (ADR-028 §4, "peer names are mesh site names"). RIDs are positional and
// get reused as the peer list shrinks; the path does not.
func namesPeerCAPath(stanzas []string, peer string) bool {
	needle := "/peers/" + peer + "/"
	for _, s := range stanzas {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// checkRWPodStanzas runs check against every RW pod's olcSyncRepl values for the
// fixture's data database, and returns the first failure.
//
// It returns an error rather than asserting, so it composes with Eventually
// while a StatefulSet rolls underneath it: a pod that is mid-restart makes the
// dial, the bind or the search fail, and that is "not settled yet", never a
// verdict. The predecessor spec folded the same errors into a bare `false`,
// which made a genuinely broken pod indistinguishable from a restarting one in
// the failure output.
func checkRWPodStanzas(replicas int32, check func(pod string, stanzas []string) error) error {
	for i := int32(0); i < replicas; i++ {
		podName := fmt.Sprintf("slapd-%d", i)
		if err := func() error {
			conn, cancel := dialPodLDAP(namespace, podName, fmt.Sprintf("%d", 14000+i))
			defer cancel()
			defer conn.Close()

			// Local pod (dialPodLDAP), so the suite-wide rootPW is the right
			// credential — see the note in BeforeEach.
			if err := conn.Bind("cn=admin,cn=config", rootPW); err != nil {
				return fmt.Errorf("%s: bind cn=admin,cn=config: %w", podName, err)
			}
			sr, err := conn.Search(ldap.NewSearchRequest(
				"cn=config", ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
				0, 0, false, fmt.Sprintf("(olcSuffix=%s)", baseDN),
				[]string{"olcSyncRepl"}, nil))
			if err != nil {
				return fmt.Errorf("%s: search cn=config: %w", podName, err)
			}
			if len(sr.Entries) == 0 {
				return fmt.Errorf("%s: no cn=config entry for suffix %s", podName, baseDN)
			}
			return check(podName, sr.Entries[0].GetEqualFoldAttributeValues("olcSyncRepl"))
		}(); err != nil {
			return err
		}
	}
	return nil
}

// dumpReplDiagnostics connects to a site's LDAP and returns a diagnostic string
// with contextCSN and syncrepl stanzas. Called in assertion messages on failure
// so the output appears in the test log.
//
// configPassword is that SITE's cn=config admin password, passed explicitly
// because it is per-cluster: this used to read the suite-wide rootPW, which is
// siteA's, so every remote invocation printed
// `config bind error: LDAP Result Code 49 "Invalid Credentials"` and the syncrepl
// stanzas — the single most useful thing to know when cross-site replication
// stalls — were never dumped. Pass "" to skip the cn=config section explicitly.
func dumpReplDiagnostics(addr, adminPassword, configPassword string) string {
	var b strings.Builder
	b.WriteString("--- replication diagnostics for " + addr + " ---\n")

	// Deadlines on every diagnostic dial: this runs inside a failure message,
	// often right after pods were replaced, and a diagnostic that blocks turns a
	// clear assertion failure into an opaque node timeout.
	conn, err := ldap.DialURL("ldap://"+addr, ldap.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}))
	if err != nil {
		fmt.Fprintf(&b, "dial error: %v\n", err)
		return b.String()
	}
	defer conn.Close()
	conn.SetTimeout(10 * time.Second)

	// contextCSN from the data DB (anonymous read of rootDSE-like operational attrs).
	if err := conn.Bind(fmt.Sprintf("cn=admin,%s", baseDN), adminPassword); err != nil {
		fmt.Fprintf(&b, "bind error: %v\n", err)
		return b.String()
	}

	sr, err := conn.Search(ldap.NewSearchRequest(
		baseDN, ldap.ScopeBaseObject, ldap.NeverDerefAliases,
		0, 0, false, "(objectClass=*)",
		[]string{"contextCSN"}, nil))
	if err != nil {
		fmt.Fprintf(&b, "contextCSN search error: %v\n", err)
	} else if len(sr.Entries) > 0 {
		csns := sr.Entries[0].GetEqualFoldAttributeValues("contextCSN")
		fmt.Fprintf(&b, "contextCSN: %v\n", csns)
	}

	// syncrepl stanzas from cn=config (need config admin).
	if configPassword == "" {
		b.WriteString("cn=config not dumped: no config admin password for this site " +
			"(multi-site runs export E2E_REMOTE_ROOT_PW; single-site uses rootPW)\n")
		return b.String()
	}
	cfgConn, err := ldap.DialURL("ldap://"+addr, ldap.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}))
	if err != nil {
		fmt.Fprintf(&b, "config dial error: %v\n", err)
		return b.String()
	}
	defer cfgConn.Close()
	cfgConn.SetTimeout(10 * time.Second)

	if err := cfgConn.Bind("cn=admin,cn=config", configPassword); err != nil {
		fmt.Fprintf(&b, "config bind error: %v\n", err)
		return b.String()
	}

	sr, err = cfgConn.Search(ldap.NewSearchRequest(
		"cn=config", ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
		0, 0, false, fmt.Sprintf("(olcSuffix=%s)", baseDN),
		[]string{"olcSyncRepl"}, nil))
	if err != nil {
		fmt.Fprintf(&b, "syncrepl search error: %v\n", err)
	} else if len(sr.Entries) > 0 {
		for _, v := range sr.Entries[0].GetEqualFoldAttributeValues("olcSyncRepl") {
			fmt.Fprintf(&b, "olcSyncRepl: %s\n", redactCredentials(v))
		}
	} else {
		b.WriteString("no data DB entry found in cn=config\n")
	}

	return b.String()
}

// ── Every replicated database converges cross-site ────────────────────────────
//
// The primary-database specs above cannot see a second database's breakage,
// and nothing else can either: per ADR-008 an idle broken link reads Synced
// (lag is only observable while writes flow), and the peer CSN check computes
// its verdict from whichever database's query succeeds — a failing database is
// silently skipped. That combination let example-db2's cross-site replication
// stay broken from the day the fixture gained a second database (2026-08-25)
// until 2026-09-13: the cluster-level ExternalPeer.bindDN bound every
// database's external stanzas as the FIRST database's replication identity,
// which ADR-020's accesslog ACL rightly denies on the second database's log
// (err=32 on the logbase search → the consumer halts and retries forever),
// and the second database's credentials Secret was never pre-created shared,
// so each site minted its own password for an entry that lives INSIDE the
// replicated DIT. See docs/reconcile-loop-fixes.md 2026-09-13.
//
// Red observed 2026-09-13 against the pre-fix lab (three-site mesh): a db1
// control marker replicated siteA→siteB within 90 s while the db2 marker never
// arrived (rid=251/252 looping `SYNC RESULT err=32 … rc -101 retrying`), and a
// remote bind as db2's replication identity failed with err=49.
//
// The primary database stays in the iteration deliberately: it is the built-in
// positive control. If it goes red too, the failure is not database-specific.

var _ = Describe("external replication of every database", Label("external-replication"), Ordered, func() {

	type crossSiteDB struct {
		name    string
		suffix  string
		adminPW string
		replPW  string
	}

	var (
		remoteAddr string
		dbs        []crossSiteDB
	)

	BeforeAll(func(ctx SpecContext) {
		if os.Getenv("E2E_EXTERNAL_REPL") != "1" {
			Skip("E2E_EXTERNAL_REPL not set; skipping external replication tests")
		}
		remoteAddr = os.Getenv("E2E_REMOTE_LDAP_ADDR")
		Expect(remoteAddr).NotTo(BeEmpty(), "E2E_REMOTE_LDAP_ADDR must be set")

		db2Name := os.Getenv("DB2_CR_NAME")
		if db2Name == "" {
			Skip("DB2_CR_NAME not set — the resource set declares a single SlapdDatabase; " +
				"the primary-database specs above already cover cross-site convergence")
		}

		// Primary database: suffix and admin password are suite-wide; the
		// replication password comes from its (shared, pre-created) Secret.
		sec1Name := envOrDefault("DB_CREDENTIALS_SECRET", dbCRName+"-credentials")
		sec1, err := k8sClient.CoreV1().Secrets(namespace).Get(ctx, sec1Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(sec1.Data["replication-password"]).NotTo(BeEmpty())
		dbs = append(dbs, crossSiteDB{
			name:    dbCRName,
			suffix:  baseDN,
			adminPW: adminPW,
			replPW:  string(sec1.Data["replication-password"]),
		})

		// Second database: suffix from its CR, credentials from its Secret.
		db2 := &ldapv1alpha1.SlapdDatabase{}
		Expect(crdClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: db2Name}, db2)).To(Succeed())
		Expect(db2.Spec.Suffix).NotTo(BeEmpty())
		sec2Name := envOrDefault("DB2_CREDENTIALS_SECRET", db2Name+"-credentials")
		sec2, err := k8sClient.CoreV1().Secrets(namespace).Get(ctx, sec2Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(sec2.Data["root-password"]).NotTo(BeEmpty())
		Expect(sec2.Data["replication-password"]).NotTo(BeEmpty())
		dbs = append(dbs, crossSiteDB{
			name:    db2Name,
			suffix:  db2.Spec.Suffix,
			adminPW: string(sec2.Data["root-password"]),
			replPW:  string(sec2.Data["replication-password"]),
		})
	}, NodeTimeout(60*time.Second))

	// (a) Marker propagation, both directions. A fresh mesh replicates each
	// database's seed via the initial refresh even over a broken delta link, so
	// only a write made AFTER convergence proves the delta path — a
	// convergence-of-seed assertion would stay green against a halted journal.
	It("a write to each database on siteA appears on siteB", func(ctx SpecContext) {
		for _, db := range dbs {
			localConn := connectLDAP(localLDAPAddr, db.suffix, db.adminPW)
			defer localConn.Close()
			remoteConn := connectLDAP(remoteAddr, db.suffix, db.adminPW)
			defer remoteConn.Close()

			uid := fmt.Sprintf("xsite-a2b-%s-%d", db.name, GinkgoRandomSeed())
			dn := addPersonEntry(localConn, db.suffix, uid)
			defer localConn.Del(ldap.NewDelRequest(dn, nil)) //nolint:errcheck

			GinkgoLogr.Info("wrote cross-site marker on siteA", "db", db.name, "dn", dn)
			Eventually(ctx, func() bool {
				return ldapExists(remoteConn, dn)
			}).WithTimeout(90*time.Second).WithPolling(3*time.Second).Should(BeTrue(),
				"database %s: entry %s should replicate from siteA to siteB within 90 s — "+
					"a red here with the primary database green means THIS database's "+
					"cross-site syncrepl is down (check its consumers for err=32/err=49 loops)",
				db.name, dn)
		}
	}, NodeTimeout(5*time.Minute))

	It("a write to each database on siteB appears on siteA", func(ctx SpecContext) {
		for _, db := range dbs {
			localConn := connectLDAP(localLDAPAddr, db.suffix, db.adminPW)
			defer localConn.Close()
			remoteConn := connectLDAP(remoteAddr, db.suffix, db.adminPW)
			defer remoteConn.Close()

			uid := fmt.Sprintf("xsite-b2a-%s-%d", db.name, GinkgoRandomSeed())
			dn := addPersonEntry(remoteConn, db.suffix, uid)
			defer remoteConn.Del(ldap.NewDelRequest(dn, nil)) //nolint:errcheck

			GinkgoLogr.Info("wrote cross-site marker on siteB", "db", db.name, "dn", dn)
			Eventually(ctx, func() bool {
				return ldapExists(localConn, dn)
			}).WithTimeout(90*time.Second).WithPolling(3*time.Second).Should(BeTrue(),
				"database %s: entry %s should replicate from siteB to siteA within 90 s",
				db.name, dn)
		}
	}, NodeTimeout(5*time.Minute))

	// (b) The whole credential chain, per database: the remote site's
	// cn=replication,<suffix> entry exists, carries a userPassword, and that
	// password matches this site's Secret. Catches what (a) cannot: markers
	// carry no userPassword, so a mesh whose replication entry lost its
	// password to an ACL strip (the 2026-04-17 class, re-reached through a
	// wrong bind identity) still propagates markers while every future
	// consumer bind is doomed.
	It("each database's replication identity can bind on the remote site", func(ctx SpecContext) {
		for _, db := range dbs {
			bindDN := fmt.Sprintf("cn=replication,%s", db.suffix)
			Eventually(ctx, func() error {
				c, err := ldap.DialURL("ldap://"+remoteAddr, ldap.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}))
				if err != nil {
					return fmt.Errorf("dial %s: %w", remoteAddr, err)
				}
				defer c.Close()
				c.SetTimeout(10 * time.Second)
				if err := c.Bind(bindDN, db.replPW); err != nil {
					return fmt.Errorf("bind %s on %s: %w", bindDN, remoteAddr, err)
				}
				return nil
			}).WithTimeout(90*time.Second).WithPolling(5*time.Second).Should(Succeed(),
				"database %s: the shared replication password must bind as %s on the remote "+
					"site — err=49 here means the sites do not share this database's "+
					"credentials Secret, or the replicated entry diverged from it",
				db.name, bindDN)
		}
	}, NodeTimeout(5*time.Minute))
})

// ── RID coordinates (ADR-031) ────────────────────────────────────────────────
//
// A RID is a coordinate in a static matrix, not an address:
//
//	rid = 1 + segment·M + offset              M = P + S
//	offset <  P   →  a provider at MY OWN site, at pod ordinal `offset`
//	offset >= P   →  MY LINK to peer site `offset − P`
//
// WHY THIS IS SPELLED OUT HERE rather than called from internal/ridcoord, which
// is where the operator's single implementation lives (RID-PLAN hazard 1: one
// coordinate function, not two).
//
// Mechanically, it cannot be imported: tests/e2e is its own Go module and
// ridcoord sits under operator/internal/, so the compiler refuses
// ("use of internal package … not allowed"). That is worth fixing — a `slctl
// rid decode` subcommand is already planned for M6 and would give this suite a
// supported decoder — and until it exists the choice is between restating the
// formula and not asserting the property at all.
//
// It is also, on its own terms, the right thing for a CHECK. Hazard 1 is about
// code that PRODUCES RIDs: two producers drift and slapd accepts the result in
// silence. A test that computed its expectation by calling the same function
// the operator called would agree with the operator by construction and could
// only ever catch a crash. The eight lines below are an independent reading of
// ADR-031 decision 1, which is what makes disagreeing with the operator
// meaningful.

// ridLayoutInForce returns the P and S of the layout this cluster computes
// coordinates in: the mesh's spec.ridLayout, defaulted PER AXIS, or the
// defaults (8, 12) for a cluster with no meshRef.
//
// Per-axis defaulting matters because the API server fills a field's default
// only when its parent object is present: a mesh declaring just maxSites
// reaches the operator with podsPerSite = 0, and the operator reads that as
// "unset", not as "zero".
func ridLayoutInForce(ctx context.Context, clusterName string) (int32, int32) {
	// ridcoord.DefaultLayout(), restated: P=8, S=12 → M=20, 49 segments.
	p, sMax := int32(8), int32(12)

	sc := &ldapv1alpha1.SlapdCluster{}
	Expect(crdClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: namespace}, sc)).To(Succeed())
	if sc.Spec.MeshRef == "" {
		return p, sMax
	}

	mesh := &ldapv1alpha1.SlapdMesh{}
	Expect(crdClient.Get(ctx, types.NamespacedName{Name: sc.Spec.MeshRef, Namespace: namespace}, mesh)).
		To(Succeed(), "cluster %q references SlapdMesh %q", clusterName, sc.Spec.MeshRef)
	if mesh.Spec.RIDLayout == nil {
		return p, sMax
	}
	if mesh.Spec.RIDLayout.PodsPerSite != 0 {
		p = mesh.Spec.RIDLayout.PodsPerSite
	}
	if mesh.Spec.RIDLayout.MaxSites != 0 {
		sMax = mesh.Spec.RIDLayout.MaxSites
	}
	return p, sMax
}

// peerSiteIndices maps each of the cluster's external peers to the site index
// its remote RID slot is keyed on.
//
// Under meshRef that index is the peer SITE's declared serverIDIndex, read off
// the SlapdMesh — not the site's position in spec.sites, which is freely
// reorderable and would name a different site tomorrow. Off-mesh it is the
// peer's own spec.siteIndex (ADR-031 decision 4), which is required there
// precisely because nothing else can supply it.
//
// Peers are taken from the RESOLVED set (clusterExternalPeerNames), so a
// mesh-driven cluster — whose spec.replication.externalPeers is empty by
// design — is read correctly rather than reported as having no peers.
func peerSiteIndices(ctx context.Context, clusterName string) map[string]int32 {
	names := clusterExternalPeerNames(ctx, clusterName)
	out := make(map[string]int32, len(names))

	sc := &ldapv1alpha1.SlapdCluster{}
	Expect(crdClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: namespace}, sc)).To(Succeed())

	if sc.Spec.MeshRef == "" {
		for _, ep := range sc.Spec.Replication.ExternalPeers {
			Expect(ep.SiteIndex).NotTo(BeNil(),
				"hand-written external peer %q declares no siteIndex: a replicating cluster outside a "+
					"mesh must declare one, because nothing else gives the peer a slot in the remote "+
					"block (ADR-031 decision 4)", ep.Name)
			out[ep.Name] = *ep.SiteIndex
		}
		return out
	}

	mesh := &ldapv1alpha1.SlapdMesh{}
	Expect(crdClient.Get(ctx, types.NamespacedName{Name: sc.Spec.MeshRef, Namespace: namespace}, mesh)).
		To(Succeed(), "cluster %q references SlapdMesh %q", clusterName, sc.Spec.MeshRef)

	byName := make(map[string]int32, len(mesh.Spec.Sites))
	for i := range mesh.Spec.Sites {
		site := mesh.Spec.Sites[i]
		Expect(site.ServerIDIndex).NotTo(BeNil(),
			"mesh %q declares site %q with no serverIDIndex", mesh.Name, site.Name)
		byName[site.Name] = *site.ServerIDIndex
	}

	for _, name := range names {
		idx, known := byName[name]
		Expect(known).To(BeTrue(),
			"the operator resolved a peer %q that mesh %q does not declare as a site — peer names ARE "+
				"mesh site names (ADR-028 §4), so a mismatch means the derivation and the object disagree",
			name, mesh.Name)
		out[name] = idx
	}
	return out
}

// remoteRIDFor is the coordinate of a link to the peer site at siteIndex, for
// the database holding segment: ADR-031's rid = 1 + segment·(P+S) + P +
// siteIndex.
func remoteRIDFor(segment, siteIndex, p, sMax int32) int32 {
	return 1 + segment*(p+sMax) + p + siteIndex
}

// decodeStanzaRID inverts the formula: which segment the RID belongs to, which
// of the two blocks the offset fell in, and the index within that block.
func decodeStanzaRID(rid, p, sMax int32) (segment int32, remote bool, index int32) {
	m := p + sMax
	offset := (rid - 1) % m
	segment = (rid - 1) / m
	if offset < p {
		return segment, false, offset
	}
	return segment, true, offset - p
}

// stanzaRID reads a stanza's rid=NNN, tolerating the X-ORDERED {N} prefix slapd
// renders values with.
func stanzaRID(stanza string) (int32, error) {
	s := strings.TrimSpace(stanza)
	if strings.HasPrefix(s, "{") {
		if i := strings.Index(s, "}"); i >= 0 {
			s = s[i+1:]
		}
	}
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "rid=") {
		return 0, fmt.Errorf("stanza does not start with rid=: %q", stanza)
	}
	field := s[len("rid="):]
	if i := strings.IndexByte(field, ' '); i >= 0 {
		field = field[:i]
	}
	n, err := strconv.Atoi(field)
	if err != nil {
		return 0, fmt.Errorf("stanza carries a non-numeric rid %q: %w", field, err)
	}
	return int32(n), nil
}

// stanzaField returns the value of `name=` in a stanza, or "".
func stanzaField(stanza, name string) string {
	for _, tok := range strings.Fields(stanza) {
		if v, ok := strings.CutPrefix(tok, name+"="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

// stanzaPeerCASite returns the peer name a stanza's tls_cacert path names
// (/etc/openldap/tls/peers/<name>/ca.crt), or "" when the stanza carries no
// per-peer CA — an in-cluster stanza, or a cross-site one on a TLS-less
// fixture.
//
// This is the second, independent witness of which site a stanza talks to: the
// operator writes the peer's NAME into that path (ADR-028 §4) by a completely
// different code path than the one that computes the RID, so agreement between
// the two is evidence and not tautology.
func stanzaPeerCASite(stanza string) string {
	const marker = "/peers/"
	path := stanzaField(stanza, "tls_cacert")
	i := strings.Index(path, marker)
	if i < 0 {
		return ""
	}
	rest := path[i+len(marker):]
	j := strings.IndexByte(rest, '/')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// stanzaProviderOrdinal returns the pod ordinal a stanza's provider= URI names,
// for an in-cluster provider addressed by its stable pod DNS
// (ldaps://<cluster>-<ordinal>.<headless>…). It returns false for anything else
// — notably a provider addressed by IP, which happens when the replication
// network is used for in-cluster traffic too (ADR-007 useForInCluster).
func stanzaProviderOrdinal(stanza, clusterName string) (int32, bool) {
	uri := stanzaField(stanza, "provider")
	host := uri
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	label := host
	if i := strings.IndexByte(label, '.'); i >= 0 {
		label = label[:i]
	}
	tail, ok := strings.CutPrefix(label, clusterName+"-")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(tail)
	if err != nil || n < 0 {
		return 0, false
	}
	return int32(n), true
}

// checkStanzaCoordinates is the assertion ADR-031 asks for, per pod: every
// stanza's RID decodes to the provider the stanza itself points at.
//
// Four things are checked, and each one is a different silent failure:
//
//   - the segment. Every stanza on this database's entry must decode to the
//     segment the SlapdDatabase declares; one that decodes elsewhere is a
//     stanza wearing another database's number, which is a cross-database RID
//     collision on the same pod (a RID is unique per SERVER, not per database).
//   - uniqueness. Two stanzas sharing a RID both load and share a connid, and
//     slapd says nothing.
//   - remote attribution. offset − P must equal the peer site's declared index,
//     cross-checked against the peer name in the stanza's own tls_cacert path.
//   - local attribution. offset must be the pod ordinal the provider URI names,
//     and must never be the reading pod's own ordinal — a pod replicating from
//     itself is a loop.
//
// It returns errors rather than asserting, so it composes with Eventually while
// the suite rolls pods and edits the mesh underneath it.
func checkStanzaCoordinates(pod string, stanzas []string, segment, p, sMax int32, peerIdx map[string]int32) error {
	// The reading pod's own ordinal, from its StatefulSet name. Parsed rather
	// than passed in so a caller cannot get the two out of step; a name that
	// does not parse simply disables the self-stanza check below instead of
	// failing the pass, since it is the weakest of the four assertions and the
	// pod name is the suite's, not the operator's.
	selfOrdinal := int32(-1)
	if tail, ok := strings.CutPrefix(pod, "slapd-"); ok {
		if n, err := strconv.Atoi(tail); err == nil {
			selfOrdinal = int32(n)
		}
	}

	if len(stanzas) == 0 {
		return fmt.Errorf("%s carries no olcSyncRepl at all: on a replicating cluster that is an "+
			"empty atomic Replace, not a settled state (RID-PLAN hazard 2)", pod)
	}

	seen := map[int32]string{}
	for _, st := range stanzas {
		rid, err := stanzaRID(st)
		if err != nil {
			return fmt.Errorf("%s: %w", pod, err)
		}
		if rid < 1 || rid > 999 {
			return fmt.Errorf("%s: rid %d is outside slapd's 0..999 band: %s", pod, rid, st)
		}
		if prev, dup := seen[rid]; dup {
			return fmt.Errorf("%s: rid %d is carried by two stanzas, which share one connid and are "+
				"indistinguishable in the logs:\n  %s\n  %s", pod, rid, prev, st)
		}
		seen[rid] = st

		gotSegment, remote, index := decodeStanzaRID(rid, p, sMax)
		if gotSegment != segment {
			return fmt.Errorf("%s: rid %d decodes to segment %d, but this database declares segment %d "+
				"(layout P=%d S=%d) — the stanza is wearing another database's number: %s",
				pod, rid, gotSegment, segment, p, sMax, st)
		}

		if remote {
			site := stanzaPeerCASite(st)
			if site == "" {
				// No per-peer CA to cross-check against (a TLS-less fixture).
				// The coordinate must still be one a declared peer owns.
				owned := false
				for _, idx := range peerIdx {
					if idx == index {
						owned = true
						break
					}
				}
				if !owned {
					return fmt.Errorf("%s: rid %d is a link to peer site index %d, which no resolved "+
						"peer declares (resolved: %v): %s", pod, rid, index, peerIdx, st)
				}
				continue
			}
			want, known := peerIdx[site]
			if !known {
				return fmt.Errorf("%s: rid %d points at peer %q (from its tls_cacert path), which is "+
					"not in the resolved peer set %v: %s", pod, rid, site, peerIdx, st)
			}
			if want != index {
				return fmt.Errorf("%s: rid %d decodes to peer site index %d, but the peer it names "+
					"(%q, from its tls_cacert path) declares site index %d. The stanza talks to one "+
					"site and is numbered for another; slapd accepts that in silence: %s",
					pod, rid, index, site, want, st)
			}
			continue
		}

		// Local block.
		if site := stanzaPeerCASite(st); site != "" {
			return fmt.Errorf("%s: rid %d decodes to the LOCAL block (pod ordinal %d) but the stanza "+
				"points at peer site %q: %s", pod, rid, index, site, st)
		}
		if selfOrdinal >= 0 && index == selfOrdinal {
			return fmt.Errorf("%s: rid %d is this pod's own ordinal (%d) — a pod never replicates "+
				"from itself: %s", pod, rid, index, st)
		}
		ordinal, ok := stanzaProviderOrdinal(st, "slapd")
		if !ok {
			// Provider addressed by IP (ADR-007 useForInCluster). The ordinal
			// is unrecoverable from the URI, so attribution is not assertable
			// here; the segment, band and uniqueness checks above still hold.
			continue
		}
		if ordinal != index {
			return fmt.Errorf("%s: rid %d decodes to in-cluster pod ordinal %d, but its provider URI "+
				"names pod %d. The stanza replicates from one pod under another pod's number: %s",
				pod, rid, index, ordinal, st)
		}
	}
	return nil
}
