# Kubernetes Cost Analyzer -- developer entry point.
#
# WHY THIS FILE EXISTS
# --------------------
# Every command needed to build, run, test and reproduce the local environment lives
# here. Nothing is created by hand. If a command is not in this file, it is not
# reproducible, and an environment nobody can reproduce is not a foundation -- it is
# a machine-specific accident that dies with your shell history.
#
# Start here:  make help

# Use bash with strict flags so a failing command in a multi-line recipe actually
# fails the target instead of silently continuing.
SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c

# Load local config if present. The leading '-' means "do not error if missing",
# so `make help` works on a fresh clone before you have run `make env`.
# `export` passes every variable through to the commands in each recipe.
-include .env
export

# --- Configuration -----------------------------------------------------------
CLUSTER_NAME      ?= kca-dev
KIND_CONFIG       ?= deploy/kind/cluster.yaml
MONITORING_NS     ?= monitoring
PG_CONTAINER      ?= kca-postgres
PG_VOLUME         ?= kca-pgdata
PG_IMAGE          ?= postgres:18-alpine
IMAGE_REPO        ?= kca
POSTGRES_USER     ?= kca
POSTGRES_DB       ?= kca_dev
POSTGRES_HOST_PORT ?= 55432

# --- Build metadata ----------------------------------------------------------
# Injected into the binary at link time rather than hardcoded in source, so an
# image can always report exactly which commit it was built from. Fallbacks keep
# this working before the first commit exists.
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
BUILDINFO  := github.com/Pheonix2507/kubernetes-cost-analyzer/internal/buildinfo
LDFLAGS    := -s -w \
	-X $(BUILDINFO).Version=$(VERSION) \
	-X $(BUILDINFO).Commit=$(COMMIT) \
	-X $(BUILDINFO).BuildTime=$(BUILD_TIME)

.DEFAULT_GOAL := help

# =============================================================================
# Help
# =============================================================================
.PHONY: help
help: ## Show this help
	@echo "Kubernetes Cost Analyzer"
	@echo ""
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'
	@echo ""

# =============================================================================
# Local environment
# =============================================================================
.PHONY: env
env: ## Create .env from .env.example, or report drift if it already exists
	@if [ -f .env ]; then \
		echo ".env already exists, leaving it alone"; \
		$(MAKE) --no-print-directory env-check; \
	else \
		cp .env.example .env && echo "created .env from .env.example"; \
	fi

.PHONY: env-check
# WHY THIS TARGET EXISTS
#
# `make env` refuses to overwrite an existing .env, which is right -- it holds local edits and a
# password. The consequence is that every variable added to .env.example afterwards is MISSING
# from an existing .env, silently, and the service quietly runs on defaults.
#
# That is usually harmless because the defaults are sensible. CLUSTER_NAME is the exception: it
# is denormalised onto every cost row, so running on the default writes data attributed to a
# cluster called "default" and nothing complains. This audit found exactly that, eight keys
# behind after four phases.
#
# So the drift is reported rather than left to be discovered.
env-check: ## Report variables present in .env.example but missing from .env
	@if [ ! -f .env ]; then echo "no .env yet; run: make env"; exit 0; fi
	@missing=$$(comm -23 \
		<(grep -oE '^[A-Z_]+=' .env.example | tr -d '=' | sort) \
		<(grep -oE '^[A-Z_]+=' .env | tr -d '=' | sort)); \
	if [ -n "$$missing" ]; then \
		echo ""; \
		echo "  .env is missing keys that .env.example defines:"; \
		echo "$$missing" | sed 's/^/    /'; \
		echo ""; \
		echo "  Defaults apply, so nothing breaks -- but CLUSTER_NAME in particular is stored on"; \
		echo "  every cost row, so running on the default attributes data to \"default\"."; \
		echo "  Copy the missing lines across from .env.example."; \
		echo ""; \
	else \
		echo ".env is in sync with .env.example"; \
	fi

.PHONY: up
up: cluster-up monitoring-up demo-up db-up ## Bring up EVERYTHING from scratch
	@echo ""
	@echo "Environment ready."
	@$(MAKE) --no-print-directory urls

.PHONY: down
down: cluster-down db-down ## Tear down everything (cluster + database)

.PHONY: reset
reset: down up ## Full clean rebuild

