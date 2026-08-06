# Multi-stage build for both binaries.
#
# WHY MULTI-STAGE
# ---------------
# The Go toolchain is ~800MB. The compiled binary is ~10MB. A single-stage build ships
# the compiler, the module cache and the entire source tree to production, where none
# of it is needed and all of it is attack surface: every package in that image is
# something a CVE scanner will flag and someone will have to triage.
#
# Multi-stage builds in one image and copies ONLY the binary into another. The final
# image contains our binary and nothing else -- no shell, no package manager, no libc.
#
#   make docker-build

# Pinned to the version in go.mod. Bump both together, deliberately.
ARG GO_VERSION=1.26

# =============================================================================
# Stage 1: build
# =============================================================================
FROM golang:${GO_VERSION} AS builder

WORKDIR /src

# LAYER CACHING: copy the manifests and download dependencies BEFORE copying source.
#
# Docker caches each layer keyed on the files it touches. Dependencies change rarely;
# source changes constantly. Splitting them this way means an ordinary code edit reuses
# the cached dependency layer, and only re-downloads when go.mod or go.sum actually
# change. Doing `COPY . .` first would invalidate the download on every single edit and
# turn a 5-second rebuild into a 2-minute one.
COPY go.mod go.sum ./

# NOTE ON BUILD CACHING -- WHY THERE ARE NO `--mount=type=cache` LINES HERE
#
# BuildKit cache mounts (`RUN --mount=type=cache,target=/go/pkg/mod`) persist the
# module and build caches ACROSS builds, even when a layer cache is invalidated. They
# are a genuine speedup on dependency bumps.
#
# They are deliberately NOT used, because they require BuildKit AND a working buildx
# plugin, and a Dockerfile that only builds on a correctly-configured machine is not
# reproducible. The layer ordering above -- manifests before source -- is the
# optimisation that actually matters, and it works on every builder.
#
# To opt in on a machine with buildx available, add the mount flags back and build
# with DOCKER_BUILDKIT=1.
RUN go mod download

COPY . .

# Build metadata, passed in by the Makefile from git.
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown

# TARGETOS and TARGETARCH are set automatically by BuildKit. Naming them explicitly is
# what makes `docker buildx build --platform linux/amd64,linux/arm64` work from an
# arm64 Mac: Go cross-compiles for free, so we never need emulation.
ARG TARGETOS
ARG TARGETARCH

# CGO_ENABLED=0 produces a STATICALLY LINKED binary. This is what makes a distroless
# (or even scratch) base image possible: with cgo enabled, the binary dynamically links
# against the build image's glibc and would fail at startup with "no such file or
# directory" -- a famously confusing error, because the file it cannot find is the
# linker, not the binary.
#
# -trimpath strips local filesystem paths from the binary. Without it, panics leak
# /Users/<you>/Desktop/... into production stack traces, and builds are not
# reproducible because the output depends on where the source happened to live.
#
# -s -w strip the symbol table and DWARF debug info, cutting roughly 30% of the size.
# The trade-off: no symbolised stack traces from a core dump. Acceptable because we get
# stack traces from our panic handler instead. Drop these flags if you need to attach
# a debugger to a production binary.
# GOARCH=${TARGETARCH} -- AND THE `=` MUST BE LITERAL. THIS LINE HAS BEEN WRONG TWICE.
#
# An empty TARGETARCH is fine: Go cannot distinguish an empty environment variable from an
# unset one, so it falls back to the build container's native architecture, which is exactly
# right for a single-platform build. Verified rather than assumed --
# `GOARCH= GOOS= go env GOARCH GOOS` prints the container's own pair.
#
# MISTAKE ONE: `GOARCH=${TARGETARCH:-arm64}`. TARGETARCH is populated only by BuildKit, and
# the legacy builder leaves it empty (see the caching note above), so the default always won
# and every image was built for arm64. Correct by accident on an Apple Silicon Mac, and
# silently broken for anyone else: the image builds, pushes, then dies at startup with
# "exec format error", which names neither the cause nor this file.
#
# MISTAKE TWO, fixing mistake one: `${TARGETARCH:+GOARCH=$TARGETARCH}`, meaning "emit the
# assignment only when the variable is set". It reads correctly and it cannot work, because
# of a POSIX rule worth knowing:
#
#   A shell recognises assignment prefixes WHEN IT PARSES the command, BEFORE any parameter
#   expansion. A word is an assignment only if it LITERALLY begins `NAME=`.
#
# `CGO_ENABLED=0` qualifies. `GOOS=${TARGETOS:-linux}` qualifies -- the name and `=` are
# literal and only the value is expanded. But a word starting with `$` does not, so the shell
# files it as the COMMAND NAME, expands it, and goes looking for an executable called
# `GOARCH=amd64`:
#
#   /bin/sh: 1: GOARCH=amd64: not found        (exit 127)
#
# You cannot synthesise an assignment through expansion. And the reason this survived review
# is the same reason mistake one did: with TARGETARCH empty, the whole word vanishes and the
# command word becomes `go`, so every local build passed. It failed the first time it ever ran
# on BuildKit, which populates TARGETARCH -- in CI, on the first push that added CI.
#
# The lesson is not about Docker. A conditional that is only ever exercised on one side of its
# condition is untested code wearing a plausible comment.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags "-s -w \
        -X github.com/Pheonix2507/kubernetes-cost-analyzer/internal/buildinfo.Version=${VERSION} \
        -X github.com/Pheonix2507/kubernetes-cost-analyzer/internal/buildinfo.Commit=${COMMIT} \
        -X github.com/Pheonix2507/kubernetes-cost-analyzer/internal/buildinfo.BuildTime=${BUILD_TIME}" \
      -o /out/api ./cmd/api \
 && CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags "-s -w \
        -X github.com/Pheonix2507/kubernetes-cost-analyzer/internal/buildinfo.Version=${VERSION} \
        -X github.com/Pheonix2507/kubernetes-cost-analyzer/internal/buildinfo.Commit=${COMMIT} \
        -X github.com/Pheonix2507/kubernetes-cost-analyzer/internal/buildinfo.BuildTime=${BUILD_TIME}" \
      -o /out/collector ./cmd/collector \
 && CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags "-s -w \
        -X github.com/Pheonix2507/kubernetes-cost-analyzer/internal/buildinfo.Version=${VERSION} \
        -X github.com/Pheonix2507/kubernetes-cost-analyzer/internal/buildinfo.Commit=${COMMIT} \
        -X github.com/Pheonix2507/kubernetes-cost-analyzer/internal/buildinfo.BuildTime=${BUILD_TIME}" \
      -o /out/rollup ./cmd/rollup

