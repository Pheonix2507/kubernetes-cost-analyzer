package recommend

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/store/postgres"
)

const mib = 1024 * 1024

// statsBuilder produces a valid ContainerStats so each test varies only the field it is about.
//
// Defaults clear every evidence gate -- a week of observation, 2000 windows, full peak coverage -- so a
// test that fires no recommendation has failed on its RULE rather than on its evidence.
func stats(mutate func(*postgres.ContainerStats)) postgres.ContainerStats {
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	s := postgres.ContainerStats{
		Namespace:    "team-x",
		WorkloadKind: "Deployment",
		WorkloadName: "app",
		Container:    "app",
		Replicas:     1,
		WindowCount:  2016, // a week at five-minute windows
		ObservedFrom: from,
		ObservedTo:   from.Add(7 * 24 * time.Hour),
		PeakCoverage: 1.0,
		QoSClass:     "Burstable",
		// Rates roughly matching an m5.large under the 70/30 split, so savings look plausible.
		CPUCostPerCoreHour: decimal.RequireFromString("0.0371"),
		MemCostPerGiBHour:  decimal.RequireFromString("0.003975"),
		TotalCost:          decimal.RequireFromString("1.00"),
	}
	if mutate != nil {
		mutate(&s)
	}
	return s
}

// kindsOf reduces a recommendation set to the kinds present, which is what most assertions care about.
func kindsOf(recs []Recommendation) map[Kind]Recommendation {
	out := map[Kind]Recommendation{}
	for _, r := range recs {
		out[r.Kind] = r
	}
	return out
}

// =============================================================================
// THE FIXTURE GRADING
// =============================================================================