.PHONY: urls
urls: ## Print local service URLs
	@echo "  Grafana     http://localhost:13000   (admin / prom-operator)"
	@echo "  Prometheus  http://localhost:19090"
	@echo "  Postgres    localhost:$(POSTGRES_HOST_PORT) db=$(POSTGRES_DB) user=$(POSTGRES_USER)"

# =============================================================================
# Kubernetes cluster
# =============================================================================
.PHONY: cluster-up
cluster-up: ## Create the kind cluster from deploy/kind/cluster.yaml
	@if kind get clusters 2>/dev/null | grep -qx "$(CLUSTER_NAME)"; then \
		echo "cluster $(CLUSTER_NAME) already exists"; \
	else \
		kind create cluster --config $(KIND_CONFIG) --wait 120s; \
	fi
	@kubectl get nodes -L node.kubernetes.io/instance-type,topology.kubernetes.io/zone,kca.io/capacity-type

.PHONY: cluster-down
cluster-down: ## Delete the kind cluster
	@kind delete cluster --name $(CLUSTER_NAME) 2>/dev/null || true

.PHONY: monitoring-up
monitoring-up: ## Install metrics-server + kube-prometheus-stack via Helm
	# --force-update makes this idempotent. Helm repos are GLOBAL machine state, not
	# per-project: without it, `helm repo add` fails if the name already exists with
	# a different URL, so this target would break on any machine that had ever added
	# these repos before. Exactly the class of bug this Makefile exists to prevent.
	helm repo add --force-update prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null
	helm repo add --force-update metrics-server https://kubernetes-sigs.github.io/metrics-server/ >/dev/null
	helm repo update >/dev/null
	# `upgrade --install` is idempotent: it installs if absent, upgrades if present.
	# Plain `helm install` errors on a second run, which makes it useless in a Makefile.
	helm upgrade --install metrics-server metrics-server/metrics-server \
		--namespace kube-system \
		--values deploy/monitoring/metrics-server-values.yaml \
		--wait --timeout 5m
	helm upgrade --install kps prometheus-community/kube-prometheus-stack \
		--namespace $(MONITORING_NS) --create-namespace \
		--values deploy/monitoring/kps-values.yaml \
		--wait --timeout 10m
	@kubectl get pods -n $(MONITORING_NS)

.PHONY: monitoring-down
monitoring-down: ## Uninstall the monitoring stack
	@helm uninstall kps -n $(MONITORING_NS) 2>/dev/null || true
	@helm uninstall metrics-server -n kube-system 2>/dev/null || true

.PHONY: demo-up
demo-up: ## Apply the demo fixture workloads
	kubectl apply -f deploy/demo-workloads/
	@echo "waiting for fixture pods to become ready..."
	@kubectl wait --for=condition=Ready pods \
		-l app.kubernetes.io/component=long-running \
		--all-namespaces --timeout=180s || true
	@kubectl get pods -A -l app.kubernetes.io/part-of=kca-fixtures -o wide

.PHONY: demo-down
demo-down: ## Remove the demo fixture workloads
	@kubectl delete -f deploy/demo-workloads/ --ignore-not-found=true

.PHONY: rbac-up
rbac-up: ## Apply the ServiceAccount, ClusterRole and ClusterRoleBinding
	kubectl apply -f deploy/rbac/

.PHONY: rbac-verify
rbac-verify: ## Prove the RBAC grants exactly what is needed and nothing more
	# `kubectl auth can-i --as=` impersonates the ServiceAccount and asks the API
	# server's authoriser directly. This tests the REAL RBAC without deploying anything,
	# which is why we can keep the fast local dev loop and still know the ClusterRole is
	# correct before Phase 10 ever builds a Deployment.
	#
	# The NEGATIVE assertions matter more than the positive ones. Any ClusterRole that
	# is too permissive still passes every "can I read?" check -- only "can I delete?"
	# returning no proves least privilege actually holds.
	@bash deploy/rbac/verify.sh

