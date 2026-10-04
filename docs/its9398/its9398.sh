#!/usr/bin/env bash
# ITS#9398 on a slaptain mesh: diagnose, heal, reproduce.
# Background: docs/ITS-9398-REPRODUCTION.md, docs/ITS-9398-HANDLING.md
#
#   diagnose   per pod × database, is  minCSN ⊆ contextCSN  on the accesslog DB?
#   heal       one write from each serverID the broken log DB is missing
#   break      force-delete a pod (SIGKILL) and show the storm it causes
#   break --graceful   the control arm: plain delete, expected clean
#
# Pass --context once per SITE. The mesh-wide map matters: the serverID a
# broken pod is missing frequently lives at ANOTHER site, and healing from the
# local site then does nothing. That is not hypothetical — it is what happened
# on the first live run (see the doc).
#
# Examples
#   ./its9398.sh diagnose --context <ctx-1> --context <ctx-2> --context <ctx-3>
#   ./its9398.sh heal     --context <ctx-1> --context <ctx-2> --context <ctx-3>
#   ./its9398.sh break    --context <ctx-1> --target 0 --yes
set -euo pipefail

CONTEXTS=() ; NAMESPACE="slaptain-testing" ; CLUSTER="slapd"
TARGET=0 ; OBSERVE=60 ; KILL=brutal ; ASSUME_YES=0 ; ALLOW_PRIV=0 ; PRIV_LABELED=0
MODE="${1:-diagnose}" ; [ $# -gt 0 ] && shift || true

die(){ echo "error: $*" >&2; exit 1; }
note(){ printf '\n\033[1m== %s\033[0m\n' "$*"; }

while [ $# -gt 0 ]; do
  case "$1" in
    --context)   CONTEXTS+=("$2"); shift 2;;
    --namespace|-n) NAMESPACE="$2"; shift 2;;
    --cluster)   CLUSTER="$2"; shift 2;;
    --target)    TARGET="$2"; shift 2;;
    --observe)   OBSERVE="$2"; shift 2;;
    --kill)      KILL="$2"; shift 2;;
    --allow-privileged) ALLOW_PRIV=1; shift;;
    --graceful)  KILL=graceful; shift;;
    --yes|-y)    ASSUME_YES=1; shift;;
    -h|--help)   sed -n '2,22p' "$0"; exit 0;;
    *) die "unknown argument: $1";;
  esac
done
[ "${#CONTEXTS[@]}" -gt 0 ] || die "give --context at least once (refusing to guess)"
command -v slctl >/dev/null || die "slctl not found in PATH"
SITE="${CONTEXTS[0]}"

kc(){ kubectl --context "$1" -n "$NAMESPACE" "${@:2}"; }
sc(){ slctl --context "$1" -n "$NAMESPACE" --cluster "$CLUSTER" --database "$2" "${@:3}"; }