// TestFixtureVerdicts is the test the whole fixture set was designed for.
//
// Each case mirrors one workload in deploy/demo-workloads using figures MEASURED from the running
// cluster -- and that word is load-bearing. An earlier version of this test used numbers I assumed
// were plausible (a 5m CPU peak for nginx) and it passed, while the real value was 0 because 2
// millicores of a core rounds to zero at whole-millicore resolution. The engine then classified a
// working nginx as idle and recommended deleting it, and only running it against the cluster
// revealed that. Test fixtures that encode a belief about the data test the belief, not the code.
//
// right-sized-worker must NOT be flagged, and that negative case is what makes the positive ones
// meaningful.
//
// Without it, a rule that ignored its input and always returned true would pass every positive
// assertion. False positives are the failure mode that kills adoption of a tool like this: engineers
// learn it cries wolf, stop reading it, and then it cannot help them with the real waste either.
func TestFixtureVerdicts(t *testing.T) {
	t.Parallel()

	engine := NewEngine(DefaultThresholds())

	tests := []struct {
		fixture   string
		stats     postgres.ContainerStats
		wantKinds []Kind
		notKinds  []Kind
		why       string
	}{
		{
			fixture: "over-provisioned-api",
			// MEASURED from the cluster: requests 500m/512Mi. CPU peak is 0 -- nginx uses under
			// 0.5m and our peak resolution is whole millicores -- and memory peak is 6Mi.
			//
			// Those exact numbers matter: with a 16Mi idle floor this container fell BELOW it and was
			// recommended for deletion. CPU cannot discriminate here at all, so the memory floor has
			// to, which is why it is 4Mi.
			stats: stats(func(s *postgres.ContainerStats) {
				s.WorkloadName, s.Container = "over-provisioned-api", "api"
				s.Replicas = 2
				s.CPURequestedMillicores, s.MemRequestedBytes = 500, 512*mib
				s.CPUAvgMillicores, s.CPUP95Millicores, s.CPUMaxMillicores = 0, 0, 0
				s.MemAvgBytes, s.MemP95Bytes, s.MemMaxBytes = 5*mib, 6*mib, 6*mib
			}),
			wantKinds: []Kind{KindRightSize},
			notKinds:  []Kind{KindIdle, KindUnderRequested},
			// Not idle: it peaks above the idle floor, so it does real work. Resize, do not delete --
			// and confusing the two would give dangerous advice.
			why: "reserves 500m and peaks at 5m: the commonest real waste pattern",
		},
		{
			fixture: "right-sized-worker  *** THE CONTROL ***",
			// MEASURED: requests 50m/32Mi, avg 42m, p95 38m, PEAK 51m. Note the peak EXCEEDS the
			// request -- right-sizing on the 42m average would have throttled it.
			stats: stats(func(s *postgres.ContainerStats) {
				s.WorkloadName, s.Container = "right-sized-worker", "worker"
				s.Replicas = 2
				s.CPURequestedMillicores, s.MemRequestedBytes = 50, 32*mib
				s.CPUAvgMillicores, s.CPUP95Millicores, s.CPUMaxMillicores = 42, 38, 51
				s.MemAvgBytes, s.MemP95Bytes, s.MemMaxBytes = 20*mib, 20*mib, 21*mib
			}),
			// NOTHING about its sizing may be flagged. p95 is 96% of the request -- healthy.
			notKinds: []Kind{KindRightSize, KindIdle, KindSetRequests, KindOverReplicated},
			why: "THE control case. Every other fixture is wasteful, so a rule that returned true " +
				"unconditionally would look correct on all of them. This one must stay silent",
		},
		{
			fixture: "memory-hoarder",
			// Measured: requests 64Mi, uses 201Mi. 314% of its memory request.
			stats: stats(func(s *postgres.ContainerStats) {
				s.WorkloadName, s.Container = "memory-hoarder", "hoarder"
				s.CPURequestedMillicores, s.MemRequestedBytes = 50, 64*mib
				s.CPUAvgMillicores, s.CPUP95Millicores, s.CPUMaxMillicores = 0, 0, 0
				s.MemAvgBytes, s.MemP95Bytes, s.MemMaxBytes = 201*mib, 200*mib, 201*mib
			}),
			wantKinds: []Kind{KindUnderRequested},
			why: "uses 3x its memory request. The recommendation must INCREASE cost, and be " +
				"CRITICAL because memory is incompressible -- the kernel OOMKills rather than throttles",
		},
		{
			fixture: "no-requests-at-all",
			// Measured: no requests, uses ~2m/6Mi.
			stats: stats(func(s *postgres.ContainerStats) {
				s.WorkloadName, s.Container = "no-requests-at-all", "freeloader"
				s.CPURequestedMillicores, s.MemRequestedBytes = 0, 0
				s.CPUAvgMillicores, s.CPUP95Millicores, s.CPUMaxMillicores = 0, 0, 0
				s.MemAvgBytes, s.MemP95Bytes, s.MemMaxBytes = 5*mib, 5*mib, 6*mib
				s.QoSClass = "BestEffort"
			}),
			wantKinds: []Kind{KindSetRequests},
			// Emphatically NOT right-size or idle: there is no request to reduce and no reservation to
			// reclaim, so either would be incoherent advice.
			notKinds: []Kind{KindRightSize, KindIdle},
			why: "declares nothing, so its cost is unattributable and silently smeared across every " +
				"other team",
		},
		{
			fixture: "idle-service",
			// Measured: requests 200m/256Mi, uses literally zero.
			stats: stats(func(s *postgres.ContainerStats) {
				s.WorkloadName, s.Container = "idle-service", "idle"
				s.CPURequestedMillicores, s.MemRequestedBytes = 200, 256*mib
				s.CPUAvgMillicores, s.CPUP95Millicores, s.CPUMaxMillicores = 0, 0, 0
				s.MemAvgBytes, s.MemP95Bytes, s.MemMaxBytes = 328*1024, 328*1024, 335*1024
			}),
			wantKinds: []Kind{KindIdle},
			// Reported as IDLE, not as over-provisioned, because the action differs: delete versus
			// resize. Only one recommendation should fire, or the reader gets contradictory advice.
			notKinds: []Kind{KindRightSize, KindUnderRequested},
			why:      "never exceeds the idle floor: delete, do not resize",
		},
		{
			// SIX IDLE REPLICAS -- the case this rule CAN detect.
			//
			// Note this is NOT the same as the deploy/demo-workloads/60-over-replicated.yaml fixture,
			// and that discrepancy is worth stating rather than hiding. That fixture was recalibrated in
			// Phase 0 until each pod was genuinely well utilised (measured: 11m against a 15m request,
			// 73%), specifically so no per-container rule would flag it.
			//
			// The consequence is that this rule cannot flag it either, and that is CORRECT rather than a
			// gap: if every pod is 73% utilised, removing replicas would overload the survivors. Whether
			// six busy pods could be replaced by two BIGGER pods is a question about TRAFFIC, not about
			// resource utilisation, and no CPU or memory metric can answer it. That needs request-rate
			// data -- the same traffic correlation idle detection wants.
			//
			// So the rule detects idle replicas, which is a real and common form of the problem, and the
			// harder form is honestly out of reach for now.
			fixture: "over-replicated (six IDLE replicas)",
			stats: stats(func(s *postgres.ContainerStats) {
				s.WorkloadName, s.Container = "over-replicated", "replica"
				s.Replicas = 6
				s.CPURequestedMillicores, s.MemRequestedBytes = 100, 128*mib
				s.CPUAvgMillicores, s.CPUP95Millicores, s.CPUMaxMillicores = 8, 10, 12
				s.MemAvgBytes, s.MemP95Bytes, s.MemMaxBytes = 20*mib, 21*mib, 21*mib
			}),
			wantKinds: []Kind{KindOverReplicated},
			why: "the waste is only visible at the replica count. Per-container analysis is a local " +
				"optimum -- six findings of 'nothing wrong' and one workload wasting most of its spend",
		},
		{
			// The REAL fixture as configured: 73% per-pod utilisation. Nothing may fire.
			fixture: "over-replicated as actually deployed (73% utilised)",
			stats: stats(func(s *postgres.ContainerStats) {
				s.WorkloadName, s.Container = "over-replicated", "replica"
				s.Replicas = 6
				s.CPURequestedMillicores, s.MemRequestedBytes = 15, 32*mib
				s.CPUAvgMillicores, s.CPUP95Millicores, s.CPUMaxMillicores = 11, 11, 12
				s.MemAvgBytes, s.MemP95Bytes, s.MemMaxBytes = 20*mib, 21*mib, 21*mib
			}),
			notKinds: []Kind{KindOverReplicated, KindRightSize, KindIdle},
			why: "six pods at 73% utilisation are NOT over-replicated by any resource metric: removing " +
				"replicas would overload the survivors. Detecting this case needs traffic data",
		},
	}

	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			t.Parallel()

			got := kindsOf(engine.Analyse([]postgres.ContainerStats{tt.stats}))

			for _, want := range tt.wantKinds {
				if _, found := got[want]; !found {
					t.Errorf("%s was NOT flagged as %s\nwhy this matters: %s\ngot: %v",
						tt.fixture, want, tt.why, keys(got))
				}
			}
			for _, notWant := range tt.notKinds {
				if r, found := got[notWant]; found {
					t.Errorf("%s was WRONGLY flagged as %s: %q\nwhy this matters: %s",
						tt.fixture, notWant, r.Summary, tt.why)
				}
			}
		})
	}
}

