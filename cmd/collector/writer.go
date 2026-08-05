package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/costing"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/domain"
	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/store/postgres"
)

// writer persists a collection result.
//
// WHY THIS LIVES IN cmd/collector RATHER THAN IN A PACKAGE
// -------------------------------------------------------
// It is wiring: it knows about the cost engine's output shape AND the database's schema, and
// its only job is to bridge them. Putting it in internal/ would create a package whose sole
// purpose is to depend on two others, which is the shape of an unnecessary layer.
//
// If a second binary ever needs to persist allocations, that is the moment to extract it --
// from two real callers rather than in anticipation of one.
type writer struct {
	db          *postgres.DB
	clusterName string
	log         *slog.Logger
}

func newWriter(db *postgres.DB, clusterName string, log *slog.Logger) *writer {
	return &writer{db: db, clusterName: clusterName, log: log}
}

// write persists the dimensions and the fact rows for one cycle.
//
// EVERYTHING HAPPENS IN ONE TRANSACTION, AND THAT IS NOT OPTIONAL
// -------------------------------------------------------------
// Fact rows carry a foreign key to pods.id, so the pod dimension row must exist before the
// fact that references it. Without a transaction there is a window in which dimensions are
// committed and facts are not, and a crash there leaves the database holding pods with no
// cost -- which reads as a cluster that suddenly went free.
//
// Wrapping both makes the cycle atomic: either the whole window is recorded or none of it is.
// Combined with the aligned window and the upsert, a retry after a crash then converges on
// exactly the same rows rather than compounding the damage.
func (w *writer) write(ctx context.Context, result costing.Result) error {
	if len(result.Allocations) == 0 {
		// Not an error. An empty cluster, or a window in which every pod was Pending, is a
		// legitimate outcome and the caller should not have to special-case it.
		w.log.Debug("nothing to persist for this window", "window", result.Window.String())
		return nil
	}

	return postgres.InTx(ctx, w.db.Pool(), func(q postgres.Querier) error {
		inv := postgres.NewInventoryRepository(q)
		allocRepo := postgres.NewAllocationRepository(q)

		clusterID, err := inv.UpsertCluster(ctx, w.clusterName, "kubernetes", "")
		if err != nil {
			return fmt.Errorf("upserting cluster: %w", err)
		}

		// Resolve each distinct dimension ONCE per cycle, not once per allocation.
		//
		// A 5,000-container cluster produces 5,000 fact rows but only a few hundred distinct
		// pods and a handful of nodes and namespaces. Upserting per fact row would issue tens
		// of thousands of statements to write the same few hundred dimension rows, and every
		// one is a round trip. These caches turn that back into the number of DISTINCT objects.
		//
		// Plain maps with no locking, because this runs on one goroutine inside a single
		// transaction -- a transaction cannot be used concurrently anyway, so there is nothing
		// to synchronise.
		nodeIDs := map[string]int64{}
		namespaceIDs := map[string]int64{}
		workloadIDs := map[string]int64{}
		podIDs := map[string]int64{}

		// The allocations carry only names, so the dimension rows are reconstructed from them.
		// That is deliberate: it means the engine's output is self-describing and the writer
		// needs no second read of the informer cache, which could by then disagree with what
		// the cycle actually measured.
		for i := range result.Allocations {
			a := &result.Allocations[i]

			nsID, ok := namespaceIDs[a.NamespaceName]
			if !ok {
				nsID, err = inv.UpsertNamespace(ctx, clusterID, domain.Namespace{
					Name:        a.NamespaceName,
					Team:        a.Team,
					CostCentre:  a.CostCentre,
					Environment: a.Environment,
				})
				if err != nil {
					return fmt.Errorf("upserting namespace %q: %w", a.NamespaceName, err)
				}
				namespaceIDs[a.NamespaceName] = nsID
			}

			var nodeID *int64
			if a.NodeName != "" {
				id, found := nodeIDs[a.NodeName]
				if !found {
					// Capacity is not carried on the fact row, so it is written as zero here and
					// corrected by cmd/api, which sees the live node objects. The alternative --
					// carrying full node capacity on every one of thousands of fact rows -- would
					// bloat the table to record a value that belongs to the node dimension.
					id, err = inv.UpsertNode(ctx, clusterID, domain.Node{
						Name:         a.NodeName,
						InstanceType: a.InstanceType,
						Zone:         a.Zone,
						CapacityType: a.CapacityType,
					})
					if err != nil {
						return fmt.Errorf("upserting node %q: %w", a.NodeName, err)
					}
					nodeIDs[a.NodeName] = id
				}
				nodeID = &id
			}

			var workloadID *int64
			if a.WorkloadName != "" {
				key := a.NamespaceName + "/" + a.WorkloadKind + "/" + a.WorkloadName
				id, found := workloadIDs[key]
				if !found {
					id, err = inv.UpsertWorkload(ctx, clusterID, nsID,
						domain.Workload{Kind: a.WorkloadKind, Name: a.WorkloadName})
					if err != nil {
						return fmt.Errorf("upserting workload %s: %w", key, err)
					}
					workloadIDs[key] = id
				}
				workloadID = &id
			}

			podKey := a.NamespaceName + "/" + a.PodName
			podID, found := podIDs[podKey]
			if !found {
				// The pod's UID is not on the fact row, so a synthetic stable identity is used:
				// namespace/name. That is a KNOWN COMPROMISE and the same one the
				// Prometheus join makes -- cAdvisor has no UID label either, so usage is already
				// matched by name. Recording a UID here that the usage data could not be matched
				// against would be a false precision.
				//
				// The consequence is that a deleted and recreated pod with the same name shares
				// one dimension row. For cost that is acceptable: the fact rows are still
				// separated by window, so no cost is lost or duplicated.
				podID, err = inv.UpsertPod(ctx, postgres.UpsertPodParams{
					ClusterID:   clusterID,
					NamespaceID: nsID,
					WorkloadID:  workloadID,
					NodeID:      nodeID,
					Pod: domain.Pod{
						UID:      podKey,
						Name:     a.PodName,
						QoSClass: a.QoSClass,
					},
				})
				if err != nil {
					return fmt.Errorf("upserting pod %s: %w", podKey, err)
				}
				podIDs[podKey] = podID
			}

			a.PodID = podID
		}

		// One batched write for every fact row: a single round trip rather than one per row.
		if err := allocRepo.InsertBatch(ctx, result.Allocations); err != nil {
			return fmt.Errorf("inserting allocations: %w", err)
		}

		w.log.Debug("persisted window",
			"window", result.Window.String(),
			"allocations", len(result.Allocations),
			"distinct_pods", len(podIDs),
			"distinct_nodes", len(nodeIDs),
			"distinct_namespaces", len(namespaceIDs),
		)
		return nil
	})
}