databases(){ kc "$1" get slapddatabase -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' \
             2>/dev/null | grep -v '^$' || true; }
rwpods(){ kc "$1" get pod -l "app.kubernetes.io/instance=$CLUSTER" \
          -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -v '^$' || true; }

# ---- the mesh-wide serverID map: SID -> "context pod" --------------------
declare -A SID_AT
build_sid_map(){
  local c p db sid
  note "Mapping serverIDs across ${#CONTEXTS[@]} site(s)"
  for c in "${CONTEXTS[@]}"; do
    db=$(databases "$c" | head -1); [ -n "$db" ] || continue
    for p in $(rwpods "$c"); do
      sid=$(sc "$c" "$db" ldapsearch --as config --pod "${p##*-}" \
             -b cn=config '(olcServerID=*)' olcServerID 2>/dev/null \
             | awk '/^olcServerID:/{print $2; exit}')
      [ -n "$sid" ] || continue
      SID_AT["$sid"]="$c $p"
      printf '  SID %-5s (hex %03x)  %s/%s\n' "$sid" "$sid" "$c" "$p"
    done
  done
}

# ---- diagnosis: minCSN \ contextCSN, per pod per database ----------------
# Returns the missing SIDs (decimal) on stdout; empty means healthy.
missing_sids(){ # $1 ctx  $2 pod  $3 database
  local out mins ctxs
  out=$(sc "$1" "$3" ldapsearch --as config --pod "${2##*-}" \
        -b "cn=accesslog-$3" -s base contextCSN minCSN 2>/dev/null) || return 0
  mins=$(grep -i '^minCSN:'     <<<"$out" | awk -F'#' '{print $3}' | sort -u)
  ctxs=$(grep -i '^contextCSN:' <<<"$out" | awk -F'#' '{print $3}' | sort -u)
  # hex in the CSN -> decimal, which is what olcServerID uses
  comm -23 <(echo "$mins") <(echo "$ctxs") | while read -r h; do
    [ -n "$h" ] && printf '%d\n' "0x$h"
  done
}

MISSING_REPORT=()
diagnose(){
  local c p db m bad=0
  note "Diagnosis: is  minCSN ⊆ contextCSN  on every accesslog DB?"
  printf '%-10s %-18s %-16s %s\n' SITE POD DATABASE 'MISSING serverIDs'
  for c in "${CONTEXTS[@]}"; do
    for db in $(databases "$c"); do
      for p in $(rwpods "$c"); do
        m=$(missing_sids "$c" "$p" "$db" | tr '\n' ' ' | sed 's/ $//')
        if [ -n "$m" ]; then
          bad=1
          printf '%-10s %-18s %-16s \033[1m%s\033[0m\n' "$c" "$p" "$db" "$m"
          MISSING_REPORT+=("$c|$p|$db|$m")
        else
          printf '%-10s %-18s %-16s ok\n' "$c" "$p" "$db"
        fi
      done
    done
  done
  return $bad
}

heal(){
  local entry c p db m sid where wc wp suffix
  diagnose || true
  [ "${#MISSING_REPORT[@]}" -gt 0 ] || { note "Nothing to heal"; return 0; }
  build_sid_map
  note "Healing: one write per missing serverID, into the database that lacks it"
  for entry in "${MISSING_REPORT[@]}"; do
    IFS='|' read -r c p db m <<<"$entry"
    suffix=$(kc "$c" get slapddatabase "$db" -o jsonpath='{.spec.suffix}')
    for sid in $m; do
      where="${SID_AT[$sid]:-}"
      if [ -z "$where" ]; then
        echo "  !! $db on $c/$p is missing SID $sid, and no --context given owns it."
        echo "     Add that site's --context, or write to $suffix from it by hand."
        continue
      fi
      read -r wc wp <<<"$where"
      echo "  $db <- write from SID $sid ($wc/$wp)"
      sc "$wc" "$db" ldapmodify --pod "${wp##*-}" >/dev/null 2>&1 <<EOF || \
        echo "     write FAILED from $wc/$wp"
dn: $suffix
changetype: modify
replace: description
description: its9398 heal write from serverID $sid
EOF
    done
  done
  note "Re-checking after 20s"; sleep 20
  MISSING_REPORT=(); diagnose && echo && echo "HEALED." || \
    { echo; echo "STILL BROKEN — see the rows above."; return 2; }
}

storm_counts(){ # $1 window
  local c p n
  printf '%-10s %-18s %12s %12s\n' SITE POD "stale/$1s" "lostsync/$1s"
  for c in "${CONTEXTS[@]}"; do
    for p in $(kc "$c" get pod -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | grep "^${CLUSTER}"); do
      n=$(kc "$c" logs "$p" -c slapd --since="${1}s" 2>/dev/null) || continue
      printf '%-10s %-18s %12s %12s%s\n' "$c" "$p" \
        "$(grep -c 'cookie is stale' <<<"$n")" "$(grep -c 'lost sync' <<<"$n")" \
        "$( [ "$(grep -c 'cookie is stale' <<<"$n")" -gt 0 ] && echo '  <-- BROKEN PROVIDER' || true)"
    done
  done
}

break_it(){
  local pod="${CLUSTER}-${TARGET}"
  kc "$SITE" get pod "$pod" >/dev/null || die "no pod $pod in $SITE/$NAMESPACE"
  case "$KILL" in graceful|hard|brutal) ;; *) die "--kill must be graceful|hard|brutal";; esac
  if [ "$ASSUME_YES" = 0 ]; then
    printf '\nAbout to kill %s in %s/%s with mode=%s.\n' "$pod" "$SITE" "$NAMESPACE" "$KILL"
    printf 'Continue? [y/N] '; read -r a; case "$a" in y|Y) ;; *) echo aborted; exit 1;; esac
  fi
  note "Pre-state"; diagnose || true

  case "$KILL" in
    graceful)
      note "kubectl delete pod (SIGTERM + grace) — the CONTROL arm"
      kc "$SITE" delete pod "$pod" --wait=true
      kc "$SITE" wait --for=condition=Ready "pod/$pod" --timeout=300s ;;
    hard)
      # NB: this is NOT a SIGKILL to slapd. It removes the API object without
      # waiting; the kubelet still terminates the container its own way, so
      # slapd generally gets SIGTERM and shuts down CLEANLY. Measured: it does
      # not reproduce the bug. Kept because it is what people reach for.
      note "kubectl delete --grace-period=0 --force (API-level only, NOT SIGKILL to slapd)"
      kc "$SITE" delete pod "$pod" --grace-period=0 --force
      kc "$SITE" wait --for=condition=Ready "pod/$pod" --timeout=300s ;;
    brutal)
      brutal_kill "$pod" ;;
  esac
  note "Observing ${OBSERVE}s of idleness"; sleep "$OBSERVE"
  storm_counts "$OBSERVE"; echo; MISSING_REPORT=(); diagnose || true
}