# =============================================================================
# Database
# =============================================================================
.PHONY: db-up
# NOTE ON THE VOLUME MOUNT PATH -- this changed in Postgres 18.
#
# Pre-18 images expected the data volume at /var/lib/postgresql/data. From 18 onwards
# the official image wants it ONE LEVEL UP, at /var/lib/postgresql, and stores data in
# a major-version-specific subdirectory beneath it. That layout is what allows
# `pg_upgrade --link` to work without crossing a mount-point boundary.
#
# Using the old path does not silently misbehave -- the image REFUSES TO START and
# prints a long explanation, which is the right call: guessing would risk a future
# major upgrade destroying data. Worth remembering as a general lesson about pinned
# major-version bumps in container images.
db-up: ## Start the local Postgres container
	@if [ -n "$$(docker ps -aq -f name='^$(PG_CONTAINER)$$')" ]; then \
		docker start $(PG_CONTAINER) >/dev/null; \
		echo "postgres: reusing existing container"; \
	else \
		docker run -d --name $(PG_CONTAINER) \
			-e POSTGRES_USER=$(POSTGRES_USER) \
			-e POSTGRES_PASSWORD=$${POSTGRES_PASSWORD:-kca_local_dev_only} \
			-e POSTGRES_DB=$(POSTGRES_DB) \
			-p $(POSTGRES_HOST_PORT):5432 \
			-v $(PG_VOLUME):/var/lib/postgresql \
			$(PG_IMAGE) >/dev/null; \
		echo "postgres: created container $(PG_CONTAINER)"; \
	fi
	@printf "waiting for postgres"
	@for i in $$(seq 1 60); do \
		if docker exec $(PG_CONTAINER) pg_isready -U $(POSTGRES_USER) -d $(POSTGRES_DB) >/dev/null 2>&1; then \
			echo " ready on localhost:$(POSTGRES_HOST_PORT)"; exit 0; \
		fi; \
		printf "."; sleep 1; \
	done; \
	echo " TIMED OUT"; docker logs --tail 30 $(PG_CONTAINER); exit 1

.PHONY: db-down
db-down: ## Stop and remove the Postgres container (keeps the data volume)
	@docker rm -f $(PG_CONTAINER) 2>/dev/null >/dev/null || true
	@echo "postgres: container removed (volume $(PG_VOLUME) kept)"

.PHONY: db-nuke
db-nuke: db-down ## Remove the Postgres container AND destroy its data volume
	@docker volume rm $(PG_VOLUME) 2>/dev/null >/dev/null || true
	@echo "postgres: volume $(PG_VOLUME) destroyed"

.PHONY: db-psql
db-psql: ## Open a psql shell against the local database
	@docker exec -it $(PG_CONTAINER) psql -U $(POSTGRES_USER) -d $(POSTGRES_DB)

# -----------------------------------------------------------------------------
# Migrations
# -----------------------------------------------------------------------------
# golang-migrate tracks the applied version in a schema_migrations table, so `up` is
# idempotent and only applies what is missing.
#
# THE RULE THIS TOOLING CANNOT ENFORCE: never edit a migration that has been applied
# anywhere. The version number is all migrate compares, so an edited file leaves the
# database and the repository silently disagreeing. Always add a new migration.
.PHONY: migrate-up
migrate-up: ## Apply all pending migrations
	migrate -path migrations -database "$(DATABASE_URL)" up

.PHONY: migrate-down
migrate-down: ## Roll back the most recent migration
	migrate -path migrations -database "$(DATABASE_URL)" down 1

.PHONY: migrate-reset
migrate-reset: ## Drop everything and re-apply from scratch (local only)
	migrate -path migrations -database "$(DATABASE_URL)" drop -f
	$(MAKE) --no-print-directory migrate-up

.PHONY: migrate-version
migrate-version: ## Show the current schema version
	@migrate -path migrations -database "$(DATABASE_URL)" version

.PHONY: migrate-create
migrate-create: ## Create a new migration pair: make migrate-create NAME=add_foo
	@test -n "$(NAME)" || (echo "usage: make migrate-create NAME=add_something" && exit 1)
	migrate create -ext sql -dir migrations -seq $(NAME)

# =============================================================================
# Go
# =============================================================================
.PHONY: run-api
run-api: ## Run the API server locally
	go run -ldflags '$(LDFLAGS)' ./cmd/api

.PHONY: run-collector
run-collector: ## Run the collector locally
	go run -ldflags '$(LDFLAGS)' ./cmd/collector

# =============================================================================
# Frontend (web/)
# =============================================================================

.PHONY: web-install
web-install: ## Install frontend dependencies
	cd web && pnpm install

.PHONY: web-dev
web-dev: ## Run the dashboard in development. Needs `make run-api` in another terminal
	cd web && pnpm dev

.PHONY: web-build
web-build: ## Production build of the dashboard
	cd web && pnpm build

.PHONY: web-gen
web-gen: ## Regenerate the frontend's TypeScript types from api/openapi.yaml
	cd web && pnpm gen:api

