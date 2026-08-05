// Package pricing turns a node into money.
//
// WHY THIS PACKAGE EXISTS
// -----------------------
// Phase 1 gives us quantities -- millicores and bytes. Phase 2 gives us somewhere to record
// them. Neither produces a bill, because a node object carries no price. This package is
// the conversion, and it is the last piece missing before Phase 4 can compute a real
// number.
//
// THE PROBLEM, WHICH IS HARDER THAN IT LOOKS
// ------------------------------------------
// An m5.large costs ONE number per hour and has TWO resources:
//
//	m5.large = $0.106/hr, 2 vCPU, 8 GiB
//	a pod reserves 500m CPU and 512Mi memory -- what does it cost?
//
// You cannot recover two per-resource prices from one instance price without adding an
// assumption. There is no objectively correct split. It is an ALLOCATION POLICY, and any
// tool that presents it as a calculation has merely hidden its assumption somewhere you
// cannot see it.
//
// So ours is explicit and configurable: a share of the instance price attributed to CPU and
// the remainder to memory, defaulting to 70/30, living in a YAML file you can read and
// change rather than a constant buried in code.
//
// HOW OTHERS DO IT
//   - Fixed ratio, as here. What OpenCost does when no per-resource pricing exists.
//   - Solve for it across an instance family: m5.large, r5.large and c5.large differ mainly
//     in memory, so regressing their prices yields per-vCPU and per-GiB prices. Elegant,
//     and it needs a complete price list to be worth doing.
//   - Use published per-resource rates where they exist. Fargate and GKE Autopilot bill per
//     vCPU-hour and per GB-hour directly, so no split is needed. Rates supports this: a
//     catalogue entry may state the rates outright and skip the split entirely.
package pricing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/domain"
)

// Sentinel errors, so callers can distinguish "we have no price for this" from "the
// catalogue is broken" without matching on message text.
var (
	// ErrNoPrice means no rate could be determined and no fallback was configured.
	ErrNoPrice = errors.New("no price available for node")
	// ErrInvalidCatalogue means the catalogue itself is unusable.
	ErrInvalidCatalogue = errors.New("invalid pricing catalogue")
)

// ratePrecision is the number of decimal places rates and costs are rounded to.
//
// It matches numeric(20,10) in the schema exactly. Rounding here rather than letting the
// database truncate on insert means the value Go computed and the value Postgres stored are
// the SAME number, so a cost recomputed in the application always agrees with the stored
// one. Silent truncation at the database boundary is how two layers come to disagree by a
// hair and nobody can say which is right.
const ratePrecision int32 = 10

// Source records HOW a rate was determined.
//
// WHY PROVENANCE IS A FIRST-CLASS FIELD
// -------------------------------------
// A cost derived from a real catalogue entry and one derived from a fallback guess look
// identical once they are a number in a database. They should not be trusted equally. A
// report that cannot distinguish them invites someone to take a fabricated figure to a
// finance meeting.
//
// So every rate carries where it came from, the fact table can record it, and Phase 5 can
// mark estimated figures in the API.
type Source string

const (
	// SourceCatalogue means an exact instance-type match was found.
	SourceCatalogue Source = "catalogue"
	// SourceExplicitRates means the catalogue stated per-resource rates directly, so no
	// split assumption was applied. The most trustworthy case.
	SourceExplicitRates Source = "explicit_rates"
	// SourceFallback means the instance type was unknown and a default was used. Real
	// resource consumption at a guessed price -- better than reporting zero, but it must be
	// visible.
	SourceFallback Source = "fallback"
)

// Rates is the per-unit price of a node's resources.
type Rates struct {
	// Currency is an ISO 4217 code. Carried on every rate so mixed-currency catalogues
	// cannot silently sum into a meaningless total -- adding USD to INR yields a number,
	// and that number is nonsense.
	Currency string

	// NodeHourly is the whole instance's price per hour. Retained alongside the derived
	// rates because node-level questions need it directly: "what is this node costing us"
	// and "how much of it is unallocated" are both answered from here, and recomputing it
	// from the per-unit rates would reintroduce rounding.
	NodeHourly decimal.Decimal

	// CPUPerCoreHour is the price of reserving one full core for one hour.
	CPUPerCoreHour decimal.Decimal
	// MemoryPerGiBHour is the price of reserving one GiB for one hour.
	MemoryPerGiBHour decimal.Decimal

	// Source records how these were derived. See the Source doc.
	Source Source
	// InstanceType is the catalogue key that matched, or "" for a fallback.
	InstanceType string
}

// Provider resolves the rates that apply to a node.
//
// WHY AN INTERFACE WITH ONE IMPLEMENTATION TODAY
// ----------------------------------------------
// Phase 0 argued against interfaces with a single implementation, and that reasoning still
// holds -- so this one needs justifying rather than assuming.
//
// It earns its place because the SECOND implementation is already specified: Phase 11 adds
// AWS, GCP and Azure providers that fetch live prices, including spot prices that change
// hourly and cannot come from a static file. There will also be a caching decorator, since
// a cloud pricing API must not be called once per node per collection cycle.
//
// That is a known requirement, not a speculative one, and the interface is what keeps the
// cost engine from ever knowing which kind of provider it holds. A concrete
// *CatalogueProvider threaded through Phase 4 would have to be unpicked from the collector
// later, which is exactly the refactor this avoids.
//
// It takes a context because the cloud implementations perform network I/O. The static
// catalogue ignores it, and that asymmetry is fine: the interface must accommodate the
// slowest implementation, or the fast one dictates a signature the slow one cannot satisfy.
type Provider interface {
	// RatesFor returns the rates applying to n.
	//
	// It returns an error wrapping ErrNoPrice when the node cannot be priced and no
	// fallback is configured. Callers should treat that as "exclude this node from the
	// report and say so", never as zero -- a node silently priced at zero understates the
	// bill and its cost gets smeared across everything else.
	RatesFor(ctx context.Context, n domain.Node) (Rates, error)
}

