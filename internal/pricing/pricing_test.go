package pricing

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/domain"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func dec(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("bad decimal %q: %v", s, err)
	}
	return d
}

// minimalCatalogue is the smallest valid catalogue, so each test varies only what it is about.
const minimalCatalogue = `
version: 1
currency: USD
split:
  cpu: 0.70
  memory: 0.30
default_region: ap-south-1
regions:
  ap-south-1:
    instance_types:
      m5.large:
        vcpu: 2
        memory_gib: 8
        hourly: "0.1060"
        spot_hourly: "0.0318"
fallback:
  vcpu: 2
  memory_gib: 8
  hourly: "0.2000"
`

func loadTestProvider(t *testing.T, yaml string) *CatalogueProvider {
	t.Helper()
	c, err := LoadCatalogue(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("LoadCatalogue: %v", err)
	}
	return NewCatalogueProvider(c, testLogger())
}

// =============================================================================
// THE INVARIANT
// =============================================================================

// TestWholeNodeForOneHourCostsTheNodeHourlyPrice is the most important test in this package.
//
// It is the only check that proves the CPU/memory split is COHERENT rather than merely
// plausible. Reserving every core and every byte of a node for one hour must cost exactly
// what the node costs for one hour:
//
//	vcpu * (price * cpu_share / vcpu) + gib * (price * mem_share / gib)
//	  = price * (cpu_share + mem_share) = price
//
// If this fails, the shares do not sum to 1, or a divisor is wrong, or a unit conversion is
// wrong -- and every cost the system reports is proportionally wrong in a way no other test
// would reveal, because all the individual numbers would still look reasonable.
//
// It is asserted across several instance shapes, because a bug in the divisor (using node
// capacity instead of the catalogue's vCPU, say) can happen to cancel out for one shape.
func TestWholeNodeForOneHourCostsTheNodeHourlyPrice(t *testing.T) {
	t.Parallel()

	cases := []struct {
		instanceType string
		vcpu         int64 // whole cores
		gib          int64
		hourly       string
	}{
		{"m5.large", 2, 8, "0.1060"},
		{"m5.xlarge", 4, 16, "0.2120"},
		{"r5.large", 2, 16, "0.1400"}, // memory-heavy
		{"c5.large", 2, 4, "0.0940"},  // cpu-heavy
		{"odd.3xcpu", 3, 7, "0.1234"}, // deliberately indivisible, to expose rounding
		{"big", 96, 384, "4.6080"},    // large, to check nothing overflows
	}

	for _, c := range cases {
		t.Run(c.instanceType, func(t *testing.T) {
			t.Parallel()

			price := InstancePrice{
				Name: c.instanceType, VCPU: float64(c.vcpu), MemoryGiB: float64(c.gib),
				Hourly: dec(t, c.hourly),
			}
			rates, err := deriveRates(price,
				decimal.NewFromFloat(0.70), decimal.NewFromFloat(0.30), "USD", SourceCatalogue)
			if err != nil {
				t.Fatalf("deriveRates: %v", err)
			}

			// Reserve the ENTIRE node for exactly one hour.
			got := Cost(rates, c.vcpu*1000, c.gib*(1<<30), time.Hour).Total()
			want := dec(t, c.hourly)

			// A TOLERANCE, not an exact equality, and being honest about why.
			//
			// The rates are rounded to 10 decimal places to match numeric(20,10) in the
			// schema. For a divisor that does not divide evenly -- 3 vCPU, 7 GiB -- that
			// rounding is real and cannot be recovered by multiplying back. Asserting exact
			// equality would mean choosing only shapes that happen to divide cleanly, which
			// would prove nothing about the general case.
			//
			// 1e-8 per node-hour is about one ten-millionth of a cent. Across a 1,000-node
			// fleet for a year that is under a cent in total, which is well inside what any
			// invoice reconciliation tolerates.
			diff := got.Sub(want).Abs()
			if diff.GreaterThan(dec(t, "0.00000001")) {
				t.Errorf("whole node for 1h = %s, want %s (diff %s)\n"+
					"the CPU/memory split does not reconcile: every cost this system reports "+
					"is proportionally wrong", got, want, diff)
			}
		})
	}
}

