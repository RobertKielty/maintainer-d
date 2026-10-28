# AGENTS Guidelines for this Repo
This document gives agents instructions and tips for working in this repository. It applies to the entire repo.

## Purpose & Overview
- `maintainer-d` is a Go service with a Postgres-backed data layer that processes GitHub webhooks and onboards projects to services (e.g., FOSSA).
- Deployment uses plain Kubernetes manifests under `deploy/manifests` (production only).

## Database Constraint
- Treat this repository as Postgres-only for production changes and new operational code.
- Do not add new SQLite-specific behavior, fallbacks, or documentation for new work unless the user explicitly asks for it.

## Deploy Rules
- Apply manifests in `deploy/manifests` with `kubectl -n maintainerd apply -k deploy/manifests` (`make manifests-apply`). That directory holds only long-lived resources (Deployments, Services, Ingresses, CronJobs, cert-manager objects, the ServiceAccount) and is safe to re-apply.
- Production image tags are pinned in `deploy/manifests/kustomization.yaml`. Deploy a new tag with `make image-set TAG=<tag>` (or a per-service `*-image-set`), then commit that file. Never `kubectl set image` or apply single manifests with `-f`; both bypass the pinned tags.
- One-shot Jobs (schema migration, backfills, add-staff, profile sync) live in `deploy/jobs/`. Apply them individually and on purpose; never apply that directory wholesale.
- Ensure the `ghcr-secret` Secret and the per-workload env Secrets exist beforehand.

## Makefile Targets (high‑value)
- `secrets` → dev-only: builds `bootstrap.env` and applies `maintainerd-bootstrap-env`.
- `manifests-apply` → apply all manifests to the cluster.
- `manifests-delete` → delete the manifests from the cluster.

## Required Secrets & Config
- Secrets (namespace `maintainerd`):
  - `maintainerd-bootstrap-env` (from `bootstrap.env`)
  - `ghcr-secret` (Docker registry creds for `ghcr.io`)
- Env vars used by the app:
  - `FOSSA_API_TOKEN`
  -  GitHub tokens/secrets for the server: `GITHUB_API_TOKEN`, `GITHUB_WEBHOOK_SECRET`.

## Local Validation & Testing
- Watch resources after apply: `kubectl -n maintainerd get deploy,job,svc,ing,pvc -w`.
- If DB schema/config changes (e.g., extensions, search functions), update and verify local DB scripts in `scripts/` (refresh/restore, init).

## Notes
- Verify Secrets exist and the image tag is pullable from the GitHub Container Registry.
- If DB configuration changes (extensions, schema requirements, etc.), update the local DB scripts in `scripts/` (e.g., `refresh-local-podman-db.sh`, `pg_restore_init.sh`) to keep dev workflows aligned.
- FOSSA rule: `remote_teams.remote_team_id` must be sourced from the FOSSA API. Never derive it from `project.id` or any local identifier.

## Coding Guidelines
- Keep changes minimal and focused. Match existing Go style and module layout.
- Avoid unrelated refactors in the same change. Update docs/Makefile if deployment surface changes.
- Use the "The Friends method" of writing commit messages to describe patches where each commit message is notionally pre-fixed with the sentence, "This is the change that " and then the commit message continues this sentence, for example, (this is the change that) "adds the repo being monitored to log output at loglevel INFO"

## Web UI Theme Rule
- When changing the web UI, always make the change work in both light mode and dark mode.
- Respect the current `ThemeProvider` / `data-theme` mechanism instead of assuming light mode.
- Prefer existing CSS variables in `web/src/app/globals.css`. When new UI colors or surfaces are needed, add theme-aware variables there and use those variables in component CSS.
- Do not ship a web UI change that only looks correct in light mode.

