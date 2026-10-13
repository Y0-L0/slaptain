package e2e_test

import (
	"bufio"
	"bytes"
	"fmt"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The slapd exporter sidecar (ADR-033 Surface 3, spec.monitoring.enabled).
//
// What this asserts, on every read-write AND read-only pod:
//
//  1. an `exporter` container is present and Running, and the pod is Ready —
//     Ready is the AND over containers, so a sidecar that cannot start is a pod
//     that has left its Service (ADR-030);
//  2. GET /metrics on port 9330, through the API server's pod proxy, answers
//     200 and carries the curated families of ADR-033 decision 4 — the
//     contract the shipped rules and dashboards read (exporterContractProblems):
//     - openldap_up 1 — this scrape read cn=Monitor, i.e. the SASL EXTERNAL
//     bind over ldapi was authorised to read the monitor database;
//     - the exporter's own openldap_exporter_dials_total and
//     openldap_exporter_binds_total, each with result="ok";
//     - one representative of every curated group: operations by op (bind and
//     search, which the exporter's own scrape and the readiness probe
//     guarantee), connections (the counter and the gauge), threads, waiters
//     by direction, sent traffic, and the start timestamp;
//     - none of the upstream exporter's DN-labelled families, and no `dn`
//     label anywhere — the shape decision 4 withdrew;
//     - openldap_mdb_* for every data suffix the SlapdDatabase CRs declare, and
//     on read-write pods also for each replicated database's own accesslog
//     suffix (ADR-019). Expectations come from the CRs, NOT from the pod —
//     reading the suffix list off the pod would mirror whatever the exporter
//     itself sees.
//
// The pod proxy is used deliberately: it needs no NodePort, no port-forward and
// no network path from the runner to the pod network, so the spec runs the same
// way on every lab.
//
// Ungated: the e2e fixture turns monitoring on (tests/e2e.sh), so every run
// covers the sidecar. Against a cluster with monitoring off it is red at (1).
var _ = Describe("slapd exporter sidecar", Label("monitor-sidecar", "monitor"), Ordered, ContinueOnFailure, func() {

	const (
		exporterContainer = "exporter"
		metricsPort       = "9330"
	)

	var (
		rwPods       []string
		roPods       []string
		dataSuffixes []string // every SlapdDatabase of cluster slapd
		logSuffixes  []string // accesslog suffixes, read-write pods only
	)

	BeforeAll(func(ctx SpecContext) {
		sc := &ldapv1alpha1.SlapdCluster{}
		Expect(crdClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "slapd"}, sc)).To(Succeed())

		rwPods = rwPodNames(ctx, "slapd")
		Expect(rwPods).NotTo(BeEmpty())
		if roSts, err := k8sClient.AppsV1().StatefulSets(namespace).
			Get(ctx, "slapd-readonly", metav1.GetOptions{}); err == nil {
			ro := int32(0)
			if roSts.Spec.Replicas != nil {
				ro = *roSts.Spec.Replicas
			}
			for i := int32(0); i < ro; i++ {
				roPods = append(roPods, fmt.Sprintf("slapd-readonly-%d", i))
			}
		}

		dbs := &ldapv1alpha1.SlapdDatabaseList{}
		Expect(crdClient.List(ctx, dbs, client.InNamespace(namespace))).To(Succeed())
		for i := range dbs.Items {
			db := &dbs.Items[i]
			if db.Spec.ClusterRef != sc.Name {
				continue
			}
			dataSuffixes = append(dataSuffixes, db.Spec.Suffix)
			if sc.NeedsAccesslog() && db.DeltaSyncEnabled() {
				logSuffixes = append(logSuffixes, ldapv1alpha1.AccesslogSuffix(db.Name))
			}
		}
		Expect(dataSuffixes).NotTo(BeEmpty(), "no SlapdDatabase references cluster slapd")
		GinkgoLogr.Info("monitor-sidecar expectations",
			"rwPods", rwPods, "roPods", roPods, "dataSuffixes", dataSuffixes, "logSuffixes", logSuffixes)
	})

	allPods := func() []string { return append(append([]string{}, rwPods...), roPods...) }

	It("runs an exporter container on every pod, which stays Ready", func(ctx SpecContext) {
		for _, name := range allPods() {
			pod, err := k8sClient.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			var spec *corev1.Container
			for i := range pod.Spec.Containers {
				if pod.Spec.Containers[i].Name == exporterContainer {
					spec = &pod.Spec.Containers[i]
				}
			}
			Expect(spec).NotTo(BeNil(), "pod %s has no %q container (containers: %v) — "+
				"is spec.monitoring.enabled set?", name, exporterContainer, containerNames(pod))

			var st *corev1.ContainerStatus
			for i := range pod.Status.ContainerStatuses {
				if pod.Status.ContainerStatuses[i].Name == exporterContainer {
					st = &pod.Status.ContainerStatuses[i]
				}
			}
			Expect(st).NotTo(BeNil(), "pod %s reports no status for %q", name, exporterContainer)
			Expect(st.State.Running).NotTo(BeNil(), "pod %s: exporter not Running: %+v", name, st.State)
			Expect(st.RestartCount).To(BeZero(), "pod %s: exporter restarted %d time(s) — "+
				"a crash-looping sidecar is noise (ADR-033)", name, st.RestartCount)
			Expect(podReady(k8sClient, namespace, name)).To(BeTrue(), "pod %s is not Ready", name)
		}
	})

	It("serves the curated openldap_ families for every database through the pod proxy", func(ctx SpecContext) {
		for _, name := range allPods() {
			By("scraping " + name + ":" + metricsPort + "/metrics")
			var (
				body []byte
				code int
			)
			// Eventually: the first scrape after a roll can race the exporter's
			// listener; a persistent failure still goes red after the budget.
			Eventually(ctx, func() error {
				res := k8sClient.CoreV1().RESTClient().Get().
					Namespace(namespace).Resource("pods").Name(name + ":" + metricsPort).
					SubResource("proxy").Suffix("metrics").Do(ctx)
				res.StatusCode(&code)
				var err error
				body, err = res.Raw()
				return err
			}).WithTimeout(60*time.Second).WithPolling(3*time.Second).Should(Succeed(),
				"pod %s: GET /metrics through the pod proxy failed (HTTP %d)", name, code)
			Expect(code).To(Equal(200), "pod %s: HTTP %d", name, code)

			want := append([]string{}, dataSuffixes...)
			if isRWPod(name, rwPods) {
				want = append(want, logSuffixes...)
			}
			Expect(exporterContractProblems(parseExposition(body), want)).To(BeEmpty(),
				"pod %s: /metrics breaks the exporter contract (ADR-033 decision 4)", name)
		}
	})
})

