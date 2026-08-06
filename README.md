# Kubernetes Cost Analyzer

Attributes Kubernetes spend to the teams, namespaces and workloads that cause it, and
recommends what to change.

The core idea is one subtraction. Kubernetes bills you for what you **reserve**
(resource requests), not for what you **use** — the scheduler holds a request against a
node whether or not the container ever touches it. The gap between the two is waste, and
in most clusters that has never been measured it is the majority of the bill.

```
waste = max(requested - used, 0)
```

Everything else in this repository is data collection, aggregation and presentation on
top of that.

The `max(..., 0)` is the part that bites. It has to be applied **per row, inside the sum** — not to
the aggregate. `GREATEST(sum(requested) - sum(used), 0)` reads as equivalent and is not: the
cancellation happens inside the two sums, before the subtraction, so an under-requested container
issues a *credit* against genuine waste elsewhere in the same group. This repo shipped that version.
Measured on real data, `kube-system` reported **0 GiB-hours** of memory waste while actually holding
50, and `team-search` was understated by 126%. The more under-requested a team's workloads were, the
more efficient it looked.

## Status: Phase 7 complete — history is now affordable to read

The collector produces cost data, the API serves it, the recommendation engine says what to change,
and a nightly rollup makes trends and monthly statements cheap: **292.7x compression, measured on
real data**, with statements that freeze once signed off. **A frontend could be built against this
today** — which is Phase 8.

What works today:

- One command rebuilds the entire local environment from files in this repo
- A 3-node kind cluster whose nodes carry fake cloud instance types, so pricing has
  something real to key on
- kube-prometheus-stack and metrics-server, trimmed to fit an 8 GiB Docker VM
- Seven fixture workloads engineered so every future detection rule has both a positive
  and a negative case
- client-go shared informers holding a live cache of nodes, namespaces, pods and
  ReplicaSets, with pod ownership resolved through to the owning Deployment
- Effective pod requests computed the way the scheduler computes them — init containers
  as a max, sidecars additive, pod overhead included
- A read-only ClusterRole, verified by assertion that every write and secret read is
  denied
- Two Go binaries with config validation, structured logging, dependency injection,
  liveness/readiness split and graceful shutdown
- A partitioned star-schema in Postgres: normalised dimensions, an immutable
  container-grain fact table, monthly RANGE partitions, and idempotent upserts
- Exact decimal money end to end (`decimal.Decimal` ↔ `numeric(20,10)`), never float
- A pricing engine: a YAML rate catalogue keyed on the same well-known instance-type
  labels a cloud provider sets, spot-aware, region-aware, with explicit provenance on
  every rate
- A cost engine that joins topology, usage and rates: a bounded worker pool over
  namespaces, best-effort per-namespace failure isolation, `max(request, usage)` billing,
  and aligned windows that make re-collection idempotent
- Per-window peak collection (`max_over_time`) alongside averages, because the two answer
  different questions: an average for cost, p95-of-peaks for right-sizing
- A recommendation engine with an evidence gate, headroom on every proposal, and savings
  that are allowed to be negative when the correct advice costs money
- A daily rollup at 292.7x compression, written by a batch job that is idempotent, backfillable,
  and mutually excluded by a Postgres advisory lock
- Immutable monthly statements with honest coverage metadata, frozen by database trigger
- Three non-root, shell-less container images

### Endpoints

| Method | Path | Returns |
|---|---|---|
| GET | `/healthz` | liveness — checks nothing, by design |
| GET | `/readyz` | readiness — per-dependency detail for Postgres and the informer cache |
| GET | `/version` | the build actually running |
| GET | `/api/v1/nodes` | capacity, allocatable, instance type, zone, spot vs on-demand, **and rates** |
| GET | `/api/v1/namespaces` | cost-allocation dimensions (team, cost-centre, environment) |
| GET | `/api/v1/pods` | requests, limits, QoS class, resolved workload. `?namespace=` filters |
| GET | `/api/v1/costs/summary` | **aggregated cost** — `?group_by=` any of 10 dimensions, filtered, sorted |
| GET | `/api/v1/allocations` | raw per-container samples, cursor-paginated |
| GET | `/api/v1/recommendations` | **what to change** — right-size, idle, under-requested, set-requests, over-replicated |
| GET | `/api/v1/costs/trend` | cost **through time** from the rollup, with period-over-period comparison |
| GET | `/api/v1/reports/monthly` | frozen monthly statements at cluster, namespace and team scope |