# A real SIGKILL to slapd. Runs a hostPID helper on the SAME node, finds the
# host PID whose namespaced PID is 1 inside the target container, and kills it.
# The kubelet then restarts the container IN PLACE: same pod, same PVCs,
# restartCount+1 — which is what an OOM kill or a crash actually looks like.
# Put the namespace back the way we found it — which is NOT the same as
# deleting the label. tests/e2e.sh sets enforce=privileged on the test
# namespace deliberately, so blindly removing it here would silently undo that
# and the next `slctl debug` would quietly lose its capabilities.
restore_ps(){
  [ "${PRIV_LABELED:-0}" = 1 ] || return 0
  PRIV_LABELED=0
  if [ -n "${PS_ENFORCE_WAS:-}" ]; then
    kubectl --context "$SITE" label ns "$NAMESPACE" --overwrite \
      "pod-security.kubernetes.io/enforce=$PS_ENFORCE_WAS" >/dev/null 2>&1 || true
    echo "  (PodSecurity enforce on $NAMESPACE restored to '$PS_ENFORCE_WAS')"
  else
    kubectl --context "$SITE" label ns "$NAMESPACE" \
      pod-security.kubernetes.io/enforce- >/dev/null 2>&1 || true
    echo "  (PodSecurity label on $NAMESPACE removed again — there was none before)"
  fi
}