// TestSplitSharesDetermineTheProportion checks the shares actually do what the catalogue
// claims: with 70/30, a full node's cost really is 70% CPU and 30% memory.
func TestSplitSharesDetermineTheProportion(t *testing.T) {
	t.Parallel()

	price := InstancePrice{Name: "m5.large", VCPU: 2, MemoryGiB: 8, Hourly: dec(t, "1.0000")}
	rates, err := deriveRates(price, decimal.NewFromFloat(0.70), decimal.NewFromFloat(0.30), "USD", SourceCatalogue)
	if err != nil {
		t.Fatalf("deriveRates: %v", err)
	}

	cost := Cost(rates, 2000, 8*(1<<30), time.Hour)
	if !cost.CPU.Equal(dec(t, "0.7")) {
		t.Errorf("CPU share of a $1.00 node-hour = %s, want 0.7", cost.CPU)
	}
	if !cost.Memory.Equal(dec(t, "0.3")) {
		t.Errorf("memory share of a $1.00 node-hour = %s, want 0.3", cost.Memory)
	}
}

// =============================================================================
// Cost arithmetic
// =============================================================================

func TestCost(t *testing.T) {
	t.Parallel()

	// m5.large at 0.1060/hr, 2 vCPU, 8 GiB, split 70/30:
	//   cpu rate = 0.1060 * 0.7 / 2 = 0.0371 per core-hour
	//   mem rate = 0.1060 * 0.3 / 8 = 0.0039750 per GiB-hour
	rates := Rates{
		Currency:         "USD",
		NodeHourly:       dec(t, "0.1060"),
		CPUPerCoreHour:   dec(t, "0.0371"),
		MemoryPerGiBHour: dec(t, "0.0039750"),
	}

	tests := []struct {
		name       string
		milli      int64
		bytes      int64
		d          time.Duration
		wantCPU    string
		wantMemory string
		why        string
	}{
		{
			name: "one core one hour", milli: 1000, bytes: 0, d: time.Hour,
			wantCPU: "0.0371", wantMemory: "0",
			why: "the rate is defined per core-hour, so this must be exactly the rate",
		},
		{
			name: "half a core for half an hour", milli: 500, bytes: 0, d: 30 * time.Minute,
			wantCPU: "0.009275", wantMemory: "0",
			why: "cost is linear in BOTH quantity and time: 0.5 * 0.5 * 0.0371",
		},
		{
			name: "one GiB one hour", milli: 0, bytes: 1 << 30, d: time.Hour,
			wantCPU: "0", wantMemory: "0.003975",
			why: "1 GiB is 2^30 bytes -- the binary gibibyte Kubernetes reports, not 10^9",
		},
		{
			name: "a five-minute collection window", milli: 500, bytes: 512 << 20, d: 5 * time.Minute,
			// 0.5 core * (5/60) h * 0.0371 = 0.00154583...
			// 0.5 GiB  * (5/60) h * 0.003975 = 0.000165625
			wantCPU: "0.0015458333", wantMemory: "0.0001656250",
			why: "the real shape of a collector sample; rounded to 10dp to match numeric(20,10)",
		},
		{
			name: "zero duration", milli: 500, bytes: 1 << 30, d: 0,
			wantCPU: "0", wantMemory: "0",
			why: "a zero-length window has no cost",
		},
		{
			name: "negative duration", milli: 500, bytes: 1 << 30, d: -time.Hour,
			wantCPU: "0", wantMemory: "0",
			why: "clock skew must not produce a NEGATIVE cost that quietly reduces a bill",
		},
		{
			name: "BestEffort container reserving nothing", milli: 0, bytes: 0, d: time.Hour,
			wantCPU: "0", wantMemory: "0",
			why: "zero here is correct. It is the reason cost is billed on max(request, usage): " +
				"the collector passes the BILLABLE amount, not the request",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Cost(rates, tt.milli, tt.bytes, tt.d)
			if !got.CPU.Equal(dec(t, tt.wantCPU)) {
				t.Errorf("CPU = %s, want %s\nwhy: %s", got.CPU, tt.wantCPU, tt.why)
			}
			if !got.Memory.Equal(dec(t, tt.wantMemory)) {
				t.Errorf("Memory = %s, want %s\nwhy: %s", got.Memory, tt.wantMemory, tt.why)
			}
		})
	}
}

