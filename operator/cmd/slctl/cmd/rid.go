package cmd

import (
	"github.com/spf13/cobra"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
	"github.com/chuck-chuck-chuck-net/slaptain/operator/internal/ridcoord"
)

// `slctl rid decode <rid>` — reading a syncrepl replica ID back out of a slapd
// log line (ADR-031, RID-PLAN M6).
//
// The whole value of ADR-031 decision 1 is that a RID stopped being an opaque
// address and became a coordinate: `rid = 1 + segment·M + offset`, with
// `segment` naming the database and `offset` naming either a pod at the
// reader's own site (`offset < P`) or a link to a peer site (`offset >= P`,
// index `offset − P`). Every term is declared or structural, so the number is
// self-describing. This command is where a human cashes that in.
//
// Three properties shape the surface, in this order:
//
//  1. **It works with no cluster.** The main user is someone reading a log on a
//     laptop. Decoding is arithmetic against a layout, and the default layout
//     is a compiled-in constant, so the offline path needs no I/O and is not a
//     degraded mode — it is the base case.
//  2. **It does better with a cluster.** The segment maps to the SlapdDatabase
//     that declares it and the site index to the site that declares it, both
//     read from CRs, and the layout comes from the mesh actually in force
//     rather than from the defaults.
//  3. **It never guesses.** Anything it could not resolve is said in words —
//     "no SlapdDatabase ... declares ridSegment 15" — and never left as
//     silence, which would read identically to "resolved to nothing".
//
// The one rule that separates this from the operator: a RID inside slapd's
// 0..999 band but outside the matrix the layout can produce is NOT an error
// here. ridcoord.Decode refuses it, which is right for an operator that must
// not issue such a number, and wrong for a diagnostic whose user took the
// number off a real stanza — very likely one written before the ADR-031
// renumber. The answer owed is "inside slapd's band, outside this mesh's
// matrix", with both ceilings named (ADR-031 Consequences, final slctl
// bullet; RID-PLAN M6).

// ridDecodeStatus is what the decode could make of the number. It is a status
// and not an error because a diagnostic always answers; the four non-OK values
// are four different sentences a human needs, not four failures.
type ridDecodeStatus string

const (
	// ridDecodeOK — the RID is a coordinate in this layout's matrix.
	ridDecodeOK ridDecodeStatus = "ok"
	// ridDecodeReserved — rid 0, which ADR-031 decision 1 reserves and never
	// allocates. A zero is a bug somewhere, never a provider.
	ridDecodeReserved ridDecodeStatus = "reserved"
	// ridDecodeOutsideMatrix — inside slapd's 0..999 band, above the largest
	// RID this layout produces (C·M). A legal syncrepl id that this mesh never
	// issued: a pre-ADR-031 stanza, another tool, or a hand edit.
	ridDecodeOutsideMatrix ridDecodeStatus = "outside-matrix"
	// ridDecodeOutsideBand — below 0 or above MaxRID. Not a syncrepl replica
	// ID at all; slapd itself would refuse it.
	ridDecodeOutsideBand ridDecodeStatus = "outside-band"
	// ridDecodeLayoutUnusable — the layout cannot produce any segment, so the
	// number cannot be decoded against anything. The fault is the layout, and
	// saying so beats reporting the RID as out of range.
	ridDecodeLayoutUnusable ridDecodeStatus = "layout-unusable"
)

