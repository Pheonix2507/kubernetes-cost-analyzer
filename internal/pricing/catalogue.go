package pricing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"

	"github.com/shopspring/decimal"
	"gopkg.in/yaml.v3"

	"github.com/Pheonix2507/kubernetes-cost-analyzer/internal/domain"
)

// Compile-time proof that the catalogue satisfies Provider.
var _ Provider = (*CatalogueProvider)(nil)

// Catalogue is the on-disk pricing file. See deploy/pricing/catalogue.yaml.
//
// WHY A FILE RATHER THAN CONSTANTS IN CODE
// ----------------------------------------
// Prices change, differ by region, and differ by negotiated discount. Anyone running this
// against their own cluster has different numbers from ours -- Reserved Instances, Savings
// Plans and Enterprise Agreements routinely move the effective rate by 40% or more, and no
// public price list knows about them. A YAML file is editable without a rebuild, reviewable
// in a pull request, and diffable when someone changes an assumption.
//
// It also makes the SPLIT ASSUMPTION visible. That is the part most tools hide, and hiding
// it is how a number nobody can justify ends up in a finance meeting.
type Catalogue struct {
	// Version guards the file format. Checked on load so a future incompatible change fails
	// with "unsupported catalogue version 2" rather than silently reading garbage -- an
	// unversioned config file is a migration you cannot perform.
	Version int `yaml:"version"`

	Currency string `yaml:"currency"`

	// Split is the allocation policy: how much of an instance's price is attributed to CPU
	// versus memory. THE central assumption of this package; see the package doc.
	Split Split `yaml:"split"`

	// Regions keys instance prices by cloud region, because the same instance type costs
	// materially different amounts in different regions -- roughly 10-25% between
	// us-east-1 and ap-south-1.
	//
	// Region-aware from the start for the same reason cluster_id exists in Phase 2's
	// schema: adding the dimension later means rewriting every entry, whereas having it now
	// costs one map lookup.
	Regions map[string]Region `yaml:"regions"`

	// DefaultRegion is used when a node carries no region label -- bare metal, or a cluster
	// where the topology labels were never set.
	DefaultRegion string `yaml:"default_region"`

	// Fallback prices an instance type absent from the catalogue. See the long note in
	// RatesFor for why this exists rather than returning an error.
	Fallback *InstancePrice `yaml:"fallback"`
}

// Split is the CPU/memory allocation policy.
type Split struct {
	CPU    float64 `yaml:"cpu"`
	Memory float64 `yaml:"memory"`
}

// Region holds one region's instance prices.
type Region struct {
	InstanceTypes map[string]InstancePrice `yaml:"instance_types"`
}

// InstancePrice is one instance type's specification and price.
type InstancePrice struct {
	// Name is filled in from the map key during load, so error messages can identify the
	// entry. Not read from the file.
	Name string `yaml:"-"`

	VCPU      float64 `yaml:"vcpu"`
	MemoryGiB float64 `yaml:"memory_gib"`

	// Hourly is the on-demand price. Parsed from a STRING in YAML, never a float.
	//
	// YAML numbers are parsed as float64, so `0.106` in the file would become the nearest
	// binary double before it ever reached decimal -- reintroducing exactly the
	// representation error this whole layer exists to avoid, at the very first step. Quoting
	// the value in YAML and parsing it as a decimal keeps it exact end to end.
	Hourly decimal.Decimal `yaml:"-"`
	// HourlyRaw is the string form as it appears in the file.
	HourlyRaw string `yaml:"hourly"`

	// SpotHourly is the spot price, if the instance is being used as spot capacity.
	SpotHourly    decimal.Decimal `yaml:"-"`
	SpotHourlyRaw string          `yaml:"spot_hourly"`

	// CPUPerCoreHour and MemoryPerGiBHour let a catalogue state per-resource rates DIRECTLY,
	// bypassing the split entirely.
	//
	// Some providers really do bill this way -- Fargate and GKE Autopilot quote per vCPU-hour
	// and per GB-hour -- and where they do, the split assumption is not merely unnecessary
	// but WRONG, because the true rates are known. A rate derived from published per-resource
	// pricing is strictly more trustworthy than one derived from a 70/30 guess, which is why
	// it gets its own Source value.
	CPUPerCoreHour      decimal.Decimal `yaml:"-"`
	CPUPerCoreHourRaw   string          `yaml:"cpu_per_core_hour"`
	MemoryPerGiBHour    decimal.Decimal `yaml:"-"`
	MemoryPerGiBHourRaw string          `yaml:"memory_per_gib_hour"`
}

