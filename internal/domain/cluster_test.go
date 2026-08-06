package domain

import "testing"

func TestProviderFromID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		providerID string
		want       string
	}{
		// The four real shapes, taken from actual clusters rather than invented. Note that the
		// number of slashes differs between them: AWS and Azure have an empty authority, kind
		// and GCE do not. A parser that assumed a host would get two of these wrong.
		{"aws has an empty authority", "aws:///ap-south-1a/i-0abc123def", "aws"},
		{"kind, verified on this project's own cluster", "kind://docker/kca-dev/kca-dev-worker", "kind"},
		{"gce", "gce://my-project/asia-south1-a/gke-node-1", "gce"},
		{"azure", "azure:///subscriptions/abc/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm0", "azure"},

		// A cluster with no cloud provider configured. Not an error: it means nobody has told
		// Kubernetes where it runs, and reporting that honestly beats inventing "kubernetes".
		{"empty", "", ""},
		// Defensive, not hypothetical: a malformed providerID must not become a provider named
		// after the whole string, which would then appear in the clusters table as a value no
		// pricing catalogue could ever match.
		{"no scheme separator", "i-0abc123def", ""},
		{"single colon is not the separator", "aws:ap-south-1a", ""},

		{"uppercase is normalised", "AWS:///ap-south-1a/i-1", "aws"},
		{"surrounding space is trimmed", "  aws:///x/i-1  ", "aws"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := providerFromID(tc.providerID); got != tc.want {
				t.Errorf("providerFromID(%q) = %q, want %q", tc.providerID, got, tc.want)
			}
		})
	}
}

func TestDescribeCluster(t *testing.T) {
	t.Parallel()

	node := func(providerID, region string) Node {
		return Node{ProviderID: providerID, Region: region}
	}

	tests := []struct {
		name         string
		nodes        []Node
		wantProvider string
		wantRegion   string
	}{
		{
			name:         "no nodes at all",
			nodes:        nil,
			wantProvider: "",
			wantRegion:   "",
		},
		{
			name:         "a single-region AWS cluster, the ordinary case",
			nodes:        []Node{node("aws:///ap-south-1a/i-1", "ap-south-1"), node("aws:///ap-south-1b/i-2", "ap-south-1")},
			wantProvider: "aws",
			wantRegion:   "ap-south-1",
		},
		{
			// THE CASE THIS FUNCTION EXISTS FOR. A cluster spanning two regions has no single
			// region, and answering with the majority would attribute the whole cluster's cost
			// to a region hosting only part of it -- silently, because a plausible value
			// provokes no questions.
			name:         "nodes in two regions yield no region, not the majority",
			nodes:        []Node{node("aws:///ap-south-1a/i-1", "ap-south-1"), node("aws:///us-east-1a/i-2", "us-east-1"), node("aws:///us-east-1b/i-3", "us-east-1")},
			wantProvider: "aws",
			wantRegion:   "",
		},
		{
			name:         "a hybrid cluster yields no provider",
			nodes:        []Node{node("aws:///ap-south-1a/i-1", "ap-south-1"), node("gce://p/asia-south1-a/n", "ap-south-1")},
			wantProvider: "",
			wantRegion:   "ap-south-1",
		},
		{
			// A node mid-join, before its cloud controller manager has stamped providerID.
			// Skipping it rather than treating it as disagreement stops the cluster appearing
			// to change clouds and back during every scale-up.
			name:         "a node with no providerID yet is skipped, not counted as conflict",
			nodes:        []Node{node("aws:///ap-south-1a/i-1", "ap-south-1"), node("", "ap-south-1")},
			wantProvider: "aws",
			wantRegion:   "ap-south-1",
		},
		{
			name:         "a node with no region label is skipped too",
			nodes:        []Node{node("aws:///ap-south-1a/i-1", ""), node("aws:///ap-south-1a/i-2", "ap-south-1")},
			wantProvider: "aws",
			wantRegion:   "ap-south-1",
		},
		{
			name:         "bare metal, nothing derivable",
			nodes:        []Node{node("", ""), node("", "")},
			wantProvider: "",
			wantRegion:   "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := DescribeCluster(tc.nodes)
			if got.Provider != tc.wantProvider {
				t.Errorf("Provider = %q, want %q", got.Provider, tc.wantProvider)
			}
			if got.Region != tc.wantRegion {
				t.Errorf("Region = %q, want %q", got.Region, tc.wantRegion)
			}
		})
	}
}