// exporterContractProblems checks one pod's parsed /metrics against the
// curated families of ADR-033 decision 4 and returns what is wrong, one line
// per problem; empty means the scrape honours the contract. suffixes are the
// openldap_mdb_* suffixes this pod must carry. Pure, so the spec's whole
// judgement of a scrape is one function of its body.
func exporterContractProblems(samples []expoSample, suffixes []string) []string {
	var problems []string
	need := func(name, key, value, why string) {
		if !hasSample(samples, name, key, value) {
			if key == "" {
				problems = append(problems, fmt.Sprintf("no %s sample — %s", name, why))
			} else {
				problems = append(problems, fmt.Sprintf("no %s{%s=%q} sample — %s", name, key, value, why))
			}
		}
	}

	if ups := sampleValues(samples, "openldap_up"); len(ups) != 1 || ups[0] != "1" {
		problems = append(problems, fmt.Sprintf("openldap_up is %v, want exactly [1] — "+
			"the exporter could not read cn=Monitor", ups))
	}
	need("openldap_exporter_dials_total", "result", "ok", "the exporter never reached slapd")
	need("openldap_exporter_binds_total", "result", "ok", "the exporter's EXTERNAL bind never succeeded")

	// One representative per curated group. bind and search are certain: the
	// exporter's own scrape is a bind and a search, the readiness probe a bind.
	for _, fam := range []string{"openldap_operations_initiated_total", "openldap_operations_completed_total"} {
		need(fam, "op", "bind", "operations are labelled by lowercased op")
		need(fam, "op", "search", "operations are labelled by lowercased op")
	}
	for _, fam := range []string{
		"openldap_connections_total", "openldap_connections_current", "openldap_max_file_descriptors",
		"openldap_threads_open", "openldap_threads_active", "openldap_threads_max",
		"openldap_sent_bytes_total", "openldap_sent_entries_total",
		"openldap_start_time_seconds",
	} {
		need(fam, "", "", "a curated cn=Monitor family is missing")
	}
	need("openldap_waiters", "direction", "read", "waiters are labelled by direction")
	need("openldap_waiters", "direction", "write", "waiters are labelled by direction")

	for _, s := range samples {
		if _, ok := s.labels["dn"]; ok {
			problems = append(problems, fmt.Sprintf("%s carries a dn label — the upstream shape "+
				"ADR-033 decision 4 withdrew", s.name))
			break
		}
	}
	for _, old := range []string{
		"openldap_monitored_object", "openldap_monitor_counter_object", "openldap_monitor_operation",
		"openldap_dial", "openldap_bind",
	} {
		if hasSample(samples, old, "", "") {
			problems = append(problems, fmt.Sprintf("%s is still served — an upstream-shaped family", old))
		}
	}

	for _, fam := range []string{
		"openldap_mdb_pages_max", "openldap_mdb_pages_used", "openldap_mdb_pages_free",
		"openldap_mdb_readers_max", "openldap_mdb_readers_used", "openldap_mdb_entries",
	} {
		got := labelValues(samples, fam, "suffix")
		for _, suffix := range suffixes {
			if !slices.Contains(got, normalizeDN(suffix)) {
				problems = append(problems, fmt.Sprintf("%s has no sample for suffix %q (got %v)", fam, suffix, got))
			}
		}
	}
	return problems
}

