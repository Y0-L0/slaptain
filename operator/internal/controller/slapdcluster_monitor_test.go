package controller

import (
	"strings"
	"testing"
)

// ADR-033 amendment (2026-10-08): the monitor database is R1 and always
// present. These pin the pure half — what gets written and when. The property
// the milestone exists to prove is the steady state: a pod that already carries
// the monitor database with the constant ACL gets ZERO writes, because every
// cn=config write is one the timeout= freeze class turned into a server-wide
// pause (48f688d), and the withdrawn implementation rewrote this ACL on every
// database add or remove.

// The ACL is pinned verbatim: it was measured live (slapd stores it unchanged
// apart from the {0} prefix), and a reworded-but-equivalent rule would no
// longer be the string that measurement covered.
func TestMonitorDBACLIsTheMeasuredConstant(t *testing.T) {
	const want = `to * by dn.children="cn=slaptain-auth" read` +
		` by dn.children="cn=peercred,cn=external,cn=auth" read by * none`
	if monitorDBACL != want {
		t.Errorf("monitorDBACL =\n  %q\nwant (ADR-033 amendment, verbatim)\n  %q", monitorDBACL, want)
	}
	// Separately named so a failure says which property broke, not just "differs".
	if !strings.HasSuffix(monitorDBACL, " by * none") {
		t.Errorf("the monitor tree must end closed — it lists the DN of every current bind: %q", monitorDBACL)
	}
	if strings.Contains(monitorDBACL, "cn=replication,") {
		t.Errorf("the legacy cn=replication,<suffix> identity must not be granted — "+
			"the monitor tree is not part of ADR-027's migration window: %q", monitorDBACL)
	}
	if strings.Contains(monitorDBACL, "gidNumber=") || strings.Contains(monitorDBACL, "uidNumber=") {
		t.Errorf("the ldapi reader must be UID-agnostic (dn.children on the peercred base), "+
			"not pinned to one uid/gid: %q", monitorDBACL)
	}
}

func TestPlanMonitor(t *testing.T) {
	const dbDN = "olcDatabase={6}monitor,cn=config"
	stored := []string{"{0}" + monitorDBACL}

	for _, tc := range []struct {
		name string
		dbDN string
		acl  []string
		want monitorPlan
	}{
		{
			name: "a pod with no monitor database gets it added",
			want: monitorPlan{addDB: true},
		},
		{
			name: "the ADD carries the ACL, so adding never also replaces it",
			acl:  []string{"{0}to * by * read"}, // ignored: no database to carry it
			want: monitorPlan{addDB: true},
		},
		{
			name: "steady state — database present, ACL stored with its {0} prefix — writes nothing",
			dbDN: dbDN,
			acl:  stored,
			want: monitorPlan{},
		},
		{
			name: "a hand-edited ACL is replaced, and nothing else is written",
			dbDN: dbDN,
			acl:  []string{"{0}to * by * read"},
			want: monitorPlan{replaceACL: true},
		},
		{
			name: "the withdrawn per-database ACL is replaced by the constant",
			dbDN: dbDN,
			acl:  []string{`{0}to * by dn.exact="cn=replication,dc=example,dc=org" read by * none`},
			want: monitorPlan{replaceACL: true},
		},
		{
			name: "a database with no olcAccess at all gets the ACL",
			dbDN: dbDN,
			acl:  nil,
			want: monitorPlan{replaceACL: true},
		},
		{
			name: "the right rule plus a hand-added second rule is not a match",
			dbDN: dbDN,
			acl:  []string{"{0}" + monitorDBACL, "{1}to * by * read"},
			want: monitorPlan{replaceACL: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := planMonitor(tc.dbDN, tc.acl); got != tc.want {
				t.Errorf("planMonitor(%q, %v) = %+v, want %+v", tc.dbDN, tc.acl, got, tc.want)
			}
		})
	}
}

// The ADD is the only write on the add path, so everything the database needs
// — including its ACL — must be in it (ADR-033 amendment: olcRootDN
// cn=admin,cn=config; the constant ACL).
func TestMonitorDBAddRequest(t *testing.T) {
	req := monitorDBAddRequest()
	if req == nil {
		t.Fatal("monitorDBAddRequest returned nil")
	}
	if req.DN != "olcDatabase=monitor,cn=config" {
		t.Errorf("DN = %q, want olcDatabase=monitor,cn=config (slapd assigns the {N})", req.DN)
	}
	attrs := map[string][]string{}
	for _, a := range req.Attributes {
		attrs[strings.ToLower(a.Type)] = a.Vals
	}

	hasOC := map[string]bool{}
	for _, v := range attrs["objectclass"] {
		hasOC[strings.ToLower(v)] = true
	}
	for _, oc := range []string{"olcdatabaseconfig", "olcmonitorconfig"} {
		if !hasOC[oc] {
			t.Errorf("objectClass %v lacks %s — (objectClass=olcMonitorConfig) is how the "+
				"next pass finds this database again", attrs["objectclass"], oc)
		}
	}
	if got := attrs["olcdatabase"]; len(got) != 1 || got[0] != "monitor" {
		t.Errorf("olcDatabase = %v, want [monitor]", got)
	}
	if got := attrs["olcrootdn"]; len(got) != 1 || got[0] != "cn=admin,cn=config" {
		t.Errorf("olcRootDN = %v, want [cn=admin,cn=config]", got)
	}
	if got := attrs["olcaccess"]; !aclsMatch(got, []string{monitorDBACL}) {
		t.Errorf("olcAccess = %v, want exactly the constant monitorDBACL (with or without {0}) — "+
			"otherwise the very next pass plans a replace", got)
	}
}