// TestRightSizedWorker_StaysSilentEvenNearTheThreshold hardens the control case.
//
// The utilisation threshold is 0.5. This walks the band just above it, because a rule that fires at
// 0.55 utilisation would flag most healthy workloads in a real cluster -- and would still pass the
// grading test above, which only checks one point.
func TestRightSizedWorker_StaysSilentEvenNearTheThreshold(t *testing.T) {
	t.Parallel()

	engine := NewEngine(DefaultThresholds())

	for _, utilisation := range []float64{0.51, 0.6, 0.7, 0.8, 0.9, 0.96} {
		s := stats(func(s *postgres.ContainerStats) {
			s.CPURequestedMillicores = 100
			s.CPUP95Millicores = int64(100 * utilisation)
			s.CPUMaxMillicores = s.CPUP95Millicores + 2
			s.MemRequestedBytes = 100 * mib
			s.MemP95Bytes = int64(100 * mib * utilisation)
			s.MemMaxBytes = s.MemP95Bytes
		})

		got := kindsOf(engine.Analyse([]postgres.ContainerStats{s}))
		if r, found := got[KindRightSize]; found {
			t.Errorf("flagged a container at %.0f%% utilisation as over-provisioned: %q\n"+
				"a rule that fires here would flag most healthy workloads in a real cluster",
				utilisation*100, r.Proposed)
		}
	}
}

// =============================================================================
// The evidence gate
// =============================================================================

// TestEvidenceGate_SilenceOnThinData is the safety property that matters most.
//
// Without it the engine would recommend deleting a service from twenty minutes of data -- and a
// nightly batch job looks completely idle for twenty-three hours out of twenty-four.
func TestEvidenceGate_SilenceOnThinData(t *testing.T) {
	t.Parallel()

	engine := NewEngine(DefaultThresholds())
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	// Egregiously over-provisioned, so ONLY the gate can be suppressing it.
	obvious := func(mutate func(*postgres.ContainerStats)) postgres.ContainerStats {
		return stats(func(s *postgres.ContainerStats) {
			s.CPURequestedMillicores, s.MemRequestedBytes = 2000, 2048*mib
			s.CPUAvgMillicores, s.CPUP95Millicores, s.CPUMaxMillicores = 1, 1, 2
			s.MemAvgBytes, s.MemP95Bytes, s.MemMaxBytes = mib, mib, 2*mib
			mutate(s)
		})
	}

	tests := []struct {
		name  string
		stats postgres.ContainerStats
		why   string
	}{
		{
			name: "too few windows",
			stats: obvious(func(s *postgres.ContainerStats) {
				s.WindowCount = 3
			}),
			why: "three samples is not a pattern",
		},
		{
			name: "observation span too short",
			stats: obvious(func(s *postgres.ContainerStats) {
				s.ObservedFrom, s.ObservedTo = from, from.Add(20*time.Minute)
				s.WindowCount = 500 // plenty of windows, but all within 20 minutes
			}),
			why: "500 windows over 20 minutes still only tells you about those 20 minutes; the " +
				"pattern that matters is the daily cycle",
		},
		{
			name: "peak data missing",
			stats: obvious(func(s *postgres.ContainerStats) {
				s.PeakCoverage = 0.2
			}),
			why: "rows predating migration 000003 carry max = 0. Treating that as a genuine zero peak " +
				"would make every historical container look idle and produce a flood of confident " +
				"deletion advice",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := engine.Analyse([]postgres.ContainerStats{tt.stats})
			if len(got) != 0 {
				t.Errorf("produced %d recommendations from insufficient evidence: %v\nwhy: %s",
					len(got), keys(kindsOf(got)), tt.why)
			}
		})
	}
}