// TestCost_IsExactNotFloating is the money-precision test. Repeatedly adding a value that
// float64 cannot represent must accumulate exactly.
func TestCost_IsExactNotFloating(t *testing.T) {
	t.Parallel()

	// 0.1 per core-hour is the canonical float trap: 0.1 is not representable in binary.
	rates := Rates{CPUPerCoreHour: dec(t, "0.1"), MemoryPerGiBHour: decimal.Zero}

	total := decimal.Zero
	for i := 0; i < 1000; i++ {
		total = total.Add(Cost(rates, 1000, 0, time.Hour).CPU)
	}

	// In float64, summing 0.1 a thousand times gives 99.99999999999859, not 100.
	if !total.Equal(dec(t, "100")) {
		t.Errorf("1000 x 0.1 = %s, want exactly 100. Floating point has crept into the "+
			"money path, and the error will compound across millions of fact rows", total)
	}
}

// =============================================================================
// Provider behaviour
// =============================================================================

func TestRatesFor_CatalogueMatch(t *testing.T) {
	t.Parallel()
	p := loadTestProvider(t, minimalCatalogue)

	rates, err := p.RatesFor(context.Background(), domain.Node{
		Name: "w1", InstanceType: "m5.large", Region: "ap-south-1", CapacityType: "on-demand",
	})
	if err != nil {
		t.Fatalf("RatesFor: %v", err)
	}

	if rates.Source != SourceCatalogue {
		t.Errorf("Source = %q, want %q", rates.Source, SourceCatalogue)
	}
	if rates.InstanceType != "m5.large" {
		t.Errorf("InstanceType = %q, want m5.large", rates.InstanceType)
	}
	if !rates.NodeHourly.Equal(dec(t, "0.1060")) {
		t.Errorf("NodeHourly = %s, want 0.1060", rates.NodeHourly)
	}
	// 0.1060 * 0.7 / 2
	if !rates.CPUPerCoreHour.Equal(dec(t, "0.0371")) {
		t.Errorf("CPUPerCoreHour = %s, want 0.0371", rates.CPUPerCoreHour)
	}
	// 0.1060 * 0.3 / 8
	if !rates.MemoryPerGiBHour.Equal(dec(t, "0.003975")) {
		t.Errorf("MemoryPerGiBHour = %s, want 0.003975", rates.MemoryPerGiBHour)
	}
	if rates.Currency != "USD" {
		t.Errorf("Currency = %q, want USD", rates.Currency)
	}
}

// TestRatesFor_SpotIsCheaper covers the case that would otherwise overstate spot cost
// several-fold -- and make spot look like a problem in the very report meant to recommend it.
func TestRatesFor_SpotIsCheaper(t *testing.T) {
	t.Parallel()
	p := loadTestProvider(t, minimalCatalogue)
	ctx := context.Background()

	onDemand, err := p.RatesFor(ctx, domain.Node{
		Name: "w1", InstanceType: "m5.large", Region: "ap-south-1", CapacityType: "on-demand"})
	if err != nil {
		t.Fatalf("on-demand: %v", err)
	}
	spot, err := p.RatesFor(ctx, domain.Node{
		Name: "w2", InstanceType: "m5.large", Region: "ap-south-1", CapacityType: "spot"})
	if err != nil {
		t.Fatalf("spot: %v", err)
	}

	if !spot.NodeHourly.Equal(dec(t, "0.0318")) {
		t.Errorf("spot NodeHourly = %s, want the spot price 0.0318", spot.NodeHourly)
	}
	if !spot.NodeHourly.LessThan(onDemand.NodeHourly) {
		t.Errorf("spot (%s) is not cheaper than on-demand (%s); the capacity_type label is "+
			"being ignored", spot.NodeHourly, onDemand.NodeHourly)
	}
	// The derived rates must be cheaper too, not just the node price.
	if !spot.CPUPerCoreHour.LessThan(onDemand.CPUPerCoreHour) {
		t.Errorf("spot CPU rate %s is not below on-demand %s", spot.CPUPerCoreHour, onDemand.CPUPerCoreHour)
	}
}