.PHONY: web-check
web-check: ## Typecheck + lint + assert the generated types match the spec
	cd web && pnpm check

.PHONY: rollup
rollup: ## Roll up yesterday (what the Phase 10 CronJob will run)
	go run -ldflags '$(LDFLAGS)' ./cmd/rollup

.PHONY: rollup-backfill
rollup-backfill: ## Roll up EVERY day that has fact rows. Safe to re-run: the rollup is a projection, not an accumulation
	go run -ldflags '$(LDFLAGS)' ./cmd/rollup -all

.PHONY: rollup-month
rollup-month: ## Roll up a month and write its statements: make rollup-month MONTH=2026-07
	@test -n "$(MONTH)" || (echo "usage: make rollup-month MONTH=2026-07" && exit 1)
	go run -ldflags '$(LDFLAGS)' ./cmd/rollup -month $(MONTH)

.PHONY: rollup-close
rollup-close: ## Roll up a month, write its statements and FREEZE them. Irreversible without a deliberate un-finalise
	@test -n "$(MONTH)" || (echo "usage: make rollup-close MONTH=2026-07" && exit 1)
	go run -ldflags '$(LDFLAGS)' ./cmd/rollup -month $(MONTH) -close

.PHONY: build
build: ## Compile all three binaries into ./bin
	@mkdir -p bin
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/api ./cmd/api
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/collector ./cmd/collector
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/rollup ./cmd/rollup
	@ls -lh bin/

.PHONY: test
test: ## Run all tests with the race detector
	go test -race -count=1 ./...

.PHONY: cover
cover: ## Run tests and open an HTML coverage report
	go test -race -count=1 -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "wrote coverage.html"

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run ./...

.PHONY: fmt
fmt: ## Format code and tidy imports
	golangci-lint fmt ./...

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: tidy
tidy: ## Tidy go.mod / go.sum
	go mod tidy

.PHONY: check
check: fmt vet lint test ## Everything CI runs for Go. See check-all for the frontend too.

.PHONY: check-all
check-all: check web-check ## Go AND the frontend
	# Separate from `check` deliberately. The Go loop is the one an engineer runs dozens of times an
	# hour and it must stay fast; the frontend adds a TypeScript pass and an ESLint pass that are
	# irrelevant to a change in internal/costing. CI runs check-all; a human usually runs check.
	#
	# web-check includes `check:api`, which regenerates the TypeScript types from api/openapi.yaml and
	# fails if the result differs from what is committed. That is the last link in the drift chain:
	# openapi_test.go asserts the spec matches the Go allow-lists, and this asserts the TypeScript
	# matches the spec. SQL -> Go -> spec -> TypeScript, every link checked by something that fails
	# loudly.
	@echo "Go and frontend checks passed."

# =============================================================================
# Docker
# =============================================================================
.PHONY: docker-build
docker-build: ## Build the container image
	# No DOCKER_BUILDKIT=1 here on purpose: forcing it fails outright on a machine
	# where the buildx plugin is not on Docker's plugin search path (a common state
	# with Colima or a Homebrew-installed buildx). The Dockerfile is written to build
	# on any builder -- see the caching note in it.
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t $(IMAGE_REPO)/api:$(VERSION) \
		-t $(IMAGE_REPO)/api:latest \
		--target api .
	# BOTH stages are built. The Dockerfile has always had a collector target, but only api was
	# ever built -- so a broken collector stage would have been discovered at deploy time in
	# Phase 10 rather than here. An image nothing builds is an image nothing tests.
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t $(IMAGE_REPO)/collector:$(VERSION) \
		-t $(IMAGE_REPO)/collector:latest \
		--target collector .
	# And the rollup, for the same reason: a target nothing builds is a target nothing tests.
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t $(IMAGE_REPO)/rollup:$(VERSION) \
		-t $(IMAGE_REPO)/rollup:latest \
		--target rollup .
	@docker images $(IMAGE_REPO)/api
	@docker images $(IMAGE_REPO)/collector
	@docker images $(IMAGE_REPO)/rollup

.PHONY: kind-load
kind-load: docker-build ## Load both images into the kind cluster (no registry needed)
	kind load docker-image $(IMAGE_REPO)/api:latest --name $(CLUSTER_NAME)
	kind load docker-image $(IMAGE_REPO)/collector:latest --name $(CLUSTER_NAME)
