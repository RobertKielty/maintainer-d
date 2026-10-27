FROM golang:1.27.3-bookworm AS build
WORKDIR /src

# Cache module downloads and build artifacts with BuildKit
# 1) copy only go.mod/sum first to maximize cache hits
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

# 2) copy the rest of the source and build using cached mounts
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -o /bootstrap ./cmd/bootstrap && \
    go build -o /maintainerd ./main.go && \
    go build -o /dot-project-sync ./cmd/dot-project-sync && \
    go build -o /sanitize ./cmd/sanitize && \
    go build -o /migrate ./cmd/migrate && \
    go build -o /onboarding-backfill ./cmd/onboarding-backfill && \
    go build -o /fossa-poller ./cmd/fossa-poller && \
    go build -o /github-profile-sync ./cmd/github-profile-sync

FROM gcr.io/distroless/base-debian12 AS maintainerd
COPY --from=build /bootstrap /usr/local/bin/bootstrap
COPY --from=build /maintainerd /usr/local/bin/maintainerd
ENTRYPOINT ["/usr/local/bin/maintainerd"]

FROM gcr.io/distroless/base-debian12 AS dot-project-sync
COPY --from=build /dot-project-sync /usr/local/bin/dot-project-sync
ENTRYPOINT ["/usr/local/bin/dot-project-sync"]

FROM gcr.io/distroless/base-debian12 AS sanitize
COPY --from=build /sanitize /usr/local/bin/sanitize
ENTRYPOINT ["/usr/local/bin/sanitize"]

FROM gcr.io/distroless/base-debian12 AS migrate
COPY --from=build /migrate /usr/local/bin/migrate
ENTRYPOINT ["/usr/local/bin/migrate"]

FROM gcr.io/distroless/base-debian12 AS onboarding-backfill
COPY --from=build /onboarding-backfill /usr/local/bin/onboarding-backfill
ENTRYPOINT ["/usr/local/bin/onboarding-backfill"]

FROM gcr.io/distroless/base-debian12 AS fossa-poller
COPY --from=build /fossa-poller /usr/local/bin/fossa-poller
ENTRYPOINT ["/usr/local/bin/fossa-poller"]

FROM gcr.io/distroless/base-debian12 AS github-profile-sync
COPY --from=build /github-profile-sync /usr/local/bin/github-profile-sync
ENTRYPOINT ["/usr/local/bin/github-profile-sync"]
