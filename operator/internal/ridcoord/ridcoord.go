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

// Package ridcoord derives syncrepl replica IDs as coordinates in a static
// matrix (ADR-031 decision 1). Pure: no Kubernetes types, no I/O.
//
//	rid = 1 + column·M + offset              M = P + S
//
//	offset <  P   →  a provider at MY OWN site, at pod ordinal `offset`
//	offset >= P   →  MY LINK to peer site `offset − P`
//
// `column` names the database and is the only allocated term in the scheme
// (ADR-031 decision 3); everything else is declared identity, so a coordinate
// has nothing to go stale about. `rid = 0` is reserved and never produced:
// zero is what a forgotten int32 assignment looks like, and slapd itself uses
// connid 0 for its own outbound client connections.
//
// This is the single implementation of the formula. Encoding it twice is
// ADR-031's first hazard — every caller, in the operator and in slctl, goes
// through Local, Remote and Decode.
package ridcoord

import (
	"errors"
	"fmt"
)

// MaxRID is slapd's ceiling on a syncrepl replica ID (SLAP_SYNC_RID_MAX): the
// RID band is carved out of the bottom of the connection-ID space, so 999 is a
// hard limit of the server and not a convention of ours.
const MaxRID = 999

// Refusal sentinels. Callers select with errors.Is; the message text is for
// humans and names the axis that failed together with its ceiling, because
// three refusals mean three different human responses.
var (
	ErrInvalidLayout       = errors.New("invalid RID layout")
	ErrColumnOutOfRange    = errors.New("RID column out of range")
	ErrOrdinalOutOfRange   = errors.New("RID pod ordinal out of range")
	ErrSiteIndexOutOfRange = errors.New("RID site index out of range")
	ErrRIDOutOfRange       = errors.New("RID out of range")
)

// Layout is the shape of the RID matrix. It is mesh-scoped (ADR-031 decision
// 5): every site must compute the same number for the same provider, so the
// layout is one value that cannot differ per site.
type Layout struct {
	PodsPerSite int32 // P — max pods at any one site
	MaxSites    int32 // S — max sites in the mesh
}

// DefaultLayout is P=8, S=12 → M=20, C=49. The defaults of
// SlapdMesh.spec.ridLayout, and what a cluster with no meshRef uses.
func DefaultLayout() Layout {
	return Layout{PodsPerSite: 8, MaxSites: 12}
}

// Validate reports whether the layout can produce at least one usable column:
// both axes at least 1, and a column width that leaves room for a column
// inside the 0..999 band. Returns an error wrapping ErrInvalidLayout.
func (l Layout) Validate() error {
	return nil
}

// ColumnWidth is M = P + S. Zero for a layout whose axes are not positive.
func (l Layout) ColumnWidth() int32 {
	return 0
}

// MaxColumns is C = floor(999 / M) — the number of databases the layout
// admits. It is 999 and not 1000 because rid = 0 is reserved, which costs
// exactly one column. Zero for an invalid layout; never divides by zero.
func (l Layout) MaxColumns() int32 {
	return 0
}

// Local is the RID of the provider at ordinal `ordinal` on this pod's own
// site, for the database holding `column`.
//
// Refuses with ErrInvalidLayout, ErrColumnOutOfRange (0 <= column < C) or
// ErrOrdinalOutOfRange (0 <= ordinal < P). Never wraps, never collides.
func (l Layout) Local(column, ordinal int32) (int32, error) {
	return 0, nil
}

// Remote is the RID of this pod's link to peer site `siteIndex`, for the
// database holding `column`. A pod holds at most one link per peer site
// (ADR-031 decision 9), which is what makes the remote block S wide.
//
// Refuses with ErrInvalidLayout, ErrColumnOutOfRange (0 <= column < C) or
// ErrSiteIndexOutOfRange (0 <= siteIndex < S).
func (l Layout) Remote(column, siteIndex int32) (int32, error) {
	return 0, nil
}

// Kind distinguishes the two blocks of a column.
type Kind int

// The enumeration starts at 1, so the zero Kind is not a Kind at all. This is
// the same reservation rid = 0 gets and for the same reason: zero is what a
// forgotten assignment produces, and an enumeration whose zero value means
// "local provider" cannot tell a decoded coordinate from an unset struct.
const (
	// KindLocal — a provider at the reader's own site, named by pod ordinal.
	KindLocal Kind = iota + 1
	// KindRemote — a link to a peer site, named by that site's index.
	KindRemote
)

// String renders a Kind for log lines and failure messages.
func (k Kind) String() string {
	switch k {
	case KindLocal:
		return "local"
	case KindRemote:
		return "remote"
	default:
		return fmt.Sprintf("Kind(%d)", int(k))
	}
}

// Coordinate is a decoded RID. Ordinal is meaningful only for KindLocal and
// SiteIndex only for KindRemote; the field that does not apply is zero.
//
// Decode always returns a valid Kind — KindLocal or KindRemote, never the zero
// value. A Coordinate carrying Kind(0) was never produced by this package.
type Coordinate struct {
	Column    int32
	Kind      Kind
	Ordinal   int32
	SiteIndex int32
}

// Decode inverts Local and Remote: it reports which column the RID belongs to,
// which of the two blocks the offset fell in, and the index within that block.
//
// Refuses with ErrInvalidLayout, or ErrRIDOutOfRange for anything below 1 or
// above the last RID the layout can produce.
func (l Layout) Decode(rid int32) (Coordinate, error) {
	return Coordinate{}, nil
}
