#!/usr/bin/env bash
#
# Proves the ClusterRole grants exactly what the analyzer needs and nothing more.
#
#   make rbac-verify
#
# HOW THIS WORKS
# --------------
# `kubectl auth can-i --as=<user>` asks the API SERVER'S OWN AUTHORISER whether a given
# identity may perform a given action. It evaluates the real RBAC graph -- every
# ClusterRole and binding that applies -- and needs nothing deployed. So we can keep the
# fast `make run-api` loop against a kubeconfig and still know the ClusterRole is correct
# long before Phase 10 builds a Deployment.
#
# Impersonation itself requires the `impersonate` verb, which your kubeconfig has as
# cluster admin on kind. On a locked-down production cluster you would not have it, and
# there the equivalent check is `kubectl auth can-i --list` run from inside the pod.
#
# WHY THE NEGATIVE CHECKS ARE THE IMPORTANT HALF
# ----------------------------------------------
# A ClusterRole granting cluster-admin passes every single "can I read?" assertion. Only
# "can I delete?" returning NO demonstrates that least privilege actually holds. A test
# suite of positive assertions cannot distinguish correct from dangerously permissive --
# which is the same reason internal/kube's fixtures include a right-sized control case.
set -uo pipefail

SA="system:serviceaccount:kca-system:kca-api"
failures=0

# want_yes <verb> <resource> [extra args]
want_yes() {
  local verb="$1" resource="$2"; shift 2
  local got
  got=$(kubectl auth can-i "$verb" "$resource" --as="$SA" "$@" 2>/dev/null)
  if [[ "$got" == "yes" ]]; then
    printf '  \033[32mPASS\033[0m  %-46s allowed\n' "$verb $resource $*"
  else
    printf '  \033[31mFAIL\033[0m  %-46s got %q, want yes (the analyzer needs this)\n' \
      "$verb $resource $*" "$got"
    failures=$((failures + 1))
  fi
}

# want_no <verb> <resource> [extra args]
want_no() {
  local verb="$1" resource="$2"; shift 2
  local got
  got=$(kubectl auth can-i "$verb" "$resource" --as="$SA" "$@" 2>/dev/null)
  if [[ "$got" == "no" ]]; then
    printf '  \033[32mPASS\033[0m  %-46s DENIED\n' "$verb $resource $*"
  else
    printf '  \033[31mFAIL\033[0m  %-46s got %q, want no (PRIVILEGE TOO BROAD)\n' \
      "$verb $resource $*" "$got"
    failures=$((failures + 1))
  fi
}

echo "Verifying RBAC for ${SA}"
echo
echo "REQUIRED -- informers cannot sync without all of these:"
want_yes list   nodes
want_yes watch  nodes
want_yes list   pods           --all-namespaces
want_yes watch  pods           --all-namespaces
want_yes list   namespaces
want_yes watch  namespaces
# The second ownership hop. Without this, every workload's cost history resets on
# each rollout, because pods would report their hash-suffixed ReplicaSet instead of
# their Deployment.
want_yes get    replicasets    --all-namespaces
want_yes list   replicasets    --all-namespaces

echo
echo "MUST BE DENIED -- these prove least privilege actually holds:"
want_no  delete pods           --all-namespaces
want_no  create pods           --all-namespaces
want_no  update nodes
want_no  patch  nodes
# Cordoning a node is an update to node objects. A cost tool suggesting you drain a
# node is useful; one able to do it unasked is a liability.
want_no  update deployments    --all-namespaces
want_no  delete deployments    --all-namespaces
want_no  create clusterrolebindings
# Reading Secrets is the classic over-grant. Nothing about cost needs them, and a
# ServiceAccount that can list Secrets cluster-wide is effectively cluster-admin,
# because it can read other components' credentials.
want_no  get    secrets        --all-namespaces
want_no  list   secrets        --all-namespaces
# ConfigMaps are a lesser version of the same problem.
want_no  list   configmaps     --all-namespaces
# Exec into a pod is remote code execution inside the cluster.
want_no  create pods/exec      --all-namespaces

echo
echo "MUST BE DENIED -- resources the code does not read, removed after an audit:"
# These were granted on the reasoning that they are "the remaining pieces of the cost picture".
# The code reads none of them, so granting them contradicted the least-privilege argument in
# the same file. Phase 6 will add PVCs, PVs and services when it genuinely needs them, in a
# commit whose diff says so.
#
# Asserting they are DENIED turns "we removed these" into a property the script enforces,
# rather than a claim in a comment that could silently drift back.
want_no  list   persistentvolumeclaims --all-namespaces
want_no  list   persistentvolumes
want_no  list   services       --all-namespaces
want_no  list   deployments    --all-namespaces
want_no  list   statefulsets   --all-namespaces
want_no  list   daemonsets     --all-namespaces
want_no  list   jobs           --all-namespaces
want_no  list   cronjobs       --all-namespaces

echo
if (( failures > 0 )); then
  echo "RBAC verification FAILED with ${failures} problem(s)."
  exit 1
fi
echo "RBAC verified: every required read is allowed, every write and secret read is denied."
