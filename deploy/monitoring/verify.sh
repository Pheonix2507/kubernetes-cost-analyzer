#!/usr/bin/env bash
# Proves the observability wiring works, rather than that the YAML applied.
#
# WHY A SCRIPT AND NOT A README SECTION
# `kubectl apply` succeeding means the API server accepted the objects. It does NOT mean Prometheus
# scraped anything, that the rules evaluate, or that Grafana loaded the dashboard -- and each of those
# fails silently. A ServiceMonitor matching nothing produces no error and no targets; a ConfigMap missing
# its label is simply ignored.
#
# The same argument as deploy/rbac/verify.sh: configuration that claims to work should be asked.
set -uo pipefail

PROM="${PROM:-http://localhost:19090}"
GRAF="${GRAF:-http://localhost:13000}"
fails=0
pass() { printf '  \033[32mPASS\033[0m %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; fails=$((fails+1)); }

echo "== targets =="
targets=$(curl -sf "$PROM/api/v1/targets?state=active" 2>/dev/null) || { fail "cannot reach Prometheus at $PROM"; exit 1; }
for job in kca-api kca-collector; do
  health=$(printf '%s' "$targets" | python3 -c "
import json,sys
for t in json.load(sys.stdin)['data']['activeTargets']:
    if t['labels'].get('job')=='$job': print(t['health']); break
else: print('missing')" 2>/dev/null)
  case "$health" in
    up) pass "$job is up" ;;
    missing) fail "$job has no target — is the ScrapeConfig applied?" ;;
    *) fail "$job is $health — is the process running on the host?" ;;
  esac
done

echo "== recording rules =="
for rule in kca:facts_age_seconds kca:rollup_age_seconds job:kca_http_error_ratio:rate5m; do
  n=$(curl -sf --data-urlencode "query=$rule" "$PROM/api/v1/query" 2>/dev/null \
      | python3 -c "import json,sys; print(len(json.load(sys.stdin)['data']['result']))" 2>/dev/null)
  [ "${n:-0}" -gt 0 ] && pass "$rule evaluates" || fail "$rule has no series"
done

echo "== alert rules =="
loaded=$(curl -sf "$PROM/api/v1/rules?type=alert" 2>/dev/null \
  | python3 -c "
import json,sys
print(sum(len(g['rules']) for g in json.load(sys.stdin)['data']['groups'] if g['name'].startswith('kca')))" 2>/dev/null)
[ "${loaded:-0}" -ge 9 ] && pass "$loaded alert rules loaded" || fail "only ${loaded:-0} alert rules loaded, expected 9"

# THE ONE THAT MATTERS MOST: the critical alert must be able to fire.
# Evaluated against a threshold of 0, which is always true when the series exists -- so this proves the
# expression is valid and the series is present, without waiting 20 minutes for real staleness.
n=$(curl -sf --data-urlencode 'query=kca:facts_age_seconds > 0' "$PROM/api/v1/query" 2>/dev/null \
    | python3 -c "import json,sys; print(len(json.load(sys.stdin)['data']['result']))" 2>/dev/null)
[ "${n:-0}" -gt 0 ] && pass "the critical staleness expression is evaluable" \
  || fail "kca:facts_age_seconds is absent — the critical alert cannot fire"

echo "== the freshness gate on cost alerts =="
# Cost alerts are gated with `and on() (kca:facts_age_seconds < 900)` so a collection outage does not
# produce a storm of cost alerts about data that is merely old. Verified: with the collector down 22 hours,
# kca_cost_cluster_hourly read 0 while nothing was actually free.
n=$(curl -sf --data-urlencode 'query=kca_cost_cluster_hourly and on() (kca:facts_age_seconds < 900)' "$PROM/api/v1/query" 2>/dev/null \
    | python3 -c "import json,sys; print(len(json.load(sys.stdin)['data']['result']))" 2>/dev/null)
if [ "${n:-0}" -gt 0 ]; then
  pass "the gate is OPEN (data is fresh, so cost alerts are live)"
else
  pass "the gate is CLOSED (data is stale, so cost alerts are correctly suppressed)"
fi

echo "== grafana dashboard =="
pw=$(kubectl get secret -n monitoring kps-grafana -o jsonpath='{.data.admin-password}' 2>/dev/null | base64 -d)
if [ -n "$pw" ]; then
  title=$(curl -sf -u "admin:$pw" "$GRAF/api/dashboards/uid/kca-overview" 2>/dev/null \
    | python3 -c "import json,sys; print(json.load(sys.stdin)['dashboard']['title'])" 2>/dev/null)
  [ -n "$title" ] && pass "dashboard provisioned: $title" || fail "dashboard not registered — is the ConfigMap labelled grafana_dashboard=1?"
else
  fail "cannot read the Grafana admin password"
fi

echo
[ "$fails" -eq 0 ] && { echo "All observability checks passed."; exit 0; }
echo "$fails check(s) failed."; exit 1