Full contract in [`api/openapi.yaml`](api/openapi.yaml), kept honest by a test asserting its enums
match the allow-lists the code enforces.

```bash
curl -H "Authorization: Bearer $KEY" \
  'localhost:8080/api/v1/costs/summary?group_by=workload&sort=wasted_cpu_core_hours'

curl -H "Authorization: Bearer $KEY" localhost:8080/api/v1/recommendations

curl -H "Authorization: Bearer $KEY" \
  'localhost:8080/api/v1/costs/trend?interval=day&group_by=team&compare=previous_period'
```

## Prerequisites

`go` 1.26+, `docker`, `kind`, `kubectl`, `helm`, `golangci-lint`. A Docker VM with at
least 6 CPUs and 8 GiB.

## Quick start

```bash
make env      # create .env from .env.example
make up       # kind cluster + monitoring + fixtures + Postgres
make run-api  # start the API on :8080
```

Then:

```bash
curl localhost:8080/healthz   # liveness
curl localhost:8080/readyz    # readiness, with per-dependency detail
curl localhost:8080/version   # which build is running
```

| Service | URL | Notes |
|---|---|---|
| Grafana | http://localhost:13000 | `admin` / `prom-operator` |
| Prometheus | http://localhost:19090 | |
| Postgres | `localhost:55432` | db `kca_dev`, user `kca` |

Ports are deliberately unusual. Grafana avoids 3000 (the Next.js dev server takes it in
Phase 8), Prometheus avoids 9090, and Postgres avoids 5432 so a Homebrew Postgres on the
host is never touched.

`make help` lists every target.

## Architecture

```
                    ┌──────────────────────────────────────────┐
                    │  Next.js  (Phase 8)                      │
                    └───────────────────┬──────────────────────┘
                                        │ REST /api/v1
                    ┌───────────────────▼──────────────────────┐
                    │  cmd/api                                 │
                    │  router → middleware → handlers          │
                    └──────┬───────────────────────────────────┘
                           │ repositories (Phase 2)
                    ┌──────▼───────────┐
                    │  PostgreSQL      │  facts + rollups
                    └──────▲───────────┘
                           │ writes
                    ┌──────┴───────────────────────────────────┐
                    │  cmd/collector                           │
                    │  scheduler → worker pool → cost engine   │
                    └───┬──────────────────────────┬───────────┘
         informers (watch)                          │ PromQL range queries
                    ┌───▼──────────┐          ┌────▼─────────────┐
                    │ K8s API      │          │ Prometheus       │
                    │ pods/nodes   │          │ cAdvisor + KSM   │
                    └──────────────┘          └──────────────────┘
```

Two binaries, deliberately. The API scales with user traffic; the collector must **not**
— two collectors would write every cost sample twice and the numbers would be silently
wrong.

**Why Postgres as well as Prometheus.** Prometheus holds the raw time series but cannot
do the joins, ownership rollups and monthly reporting that cost allocation needs, and its
retention is finite. Postgres holds the allocation facts and the aggregates.

### Layout

```
cmd/api          HTTP API — wiring only, no logic
cmd/collector    the collection loop: aligned windows, cost engine, writer
cmd/rollup       the batch job: nightly rollup, backfill, monthly statements
internal/
  buildinfo      version/commit injected at link time
  config         env loading + validation, stdlib only
  logging        log/slog setup and request-scoped loggers
  health         Checker interface + concurrent readiness aggregator
  domain         the vocabulary all three layers share; stdlib imports only
  kube           client-go: dual-mode client, shared informers, pure translation
  pricing        rate catalogue, the CPU/memory split, pure cost arithmetic
  prom           PromQL usage queries (the container!="" filter matters -- see below)
  costing        the join: topology x usage x rates -> cost, with a bounded worker pool
  recommend      the rule engine: evidence gates, p95 right-sizing, severity by failure mode
  rollup         batch orchestration: transaction boundaries, failure isolation, backfill
  httpapi        server, router, JSON responses, handlers for every endpoint
    middleware   request ID, access log, panic recovery, API-key auth, rate limiting
  store/postgres pgx pool, Querier seam, dimension + fact + rollup + statement repositories
migrations       numbered SQL, applied with golang-migrate
deploy/
  kind           cluster definition
  monitoring     Helm values
  demo-workloads fixture workloads
  rbac           read-only ClusterRole + a script that proves it
  pricing        the rate catalogue (edit this; the shipped prices are indicative)
```