// TestIdle_NeedsALongerWindowThanOtherRules covers the asymmetry. "Delete this" is the most
// destructive advice the engine gives, so it demands the most evidence.
func TestIdle_NeedsALongerWindowThanOtherRules(t *testing.T) {
	t.Parallel()

	engine := NewEngine(DefaultThresholds())
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	idleButBriefly := stats(func(s *postgres.ContainerStats) {
		s.ObservedFrom, s.ObservedTo = from, from.Add(2*time.Hour)
		s.WindowCount = 24
		s.CPURequestedMillicores, s.MemRequestedBytes = 200, 256*mib
		s.CPUAvgMillicores, s.CPUP95Millicores, s.CPUMaxMillicores = 0, 0, 0
		s.MemAvgBytes, s.MemP95Bytes, s.MemMaxBytes = 300*1024, 300*1024, 300*1024
	})

	got := kindsOf(engine.Analyse([]postgres.ContainerStats{idleButBriefly}))
	if _, found := got[KindIdle]; found {
		t.Error("recommended deletion from 2 hours of data; a workload idle for two hours may " +
			"simply be between batches")
	}
	// It should still be reported as over-provisioned, which is safe advice on the same evidence.
	if _, found := got[KindRightSize]; !found {
		t.Error("did not flag it as over-provisioned either; a 2-hour window is enough for a resize " +
			"recommendation even though it is not enough for a deletion")
	}
}

// =============================================================================
// The dangerous direction
// =============================================================================

// proposedCPUFromString recovers the millicore figure out of a Proposed string such as
// "cpu: 120m, memory: 12Mi".
//
// WHY PARSE THE STRING RATHER THAN READ A FIELD
// ---------------------------------------------
// The string IS the deliverable. A human copies it into a manifest, so a correct internal value that
// formats wrongly is still an incident -- and a test that read the field would pass while the thing
// someone actually pastes was wrong. Testing the rendered artefact is the whole point.
func proposedCPUFromString(t *testing.T, proposed string) int64 {
	t.Helper()

	for _, part := range strings.Split(proposed, ",") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(part, "cpu: ") {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(part, "cpu: "), "m"), 10, 64)
		if err != nil {
			t.Fatalf("cannot parse the cpu figure out of %q: %v", proposed, err)
		}
		return n
	}
	t.Fatalf("no cpu figure in %q", proposed)
	return 0
}

// TestRightSize_NeverProposesBelowThePeak is the property that stops this tool causing an incident.
//
// A proposal below the observed peak means throttling or an OOM kill. p95 x headroom must always leave
// room above p95, and the proposal must never exceed the current request either -- an "optimisation"
// that increased the request would be incoherent.
//
// An earlier version of this test computed the lower bound and then never compared anything to it, so
// the only assertions left were "the evidence echoes the input" and "the proposal is not empty". It
// passed, it was named after the safety property, and it did not test the safety property at all. The
// linter caught the unused variable; the misleading name is what actually mattered.
func TestRightSize_NeverProposesBelowThePeak(t *testing.T) {
	t.Parallel()

	th := DefaultThresholds()
	engine := NewEngine(th)

	// A wide spread including 1m, where the millicore floor rather than the headroom factor decides.
	for _, p95 := range []int64{1, 5, 17, 100, 499} {
		s := stats(func(s *postgres.ContainerStats) {
			s.CPURequestedMillicores = 2000
			s.CPUP95Millicores = p95
			s.CPUMaxMillicores = p95 + 10
			s.MemRequestedBytes = 1024 * mib
			s.MemP95Bytes = 10 * mib
			s.MemMaxBytes = 12 * mib
		})

		recs := engine.Analyse([]postgres.ContainerStats{s})
		got, found := kindsOf(recs)[KindRightSize]
		if !found {
			t.Fatalf("p95=%d was not flagged; the fixture is meant to be egregious", p95)
		}
		if got.Evidence.CPUP95Millicores != p95 {
			t.Errorf("evidence p95 = %d, want %d", got.Evidence.CPUP95Millicores, p95)
		}

		proposed := proposedCPUFromString(t, got.Proposed)

		// THE SAFETY PROPERTY. p95 means 5% of windows exceeded it, so a proposal AT p95 throttles one
		// window in twenty. It must clear p95 by the headroom factor -- or by the 1m floor when the
		// headroom multiple rounds back down to p95, which is exactly what happens at p95=1.
		wantAtLeast := int64(float64(p95) * th.HeadroomFactor)
		if wantAtLeast <= p95 {
			wantAtLeast = p95 + 1
		}
		if proposed < wantAtLeast {
			t.Errorf("p95=%d proposed %dm, want at least %dm: sizing at or below p95 means CFS "+
				"throttling in the windows that exceeded it", p95, proposed, wantAtLeast)
		}

		// And never upwards. Right-sizing is a REDUCTION; a rule that raised the request has confused
		// itself with under_requested, and the saving figure would then be negative under a kind that
		// promises a saving.
		if proposed > s.CPURequestedMillicores {
			t.Errorf("p95=%d proposed %dm against a %dm request: right_size must only ever reduce",
				p95, proposed, s.CPURequestedMillicores)
		}
		if got.EstimatedMonthlySaving.IsNegative() {
			t.Errorf("p95=%d: right_size reported a NEGATIVE saving (%s), which contradicts its kind",
				p95, got.EstimatedMonthlySaving)
		}
	}
}

