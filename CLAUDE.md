# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**maintainer-d** tracks maintainers for CNCF Projects using multiple data sources. It is a CNCF-internal project. The system consists of:

- **maintainerd server** (`main.go`, `onboarding/`) — listens for GitHub webhooks to initiate project onboarding workflows (e.g., FOSSA license scanning)
- **web-bff** (`cmd/web-bff/`) — Go REST API backend serving the Next.js web app
- **web** (`web/`) — Next.js 16 / React 19 frontend with App Router
- **PostgreSQL** database accessed via GORM (SQLite used locally/in tests)

## Running the Web App Locally

```bash
scripts/run-local-web.sh
```

Starts the web-bff API server (`:8001`) and the Next.js frontend (`:3000`) together against the local Postgres dev DB (`maintainerd_local` on `127.0.0.1:55432`). Set `RUN_LOCAL_WEB_PRESERVE_ENV=true` to keep existing env vars instead of forcing local-safe defaults.

`BFF_TEST_MODE=true` (the default) enables `/auth/test-login?login=<github_username>` for local sign-in without real OAuth.

## Go Testing

```bash
make test                        # run all Go tests
make test-verbose                # verbose output
make test-coverage               # generate coverage.out
make test-race                   # with race detector
make test-package PKG=onboarding # single package
make lint                        # golangci-lint
make fmt                         # go fmt
make vet                         # go vet
make ci-local                    # full CI check (run before pushing)
```

Single test by function name:
```bash
go test -v -run TestFossaChosen ./onboarding/...
go test -v -run TestFossaChosen/successful_onboarding ./onboarding/...
```

`make ci-local` replicates what GitHub Actions runs: dependency verify, fmt, vet, staticcheck, race-detected tests, web lint/typecheck/BDD.

## Web BDD Tests (Cucumber + Playwright)

Feature files live in `features/web/*.feature`. Step definitions are in `web/tests/steps/`.

```bash
make test-web                                      # run all BDD tests
WEB_BDD_USE_MICROCKS=true make test-web            # with mocked FOSSA API
WEB_BDD_USE_MICROCKS=true BDD_FEATURE=../features/web/maintainer_profile.feature make test-web  # single feature
make test-web-podman                               # run in Playwright container (non-Ubuntu hosts)
```

Microcks mocks the FOSSA API. The mock contract is at `microcks/fossa-api-mock.yaml`. Start/stop Microcks manually with `scripts/microcks-up.sh` / `scripts/microcks-down.sh` (UI at `http://localhost:8585`).

## Architecture

### Data flow
```
Web UI → web-bff (Go REST API, :8000) → PostgreSQL
```

### Key packages
- `model/` — Core data models (Maintainer, Project, Company); shared across the codebase
- `db/` — GORM store interface (`store.go`) and implementation (`store_impl.go`)
- `onboarding/` — Webhook-triggered onboarding workflows; server + tasks
- `plugins/fossa/` — FOSSA API client
- `refparse/` — Parses maintainer references from project YAML files
- `cmd/web-bff/` — REST API handlers, GitHub OAuth, session management
- `cmd/web-bff-seed/` — Seeds test database
- `cmd/migrate/` — Database migrations

### Web app (`web/src/app/`)
Next.js App Router. Routes include `/projects/[id]`, `/maintainers/[id]`, `/companies/[id]`, `/search`. The `AppShell` component (`web/src/components/AppShell.tsx`) wraps all pages.

## FOSSA Rule

`remote_teams.remote_team_id` must always be sourced from the FOSSA API. Never derive it from `project.id` or any local identifier.

## Commit Messages

Use the "Friends episode nameing method": write commit messages as if prefixed with *"This is the change that…"*. For example: `"adds the repo being monitored to log output at loglevel INFO"`.

## Deployment

Kubernetes manifests are in `deploy/manifests/` (no Helm). See `OPS.MD` for operational commands.

```bash
make secrets           # dev-only: build bootstrap.env and apply maintainerd-bootstrap-env
make manifests-apply   # apply all manifests to cluster
make manifests-delete  # delete manifests from cluster
```

Required secrets in namespace `maintainerd`: `ghcr-secret` plus the per-workload env secrets (`maintainerd-server-env`, `maintainerd-db-env`, `maintainerd-web-env`, `maintainerd-web-bff-env`).

`deploy/manifests/` holds only long-lived resources and is safe to re-apply. One-shot Jobs (schema migration, backfills, add-staff) live in `deploy/jobs/` and are applied individually, never wholesale.

Production image tags are pinned in `deploy/manifests/kustomization.yaml`, which `make manifests-apply` applies with `kubectl apply -k`. Deploy with `make image-set TAG=<tag>` (or a per-service `*-image-set TAG=<tag>`) and commit the updated kustomization; `make manifests-diff` shows any drift between git and the cluster.
