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
	db *postgres.DB
	// inventory is the AUTHORITATIVE source for dimension rows.
	//
	// An earlier version reconstructed nodes and namespaces from the fact rows, which carry
	// only the handful of fields denormalised onto them. The result was node rows with
	// capacity_cpu_millicores = 0, because a fact row has no reason to carry node capacity --
	// and a comment claimed cmd/api would "correct" them later, which was simply false:
	// cmd/api never writes to the database at all.
	//
	// Dimensions come from the live objects; facts come from the engine. Each from the source
	// that actually knows.
	inventory   costing.Inventory
	clusterName string
	log         *slog.Logger
}

func newWriter(db *postgres.DB, inventory costing.Inventory, clusterName string, log *slog.Logger) *writer {
	return &writer{db: db, inventory: inventory, clusterName: clusterName, log: log}
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

		// NODES AND NAMESPACES COME FROM THE LIVE OBJECTS, not from the fact rows.
		//
		// The informer cache holds the complete Node and Namespace objects -- capacity,
		// allocatable, the full label map. A fact row carries only the few fields denormalised
		// onto it for reporting, so reconstructing dimensions from facts produces rows that are
		// technically present and substantively empty.
		//
		// There are only tens of nodes and namespaces, so writing them all is cheaper than the
		// conditional bookkeeping it replaces, and it means the dimension tables are complete
		// even for a node currently hosting no pods -- which is itself a finding, since an empty
		// node still costs money.
		nodes, err := w.inventory.Nodes()
		if err != nil {
			return fmt.Errorf("listing nodes for dimension rows: %w", err)
		}
		nodeIDs := make(map[string]int64, len(nodes))
		for _, n := range nodes {
			id, upsertErr := inv.UpsertNode(ctx, clusterID, n)
			if upsertErr != nil {
				return fmt.Errorf("upserting node %q: %w", n.Name, upsertErr)
			}
			nodeIDs[n.Name] = id
		}

		namespaces, err := w.inventory.Namespaces()
		if err != nil {
			return fmt.Errorf("listing namespaces for dimension rows: %w", err)
		}
		namespaceIDs := make(map[string]int64, len(namespaces))
		for _, ns := range namespaces {
			id, upsertErr := inv.UpsertNamespace(ctx, clusterID, ns)
			if upsertErr != nil {
				return fmt.Errorf("upserting namespace %q: %w", ns.Name, upsertErr)
			}
			namespaceIDs[ns.Name] = id
		}

		// Workloads and pods ARE resolved from the allocations, and for those it is the right
		// source: the engine already resolved each pod's owning workload during collection, and
		// re-reading the cache could disagree with what the cycle actually measured.
		//
		// Cached so each distinct workload and pod is upserted once per cycle rather than once
		// per fact row. A 5,000-container cluster has 5,000 facts but only a few hundred pods,
		// and every avoided upsert is a saved round trip.
		//
		// Plain maps with no locking: this runs on one goroutine inside a single transaction,
		// and a transaction cannot be used concurrently anyway.
		workloadIDs := map[string]int64{}
		podIDs := map[string]int64{}

		for i := range result.Allocations {
			a := &result.Allocations[i]

			nsID, ok := namespaceIDs[a.NamespaceName]
			if !ok {
				// The namespace was collected but is no longer in the cache -- deleted between
				// collection and write. Its cost is real and already measured, so the row is
				// worth keeping rather than discarding, and a minimal dimension row is enough
				// to hang it on.
				nsID, err = inv.UpsertNamespace(ctx, clusterID, domain.Namespace{
					Name: a.NamespaceName, Team: a.Team,
					CostCentre: a.CostCentre, Environment: a.Environment,
				})
				if err != nil {
					return fmt.Errorf("upserting vanished namespace %q: %w", a.NamespaceName, err)
				}
				namespaceIDs[a.NamespaceName] = nsID
			}

			var nodeID *int64
			if a.NodeName != "" {
				// Same reasoning: a node removed by the autoscaler mid-cycle still hosted these
				// containers for the window being priced.
				id, found := nodeIDs[a.NodeName]
				if !found {
					id, err = inv.UpsertNode(ctx, clusterID, domain.Node{
						Name: a.NodeName, InstanceType: a.InstanceType,
						Zone: a.Zone, CapacityType: a.CapacityType,
					})
					if err != nil {
						return fmt.Errorf("upserting vanished node %q: %w", a.NodeName, err)
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