// TestOverReplicated_OnlyForKindsWithATunableReplicaCount is a REGRESSION TEST for advice the tool
// gave about a field that does not exist.
//
// The first live run of the recommendations endpoint produced, at warning severity:
//
//	kindnet                       over_replicated  scale to 2 replicas   saving 1.2644
//	kps-prometheus-node-exporter  over_replicated  scale to 2 replicas   saving 0.1911
//
// Both are DaemonSets running one pod per node on a three-node cluster. There is no replicas field to
// set. And had there been, acting on it would have left a node with no network plugin and no metrics
// exporter -- so the tool was confidently recommending an outage in exchange for 1.26 dollars a month.
//
// The rule read `count(DISTINCT pod_name) = 3` and never asked why there were three. For a Deployment
// that is a decision; for a DaemonSet it is the node count. The identical number carries opposite
// meanings, which is a good reminder that an aggregate is not self-describing.
//
// Note what is asserted for the DaemonSet: silence from THIS rule, not silence overall. An
// over-provisioned DaemonSet still gets a right_size finding on its per-pod requests, which is the
// change someone can actually make.
func TestOverReplicated_OnlyForKindsWithATunableReplicaCount(t *testing.T) {
	t.Parallel()

	engine := NewEngine(DefaultThresholds())

	// Six lightly-used replicas: egregious enough that only the KIND can decide the outcome.
	lightlyUsed := func(kind string) postgres.ContainerStats {
		return stats(func(s *postgres.ContainerStats) {
			s.WorkloadKind = kind
			s.Replicas = 6
			s.CPURequestedMillicores, s.MemRequestedBytes = 100, 128*mib
			s.CPUAvgMillicores, s.CPUP95Millicores, s.CPUMaxMillicores = 8, 10, 12
			s.MemAvgBytes, s.MemP95Bytes, s.MemMaxBytes = 20*mib, 21*mib, 21*mib
		})
	}

	tests := []struct {
		kind      string
		wantFires bool
		why       string
	}{
		{"Deployment", true, "replicas is a field an operator chose and can change"},
		{"ReplicaSet", true, "same, one level down"},
		{"StatefulSet", true, "replicas is tunable, though ordinal identity makes it a bigger decision"},
		{"DaemonSet", false, "one pod per node. THE BUG: advised scaling kindnet to 2 replicas"},
		{"Job", false, "parallelism is the job's own contract and the pods are finite"},
		{"CronJob", false, "same, per invocation"},
		{"Node", false, "how static pods surface: owned by the Node, with no controller to scale"},
		{"", false, "a bare pod has no controller, so there is nothing to change"},
		{"Rollout", false, "an unknown CRD must default to SILENT. An allow-list fails by missing a " +
			"finding; a deny-list would fail by inventing advice about someone else's resource"},
	}

	for _, tt := range tests {
		name := tt.kind
		if name == "" {
			name = "(bare pod)"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, fired := kindsOf(engine.Analyse([]postgres.ContainerStats{lightlyUsed(tt.kind)}))[KindOverReplicated]
			if fired != tt.wantFires {
				t.Errorf("over_replicated fired = %v for kind %q, want %v\nwhy: %s",
					fired, tt.kind, tt.wantFires, tt.why)
			}
		})
	}
}

// TestOverReplicated_DaemonSetStillGetsRightSized is the other half of the gate above.
//
// Suppressing over_replicated must not suppress the finding a DaemonSet CAN act on. An over-provisioned
// DaemonSet is a real and common waste pattern -- it is multiplied by every node in the cluster -- and
// the fix is smaller per-pod requests, not fewer pods.
func TestOverReplicated_DaemonSetStillGetsRightSized(t *testing.T) {
	t.Parallel()

	recs := NewEngine(DefaultThresholds()).Analyse([]postgres.ContainerStats{
		stats(func(s *postgres.ContainerStats) {
			s.WorkloadKind, s.WorkloadName = "DaemonSet", "kindnet"
			s.Replicas = 3
			s.CPURequestedMillicores, s.MemRequestedBytes = 100, 128*mib
			s.CPUAvgMillicores, s.CPUP95Millicores, s.CPUMaxMillicores = 8, 10, 12
			s.MemAvgBytes, s.MemP95Bytes, s.MemMaxBytes = 20*mib, 21*mib, 22*mib
		}),
	})

	byKind := kindsOf(recs)
	if _, fired := byKind[KindOverReplicated]; fired {
		t.Error("over_replicated fired for a DaemonSet")
	}
	if _, fired := byKind[KindRightSize]; !fired {
		t.Error("no right_size finding for an over-provisioned DaemonSet. Gating the replica rule must " +
			"not silence the recommendation that IS actionable -- and a DaemonSet's waste is multiplied " +
			"by every node in the cluster")
	}
}