// TestRatesFor_SpotWithoutSpotPriceFallsBackToOnDemand covers a partially filled catalogue.
// Charging on-demand for spot OVERSTATES the cost, which is the safe direction to be wrong:
// it never makes something look cheaper than it is.
func TestRatesFor_SpotWithoutSpotPriceFallsBackToOnDemand(t *testing.T) {
	t.Parallel()

	p := loadTestProvider(t, `
version: 1
currency: USD
split: {cpu: 0.7, memory: 0.3}
default_region: r1
regions:
  r1:
    instance_types:
      m5.large: {vcpu: 2, memory_gib: 8, hourly: "0.1060"}
`)

	rates, err := p.RatesFor(context.Background(), domain.Node{
		Name: "w1", InstanceType: "m5.large", Region: "r1", CapacityType: "spot"})
	if err != nil {
		t.Fatalf("RatesFor: %v", err)
	}
	if !rates.NodeHourly.Equal(dec(t, "0.1060")) {
		t.Errorf("NodeHourly = %s, want the on-demand 0.1060 when no spot price is listed",
			rates.NodeHourly)
	}
}

// TestRatesFor_ExplicitRatesBypassTheSplit covers the Fargate/Autopilot case. Where the true
// per-resource rates are published, applying a 70/30 guess on top of them is strictly worse.
func TestRatesFor_ExplicitRatesBypassTheSplit(t *testing.T) {
	t.Parallel()

	p := loadTestProvider(t, `
version: 1
currency: USD
split: {cpu: 0.7, memory: 0.3}
default_region: r1
regions:
  r1:
    instance_types:
      fargate:
        vcpu: 1
        memory_gib: 2
        cpu_per_core_hour: "0.04656"
        memory_per_gib_hour: "0.00511"
`)

	rates, err := p.RatesFor(context.Background(), domain.Node{
		Name: "f1", InstanceType: "fargate", Region: "r1"})
	if err != nil {
		t.Fatalf("RatesFor: %v", err)
	}

	if rates.Source != SourceExplicitRates {
		t.Errorf("Source = %q, want %q so the report can show these are published rather "+
			"than derived", rates.Source, SourceExplicitRates)
	}
	if !rates.CPUPerCoreHour.Equal(dec(t, "0.04656")) {
		t.Errorf("CPUPerCoreHour = %s, want the stated 0.04656 (the split must NOT be applied)",
			rates.CPUPerCoreHour)
	}
	if !rates.MemoryPerGiBHour.Equal(dec(t, "0.00511")) {
		t.Errorf("MemoryPerGiBHour = %s, want the stated 0.00511", rates.MemoryPerGiBHour)
	}
}

// =============================================================================
// Unknown instance types -- the edge case that decides whether the tool is trustworthy
// =============================================================================