// ridResolution is the naming context a decode resolves against: the layout in
// force, where it came from and whether that provenance can be trusted, plus
// whatever the cluster declares about segments, sites and pods.
//
// Every map is allowed to be nil. A nil map means "not consulted", which the
// decode reports differently from "consulted and found nothing" — the
// distinction property 3 above exists for.
type ridResolution struct {
	// Layout is the matrix shape the number is decoded against.
	Layout ridcoord.Layout
	// LayoutSource is human-readable provenance, printed verbatim: the default,
	// a named SlapdMesh, or an explicit --layout.
	LayoutSource string
	// LayoutTrusted is false when the layout shown is a fallback standing in
	// for one that could not be read — a cluster naming a SlapdMesh that is not
	// readable. The arithmetic still runs, because a best-effort decode beats
	// no decode, but the result says so.
	LayoutTrusted bool

	// ClusterName is the SlapdCluster consulted, empty when none was.
	ClusterName string
	// Replicas is that cluster's spec.replicas, used only to notice that a
	// decoded local ordinal names no pod it currently runs.
	Replicas int32
	// MeshName is the SlapdMesh consulted, empty when none was.
	MeshName string

	// Databases maps a declared ridSegment to the SlapdDatabase names declaring
	// it. A slice, not a string, because two databases on one segment is a real
	// state (ADR-031 decision 3 refuses it in the operator) and a decode that
	// could only show one would hide exactly the collision a human is hunting.
	// Nil means no cluster was consulted.
	Databases map[int32][]string
	// Sites maps a declared serverIDIndex to the site name declaring it
	// (ADR-028). Nil means no mesh was consulted.
	Sites map[int32]string
}

// ridDecodeResult is one decoded number, in full. Pointer fields are nil when
// the term does not apply: Segment for anything but a decoded coordinate,
// Ordinal for a remote one, SiteIndex for a local one.
type ridDecodeResult struct {
	RID    int32
	Status ridDecodeStatus
	// Layout is the layout the decode actually used, echoed so the output can
	// state it without the caller carrying the resolution alongside.
	Layout       ridcoord.Layout
	LayoutSource string

	Segment   *int32
	Kind      ridcoord.Kind // the zero Kind when Status is not ridDecodeOK
	Ordinal   *int32
	SiteIndex *int32

	// Databases names every SlapdDatabase declaring Segment — empty when none
	// does, or when no cluster was consulted. Notes distinguishes the two.
	Databases []string
	// Pod is the pod a local coordinate names (`<cluster>-<ordinal>`), empty
	// when no cluster was consulted.
	Pod string
	// Site is the site a remote coordinate names, empty when no mesh was
	// consulted or no site declares the index.
	Site string

	// Summary is the one sentence that answers the question the user asked.
	// Never empty, for any status.
	Summary string
	// Notes carries everything that could not be resolved, each as a complete
	// sentence naming what was looked for and where.
	Notes []string
}

// ptrI32 is the int32 counterpart of ptrI64, for the optional terms of a
// decoded coordinate.
func ptrI32(v int32) *int32 { return &v }

// decodeRID is the whole decision, pure.
//
// It never returns an error: the four non-OK statuses are the answers for the
// four ways a number can fail to be a coordinate, and each carries a Summary
// saying which. In particular a RID in (C·M, MaxRID] yields
// ridDecodeOutsideMatrix with both ceilings named — ridcoord.Decode's refusal
// is consumed here, not surfaced.
func decodeRID(rid int32, res ridResolution) ridDecodeResult {
	_ = rid
	_ = res
	return ridDecodeResult{}
}

// parseRIDLayoutFlag parses the --layout value, `<podsPerSite>x<maxSites>` —
// `8x12` being the default layout. The order is pods first, sites second,
// matching ADR-031 decision 5's own table and the `M = P + S` formula.
//
// A syntactically well-formed layout that cannot produce a segment is returned
// as-is with a nil error: it is the decode's job to report an unusable layout
// as ridDecodeLayoutUnusable, so that the message names the arithmetic rather
// than the flag. Only a value that is not two numbers is a parse error.
func parseRIDLayoutFlag(s string) (ridcoord.Layout, error) {
	_ = s
	return ridcoord.Layout{}, nil
}

// buildRIDResolution assembles the naming context from what could be read.
//
// All three inputs are optional, and the shape of the answer follows what was
// supplied:
//
//   - sc nil: the offline base case. Default layout, trusted, no maps.
//   - sc without meshRef: default layout, trusted, databases resolved.
//   - sc with meshRef, mesh non-nil: the mesh's spec.ridLayout, defaulted PER
//     AXIS exactly as the operator's own ridLayoutFor does — a zero axis is
//     unset, not unusable, because API-server defaulting does not run on a
//     manifest read off disk.
//   - sc with meshRef, mesh nil: the mesh could not be read. The default layout
//     stands in, LayoutTrusted is false, and the decode says so. The operator
//     withholds here; a diagnostic answers and labels the answer.
//
// dbs are filtered to those naming sc, and only those declaring a ridSegment.
// They are ignored entirely when sc is nil, since clusterRef cannot be matched
// against anything.
func buildRIDResolution(sc *ldapv1alpha1.SlapdCluster, mesh *ldapv1alpha1.SlapdMesh, dbs []ldapv1alpha1.SlapdDatabase) ridResolution {
	_, _, _ = sc, mesh, dbs
	return ridResolution{}
}

