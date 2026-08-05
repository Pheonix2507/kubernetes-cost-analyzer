# Kubernetes Cost Analyzer

Attributes Kubernetes spend to the teams, namespaces and workloads that cause it, and
recommends what to change.

The core idea is one subtraction. Kubernetes bills you for what you **reserve**
(resource requests), not for what you **use** — the scheduler holds a request against a
node whether or not the container ever touches it. The gap between the two is waste, and
in most clusters that has never been measured it is the majority of the bill.

```
waste = requested - used
```

Everything else in this repository is data collection, aggregation and presentation on
top of that.

## Status: Phase 4 complete — it produces real numbers

The collector runs, queries Prometheus for observed usage, joins it to cluster topology and
rates, and writes per-container cost into Postgres. **The product works end to end.** What
remains is reporting on it (Phases 5-8) and operating it (9-10).

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
- A non-root, shell-less container image

### Endpoints

| Method | Path | Returns |
|---|---|---|
| GET | `/healthz` | liveness — checks nothing, by design |
| GET | `/readyz` | readiness — per-dependency detail for Postgres and the informer cache |
| GET | `/version` | the build actually running |
| GET | `/api/v1/nodes` | capacity, allocatable, instance type, zone, spot vs on-demand, **and rates** |
| GET | `/api/v1/namespaces` | cost-allocation dimensions (team, cost-centre, environment) |
| GET | `/api/v1/pods` | requests, limits, QoS class, resolved workload. `?namespace=` filters |

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
cmd/collector    background collection loop (skeleton)
internal/
  buildinfo      version/commit injected at link time
  config         env loading + validation, stdlib only
  logging        log/slog setup and request-scoped loggers
  health         Checker interface + concurrent readiness aggregator
  httpapi        server, router, JSON responses, health handlers
    middleware   request ID, structured access log, panic recovery
  domain         the vocabulary all three layers share; stdlib imports only
  kube           client-go: dual-mode client, shared informers, pure translation
  pricing        rate catalogue, the CPU/memory split, pure cost arithmetic
  prom           PromQL usage queries (the container!="" filter matters -- see below)
  costing        the join: topology x usage x rates -> cost, with a bounded worker pool
  store/postgres pgx pool, Querier seam, dimension + fact repositories
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
| `over-replicated` | team-platform | Flag — each pod is fine, six of them is not |
| `abandoned-migration-data` | team-payments | Flag — 2Gi PVC bound with no consumer |

`right-sized-worker` matters most. Every other fixture is wasteful, so a rule that
returns "everything is wasteful" — or ignores its input entirely — would look correct on
all of them. False positives are what kills adoption of a tool like this.

`no-requests-at-all` is a trap for our own engine: cost computed from requests alone
reports it as free, and its real cost gets smeared silently across every other team. This
is why cost must be billed on `max(request, usage)`.

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

## Development

```bash
make migrate-up    # apply migrations
make migrate-reset # drop and re-apply from scratch
make check         # fmt + vet + lint + test — everything CI runs
make test          # go test -race
make docker-build  # container image
make db-psql       # psql shell
make rbac-up       # apply the ServiceAccount, ClusterRole and binding
make rbac-verify   # prove the RBAC grants reads and denies every write
make reset         # tear down and rebuild everything
```

### On RBAC verification

`make rbac-verify` impersonates the ServiceAccount via `kubectl auth can-i --as=` and
asks the API server's own authoriser. It tests the real ClusterRole without deploying
anything, so the fast local loop stays intact.

The **negative** assertions are the point. A ClusterRole granting cluster-admin passes
every "can I read?" check; only `delete pods → no` and `list secrets → no` demonstrate
that least privilege holds. Same reasoning as the `right-sized-worker` fixture.

## Roadmap

| Phase | Delivers |
|---|---|
| **0** ✅ | Reproducible environment + Go skeleton |
| **1** ✅ | Live inventory via client-go informers + RBAC |
| **2** ✅ | Postgres schema, migrations, repositories |
| **3** ✅ | Pricing engine behind a provider interface |
| **4** ✅ | Cost engine + Prometheus collector (worker pools, channels, context) |
| 5 | REST API v1 — pagination, filtering, auth, rate limiting, OpenAPI |
| 6 | Idle detection + recommendation engine |
| 7 | Rollups, historical trends, monthly reports |
| 8 | Next.js frontend |
| 9 | Observability — own metrics, recording rules, Grafana dashboards as code |
| 10 | Helm chart + GitHub Actions |
| 11+ | Multi-cluster; AWS, GCP and Azure pricing |