// hasExplicitRates reports whether both per-resource rates were supplied.
//
// BOTH, not either: a catalogue giving only a CPU rate has no defensible way to price
// memory, and silently falling back to the split for one resource while honouring an
// explicit rate for the other would produce a total that matches neither model.
func (p InstancePrice) hasExplicitRates() bool {
	return p.CPUPerCoreHourRaw != "" && p.MemoryPerGiBHourRaw != ""
}

// CatalogueProvider serves rates from a loaded Catalogue.
type CatalogueProvider struct {
	catalogue Catalogue
	cpuShare  decimal.Decimal
	memShare  decimal.Decimal
	log       *slog.Logger

	// fallbackWarnings counts how many times an unknown instance type has been priced from
	// the fallback.
	//
	// A counter rather than a log line per occurrence: with 5,000 nodes on an unrecognised
	// instance family, per-node logging would emit 5,000 identical warnings every collection
	// cycle and bury everything else. Phase 9 exposes this as a metric, which is the correct
	// shape for "how often is this happening" -- a question logs answer badly and counters
	// answer exactly.
	//
	// atomic because RatesFor is called concurrently from the collector's worker pool.
	fallbackWarnings atomic.Int64
}

// LoadCatalogue reads and validates a catalogue from r.
//
// It takes an io.Reader rather than a path so tests can supply a string, and so a future
// version can load from a ConfigMap or an HTTP endpoint without touching this function.
func LoadCatalogue(r io.Reader) (Catalogue, error) {
	var c Catalogue

	dec := yaml.NewDecoder(r)
	// KnownFields makes an unrecognised key a hard error rather than a silent no-op.
	//
	// This matters more for pricing than almost anywhere else: a typo like `hourly_price`
	// instead of `hourly` would otherwise leave the price at zero, and the service would
	// start happily and report that everything is free. A config file that silently ignores
	// what you wrote is worse than one that refuses to load.
	dec.KnownFields(true)

	if err := dec.Decode(&c); err != nil {
		return Catalogue{}, fmt.Errorf("%w: %w", ErrInvalidCatalogue, err)
	}
	if err := c.normalise(); err != nil {
		return Catalogue{}, err
	}
	return c, nil
}

// LoadCatalogueFile reads a catalogue from disk.
func LoadCatalogueFile(path string) (Catalogue, error) {
	f, err := os.Open(path) //nolint:gosec // operator-supplied config path, by design
	if err != nil {
		return Catalogue{}, fmt.Errorf("%w: opening %s: %w", ErrInvalidCatalogue, path, err)
	}
	// Explicitly ignored: this is a read-only open, so Close cannot report a lost write,
	// and there is nothing actionable to do about a failure to release a descriptor we are
	// finished with. The blank assignment states that this was considered.
	defer func() { _ = f.Close() }()

	c, err := LoadCatalogue(f)
	if err != nil {
		// Name the file. A parse error with no filename is infuriating when several
		// catalogues exist.
		return Catalogue{}, fmt.Errorf("loading %s: %w", path, err)
	}
	return c, nil
}