// TestProposedCPU_HeadroomSurvivesSmallValues is a REGRESSION TEST for the bug the honest version of
// TestRightSize_NeverProposesBelowThePeak found.
//
// proposedCPU multiplied p95 by the headroom factor and converted to int64, which TRUNCATES toward
// zero. At the 1.2 default that meant p95 values of 1 through 4 millicores got a proposal exactly
// equal to p95 -- no headroom at all -- because 1.2, 2.4, 3.6 and 4.8 all truncate back down.
//
// That band is not exotic. Every quiet container in this cluster lives in it: an idling nginx measures
// 0-2 millicores. So the tool would have proposed a request equal to p95 for the majority of the
// containers it had anything to say about, and p95 means 5% of windows exceeded it.
//
// The lesson is not "remember to use Ceil". It is that a safety margin rounded DOWN is not a safety
// margin, so the rounding direction has to be chosen deliberately rather than inherited from whatever
// a type conversion happens to do.
func TestProposedCPU_HeadroomSurvivesSmallValues(t *testing.T) {
	t.Parallel()

	e := NewEngine(DefaultThresholds())

	// The whole band where truncation used to bite, plus 5 as the first value that survived it.
	for _, p95 := range []int64{1, 2, 3, 4, 5} {
		got := e.proposedCPU(stats(func(s *postgres.ContainerStats) { s.CPUP95Millicores = p95 }))
		if got <= p95 {
			t.Errorf("proposedCPU(p95=%dm) = %dm, which is not ABOVE p95. Truncation ate the "+
				"headroom: %d x %.1f = %.1f, and int64() rounds that back to %d",
				p95, got, p95, DefaultThresholds().HeadroomFactor,
				float64(p95)*DefaultThresholds().HeadroomFactor, got)
		}
	}
}

// TestProposedCPU_HasHeadroomAndAFloor covers the arithmetic directly.
func TestProposedCPU_HasHeadroomAndAFloor(t *testing.T) {
	t.Parallel()

	e := NewEngine(DefaultThresholds())

	// Headroom above p95, always.
	if got := e.proposedCPU(stats(func(s *postgres.ContainerStats) { s.CPUP95Millicores = 100 })); got <= 100 {
		t.Errorf("proposedCPU for p95=100 is %d, which is not above p95; p95 means 5%% of windows "+
			"EXCEEDED it, so sizing at p95 throttles one window in twenty", got)
	}
	// Floored at 1, never 0. A request of 0 means BestEffort, so a right-size recommendation would
	// turn into the setRequests problem.
	if got := e.proposedCPU(stats(func(s *postgres.ContainerStats) { s.CPUP95Millicores = 0 })); got < 1 {
		t.Errorf("proposedCPU for p95=0 is %d; a request of 0 means BestEffort", got)
	}
}

// TestProposedMemory_RoundsUpAndHasAFloor covers a usability detail with a correctness edge.
func TestProposedMemory_RoundsUpAndHasAFloor(t *testing.T) {
	t.Parallel()

	e := NewEngine(DefaultThresholds())

	// Rounded to a whole MiB, because a manifest is written in MiB and "37.4Mi" cannot be pasted.
	got := e.proposedMemory(stats(func(s *postgres.ContainerStats) { s.MemP95Bytes = 31*mib + 700*1024 }))
	if got%mib != 0 {
		t.Errorf("proposedMemory = %d bytes, which is not a whole number of MiB", got)
	}
	// Rounded UP, never down: down would propose less than p95 x headroom.
	if got < int64(float64(31*mib+700*1024)*DefaultThresholds().HeadroomFactor) {
		t.Errorf("proposedMemory = %d, which is below p95 x headroom", got)
	}
	// Floored at 16Mi: no container survives on less, and proposing below it causes an immediate OOM.
	if got := e.proposedMemory(stats(func(s *postgres.ContainerStats) { s.MemP95Bytes = 1024 })); got < 16*mib {
		t.Errorf("proposedMemory for a tiny p95 is %d, below the 16Mi floor", got)
	}
}

// =============================================================================
// Severity, sign and ordering
// =============================================================================

