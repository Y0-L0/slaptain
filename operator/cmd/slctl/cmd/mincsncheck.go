package cmd

import (
	"fmt"
	"sort"
	"strings"
)

// logCSNReading is one accesslog database's minCSN and contextCSN as one pod
// shows them, read as cn=admin,cn=config (ADR-020 denies everyone else).
type logCSNReading struct {
	Pod        string
	Log        string // the accesslog suffix, cn=accesslog-<db>
	MinCSN     []string
	ContextCSN []string
	// Err is non-empty when the entry could not be read; the CSN fields are
	// then meaningless.
	Err string
}

// checkAccesslogMinCSN is the ITS#9398 storm detector
// (docs/ITS-9398-STORM-SIGNATURE.md): on an accesslog database, a minCSN sid
// that contextCSN does not track makes syncprov refuse EVERY consumer of that
// log with "sync cookie is stale", and the refused consumer full-refreshes and
// comes back with the same cookie. The contradiction is local to one entry, so
// no cross-pod comparison is needed. The reverse direction (contextCSN tracks
// more sids than minCSN) is benign and common.
//
//	fail — some pod's log carries a minCSN sid absent from its contextCSN.
//	warn — no storm seen, but a log could not be read: unreadable evidence
//	       never counts toward pass.
//	pass — every log read, every minCSN sid tracked.
//
// A firing sample is conclusive. A clean one means clean now: the condition
// has been seen to clear by itself.
func checkAccesslogMinCSN(readings []logCSNReading) checkResult {
	const name = "accesslog-mincsn"
	if len(readings) == 0 {
		return checkResult{Name: name, Status: checkPass, Detail: "no accesslog databases"}
	}

	var storms, unread []string
	for _, r := range readings {
		if r.Err != "" {
			unread = append(unread, fmt.Sprintf("%s %s (%s)", r.Pod, r.Log, r.Err))
			continue
		}
		tracked := map[string]bool{}
		for _, c := range r.ContextCSN {
			tracked[csnSIDField(c)] = true
		}
		var missing []string
		for _, c := range r.MinCSN {
			if sid := csnSIDField(c); !tracked[sid] {
				missing = append(missing, sid)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			storms = append(storms, fmt.Sprintf("%s %s: minCSN sid %s not in contextCSN",
				r.Pod, r.Log, strings.Join(missing, ",")))
		}
	}

	var parts []string
	if len(storms) > 0 {
		parts = append(parts, strings.Join(storms, "; ")+
			" — ITS#9398: that pod refuses every consumer of the log with 'sync cookie is stale'"+
			" (docs/ITS-9398-STORM-SIGNATURE.md)")
	}
	if len(unread) > 0 {
		parts = append(parts, "not read: "+strings.Join(unread, "; "))
	}
	switch {
	case len(storms) > 0:
		return checkResult{Name: name, Status: checkFail, Detail: strings.Join(parts, "; ")}
	case len(unread) > 0:
		return checkResult{Name: name, Status: "warn", Detail: strings.Join(parts, "; ")}
	}
	return checkResult{Name: name, Status: checkPass,
		Detail: fmt.Sprintf("%d accesslog readings: every minCSN sid tracked in contextCSN", len(readings))}
}

// csnSIDField returns a CSN's sid field (YYYYMMDDHHMMSS.µsZ#count#sid#mod),
// lowercased because it is hex. A value that does not parse is returned whole,
// so it can never match a tracked sid and surfaces in the detail as-is.
func csnSIDField(csn string) string {
	parts := strings.Split(csn, "#")
	if len(parts) < 4 {
		return csn
	}
	return strings.ToLower(parts[2])
}

// readLogCSNs completes a pod's accesslog readings: every accesslog naming
// context the pod serves that the config-bound pass did not read (no config
// password, a failed bind) gets a reading carrying why, so the check can say
// "not read" instead of mistaking the gap for a clean pod.
func readLogCSNs(pod string, namingContexts []string, read []logCSNReading, configError string) []logCSNReading {
	seen := map[string]bool{}
	for _, r := range read {
		seen[r.Log] = true
	}
	why := configError
	if why == "" {
		why = "cn=config not queried"
	}
	for _, nc := range namingContexts {
		if isAnyAccesslogSuffix(nc) && !seen[nc] {
			read = append(read, logCSNReading{Pod: pod, Log: nc, Err: why})
		}
	}
	return read
}