## The fixtures are the test data

`deploy/demo-workloads/` contains seven workloads chosen so each detection rule can be
proven right *and* proven not to over-fire. Every value was set by measuring with
`kubectl top`, not guessed.

| Workload | Namespace | Expected verdict |
|---|---|---|
| `over-provisioned-api` | team-payments | Flag — reserves 500m/512Mi, uses ~2m/5Mi |
| `right-sized-worker` | team-platform | **Do not flag** — the control case, ~70% utilised |
| `memory-hoarder` | team-search | Flag — uses 3x its memory request (a *reliability* risk) |
| `no-requests-at-all` | team-search | Flag — consumes real resources, bills as zero |
| `idle-service` | team-search | Flag — zero CPU; recommend **delete**, not resize |
| `over-replicated` | team-platform | **Not flagged, and that is correct** — see below |
| `abandoned-migration-data` | team-payments | Not yet detected — needs a PVC lister |

`right-sized-worker` matters most. Every other fixture is wasteful, so a rule that
returns "everything is wasteful" — or ignores its input entirely — would look correct on
all of them. False positives are what kills adoption of a tool like this.

`no-requests-at-all` is a trap for our own engine: cost computed from requests alone
reports it as free, and its real cost gets smeared silently across every other team. This
is why cost must be billed on `max(request, usage)`.

`over-replicated` is the fixture that did not work out, and it is more useful stated plainly than
quietly re-tuned. It was calibrated until each pod was genuinely well utilised (11m against a 15m
request, 73%) specifically so no per-container rule would fire — the premise being "six replicas
where two would do, and per-pod analysis finds nothing". But nothing else can find it either: if
every pod is 73% utilised, removing replicas overloads the survivors. Whether six busy pods could be
replaced by two *bigger* ones is a question about **traffic**, and no CPU or memory metric answers
it. The rule detects six *idle* replicas, which is the common form of the problem; the harder form
needs request-rate data and is honestly out of reach for now.

The fake instance-type labels on the kind nodes (`m5.large`, `m5.xlarge`, one spot node,
two zones) use the same well-known label keys a cloud provider sets, so the pricing engine
will work unchanged against a real EKS cluster.

## The data model

A star schema: normalised dimensions for mutable current state, one denormalised,
immutable fact table for history.

```
   nodes ──┐
namespaces ─┤
 workloads ─┼──►  container_allocations   (one row per container per window)
     pods ──┘         partitioned by month on window_start
```

Three decisions drive it, and each exists to prevent a specific way of being wrong:

**Container grain, not pod grain.** A pod with a bloated sidecar and a well-sized app
container averages out to "fine" at pod grain. That is the commonest real waste pattern on
any service-mesh cluster, and it is invisible unless the grain is per container. Pod and
workload figures are `SUM`s over this; the reverse is not recoverable.

**Attribution is denormalised onto the fact row.** `team`, `cost_centre`, `instance_type`
and the rates used are copied onto each row at collection time. If `team` were joined from
`namespaces`, relabelling a namespace would silently rewrite every historical report — last
month's reconciled figure would change after the fact. Normalise mutable state, denormalise
immutable history.

**Money is `numeric(20,10)` and `decimal.Decimal`, never float.** `float64` cannot represent
`0.1`, and the error compounds under `SUM` across millions of rows in a number someone
reconciles against a cloud invoice. Worse, the drift depends on summation order, which the
planner may change between runs — so the report disagrees with itself.

Two properties the schema enforces rather than trusts:

- **Idempotency.** The primary key `(window_start, pod_id, container_name)` plus
  `ON CONFLICT DO UPDATE` means a retried collection window converges instead of
  double-counting. A bare `INSERT` would inflate the bill on every retry.
- **The billing rule.** A `CHECK` constraint asserts the stored billable amount really is
  `max(requested, used)`, so even SQL written outside this repo cannot break it.

## Pricing: the assumption you should know about

An `m5.large` costs **one** number per hour and has **two** resources. Splitting that price
between CPU and memory is an *allocation policy*, not a calculation — there is no objectively
correct answer, and any tool implying otherwise has hidden its assumption somewhere you
cannot see it.

Ours lives in `deploy/pricing/catalogue.yaml`, in the open:

```yaml
split:
  cpu: 0.70      # must sum to 1, or the loader refuses to start
  memory: 0.30
```

```
cpu_per_core_hour = hourly × cpu_share / vcpu_count
mem_per_gib_hour  = hourly × mem_share / memory_gib
```

The invariant that makes this coherent rather than merely plausible: **reserving a whole node
for one hour must cost exactly the node's hourly price**, because the shares sum to 1. That is
tested directly across six instance shapes, including a deliberately indivisible 3-vCPU/7-GiB
one so the rounding is real rather than hidden by convenient arithmetic.

Three details that decide whether the numbers can be trusted:

- **We divide by the catalogue's vCPU count, not the node's reported capacity.** You are
  billed for the instance you rented. This is also why the fake labels on the kind nodes work
  — they report 6 CPU but price as the 2 vCPU an `m5.large` actually has.
- **Every rate carries its provenance** (`catalogue`, `explicit_rates`, `fallback`). A cost
  derived from a guess must never look identical to one derived from a real price, or someone
  will take a fabricated figure to a finance meeting.
- **An unknown instance type is priced from a stated fallback and marked as such — never
  zero.** Zero is the tempting option and the worst one: every pod on that node reports as
  free, the cluster total understates the bill, and because the missing money still has to go
  somewhere in a percentage breakdown, every *other* team appears to consume a larger share
  than it does.

The prices shipped in the catalogue are indicative AWS ap-south-1 list prices and are
approximately wrong for you. Reserved Instances, Savings Plans and Enterprise Agreements move
the effective rate by 40% or more. Replace them with the numbers on your invoice.

## How a cost figure is produced

```
informers ──► what exists, and what each CONTAINER reserved
Prometheus ──► what it actually USED over the window
catalogue ──► what a core-hour and a GiB-hour COST on that node
                        │
                        ▼
        billable = max(requested, used)          <- the whole product
        cost     = billable x window x rate
                        │
                        ▼
              container_allocations
```

Four things this gets right that the obvious implementation does not:

**`container!=""` on every PromQL query.** cAdvisor emits a pod-level aggregate series *and*
one per container. Omit the filter and every pod is counted twice — measured on this cluster:
24.0Mi without it, 11.5Mi with it, a **2.1× overcount**.

**Windows are aligned to the interval, not to `now`.** `[now-5m, now)` means a restart at
09:04:10 records a window overlapping the one recorded at 09:02:37. They share no primary key,
so the fact table cannot deduplicate them and the overlap is billed twice — the bill grows with
every restart. Truncating means every process agrees where boundaries are. Verified: three
collector runs over one window produce 37 rows, not 111.

**Init containers are excluded; sidecars are not.** Kubernetes puts both in
`spec.initContainers`, distinguished only by `restartPolicy: Always`. Read it naively and
either every service-mesh proxy vanishes from the bill, or every migration container is charged
forever for ten seconds of work.

**One namespace failing does not lose the other 39.** `errgroup` cancels its context on the
first error, which is right for all-or-nothing work and catastrophic here. The closures always
return `nil` and record failures separately, so `errgroup` acts purely as a bounded pool.
Partial coverage that *reports its gaps* beats a total blank.

## Recommendations: turning cost into advice

**The statistic must match the question.** Cost is an integral, so it uses the *average* over each
window. Right-sizing is a safety question, so it uses **p95 of the per-window peaks**. Sizing a
request on the average guarantees throttling — an average by definition sits below half the
observations. Two different questions, two different statistics, both collected.

**Every rule passes an evidence gate first.** A minimum window count, a minimum observation *span*,
and peak coverage. The span matters more than the count: a hundred windows over one hour still only
describes that hour, so a batch job that runs on Sundays looks abandoned on a Tuesday. The default
range for this endpoint is **7 days**, not the 24 hours the cost endpoints use, for exactly that
reason.

**CPU and memory fail differently, so severity differs.** CPU is compressible — exceeding a request
means CFS throttling, which is slow. Memory is not — exceeding it means the kernel OOMKills the
container. So an under-requested memory finding is `critical` even though acting on it *increases*
cost, while a large CPU saving is merely `info`.

**Savings are allowed to be negative, and are never netted against positives.** The response reports
`potential_monthly_saving` and `required_monthly_increase` as two separate fields. A single net
figure would let a large right-sizing win cancel out a memory increase someone *must* make, and the
page would read "net saving $30" with the reliability fix invisible inside it.