// TestUnderRequested_MemoryIsCriticalCPUIsWarning covers the asymmetry at the heart of that rule.
// CPU is compressible (throttled, slower); memory is not (evicted, OOMKilled).
func TestUnderRequested_MemoryIsCriticalCPUIsWarning(t *testing.T) {
	t.Parallel()

	engine := NewEngine(DefaultThresholds())

	cpuOnly := stats(func(s *postgres.ContainerStats) {
		s.CPURequestedMillicores, s.CPUP95Millicores, s.CPUMaxMillicores = 100, 150, 160
		s.MemRequestedBytes, s.MemP95Bytes, s.MemMaxBytes = 256*mib, 100*mib, 110*mib
	})
	memToo := stats(func(s *postgres.ContainerStats) {
		s.CPURequestedMillicores, s.CPUP95Millicores, s.CPUMaxMillicores = 100, 150, 160
		s.MemRequestedBytes, s.MemP95Bytes, s.MemMaxBytes = 64*mib, 200*mib, 210*mib
	})

	cpuRec := kindsOf(engine.Analyse([]postgres.ContainerStats{cpuOnly}))[KindUnderRequested]
	if cpuRec.Severity != SeverityWarning {
		t.Errorf("CPU-only under-request severity = %q, want warning: exceeding a CPU request is "+
			"throttling, which is a performance problem", cpuRec.Severity)
	}

	memRec := kindsOf(engine.Analyse([]postgres.ContainerStats{memToo}))[KindUnderRequested]
	if memRec.Severity != SeverityCritical {
		t.Errorf("memory under-request severity = %q, want critical: memory is incompressible, so "+
			"the kernel OOMKills rather than slowing it down", memRec.Severity)
	}
}

// TestUnderRequested_SavingIsNegative covers the property that makes this tool credible. A tool whose
// advice is all one-directional is not trusted with production.
func TestUnderRequested_SavingIsNegative(t *testing.T) {
	t.Parallel()

	engine := NewEngine(DefaultThresholds())
	s := stats(func(s *postgres.ContainerStats) {
		s.MemRequestedBytes, s.MemP95Bytes, s.MemMaxBytes = 64*mib, 200*mib, 210*mib
	})

	rec := kindsOf(engine.Analyse([]postgres.ContainerStats{s}))[KindUnderRequested]
	if !rec.EstimatedMonthlySaving.IsNegative() {
		t.Errorf("saving = %s, want NEGATIVE: raising a request costs money, and presenting that "+
			"honestly is what makes the positive savings credible", rec.EstimatedMonthlySaving)
	}
}

// TestAnalyse_OrdersBySeverityNotBySaving covers the sort. Ordering by money would bury an imminent
// OOM kill beneath a list of modest efficiencies.
func TestAnalyse_OrdersBySeverityNotBySaving(t *testing.T) {
	t.Parallel()

	engine := NewEngine(DefaultThresholds())

	// A large saving at info severity, and a small cost INCREASE at critical.
	bigSaving := stats(func(s *postgres.ContainerStats) {
		s.WorkloadName, s.Container = "wasteful", "app"
		s.CPURequestedMillicores, s.MemRequestedBytes = 4000, 4096*mib
		s.CPUAvgMillicores, s.CPUP95Millicores, s.CPUMaxMillicores = 10, 20, 25
		s.MemAvgBytes, s.MemP95Bytes, s.MemMaxBytes = 50*mib, 60*mib, 65*mib
	})
	oomRisk := stats(func(s *postgres.ContainerStats) {
		s.WorkloadName, s.Container = "at-risk", "app"
		s.CPURequestedMillicores, s.CPUP95Millicores, s.CPUMaxMillicores = 100, 50, 60
		s.MemRequestedBytes, s.MemP95Bytes, s.MemMaxBytes = 64*mib, 200*mib, 210*mib
	})

	recs := engine.Analyse([]postgres.ContainerStats{bigSaving, oomRisk})
	if len(recs) < 2 {
		t.Fatalf("got %d recommendations, want at least 2", len(recs))
	}
	if recs[0].Severity != SeverityCritical {
		t.Errorf("first recommendation is %q severity (%s), want critical first: a large saving "+
			"must not bury an imminent OOM kill", recs[0].Severity, recs[0].Kind)
	}
}

// TestOverReplicated_NeverProposesBelowTwo covers the availability floor. A single replica has no
// availability during a rolling update or a node drain, so recommending a scale to 1 trades a small
// saving for an outage window.
func TestOverReplicated_NeverProposesBelowTwo(t *testing.T) {
	t.Parallel()

	engine := NewEngine(DefaultThresholds())

	for _, replicas := range []int{3, 6, 20, 50} {
		s := stats(func(s *postgres.ContainerStats) {
			s.Replicas = replicas
			s.CPURequestedMillicores, s.MemRequestedBytes = 1000, 128*mib
			// Essentially no load, so the arithmetic would happily propose 1.
			s.CPUAvgMillicores, s.CPUP95Millicores, s.CPUMaxMillicores = 1, 1, 2
			s.MemAvgBytes, s.MemP95Bytes, s.MemMaxBytes = mib, mib, 2*mib
		})

		rec, found := kindsOf(engine.Analyse([]postgres.ContainerStats{s}))[KindOverReplicated]
		if !found {
			t.Errorf("%d nearly-idle replicas were not flagged", replicas)
			continue
		}
		if rec.Proposed == "1 replicas" {
			t.Errorf("proposed scaling %d replicas to 1; a single replica has no availability during "+
				"a rolling update or a node drain", replicas)
		}
	}
}

