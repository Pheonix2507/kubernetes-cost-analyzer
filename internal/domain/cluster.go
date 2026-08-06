package domain

import "strings"

// ClusterProfile is what a cluster can work out about itself by looking at its own nodes.
//
// WHY DERIVE THIS RATHER THAN CONFIGURE IT
// ----------------------------------------
// Until Phase 11 the collector passed the literals "kubernetes" and "" for a cluster's provider and
// region, so two columns that existed since the baseline schema held the same two values forever.
// Configuring them instead would have been an improvement of the smallest possible kind: a second
// place to state a fact the cluster already knows, which drifts the first time somebody moves a
// cluster between regions and updates the cluster but not the environment variable.
//
// A cluster's provider and region are properties of the machines it runs on. The machines are in the
// informer cache already. So they are read, not declared.
//
// The one piece that genuinely cannot be derived is the billing account: nothing in the Kubernetes
// API names the account, project or subscription that pays for the nodes. That is why
// CLUSTER_ACCOUNT is configuration and these two are not.
type ClusterProfile struct {
	// Provider is the scheme of the nodes' providerID: aws, gce, azure, kind. Empty when the
	// nodes carry no providerID, or when they disagree.
	Provider string
	// Region is the shared topology.kubernetes.io/region label. Empty when unset, or when the
	// nodes disagree.
	Region string
}

// DescribeCluster derives a profile from a cluster's nodes.
//
// WHY DISAGREEMENT PRODUCES EMPTY RATHER THAN A WINNER
// ---------------------------------------------------
// The tempting rule is "take the most common value", and it is wrong in the case that matters. A
// genuinely multi-region cluster has no single region, so reporting the majority one attributes
// every cost in the cluster to a region that hosts only some of it -- and it does so silently, since
// a plausible value provokes no questions. Likewise a hybrid cluster with nodes in two clouds has no
// single provider.
//
// Empty means "the nodes do not agree on one answer", which is the truth, and it is visible: an
// operator seeing a blank region asks why, whereas one seeing `us-east-1` on a cluster spanning
// three regions asks nothing at all. Per-node region is still recorded on every node row, so nothing
// is lost, and the pricing engine keys off the NODE's region rather than the cluster's for exactly
// this reason.
func DescribeCluster(nodes []Node) ClusterProfile {
	return ClusterProfile{
		Provider: soleValue(nodes, func(n Node) string { return providerFromID(n.ProviderID) }),
		Region:   soleValue(nodes, func(n Node) string { return n.Region }),
	}
}

// soleValue returns the one non-empty value every node agrees on, or "".
//
// Nodes yielding an empty value are SKIPPED rather than counted as disagreement. That matters during
// a rolling upgrade or a scale-up: a node that has joined but whose cloud controller manager has not
// yet stamped its providerID would otherwise blank the whole cluster's provider for a few minutes,
// and the cluster would appear to change clouds and back again.
func soleValue(nodes []Node, get func(Node) string) string {
	var found string
	for _, n := range nodes {
		v := strings.TrimSpace(get(n))
		if v == "" {
			continue
		}
		if found == "" {
			found = v
			continue
		}
		if v != found {
			return ""
		}
	}
	return found
}

// providerFromID extracts the scheme from a providerID.
//
// Parsed by hand rather than with net/url, deliberately. `aws:///ap-south-1a/i-0abc` has an EMPTY
// authority (note the three slashes), and Azure's is stranger still; url.Parse accepts them but the
// only field wanted here is the scheme, and cutting at the separator cannot fail, cannot allocate an
// error, and cannot surprise anyone reading it. Reaching for a URL parser to read the text before a
// colon is how a one-line function acquires an error return.
func providerFromID(providerID string) string {
	scheme, _, ok := strings.Cut(providerID, "://")
	if !ok {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(scheme))
}
