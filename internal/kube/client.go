// Package kube is our window onto the Kubernetes API server.
//
// WHY THIS PACKAGE EXISTS
// -----------------------
// Cost is four things multiplied: what is running, how much it RESERVED, what that
// resource costs, and for how long. Prometheus (Phase 4) supplies usage over time and
// the pricing engine (Phase 3) supplies rates. This package supplies the topology --
// the object graph that says a pod exists, requested 500m of CPU, landed on an
// m5.large in ap-south-1b, and belongs to a Deployment owned by the payments team.
//
// WHY NOT JUST READ kube-state-metrics FROM PROMETHEUS
// ---------------------------------------------------
// KSM does publish kube_pod_container_resource_requests, so this is a fair question.
// Three reasons:
//
//  1. STALENESS COMPOUNDS. KSM watches the API server, Prometheus scrapes KSM every
//     30s, and we would query Prometheus. Two hops of lag on data that exists live.
//  2. PromQL IS BAD AT GRAPHS. Resolving pod -> ReplicaSet -> Deployment is a join.
//     In PromQL that means label_replace gymnastics that break on naming edge cases.
//  3. METRICS ARE LOSSY. QoS class, container names, volume mounts, PVC bindings, the
//     full node label set -- KSM exposes some of it, in whatever shape it chose. The
//     API server has the actual objects.
//
// KSM is itself informers plus a metrics renderer. We are going one layer down, to
// the same source it uses.
//
// EVERYTHING HERE IS READ-ONLY. We never create, update or delete. That is enforced
// twice: by only ever calling listers, and by the RBAC in deploy/rbac, which grants
// get/list/watch and nothing else. A cost tool has no business mutating the cluster
// it observes, and least privilege means a bug here cannot damage anything.
package kube

import (
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/config"
)

// RESTConfig builds the connection settings for the API server, working both inside
// a pod and on a developer laptop.
//
// THE TWO WAYS A PROCESS AUTHENTICATES TO KUBERNETES
// -------------------------------------------------
// IN-CLUSTER: the kubelet mounts a ServiceAccount token into every pod at
// /var/run/secrets/kubernetes.io/serviceaccount/, alongside the cluster CA
// certificate, and sets KUBERNETES_SERVICE_HOST/PORT. rest.InClusterConfig() reads
// exactly those. The token is short-lived and auto-rotated by the kubelet (projected
// service account tokens), which is why you must never cache its contents yourself --
// client-go re-reads the file.
//
// OUT-OF-CLUSTER: a kubeconfig file describing clusters, users and contexts.
//
// Supporting both from one function is what lets `make run-api` on your laptop and a
// pod in the cluster run identical code. The alternative -- a build tag or a "dev
// mode" branch -- means the path you test locally is not the path that runs in
// production.
func RESTConfig(cfg config.Kube) (*rest.Config, error) {
	// An explicit path always wins: if someone set KUBECONFIG, honour it rather than
	// silently preferring in-cluster credentials. Surprising a developer about which
	// cluster they are talking to is worse than any convenience.
	if cfg.ConfigPath == "" && cfg.Context == "" {
		// Try in-cluster first. This returns rest.ErrNotInCluster when the
		// environment variables are absent, which is the normal case on a laptop and
		// NOT an error worth reporting.
		if inCluster, err := rest.InClusterConfig(); err == nil {
			applyRateLimits(inCluster, cfg)
			return inCluster, nil
		}
	}

	// Fall back to kubeconfig, using client-go's standard discovery: the explicit
	// path if given, else $KUBECONFIG, else ~/.kube/config. Reusing the library's
	// loading rules matters -- they also handle multi-file KUBECONFIG lists, which a
	// hand-rolled os.ReadFile would silently get wrong.
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if cfg.ConfigPath != "" {
		rules.ExplicitPath = cfg.ConfigPath
	}

	overrides := &clientcmd.ConfigOverrides{}
	if cfg.Context != "" {
		overrides.CurrentContext = cfg.Context
	}

	restCfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}

	applyRateLimits(restCfg, cfg)
	return restCfg, nil
}

// applyRateLimits overrides client-go's default client-side throttling.
//
// See the long comment on config.Kube.QPS: the defaults are 5 QPS / 10 burst, and
// exceeding them causes SILENT QUEUING rather than an error. The symptom is latency
// that looks like a slow API server, and it is invisible in the API server's own
// metrics because the requests never arrive there.
func applyRateLimits(restCfg *rest.Config, cfg config.Kube) {
	restCfg.QPS = cfg.QPS
	restCfg.Burst = cfg.Burst

	// A User-Agent identifying us by name in the API server's audit log. Default is a
	// generic Go http client string, which is useless when someone is trying to work
	// out which of thirty workloads is hammering the control plane.
	restCfg.UserAgent = "kubernetes-cost-analyzer"
}

// NewClientset builds a typed Kubernetes client.
//
// "Typed" means generated, compile-time-checked accessors: clientset.CoreV1().Pods(ns)
// returns *corev1.PodList, not a map[string]any. The alternatives:
//
//   - dynamic.Interface works with unstructured objects and is what you need for
//     arbitrary CRDs discovered at runtime. We know our types at compile time, so we
//     would be giving up type safety for nothing.
//   - controller-runtime wraps all of this with a manager, leader election and
//     reconcile loops. Excellent for operators. We are not writing a controller: we
//     never reconcile desired against actual state, we only read. It would add a
//     large dependency to hide the very machinery worth understanding here.
func NewClientset(restCfg *rest.Config) (*kubernetes.Clientset, error) {
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("building kubernetes clientset: %w", err)
	}
	return clientset, nil
}