func containerNames(pod *corev1.Pod) []string {
	var out []string
	for _, c := range pod.Spec.Containers {
		out = append(out, c.Name)
	}
	return out
}

func isRWPod(name string, rw []string) bool {
	for _, p := range rw {
		if p == name {
			return true
		}
	}
	return false
}

// normalizeDN lowercases a DN and drops the spaces after RDN separators, which
// is all the difference a CR's suffix and slapd's namingContexts ever show in
// this fixture. Label values are compared through the same function.
func normalizeDN(dn string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(dn)), ", ", ",")
}

// expoSample is one parsed line of the Prometheus text exposition format.
type expoSample struct {
	name   string
	labels map[string]string
	value  string
}

// parseExposition parses the text exposition format just far enough for these
// assertions: metric name, labels and the value as written. Label values are
// unescaped (\" \\ \n) and passed through normalizeDN so DN labels compare
// case-insensitively.
func parseExposition(body []byte) []expoSample {
	var out []expoSample
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		s := expoSample{labels: map[string]string{}}
		brace := strings.IndexByte(line, '{')
		space := strings.IndexByte(line, ' ')
		if brace < 0 || (space >= 0 && space < brace) {
			s.name = line[:max(space, 0)]
			if space >= 0 {
				s.value = strings.TrimSpace(line[space+1:])
			}
			out = append(out, s)
			continue
		}
		s.name = line[:brace]
		rest := line[brace+1:]
		for len(rest) > 0 && rest[0] != '}' {
			eq := strings.IndexByte(rest, '=')
			if eq < 0 || eq+1 >= len(rest) || rest[eq+1] != '"' {
				break
			}
			key := strings.TrimLeft(rest[:eq], ", ")
			var val strings.Builder
			i := eq + 2
			for ; i < len(rest) && rest[i] != '"'; i++ {
				if rest[i] == '\\' && i+1 < len(rest) {
					i++
					switch rest[i] {
					case 'n':
						val.WriteByte('\n')
					default:
						val.WriteByte(rest[i])
					}
					continue
				}
				val.WriteByte(rest[i])
			}
			s.labels[key] = normalizeDN(val.String())
			if i+1 > len(rest) {
				break
			}
			rest = strings.TrimLeft(rest[i+1:], ",")
		}
		if end := strings.LastIndexByte(line, '}'); end >= 0 {
			s.value = strings.TrimSpace(line[end+1:])
		}
		out = append(out, s)
	}
	return out
}

// hasSample reports a sample of family name — exactly that name, so a counter
// is asked for with its _total — whose label key equals value. An empty key
// asks only for the family.
func hasSample(samples []expoSample, name, key, value string) bool {
	for _, s := range samples {
		if s.name == name && (key == "" || s.labels[key] == value) {
			return true
		}
	}
	return false
}

// sampleValues returns the value of every sample of family name, as written.
func sampleValues(samples []expoSample, name string) []string {
	var out []string
	for _, s := range samples {
		if s.name == name {
			out = append(out, s.value)
		}
	}
	return out
}

// labelValues returns every value label key takes on family name.
func labelValues(samples []expoSample, name, key string) []string {
	var out []string
	for _, s := range samples {
		if s.name == name {
			if v, ok := s.labels[key]; ok {
				out = append(out, v)
			}
		}
	}
	return out
}