// ResourceCost is the money attributable to one reservation over one window.
type ResourceCost struct {
	CPU    decimal.Decimal
	Memory decimal.Decimal
}

// Total returns the sum. A method rather than a stored field, so it cannot disagree with
// its parts -- the same reasoning as the generated total_cost column in the schema.
func (c ResourceCost) Total() decimal.Decimal { return c.CPU.Add(c.Memory) }

// Cost computes what reserving cpuMillicores and memoryBytes for the given duration costs
// at these rates.
//
// PURE FUNCTION, DELIBERATELY. No clock, no I/O, no state. Every interesting property of
// the cost model can therefore be tested with struct literals -- including the invariant
// that pricing a whole node for an hour returns the node's hourly price, which is the one
// check that proves the split is coherent rather than merely plausible.
//
// A NOTE ON UNITS, WHICH IS WHERE THIS GOES WRONG IN PRACTICE
// The inputs are millicores and BYTES, matching internal/kube and the database, so no
// caller has to remember a conversion. The rates are per CORE-hour and per GiB-hour,
// matching how clouds quote prices. The conversion between the two lives here, once,
// instead of at every call site where half of them would use 1000 for a GiB.
func Cost(rates Rates, cpuMillicores, memoryBytes int64, d time.Duration) ResourceCost {
	// A zero or negative window has no cost. Guarding here rather than trusting the caller
	// means a clock skew that produces a reversed window yields zero rather than a negative
	// cost that quietly reduces someone's bill.
	if d <= 0 {
		return ResourceCost{CPU: decimal.Zero, Memory: decimal.Zero}
	}

	// Hours as an exact decimal. Deliberately NOT d.Hours(), which returns float64 and
	// would reintroduce binary floating point into the one calculation this package exists
	// to keep exact. Nanoseconds are an exact integer, so dividing by an exact constant
	// keeps the whole chain in decimal.
	hours := decimal.NewFromInt(int64(d)).DivRound(decimal.NewFromInt(int64(time.Hour)), 18)

	// Millicores to cores, bytes to GiB. 1 GiB is 2^30 bytes -- the binary gibibyte, which
	// is what Kubernetes means by "Gi" and what container_memory_working_set_bytes reports.
	// Cloud providers usually quote "GB" and often mean 10^9. Mixing the two is a silent 7%
	// error, so the constant is named and stated rather than inlined as 1073741824.
	const bytesPerGiB = 1 << 30
	cores := decimal.NewFromInt(cpuMillicores).DivRound(decimal.NewFromInt(1000), 18)
	gib := decimal.NewFromInt(memoryBytes).DivRound(decimal.NewFromInt(bytesPerGiB), 18)

	return ResourceCost{
		CPU:    cores.Mul(hours).Mul(rates.CPUPerCoreHour).Round(ratePrecision),
		Memory: gib.Mul(hours).Mul(rates.MemoryPerGiBHour).Round(ratePrecision),
	}
}

// deriveRates splits an instance's hourly price into per-unit rates.
//
// THE ARITHMETIC, AND THE INVARIANT IT MUST SATISFY
//
//	cpu_per_core_hour = price * cpu_share / vcpu_count
//	mem_per_gib_hour  = price * mem_share / memory_gib
//
// Reserving the WHOLE node for one hour must then cost exactly the node's hourly price:
//
//	vcpu * (price * cpu_share / vcpu) + gib * (price * mem_share / gib)
//	  = price * cpu_share + price * mem_share
//	  = price * (cpu_share + mem_share)
//	  = price, because the shares sum to 1
//
// That identity is what makes the split coherent rather than merely plausible, and it is
// tested directly. It holds exactly in real arithmetic; in fixed-precision decimal the
// division introduces a bounded rounding error, which the test asserts against a tolerance
// rather than pretending is zero. Being honest about that is better than choosing shares
// that happen to divide evenly and calling it proof.
//
// WHY WE DIVIDE BY THE CATALOGUE'S vCPU COUNT, NOT THE NODE'S REPORTED CAPACITY
// You are billed for the instance you rented. A kubelet reporting slightly different
// capacity, or a kind node reporting the host's 6 CPUs while labelled m5.large, does not
// change the invoice. This is also precisely why the fake labels in
// deploy/kind/cluster.yaml work: the node prices as 2 vCPU because that is what an
// m5.large IS, regardless of what it reports.
func deriveRates(price InstancePrice, cpuShare, memShare decimal.Decimal, currency string, src Source) (Rates, error) {
	if price.VCPU <= 0 || price.MemoryGiB <= 0 {
		// Guarded rather than allowed to divide by zero. A malformed catalogue entry must
		// fail loudly at load time, not produce +Inf rates that silently make one node
		// account for the entire bill.
		return Rates{}, fmt.Errorf("%w: instance %q has vcpu=%v memory_gib=%v, both must be positive",
			ErrInvalidCatalogue, price.Name, price.VCPU, price.MemoryGiB)
	}

	vcpu := decimal.NewFromFloat(price.VCPU)
	gib := decimal.NewFromFloat(price.MemoryGiB)

	return Rates{
		Currency:         currency,
		NodeHourly:       price.Hourly.Round(ratePrecision),
		CPUPerCoreHour:   price.Hourly.Mul(cpuShare).DivRound(vcpu, ratePrecision),
		MemoryPerGiBHour: price.Hourly.Mul(memShare).DivRound(gib, ratePrecision),
		Source:           src,
		InstanceType:     price.Name,
	}, nil
}