// renderRIDDecodeText renders the human form: the number, the layout it was
// read against, the terms it decomposes into, and the notes. It is the text a
// person reads at 3am, so every resolved name appears next to the number it
// resolves.
func renderRIDDecodeText(r ridDecodeResult) string {
	_ = r
	return ""
}

// ridLayoutJSON is the layout as emitted, with the two derived quantities
// included so a consumer need not re-implement the formula to know where the
// matrix ends.
type ridLayoutJSON struct {
	PodsPerSite  int32  `json:"podsPerSite"`
	MaxSites     int32  `json:"maxSites"`
	SegmentWidth int32  `json:"segmentWidth"`
	MaxSegments  int32  `json:"maxSegments"`
	Source       string `json:"source"`
	Trusted      bool   `json:"trusted"`
}

// ridDecodeJSON is the --json form of a decode.
type ridDecodeJSON struct {
	RID       int32         `json:"rid"`
	Status    string        `json:"status"`
	Layout    ridLayoutJSON `json:"layout"`
	Segment   *int32        `json:"segment,omitempty"`
	Kind      string        `json:"kind,omitempty"`
	Ordinal   *int32        `json:"ordinal,omitempty"`
	SiteIndex *int32        `json:"siteIndex,omitempty"`
	Databases []string      `json:"databases,omitempty"`
	Pod       string        `json:"pod,omitempty"`
	Site      string        `json:"site,omitempty"`
	Summary   string        `json:"summary"`
	Notes     []string      `json:"notes,omitempty"`
}

// buildRIDDecodeJSON projects a result onto the emitted form.
func buildRIDDecodeJSON(r ridDecodeResult, trusted bool) ridDecodeJSON {
	_, _ = r, trusted
	return ridDecodeJSON{}
}

var (
	ridLayoutFlag  string
	ridOfflineFlag bool
	ridClusterFlag string
)

var ridCmd = &cobra.Command{
	Use:   "rid",
	Short: "Work with syncrepl replica IDs",
	Long:  "Commands for reading and checking syncrepl replica IDs (RIDs), which ADR-031 makes coordinates in a (database, provider) matrix.",
}

var ridDecodeCmd = &cobra.Command{
	Use:   "decode <rid>",
	Short: "Decode a syncrepl RID into (database, provider)",
	Long: "Decode a syncrepl replica ID into the database segment and the provider it names.\n\n" +
		"Works offline against the default RID layout (8x12), which is all a RID needs to be\n" +
		"decoded. When a cluster is reachable, the segment is resolved to the SlapdDatabase\n" +
		"declaring it, the peer site index to the site declaring it, and the layout is read\n" +
		"from the SlapdMesh actually in force.",
	Args: cobra.ExactArgs(1),
	RunE: runRIDDecode,
}

func init() {
	ridCmd.AddCommand(ridDecodeCmd)
	rootCmd.AddCommand(ridCmd)

	ridDecodeCmd.Flags().StringVar(&ridLayoutFlag, "layout", "",
		"RID layout to decode against, as <podsPerSite>x<maxSites> (default: the mesh's, else 8x12)")
	ridDecodeCmd.Flags().BoolVar(&ridOfflineFlag, "offline", false,
		"do not contact a cluster; decode arithmetically only")
	ridDecodeCmd.Flags().StringVar(&ridClusterFlag, "cluster", "",
		"SlapdCluster to resolve segments and sites against (only needed when the namespace holds more than one)")
}

func runRIDDecode(cmd *cobra.Command, args []string) error {
	_, _ = cmd, args
	return nil
}
