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

## Status: Phase 1 complete

A reproducible local environment, a production-shaped Go skeleton, and live cluster
topology served over a read-only API. **No pricing yet** — that is Phase 3.

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
- A 17 MB non-root, shell-less container image

### Endpoints

| Method | Path | Returns |
|---|---|---|
| GET | `/healthz` | liveness — checks nothing, by design |
| GET | `/readyz` | readiness — per-dependency detail for Postgres and the informer cache |
| GET | `/version` | the build actually running |
| GET | `/api/v1/nodes` | capacity, allocatable, instance type, zone, spot vs on-demand |
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
  kube           client-go: dual-mode client, shared informers, pure translation
  store/postgres pgx pool; repositories from Phase 2
deploy/
  kind           cluster definition
  monitoring     Helm values
  demo-workloads fixture workloads
  rbac           read-only ClusterRole + a script that proves it
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

## Development

```bash
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
| 2 | Postgres schema, migrations, repositories |
| 3 | Pricing engine behind a provider interface |
| 4 | Cost engine + Prometheus collector (worker pools, channels, context) |
| 5 | REST API v1 — pagination, filtering, auth, rate limiting, OpenAPI |
| 6 | Idle detection + recommendation engine |
| 7 | Rollups, historical trends, monthly reports |
| 8 | Next.js frontend |
| 9 | Observability — own metrics, recording rules, Grafana dashboards as code |
| 10 | Helm chart + GitHub Actions |
| 11+ | Multi-cluster; AWS, GCP and Azure pricing |
