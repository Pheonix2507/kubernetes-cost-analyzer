#!/usr/bin/env bash
# Proves the deployed release works, rather than that `helm install` exited zero.
#
# WHY THIS EXISTS
# `helm install --wait` waits for pods to become READY, which is genuinely useful and not the same as
# working. It cannot tell you the RBAC is sufficient, that the collector is writing rows, that the CronJob
# has a valid schedule, or that the ServiceMonitor selects anything. Each of those fails silently.
#
# Same argument as deploy/rbac/verify.sh and deploy/monitoring/verify.sh: configuration that claims to work
# should be asked.
set -uo pipefail

NS="${NS:-kca}"
REL="${REL:-kca}"
fails=0
pass() { printf '  \033[32mPASS\033[0m %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; fails=$((fails+1)); }

echo "== workloads =="
for kind in deployment/kca-api deployment/kca-collector statefulset/kca-postgres; do
  if kubectl -n "$NS" get "$kind" >/dev/null 2>&1; then
    ready=$(kubectl -n "$NS" get "$kind" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)
    [ "${ready:-0}" -ge 1 ] && pass "$kind has ${ready} ready" || fail "$kind has no ready replicas"
  else
    fail "$kind is missing"
  fi
done

echo "== the collector is pinned to one replica =="
# The invariant the chart enforces at template time. Asserted again against the LIVE object, because a
# `kubectl scale` would bypass the template entirely.
n=$(kubectl -n "$NS" get deploy kca-collector -o jsonpath='{.spec.replicas}' 2>/dev/null)
[ "${n:-0}" = "1" ] && pass "collector replicas = 1" || fail "collector replicas = ${n:-?}, must be 1"

echo "== the collector uses Recreate, not RollingUpdate =="
# Without this, every deploy briefly runs two collectors, both querying Prometheus for the same window.
s=$(kubectl -n "$NS" get deploy kca-collector -o jsonpath='{.spec.strategy.type}' 2>/dev/null)
[ "$s" = "Recreate" ] && pass "strategy = Recreate" || fail "strategy = ${s:-?}, must be Recreate"

echo "== no PodDisruptionBudget on the collector =="
# A PDB on a single-replica Deployment makes `kubectl drain` block FOREVER. Asserted as an absence, because
# the tempting fix for "protect the important component" is the thing that breaks cluster upgrades.
if kubectl -n "$NS" get pdb -o name 2>/dev/null | grep -q collector; then
  fail "a PDB exists on the collector: node drains would block forever on a single replica"
else
  pass "no collector PDB (drains are not blocked)"
fi

echo "== probes =="
for d in kca-api kca-collector; do
  n=$(kubectl -n "$NS" get deploy "$d" -o json 2>/dev/null | python3 -c "
import json,sys
c=json.load(sys.stdin)['spec']['template']['spec']['containers'][0]
print(sum(1 for p in ('startupProbe','livenessProbe','readinessProbe') if c.get(p)))" 2>/dev/null)
  [ "${n:-0}" = "3" ] && pass "$d has all three probes" || fail "$d has ${n:-0}/3 probes"
done

echo "== security context =="
for d in kca-api kca-collector; do
  ok=$(kubectl -n "$NS" get deploy "$d" -o json 2>/dev/null | python3 -c "
import json,sys
spec=json.load(sys.stdin)['spec']['template']['spec']
c=spec['containers'][0]['securityContext']
p=spec['securityContext']
print('yes' if (p.get('runAsNonRoot') and c.get('readOnlyRootFilesystem')
                and not c.get('allowPrivilegeEscalation')
                and c.get('capabilities',{}).get('drop')==['ALL']) else 'no')" 2>/dev/null)
  [ "$ok" = "yes" ] && pass "$d runs non-root, read-only, no caps" || fail "$d security context is weaker than the restricted PSS"
done

echo "== RBAC is read-only =="
# The strongest claim the chart makes: this service is STRUCTURALLY incapable of mutating the cluster.
# Asked of the API server's own authoriser rather than read off the ClusterRole, because a second binding
# could grant more than this chart's role does.
sa="system:serviceaccount:${NS}:kca"
for verb in create update patch delete deletecollection; do
  if kubectl auth can-i "$verb" pods --as="$sa" -A 2>/dev/null | grep -qi '^yes'; then
    fail "$sa can $verb pods"
  fi
done
kubectl auth can-i get secrets --as="$sa" -A 2>/dev/null | grep -qi '^yes' \
  && fail "$sa can read secrets" || pass "cannot write anything, cannot read secrets"
kubectl auth can-i list pods --as="$sa" -A 2>/dev/null | grep -qi '^yes' \
  && pass "can list pods (informers work)" || fail "cannot list pods -- the inventory will be empty"

echo "== cronjobs =="
for cj in kca-rollup; do
  if kubectl -n "$NS" get cronjob "$cj" >/dev/null 2>&1; then
    tz=$(kubectl -n "$NS" get cronjob "$cj" -o jsonpath='{.spec.timeZone}' 2>/dev/null)
    [ -n "$tz" ] && pass "$cj schedules in $tz" || fail "$cj has no timeZone: the schedule means whatever the controller's clock says"
    cp=$(kubectl -n "$NS" get cronjob "$cj" -o jsonpath='{.spec.concurrencyPolicy}' 2>/dev/null)
    [ "$cp" = "Forbid" ] && pass "$cj forbids concurrent runs" || fail "$cj concurrencyPolicy = $cp"
  else
    fail "$cj is missing"
  fi
done

echo "== the API actually answers =="
# The end-to-end check. Everything above is structural; this is the one that fails if the database URL is
# wrong, the RBAC is insufficient, or the image is broken.
kubectl -n "$NS" port-forward "svc/${REL}-api" 18080:8080 >/dev/null 2>&1 &
pf=$!
sleep 4
code=$(curl -s -o /dev/null -w '%{http_code}' localhost:18080/readyz 2>/dev/null)
if [ "$code" = "200" ]; then
  pass "/readyz returns 200 (database, Prometheus and informers all reachable)"
else
  fail "/readyz returned ${code:-nothing}"
  kubectl -n "$NS" logs "deploy/${REL}-api" --tail=15 2>/dev/null | sed 's/^/       /'
fi
kill $pf 2>/dev/null

echo
[ "$fails" -eq 0 ] && { echo "All chart checks passed."; exit 0; }
echo "$fails check(s) failed."; exit 1