// normalise parses the string-typed money fields and validates the whole catalogue.
//
// ALL VALIDATION HAPPENS AT LOAD TIME, and that is the point. A malformed price discovered
// during a collection cycle is a mid-flight failure that has already written some rows; the
// same problem discovered at startup is a clear message and a process that never serves
// wrong numbers. Same reasoning as internal/config refusing to start.
func (c *Catalogue) normalise() error {
	var errs []error

	if c.Version != 1 {
		errs = append(errs, fmt.Errorf("%w: unsupported version %d (want 1)", ErrInvalidCatalogue, c.Version))
	}
	if strings.TrimSpace(c.Currency) == "" {
		errs = append(errs, fmt.Errorf("%w: currency is required", ErrInvalidCatalogue))
	}

	// The shares MUST sum to 1, or the invariant that a whole node costs its hourly price
	// silently breaks -- and it breaks quietly, producing totals that are consistently 10%
	// wrong with nothing to indicate why.
	sum := c.Split.CPU + c.Split.Memory
	const tolerance = 1e-9
	switch {
	case c.Split.CPU < 0 || c.Split.Memory < 0:
		errs = append(errs, fmt.Errorf("%w: split shares must not be negative (cpu=%v memory=%v)",
			ErrInvalidCatalogue, c.Split.CPU, c.Split.Memory))
	case sum < 1-tolerance || sum > 1+tolerance:
		errs = append(errs, fmt.Errorf("%w: split shares must sum to 1, got cpu=%v + memory=%v = %v",
			ErrInvalidCatalogue, c.Split.CPU, c.Split.Memory, sum))
	}

	if len(c.Regions) == 0 {
		errs = append(errs, fmt.Errorf("%w: at least one region is required", ErrInvalidCatalogue))
	}
	if c.DefaultRegion != "" {
		if _, ok := c.Regions[c.DefaultRegion]; !ok {
			errs = append(errs, fmt.Errorf("%w: default_region %q is not present in regions",
				ErrInvalidCatalogue, c.DefaultRegion))
		}
	}

	for regionName, region := range c.Regions {
		for typeName, price := range region.InstanceTypes {
			price.Name = typeName
			if err := price.parse(); err != nil {
				errs = append(errs, fmt.Errorf("region %s, instance %s: %w", regionName, typeName, err))
				continue
			}
			if price.VCPU <= 0 || price.MemoryGiB <= 0 {
				errs = append(errs, fmt.Errorf("%w: region %s instance %s: vcpu and memory_gib must be positive",
					ErrInvalidCatalogue, regionName, typeName))
			}
			// Writing back through the map is required: `price` is a COPY, since Go map
			// values are not addressable. Forgetting this is a classic Go bug -- the parse
			// appears to succeed and every Hourly stays zero.
			region.InstanceTypes[typeName] = price
		}
	}

	if c.Fallback != nil {
		c.Fallback.Name = "fallback"
		if err := c.Fallback.parse(); err != nil {
			errs = append(errs, fmt.Errorf("fallback: %w", err))
		} else if c.Fallback.VCPU <= 0 || c.Fallback.MemoryGiB <= 0 {
			errs = append(errs, fmt.Errorf("%w: fallback vcpu and memory_gib must be positive", ErrInvalidCatalogue))
		}
	}

	// errors.Join, so one load reports every problem in the file rather than making the
	// operator fix them one restart at a time. Same argument as internal/config.Validate.
	return errors.Join(errs...)
}

// parse converts the string money fields into decimals.
func (p *InstancePrice) parse() error {
	parseField := func(raw, field string) (decimal.Decimal, error) {
		if strings.TrimSpace(raw) == "" {
			return decimal.Zero, nil
		}
		d, err := decimal.NewFromString(strings.TrimSpace(raw))
		if err != nil {
			return decimal.Zero, fmt.Errorf("%w: %s=%q is not a valid decimal: %w",
				ErrInvalidCatalogue, field, raw, err)
		}
		if d.IsNegative() {
			return decimal.Zero, fmt.Errorf("%w: %s=%q must not be negative", ErrInvalidCatalogue, field, raw)
		}
		return d, nil
	}

	var errs []error
	var err error

	if p.Hourly, err = parseField(p.HourlyRaw, "hourly"); err != nil {
		errs = append(errs, err)
	}
	if p.SpotHourly, err = parseField(p.SpotHourlyRaw, "spot_hourly"); err != nil {
		errs = append(errs, err)
	}
	if p.CPUPerCoreHour, err = parseField(p.CPUPerCoreHourRaw, "cpu_per_core_hour"); err != nil {
		errs = append(errs, err)
	}
	if p.MemoryPerGiBHour, err = parseField(p.MemoryPerGiBHourRaw, "memory_per_gib_hour"); err != nil {
		errs = append(errs, err)
	}

	// An entry with neither an hourly price nor explicit rates prices nothing, and would
	// silently report every pod on it as free.
	if p.HourlyRaw == "" && !p.hasExplicitRates() {
		errs = append(errs, fmt.Errorf("%w: needs either `hourly` or both `cpu_per_core_hour` and `memory_per_gib_hour`",
			ErrInvalidCatalogue))
	}

	return errors.Join(errs...)
}

// NewCatalogueProvider returns a Provider backed by c.
func NewCatalogueProvider(c Catalogue, log *slog.Logger) *CatalogueProvider {
	return &CatalogueProvider{
		catalogue: c,
		cpuShare:  decimal.NewFromFloat(c.Split.CPU),
		memShare:  decimal.NewFromFloat(c.Split.Memory),
		log:       log,
	}
}