**Headroom rounds up, always.** `p95 x 1.2` was truncated to an integer, which erased the margin
entirely for p95 values of 1–4 millicores — the band most quiet containers live in. Four of the
first five values got no headroom at all and the proposal landed exactly *on* p95, which by
definition 5% of windows exceed. A safety margin rounded down is not a safety margin.

**Over-replication only applies where the replica count is a field someone can set.** The first live
run advised scaling `kindnet` and `node-exporter` to 2 replicas. Both are DaemonSets: one pod per
node, no `replicas` field, and acting on it would have left a node with no network plugin in exchange
for $1.26 a month. The rule read `count(DISTINCT pod_name) = 3` without asking *why* there were
three. The gate is an **allow-list** of kinds, so an unknown CRD defaults to silence rather than to
confident advice about someone else's resource.

## Rollups: making history affordable

**The problem, measured before writing anything.** 74,925 fact rows over 8 days of a 23-container
cluster, and `sum(total_cost) GROUP BY namespace` over a year read 3,372 buffers (~26 MB) to produce
six rows. Projected to 5,000 containers at 288 five-minute windows a day: 1.44 M rows/day, 525 M/year.
A dashboard drawing a twelve-point annual trend would scan half a billion rows — not because the query
is bad, but because the data is stored at 288x the resolution the question needs.

**Measured after:** 74,925 rows → 256, or **292.7x**. Query cost 3,372 buffers → **18**.

### The rule that decided the whole design: not every aggregate rolls up

| Aggregate | Rolls up? | Why |
|---|---|---|
| `sum` | ✅ | the sum of sums is the sum |
| `max` / `min` | ✅ | the max of maxes is the max |
| `count` | ✅ | it is a sum |
| `avg` | ⚠️ **only as sum ÷ count** | averaging averages is wrong unless every count is equal, and window counts are never equal once a container starts mid-day |
| **`percentile`** | ❌ **never** | p95 of daily p95s is not the p95 of the data. Nothing fixes this but a mergeable sketch |

Two consequences designed in rather than discovered:

**The rollup stores core-hours, not millicores.** A core-hour is a sum, so it re-aggregates correctly
at any grain. Storing `avg_millicores` would make a monthly average weight a two-hour day identically
to a twenty-four-hour one.

**The recommendation engine keeps reading the fact table**, because its p95 cannot come from a rollup.
That is an architectural boundary, not an omission — and it is why raw-fact retention must always
exceed the recommendation window.

### DELETE-then-INSERT, not upsert

Upsert is what the fact table uses, so consistency argued for it. It is wrong for a rollup, and the
reason is the difference between a row and a *projection*. An upsert can only correct rows it is given:
if a dimension tuple that existed on Monday no longer appears in Monday's facts — a mislabelled
namespace corrected, a duplicated pod cleaned up — re-running writes the new rows and **leaves the old
one forever**. Nothing will reference its tuple again, so nothing will overwrite it.

`DELETE` then `INSERT ... SELECT`, in one transaction, makes the operation *"make the rollup equal the
fact table for this day"*. Verified: dropping the whole rollup and rebuilding from facts takes 711 ms
and reproduces figures identical to the fact table for every namespace, to the last decimal place.

### The aggregation never leaves Postgres

`INSERT ... SELECT`, not "SELECT into Go, aggregate, INSERT back". The instinctive shape would move
1.44 M rows/day over the network to produce 4,900, and reimplement Postgres's aggregation in Go where
it can drift from the SQL the summary endpoint uses. Cost figures computed two ways eventually
disagree, and then nobody knows which is right.

### Which table answers a trend, and saying so

A rollup only pays off if something reads it, and only stays *correct* if that something declines to
read it for the questions it cannot answer:

```
interval=hour        -> raw_facts    the rollup's finest grain is a day
group_by=pod         -> raw_facts    pod is the one dimension the rollup drops
otherwise            -> daily_rollup ~293x less data
```

`source` is in the response body, as part of the contract. The two do not answer identically, and *"the
trend disagrees with the summary"* should be answerable from the response rather than by reading our
source. There is a test asserting the two agree for any question both can serve.