brutal_kill(){
  local pod="$1" node cid short helper image rc0 rc1 phase i
  node=$(kc "$SITE" get pod "$pod" -o jsonpath='{.spec.nodeName}')
  cid=$(kc "$SITE" get pod "$pod" -o jsonpath='{.status.containerStatuses[?(@.name=="slapd")].containerID}')
  cid="${cid##*://}"; short="${cid:0:12}"
  image=$(kc "$SITE" get pod "$pod" -o jsonpath='{.spec.initContainers[0].image}')
  rc0=$(kc "$SITE" get pod "$pod" -o jsonpath='{.status.containerStatuses[?(@.name=="slapd")].restartCount}')
  helper="its9398-kill-${short}"
  note "SIGKILL slapd in $pod (container $short) on node $node, restartCount=$rc0"

  kc "$SITE" delete pod "$helper" --ignore-not-found >/dev/null 2>&1 || true

  # A hostPID+privileged helper needs the namespace to permit it. Many
  # clusters enforce 'baseline' by default, with no namespace label to show
  # for it, so this is only discoverable by being refused.
  if [ "$ALLOW_PRIV" = 1 ]; then
    PS_ENFORCE_WAS=$(kc "$SITE" get ns "$NAMESPACE" \
      -o 'jsonpath={.metadata.labels.pod-security\.kubernetes\.io/enforce}' 2>/dev/null || true)
    if [ "$PS_ENFORCE_WAS" = "privileged" ]; then
      echo "  (namespace is already enforce=privileged — nothing to relax)"
    else
      note "Relaxing PodSecurity on $NAMESPACE for the kill (restored afterwards)"
      kc "$SITE" label ns "$NAMESPACE" --overwrite \
        pod-security.kubernetes.io/enforce=privileged >/dev/null
      PRIV_LABELED=1
      trap 'restore_ps' EXIT
    fi
  fi

  cat <<EOF | kc "$SITE" apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: { name: $helper }
spec:
  hostPID: true
  nodeName: $node
  restartPolicy: Never
  tolerations: [{ operator: Exists }]
  containers:
  - name: kill
    image: $image
    securityContext: { privileged: true, runAsUser: 0 }
    command: ["sh","-c"]
    args:
    - |
      ID=$short
      for f in \$(grep -rl \$ID /proc/[0-9]*/cgroup 2>/dev/null); do
        pid=\$(echo \$f | cut -d/ -f3)
        nspid=\$(grep NSpid /proc/\$pid/status 2>/dev/null | awk '{print \$NF}')
        [ "\$nspid" = "1" ] || continue
        echo "killing host pid \$pid (container \$ID)"
        kill -9 \$pid
        exit 0
      done
      echo "container init not found for \$ID" >&2; exit 1
EOF
  for i in $(seq 1 30); do
    phase=$(kc "$SITE" get pod "$helper" -o jsonpath='{.status.phase}' 2>/dev/null || true)
    case "$phase" in Succeeded|Failed) break;; esac; sleep 2
  done
  echo "  helper: $(kc "$SITE" logs "$helper" 2>&1 | tr '\n' ' ')"
  kc "$SITE" delete pod "$helper" --ignore-not-found >/dev/null 2>&1 || true
  restore_ps
  if [ "$phase" != "Succeeded" ]; then
    echo
    echo "The brutal kill needs a hostPID + privileged helper pod. If that was"
    echo "refused by PodSecurity, re-run with --allow-privileged (which labels"
    echo "the namespace for the duration and removes the label again), or do it"
    echo "by hand:"
    echo "  kubectl --context $SITE label ns $NAMESPACE pod-security.kubernetes.io/enforce=privileged --overwrite"
    die "brutal kill failed (helper phase=${phase:-none})"
  fi

  note "Waiting for slapd to restart in place"
  for i in $(seq 1 60); do
    rc1=$(kc "$SITE" get pod "$pod" -o jsonpath='{.status.containerStatuses[?(@.name=="slapd")].restartCount}' 2>/dev/null || echo "$rc0")
    [ "${rc1:-$rc0}" -gt "$rc0" ] && break; sleep 2
  done
  echo "  restartCount $rc0 -> ${rc1:-?}"
  kc "$SITE" wait --for=condition=Ready "pod/$pod" --timeout=300s
}

case "$MODE" in
  diagnose) diagnose && { echo; echo "All accesslog DBs healthy."; } || \
            { echo; echo "EXPOSED/BROKEN — the rows above name the missing serverIDs."; exit 2; } ;;
  heal)     heal ;;
  break)    break_it ;;
  *) die "mode must be diagnose | heal | break";;
esac