// TestRatesFor_UnknownInstanceTypeUsesFallbackAndSaysSo is the important one. The wrong
// answer here is ZERO: every pod on the node would report as free, the total would
// understate the bill, and every other team would appear to consume a larger share.
func TestRatesFor_UnknownInstanceTypeUsesFallbackAndSaysSo(t *testing.T) {
	t.Parallel()
	p := loadTestProvider(t, minimalCatalogue)

	rates, err := p.RatesFor(context.Background(), domain.Node{
		Name: "w9", InstanceType: "m7i.metal-48xl", Region: "ap-south-1"})
	if err != nil {
		t.Fatalf("RatesFor returned an error for an unknown type despite a configured fallback: %v", err)
	}

	// NOT zero. A node consuming real resources must never be priced at nothing.
	if rates.NodeHourly.IsZero() {
		t.Error("an unknown instance type was priced at ZERO; its cost would be silently " +
			"smeared across every other team")
	}
	if !rates.NodeHourly.Equal(dec(t, "0.2000")) {
		t.Errorf("NodeHourly = %s, want the fallback 0.2000", rates.NodeHourly)
	}
	// And it must be LABELLED as a guess, so nobody mistakes it for a real figure.
	if rates.Source != SourceFallback {
		t.Errorf("Source = %q, want %q -- an estimated cost that is indistinguishable from "+
			"a real one is worse than no cost at all", rates.Source, SourceFallback)
	}
	if rates.InstanceType != "" {
		t.Errorf("InstanceType = %q, want empty: nothing matched, and claiming a match would "+
			"make a guessed rate look real in the stored data", rates.InstanceType)
	}
	if p.FallbackCount() != 1 {
		t.Errorf("FallbackCount() = %d, want 1 so Phase 9 can report how much of the bill is guessed",
			p.FallbackCount())
	}
}