**Pod is dropped on purpose.** It costs 1.6x of the compression (253x with pods, 293x without), which
is not the argument. A pod name is not a stable identity: a Deployment rollout replaces every pod, so a
per-pod daily series fragments on every deploy. Per-pod detail lives in `/api/v1/allocations`.

### The UTC-day grain, and what it costs

`date_trunc('day', ts)` depends on the session timezone, so the same rows bucket differently for
different readers. Fixing it at UTC makes the rollup deterministic and byte-reproducible.

The cost, stated plainly: an IST month boundary falls at 18:30 UTC the previous day, so an IST-aligned
month is 30 UTC days plus two half-days this table has already merged into their neighbours. **That is
unrecoverable from the rollup** — the fact table is the only thing that can answer it, which is another
reason retention matters.

## Monthly statements: not "what is true now" but "what we said"

Every other table here answers *what is true now*. This one is stored, because a statement must not
change after it is issued: backfill a missing day, correct a price, or fix a rollup bug, and a
computed-on-demand figure for last August silently becomes a different number from the one somebody
already reported.

`finalised_at` is the line — `null` is provisional and regenerable, set is frozen. **Frozen is enforced
by database trigger**, not only by the writer. The upsert carries `WHERE finalised_at IS NULL` so the
normal path skips signed-off rows gracefully and reports how many it left alone; the trigger is the
backstop for every *other* writer, because an invariant enforced only by the one function that
currently writes a table lasts exactly until the second writer appears.

**Coverage is what separates a statement from a chart.** A month containing a collector outage produces
a figure that is confidently too low, and nothing about the figure reveals it. Coverage is
`days_with_data / days_in_month`, so it is day-level — a day the collector ran for one hour counts as
full — which is why `window_count` is reported alongside and documented as the way to see a partial day.

Three scopes in **one pass** via `GROUPING SETS`. Three separate queries would scan the month three
times and could see different data between them, so a cluster statement might not equal the sum of its
namespace statements for no reason a reader could discover.

An unlabelled container gets **no team statement** rather than one belonging to `""`. So the gap between
a cluster statement and the sum of its team statements *is* the unattributed spend — on this cluster,
half the bill, because kube-system and monitoring carry no team label.

## The batch job: idempotency, backfill, and one lock

`cmd/rollup` is a third binary rather than a ticker in the collector, and the reasons are lifecycle:

- **A batch job completes.** It has an exit code, so "did last night's rollup succeed" is answerable. A
  goroutine in a service has no exit code — its failures are visible only if someone reads the logs.
- **Backfill is a CLI invocation.** A daemon cannot be asked "roll up July", so it grows a second code
  path for backfill, and two code paths for the same computation eventually disagree.
- **Failure domains.** A rollup that OOMs must not stop collection. Collection cannot be caught up —
  Prometheus retention expires — whereas a rollup recomputes from facts at any time. The recoverable
  job must not be able to kill the unrecoverable one.

**One transaction per day, not one per range.** A failure on day 40 of a 60-day backfill must keep the
39 days already written, and a failing day is recorded and the run *continues* — the same decision
`internal/costing` makes for a failing namespace. Then a non-zero exit, after doing everything possible.

**Yesterday, never today.** Today is incomplete, so rolling it up writes a figure correct for the hours
so far and wrong for the day — and because the rollup is a projection, the next run replaces it, so the
value flickers instead of converging.

**A Postgres advisory lock, not a Kubernetes Lease.** `concurrencyPolicy: Forbid` is not a guarantee.
A Lease needs new RBAC and a TTL you wait out after a crash. The contested resource *is* the database,
and an advisory lock dies with its connection — so a hard crash releases it immediately. A contended run
**exits 0**: the holder is already doing the work, and a CronJob whose failure count increments every
time it overlaps a backfill has a failure count that means nothing.

The subtlety worth knowing: *session*-scoped locks need a **pinned connection**. The obvious
`pool.Exec(...pg_try_advisory_lock...)` then `pool.Exec(...unlock...)` takes the lock on one pooled
connection and releases on another. `pg_advisory_unlock` returns false and warns rather than raising, so
nothing appears to fail — the lock stays held until that connection recycles, and every later run exits
"another process holds the lock" while no process does. **A job that stops working by succeeding.**
Measured, that leak really does happen: releasing on a cancelled context fails with
`context already done`, the connection returns to the pool healthy, and `pg_locks` still shows the lock.

## API design decisions