// RatesFor implements Provider.
func (p *CatalogueProvider) RatesFor(_ context.Context, n domain.Node) (Rates, error) {
	region := n.Region
	if region == "" {
		region = p.catalogue.DefaultRegion
	}

	price, found := p.lookup(region, n.InstanceType)
	if !found {
		return p.fallbackRates(n, region)
	}

	// Spot capacity is priced from the spot rate when one is published.
	//
	// Spot is typically 60-90% cheaper, so treating a spot node as on-demand overstates its
	// cost several-fold -- and it would make spot look like a problem in the very report
	// meant to recommend it. Note the honest limitation: real spot prices vary by
	// availability zone and change continuously, so a static value is an approximation.
	// Phase 11's AWS provider reads the Spot Price History API.
	source := SourceCatalogue
	if n.CapacityType == "spot" && !price.SpotHourly.IsZero() {
		price.Hourly = price.SpotHourly
	}

	// Explicit per-resource rates bypass the split entirely -- they are known rather than
	// assumed, so the assumption must not be applied on top of them.
	if price.hasExplicitRates() {
		return Rates{
			Currency:         p.catalogue.Currency,
			NodeHourly:       price.Hourly.Round(ratePrecision),
			CPUPerCoreHour:   price.CPUPerCoreHour.Round(ratePrecision),
			MemoryPerGiBHour: price.MemoryPerGiBHour.Round(ratePrecision),
			Source:           SourceExplicitRates,
			InstanceType:     price.Name,
		}, nil
	}

	return deriveRates(price, p.cpuShare, p.memShare, p.catalogue.Currency, source)
}

// lookup finds an instance price, falling back to the default region.
func (p *CatalogueProvider) lookup(region, instanceType string) (InstancePrice, bool) {
	if instanceType == "" {
		return InstancePrice{}, false
	}
	if r, ok := p.catalogue.Regions[region]; ok {
		if price, ok := r.InstanceTypes[instanceType]; ok {
			return price, true
		}
	}
	// A known instance type in an unlisted region is better priced at another region's rate
	// than not at all: the instance-type spec is identical and only the price differs, so the
	// error is bounded at roughly 25% rather than being total.
	if region != p.catalogue.DefaultRegion {
		if r, ok := p.catalogue.Regions[p.catalogue.DefaultRegion]; ok {
			if price, ok := r.InstanceTypes[instanceType]; ok {
				return price, true
			}
		}
	}
	return InstancePrice{}, false
}

// fallbackRates prices a node whose instance type is not in the catalogue.
//
// WHY A FALLBACK RATHER THAN AN ERROR, AND WHY NOT ZERO
// ----------------------------------------------------
// Three options exist for an unknown instance type, and two of them are wrong:
//
//	FAIL     -- a cost tool that refuses to report anything because one node in a
//	            fifty-node cluster is an unrecognised type is a tool nobody keeps running.
//	            New instance families appear constantly.
//	ZERO     -- the worst option, and the tempting one. Every pod on that node reports as
//	            free, the cluster total silently understates the bill, and because the
//	            missing money has to go somewhere in a percentage breakdown, every OTHER
//	            team appears to consume a larger share than it does. A wrong number that
//	            looks plausible is worse than a missing one.
//	FALLBACK -- price it at a stated default, mark the result SourceFallback, and count the
//	            occurrence. The figure is approximate and SAYS SO.
//
// So: fallback, loudly. The Source field carries the caveat all the way into the database
// and out through the API, and the counter becomes a metric in Phase 9 so "how much of my
// bill is guessed?" has an answer.
//
// If no fallback is configured, this returns ErrNoPrice -- because a silent zero is never
// acceptable, and an operator who has deliberately not configured a fallback has chosen to
// be told.
func (p *CatalogueProvider) fallbackRates(n domain.Node, region string) (Rates, error) {
	if p.catalogue.Fallback == nil {
		return Rates{}, fmt.Errorf("%w: node %q has instance type %q in region %q, which is not in the "+
			"catalogue and no fallback is configured",
			ErrNoPrice, n.Name, n.InstanceType, region)
	}

	count := p.fallbackWarnings.Add(1)
	// Logged only for the first few occurrences. Beyond that the counter is the signal, and
	// repeating an identical warning thousands of times per cycle would bury every other log
	// line -- the same alert-fatigue argument as the probe log levels in Phase 0's audit.
	if count <= 5 {
		p.log.Warn("pricing an unknown instance type from the fallback rate",
			"node", n.Name,
			"instance_type", n.InstanceType,
			"region", region,
			"occurrences", count,
			"note", "the resulting cost is approximate; add this instance type to the catalogue",
		)
	}

	rates, err := deriveRates(*p.catalogue.Fallback, p.cpuShare, p.memShare, p.catalogue.Currency, SourceFallback)
	if err != nil {
		return Rates{}, err
	}
	// The catalogue key is deliberately NOT reported as the matched instance type: nothing
	// matched, and claiming otherwise would make a guessed rate indistinguishable from a
	// real one in the stored data.
	rates.InstanceType = ""
	return rates, nil
}

// FallbackCount returns how many times the fallback rate has been used.
//
// Exposed so Phase 9 can publish it as a metric and Phase 5 can annotate a report. A cost
// figure derived partly from guesses should be legible as such at every layer.
func (p *CatalogueProvider) FallbackCount() int64 { return p.fallbackWarnings.Load() }