// TestOverReplicated_NotFlaggedAtTheFloor covers the lower bound. A 2-replica workload is already
// minimal, so there is nothing to propose.
func TestOverReplicated_NotFlaggedAtTheFloor(t *testing.T) {
	t.Parallel()

	engine := NewEngine(DefaultThresholds())
	s := stats(func(s *postgres.ContainerStats) {
		s.Replicas = 2
		s.CPURequestedMillicores, s.CPUP95Millicores, s.CPUMaxMillicores = 1000, 1, 2
		s.MemRequestedBytes, s.MemP95Bytes, s.MemMaxBytes = 128*mib, mib, 2*mib
	})

	if _, found := kindsOf(engine.Analyse([]postgres.ContainerStats{s}))[KindOverReplicated]; found {
		t.Error("flagged a 2-replica workload; that is already the availability minimum")
	}
}

// TestMinSavingRatio_SuppressesNoise covers why a threshold exists at all. A list of forty marginal
// recommendations gets ignored wholesale, taking the one worth acting on with it.
func TestMinSavingRatio_SuppressesNoise(t *testing.T) {
	t.Parallel()

	engine := NewEngine(DefaultThresholds())

	// 48% utilisation: just inside the right-size threshold, so the saving is small.
	marginal := stats(func(s *postgres.ContainerStats) {
		s.CPURequestedMillicores, s.CPUP95Millicores, s.CPUMaxMillicores = 100, 48, 50
		s.MemRequestedBytes, s.MemP95Bytes, s.MemMaxBytes = 100*mib, 48*mib, 50*mib
	})

	recs := engine.Analyse([]postgres.ContainerStats{marginal})
	for _, r := range recs {
		if r.Kind == KindRightSize {
			// A ~40% reduction IS worth reporting, so this documents the behaviour rather than
			// forbidding it. The assertion is that the saving is real, not trivial.
			if r.EstimatedMonthlySaving.IsZero() || r.EstimatedMonthlySaving.IsNegative() {
				t.Errorf("right-size fired with a non-positive saving of %s", r.EstimatedMonthlySaving)
			}
		}
	}
}

// TestConfidence_TracksObservationSpan covers why span rather than window count. A hundred windows
// over an hour still only tells you about that hour.
func TestConfidence_TracksObservationSpan(t *testing.T) {
	t.Parallel()

	e := NewEngine(DefaultThresholds())
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		span time.Duration
		want Confidence
		why  string
	}{
		{2 * time.Hour, ConfidenceLow, "two hours misses the daily cycle entirely"},
		{25 * time.Hour, ConfidenceMedium, "a day covers the daily peak, the pattern that matters most"},
		{8 * 24 * time.Hour, ConfidenceHigh, "a week covers weekday and weekend patterns"},
	}

	for _, tt := range tests {
		s := stats(func(s *postgres.ContainerStats) {
			s.ObservedFrom, s.ObservedTo = from, from.Add(tt.span)
		})
		if got := e.confidenceFor(s); got != tt.want {
			t.Errorf("confidence over %v = %q, want %q\nwhy: %s", tt.span, got, tt.want, tt.why)
		}
	}
}

// TestNewEngine_RejectsDangerousThresholds covers the zero-value trap. A zero MinWindows would disable
// the evidence floor entirely -- the most dangerous possible default -- and a headroom factor below 1
// would propose requests BELOW the observed p95.
func TestNewEngine_RejectsDangerousThresholds(t *testing.T) {
	t.Parallel()

	e := NewEngine(Thresholds{}) // everything zero
	d := DefaultThresholds()

	if e.thresholds.MinWindows != d.MinWindows {
		t.Errorf("MinWindows = %d with a zero-value config, want the default %d; zero would disable "+
			"the evidence floor", e.thresholds.MinWindows, d.MinWindows)
	}
	if e.thresholds.HeadroomFactor < 1 {
		t.Errorf("HeadroomFactor = %v, want at least 1; below 1 proposes a request BELOW the "+
			"observed p95", e.thresholds.HeadroomFactor)
	}
	if e.thresholds.MinReplicasToKeep < 1 {
		t.Errorf("MinReplicasToKeep = %d, want at least 1", e.thresholds.MinReplicasToKeep)
	}

	// And an explicitly dangerous headroom is corrected rather than honoured.
	e2 := NewEngine(Thresholds{HeadroomFactor: 0.5})
	if e2.thresholds.HeadroomFactor < 1 {
		t.Errorf("HeadroomFactor 0.5 was accepted; it would propose requests below the observed peak")
	}
}

func keys(m map[Kind]Recommendation) []Kind {
	out := make([]Kind, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
