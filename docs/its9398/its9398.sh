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
TARGET=0 ; OBSERVE=60 ; GRACEFUL=0 ; ASSUME_YES=0
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
    --graceful)  GRACEFUL=1; shift;;
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
  if [ "$ASSUME_YES" = 0 ]; then
    printf '\nAbout to %s delete %s in %s/%s.\n' \
      "$( [ "$GRACEFUL" = 1 ] && echo 'GRACEFULLY' || echo 'SIGKILL (--grace-period=0 --force)')" \
      "$pod" "$SITE" "$NAMESPACE"
    [ "$GRACEFUL" = 1 ] || printf 'Expect a permanent stale-cookie storm until healed.\n'
    printf 'Continue? [y/N] '; read -r a; case "$a" in y|Y) ;; *) echo aborted; exit 1;; esac
  fi
  note "Writing from every mapped serverID first, so minCSN names them all"
  build_sid_map; heal >/dev/null 2>&1 || true
  if [ "$GRACEFUL" = 1 ]; then
    note "Deleting $pod gracefully (SIGTERM — control arm)"; kc "$SITE" delete pod "$pod" --wait=true
  else
    note "Deleting $pod with --grace-period=0 --force (SIGKILL)"
    kc "$SITE" delete pod "$pod" --grace-period=0 --force
  fi
  note "Waiting for $pod"; kc "$SITE" wait --for=condition=Ready "pod/$pod" --timeout=300s
  note "Observing ${OBSERVE}s of idleness"; sleep "$OBSERVE"
  storm_counts "$OBSERVE"; echo; diagnose || true
}

case "$MODE" in
  diagnose) diagnose && { echo; echo "All accesslog DBs healthy."; } || \
            { echo; echo "EXPOSED/BROKEN — the rows above name the missing serverIDs."; exit 2; } ;;
  heal)     heal ;;
  break)    break_it ;;
  *) die "mode must be diagnose | heal | break";;
esac