// TestRatesFor_NoFallbackConfiguredReturnsErrNoPrice covers the operator who has chosen to
// be told rather than given an estimate. Both are defensible; a silent zero is not.
func TestRatesFor_NoFallbackConfiguredReturnsErrNoPrice(t *testing.T) {
	t.Parallel()

	p := loadTestProvider(t, `
version: 1
currency: USD
split: {cpu: 0.7, memory: 0.3}
default_region: r1
regions:
  r1:
    instance_types:
      m5.large: {vcpu: 2, memory_gib: 8, hourly: "0.1060"}
`)

	_, err := p.RatesFor(context.Background(), domain.Node{Name: "w9", InstanceType: "unknown.type", Region: "r1"})
	if err == nil {
		t.Fatal("RatesFor succeeded for an unknown type with no fallback; want an error")
	}
	if !errors.Is(err, ErrNoPrice) {
		t.Errorf("error = %v, want it to wrap ErrNoPrice so callers can distinguish "+
			"'unpriceable' from 'broken catalogue'", err)
	}
	// The message must name the node and the type, or diagnosing it means guessing.
	for _, want := range []string{"w9", "unknown.type"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestRatesFor_NodeWithNoInstanceTypeLabel covers bare metal, and any cluster where the
// topology labels were never applied.
func TestRatesFor_NodeWithNoInstanceTypeLabel(t *testing.T) {
	t.Parallel()
	p := loadTestProvider(t, minimalCatalogue)

	rates, err := p.RatesFor(context.Background(), domain.Node{Name: "bare-metal-1"})
	if err != nil {
		t.Fatalf("RatesFor: %v", err)
	}
	if rates.Source != SourceFallback {
		t.Errorf("Source = %q, want %q for an unlabelled node", rates.Source, SourceFallback)
	}
}

// TestRatesFor_UnknownRegionFallsBackToDefaultRegion covers a known instance type in an
// unlisted region. The spec is identical and only the price differs, so the error is bounded
// at roughly 25% rather than being total -- much better than the fallback rate.
func TestRatesFor_UnknownRegionFallsBackToDefaultRegion(t *testing.T) {
	t.Parallel()
	p := loadTestProvider(t, minimalCatalogue)

	rates, err := p.RatesFor(context.Background(), domain.Node{
		Name: "w1", InstanceType: "m5.large", Region: "eu-west-9"})
	if err != nil {
		t.Fatalf("RatesFor: %v", err)
	}
	if rates.Source != SourceCatalogue {
		t.Errorf("Source = %q, want %q: the instance type IS known, only the region is not",
			rates.Source, SourceCatalogue)
	}
	if !rates.NodeHourly.Equal(dec(t, "0.1060")) {
		t.Errorf("NodeHourly = %s, want the default region's 0.1060", rates.NodeHourly)
	}
}

// =============================================================================
// Catalogue loading and validation
// =============================================================================

// TestLoadCatalogue_Rejects covers the validation that must happen at load time. A malformed
// price discovered mid-collection has already written rows; the same problem at startup is a
// clear message and a process that never serves wrong numbers.
func TestLoadCatalogue_Rejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		yaml string
		want string
		why  string
	}{
		{
			name: "split shares that do not sum to 1",
			yaml: `
version: 1
currency: USD
split: {cpu: 0.8, memory: 0.3}
default_region: r1
regions: {r1: {instance_types: {m5.large: {vcpu: 2, memory_gib: 8, hourly: "1"}}}}`,
			want: "sum to 1",
			why: "shares summing to 1.1 would inflate every cost by 10% with nothing to " +
				"indicate why -- the invariant breaks silently",
		},
		{
			name: "unsupported version",
			yaml: `
version: 2
currency: USD
split: {cpu: 0.7, memory: 0.3}
default_region: r1
regions: {r1: {instance_types: {m5.large: {vcpu: 2, memory_gib: 8, hourly: "1"}}}}`,
			want: "unsupported version",
			why:  "an unversioned config file is a migration you cannot perform",
		},
		{
			name: "unknown field (a typo)",
			yaml: `
version: 1
currency: USD
split: {cpu: 0.7, memory: 0.3}
default_region: r1
regions: {r1: {instance_types: {m5.large: {vcpu: 2, memory_gib: 8, hourly_price: "1"}}}}`,
			want: "field hourly_price",
			why: "THE critical one. Without KnownFields this typo leaves hourly at zero and " +
				"the service reports that everything is free",
		},
		{
			name: "missing currency",
			yaml: `
version: 1
split: {cpu: 0.7, memory: 0.3}
default_region: r1
regions: {r1: {instance_types: {m5.large: {vcpu: 2, memory_gib: 8, hourly: "1"}}}}`,
			want: "currency is required",
			why:  "summing USD and INR yields a number, and that number is nonsense",
		},
		{
			name: "zero vcpu would divide by zero",
			yaml: `
version: 1
currency: USD
split: {cpu: 0.7, memory: 0.3}
default_region: r1
regions: {r1: {instance_types: {broken: {vcpu: 0, memory_gib: 8, hourly: "1"}}}}`,
			want: "must be positive",
			why:  "otherwise the rate becomes +Inf and one node accounts for the entire bill",
		},
		{
			name: "negative price",
			yaml: `
version: 1
currency: USD
split: {cpu: 0.7, memory: 0.3}
default_region: r1
regions: {r1: {instance_types: {broken: {vcpu: 2, memory_gib: 8, hourly: "-1"}}}}`,
			want: "must not be negative",
			why:  "a negative rate would reduce the bill of whoever used that node",
		},
		{
			name: "unparseable price",
			yaml: `
version: 1
currency: USD
split: {cpu: 0.7, memory: 0.3}
default_region: r1
regions: {r1: {instance_types: {broken: {vcpu: 2, memory_gib: 8, hourly: "cheap"}}}}`,
			want: "not a valid decimal",
			why:  "must fail at load, not silently price at zero",
		},
		{
			name: "entry with no price at all",
			yaml: `
version: 1
currency: USD
split: {cpu: 0.7, memory: 0.3}
default_region: r1
regions: {r1: {instance_types: {broken: {vcpu: 2, memory_gib: 8}}}}`,
			want: "needs either",
			why:  "an entry with no price reports every pod on that node as free",
		},
		{
			name: "default_region not in regions",
			yaml: `
version: 1
currency: USD
split: {cpu: 0.7, memory: 0.3}
default_region: nowhere
regions: {r1: {instance_types: {m5.large: {vcpu: 2, memory_gib: 8, hourly: "1"}}}}`,
			want: "not present in regions",
			why:  "an unlabelled node would silently find no prices at all",
		},
		{
			name: "no regions",
			yaml: `
version: 1
currency: USD
split: {cpu: 0.7, memory: 0.3}
regions: {}`,
			want: "at least one region",
			why:  "a catalogue that prices nothing is a misconfiguration, not a valid state",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := LoadCatalogue(strings.NewReader(tt.yaml))
			if err == nil {
				t.Fatalf("LoadCatalogue accepted an invalid catalogue\nwhy this matters: %s", tt.why)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q\nwhy this matters: %s", err, tt.want, tt.why)
			}
		})
	}
}