**Cursor pagination, not offset.** `OFFSET 5000` makes Postgres produce and discard 5,000 rows, and
it is *inconsistent under concurrent writes* — the collector appends every five minutes, so rows
shift between requests and a client silently sees a duplicate or misses one. Keyset pagination on
`(window_start, pod_id, container_name)` is an index range scan and stable. Verified: walking every
page returns 74 rows with **zero duplicates**, matching the table exactly.

**Filters and sorts go through allow-lists.** SQL placeholders bind *values*, not *identifiers* —
there is no `GROUP BY $1`. So the request carries a symbolic name, the repository maps it to columns
it owns, and an unrecognised name is a 400 rather than a query.

**Over-large `limit` is refused, not clamped.** Silently returning 100 rows to a caller who asked for
10,000 makes them believe they have everything — and a client paginating on "did I get fewer rows
than I asked for?" would stop early and lose data.

**Auth exempts the probes, and that is not convenience.** The kubelet cannot present a credential, so
a 401 on `/healthz` reads as a probe failure and the container is killed. Enabling authentication
would put the service into CrashLoopBackOff. The rate limiter exempts them for the same reason.

**Production refuses to start without `API_KEYS`.** Secure by default means the *insecure*
configuration is the one that will not run. Development starts open and warns loudly on every boot.

**Keys are compared in constant time** against a SHA-256 digest. A plain `==` returns on the first
differing byte, so it takes measurably longer the more of the prefix matched — an attacker recovers
the key one position at a time, in linear rather than exponential attempts.

## Development

```bash
make migrate-up    # apply migrations
make migrate-reset # drop and re-apply from scratch
make rollup            # roll up yesterday (what the Phase 10 CronJob will run)
make rollup-backfill   # roll up every day with fact rows. Safe to re-run: it is a projection
make rollup-month MONTH=2026-07   # roll up a month and write its statements
make rollup-close MONTH=2026-07   # ...and FREEZE them. Irreversible without a deliberate un-finalise
make check         # fmt + vet + lint + test — everything CI runs
make test          # go test -race
make docker-build  # container image
make db-psql       # psql shell
make env-check     # report .env keys that .env.example has gained since you created it
make rbac-up       # apply the ServiceAccount, ClusterRole and binding
make rbac-verify   # prove the RBAC grants reads and denies every write
make reset         # tear down and rebuild everything
```

### On RBAC verification

`make rbac-verify` impersonates the ServiceAccount via `kubectl auth can-i --as=` and
asks the API server's own authoriser. It tests the real ClusterRole without deploying
anything, so the fast local loop stays intact.

The **negative** assertions are the point. A ClusterRole granting cluster-admin passes every
"can I read?" check; only `delete pods → no` and `list secrets → no` demonstrate that least
privilege holds. Same reasoning as the `right-sized-worker` fixture.

The grant is exactly `get`/`list`/`watch` on **nodes, namespaces, pods and replicasets** — the
four the code actually reads. An audit found nine further resources granted on the reasoning that
they were "the remaining pieces of the cost picture"; the code read none of them, which
contradicted the least-privilege argument three paragraphs above it in the same file. They were
removed, and `verify.sh` now asserts each is **denied**, so the removal is enforced rather than
claimed.

Phase 6 did not need them after all, and the grant is unchanged as a result. Two findings it would
have improved are noted honestly instead of quietly added: orphaned-PVC detection needs a PVC
lister, and reliable idle detection needs traffic data from Services. Both remain ungranted until
there is code that reads them.

## Roadmap

| Phase | Delivers |
|---|---|
| **0** ✅ | Reproducible environment + Go skeleton |
| **1** ✅ | Live inventory via client-go informers + RBAC |
| **2** ✅ | Postgres schema, migrations, repositories |
| **3** ✅ | Pricing engine behind a provider interface |
| **4** ✅ | Cost engine + Prometheus collector (worker pools, channels, context) |
| **5** ✅ | REST API v1 — pagination, filtering, auth, rate limiting, OpenAPI |
| **6** ✅ | Idle detection + recommendation engine (p95 right-sizing, evidence gates) |
| **7** ✅ | Daily rollups, trends, immutable monthly statements, advisory-lock batch job |
| 8 | Next.js frontend |
| 9 | Observability — own metrics, recording rules, Grafana dashboards as code |
| 10 | Helm chart + GitHub Actions |
| 11+ | Multi-cluster; AWS, GCP and Azure pricing |
