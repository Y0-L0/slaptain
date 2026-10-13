package controller

import (
	ldap "github.com/go-ldap/ldap/v3"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The monitor backend (ADR-033 amendment 2026-10-08): the monitor database
// present and its ACL converged on every pod, RW and RO
// alike, every pass (ADR-024 R1). No switch: there is no off-transition, so
// there is nothing to get wrong about one.
//
// This file holds the pure half — what to write and whether to write it. The
// LDAP half runs from reconcilePodInfrastructure and only executes what
// planMonitor decides.

// monitorDBACL is the single olcAccess rule on the monitor database. It is a
// CONSTANT, independent of the database set: that is what makes its modify
// fire only after a hand edit or an upgrade that changes this value, never on
// a database add or remove (the withdrawn per-database monitorACL did).
//
//   - dn.children of the auth suffix: every node-local replication identity
//     (ADR-027) can read, so ADR-008's reuse survives without enumerating
//     databases. The legacy cn=replication,<suffix> is deliberately NOT
//     granted.
//   - dn.children of the peercred base: any local-socket peer, whatever its
//     UID/GID — the future sidecar's reader. No olcAuthzRegexp needed.
//   - by * none: anonymous and every other identity get "No such object".
//
// The rootDN (cn=admin,cn=config) bypasses it. Measured live: slapd stores it
// unchanged apart from the {0} ordering prefix, so aclsMatch matches it.
const monitorDBACL = `to * by dn.children="` + ldapv1alpha1.AuthSuffix + `" read` +
	` by dn.children="cn=peercred,cn=external,cn=auth" read` +
	` by * none`

// monitorPlan is what one pod needs written to reach the desired monitor
// state. The zero value is the steady state: nothing to write.
type monitorPlan struct {
	// addDB: add the monitor database (monitorDBAddRequest), which carries the
	// ACL in the ADD itself.
	addDB bool
	// replaceACL: replace the existing monitor database's olcAccess with
	// monitorDBACL.
	replaceACL bool
}

// planMonitor decides, without I/O, what a pod needs for the monitor backend.
//
// back_monitor is a STATIC backend in every slaptain image (slapd -VVV, 2.7.1
// and -ol26), so nothing loads a module: an olcModuleLoad of it is a no-op
// (ADR-033). The plan is about the database and its ACL only.
//
//   - monitorDBDN: the monitor database's DN as found by
//     (objectClass=olcMonitorConfig), or "" when the pod has none.
//   - currentACL: that database's olcAccess values as stored; ignored when
//     monitorDBDN is "".
//
// Contract:
//   - no monitor database → addDB, never replaceACL alongside it: the ADD
//     carries the ACL, so a separate modify would be a second cn=config write
//     for nothing.
//   - monitor database present → never addDB; replaceACL exactly when
//     currentACL does not match monitorDBACL by aclsMatch.
func planMonitor(monitorDBDN string, currentACL []string) monitorPlan {
	if monitorDBDN == "" {
		return monitorPlan{addDB: true}
	}
	return monitorPlan{replaceACL: !aclsMatch(currentACL, []string{monitorDBACL})}
}

// monitorDBAddRequest is the ADD that creates the monitor database:
// objectClass olcDatabaseConfig + olcMonitorConfig, olcDatabase "monitor",
// olcRootDN cn=admin,cn=config (cn=monitor is server-scope and has no data
// admin of its own), and olcAccess = monitorDBACL as its only rule. The DN is
// olcDatabase=monitor,cn=config; slapd assigns the {N}.
func monitorDBAddRequest() *ldap.AddRequest {
	req := ldap.NewAddRequest("olcDatabase=monitor,cn=config", nil)
	req.Attribute("objectClass", []string{"olcDatabaseConfig", "olcMonitorConfig"})
	req.Attribute("olcDatabase", []string{"monitor"})
	req.Attribute("olcRootDN", []string{"cn=admin,cn=config"})
	req.Attribute("olcAccess", []string{"{0}" + monitorDBACL})
	return req
}