// TestLoadCatalogue_ReportsAllProblemsAtOnce mirrors the config package: an operator with
// three broken entries should learn about all three from one load, not one per restart.
func TestLoadCatalogue_ReportsAllProblemsAtOnce(t *testing.T) {
	t.Parallel()

	_, err := LoadCatalogue(strings.NewReader(`
version: 1
currency: USD
split: {cpu: 0.7, memory: 0.3}
default_region: r1
regions:
  r1:
    instance_types:
      bad1: {vcpu: 2, memory_gib: 8, hourly: "nonsense"}
      bad2: {vcpu: 0, memory_gib: 8, hourly: "1"}
      bad3: {vcpu: 2, memory_gib: 8, hourly: "-5"}
`))
	if err == nil {
		t.Fatal("LoadCatalogue accepted three broken entries")
	}
	msg := err.Error()
	for _, want := range []string{"bad1", "bad2", "bad3"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error is missing %q; got:\n%s", want, msg)
		}
	}
}

// TestLoadCatalogueFile_ShippedCatalogueIsValid loads the real file from deploy/pricing, so a
// typo committed there fails the build rather than the cluster.
//
// It also asserts the fixtures' three instance types are present: if someone removes
// m5.large from the catalogue, every demo node silently drops to the fallback rate and the
// numbers quietly become approximations.
func TestLoadCatalogueFile_ShippedCatalogueIsValid(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "deploy", "pricing", "catalogue.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("shipped catalogue not found at %s: %v", path, err)
	}

	c, err := LoadCatalogueFile(path)
	if err != nil {
		t.Fatalf("the shipped catalogue does not load: %v", err)
	}

	region, ok := c.Regions[c.DefaultRegion]
	if !ok {
		t.Fatalf("default region %q missing", c.DefaultRegion)
	}
	// Exactly the types deploy/kind/cluster.yaml labels its nodes with.
	for _, want := range []string{"t3.medium", "m5.large", "m5.xlarge"} {
		if _, found := region.InstanceTypes[want]; !found {
			t.Errorf("catalogue is missing %q, which deploy/kind/cluster.yaml uses; every "+
				"demo node would silently fall back to an approximate rate", want)
		}
	}

	// And it must price the fixtures' nodes without falling back.
	p := NewCatalogueProvider(c, testLogger())
	for _, n := range []domain.Node{
		{Name: "cp", InstanceType: "t3.medium", Region: "ap-south-1", CapacityType: "on-demand"},
		{Name: "w1", InstanceType: "m5.large", Region: "ap-south-1", CapacityType: "on-demand"},
		{Name: "w2", InstanceType: "m5.xlarge", Region: "ap-south-1", CapacityType: "spot"},
	} {
		rates, err := p.RatesFor(context.Background(), n)
		if err != nil {
			t.Errorf("cannot price fixture node %s: %v", n.Name, err)
			continue
		}
		if rates.Source == SourceFallback {
			t.Errorf("fixture node %s fell back to an approximate rate", n.Name)
		}
	}
	if p.FallbackCount() != 0 {
		t.Errorf("FallbackCount() = %d, want 0 for the fixture nodes", p.FallbackCount())
	}
}

// Guard against a nil logger reaching the fallback path, which logs.
func TestNewCatalogueProvider_UsableWithDiscardLogger(t *testing.T) {
	t.Parallel()
	c, err := LoadCatalogue(strings.NewReader(minimalCatalogue))
	if err != nil {
		t.Fatalf("LoadCatalogue: %v", err)
	}
	p := NewCatalogueProvider(c, slog.New(slog.DiscardHandler))
	if _, err := p.RatesFor(context.Background(), domain.Node{Name: "x", InstanceType: "nope"}); err != nil {
		t.Errorf("RatesFor: %v", err)
	}
}