# =============================================================================
# Stage 2: api runtime
# =============================================================================
#
# WHY distroless AND NOT alpine
# -----------------------------
# alpine is ~7MB and includes a shell, apk, and busybox. distroless static is ~2MB and
# includes CA certificates, /etc/passwd and timezone data -- nothing else. No shell at
# all.
#
# No shell is a SECURITY PROPERTY, not an inconvenience. Most container escape and
# lateral-movement techniques begin with executing something in the container. If an
# attacker achieves RCE here, there is no sh, no curl, no wget and no package manager
# to pivot with. It also shrinks the CVE surface to approximately zero, so scanner
# output stays meaningful instead of being 200 findings nobody reads.
#
# THE COST, STATED HONESTLY: `kubectl exec` gives you nothing to run. Debugging relies
# on `kubectl debug` ephemeral containers instead. That is a real workflow change, and
# it is the correct trade for a production image.
FROM gcr.io/distroless/static-debian12:nonroot AS api

# CA certificates come with the base image, which matters for outbound HTTPS -- to
# cloud pricing APIs in later phases. A `scratch` base would have none, and every TLS
# call would fail with "x509: certificate signed by unknown authority".

COPY --from=builder /out/api /usr/local/bin/api

# Run as a non-root user (uid 65532, provided by the :nonroot tag).
#
# Containers run as root by default, and root in a container is root on the host kernel
# for anything that escapes namespace isolation. A non-root uid is also what lets this
# pod satisfy a restricted PodSecurityStandard, which any serious cluster enforces.
#
# Declared here rather than only in the Kubernetes manifest so the image is safe by
# default wherever it runs -- including `docker run` on a laptop.
USER nonroot:nonroot

EXPOSE 8080

# ENTRYPOINT in exec form (a JSON array), NOT shell form.
#
# Shell form (`ENTRYPOINT /usr/local/bin/api`) wraps the process in `/bin/sh -c`, which
# would fail immediately here because there is no shell. More importantly, sh becomes
# PID 1 and does NOT forward signals to its child, so SIGTERM never reaches our
# process. Every graceful-shutdown mechanism in internal/httpapi would be silently
# dead, and every deploy would drop in-flight requests -- with nothing in the logs to
# explain why. Exec form makes our binary PID 1 and it receives signals directly.
ENTRYPOINT ["/usr/local/bin/api"]

# =============================================================================
# Stage 3: collector runtime
# =============================================================================
FROM gcr.io/distroless/static-debian12:nonroot AS collector

COPY --from=builder /out/collector /usr/local/bin/collector

USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/collector"]

# =============================================================================
# Stage 4: rollup runtime
# =============================================================================
#
# A BATCH JOB, not a service, and the image reflects that in one way worth noting: there is no CMD
# alongside the ENTRYPOINT. Kubernetes appends a CronJob's `args` to the entrypoint, so the schedule
# supplies the flags -- `-month 2026-07 -close` for the monthly close, nothing at all for the nightly
# run. A CMD with default flags would be silently overridden by any args and silently applied when
# there are none, which is the worst of both.
#
# Same distroless base as the others: no shell, so a compromised job cannot run one. That matters more
# for this binary than the API, because a CronJob's pod spec is the sort of thing that ends up with
# broader database credentials than a read-only service needs.
FROM gcr.io/distroless/static-debian12:nonroot AS rollup

COPY --from=builder /out/rollup /usr/local/bin/rollup

USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/rollup"]
