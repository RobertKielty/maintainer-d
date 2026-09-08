TOPDIR=$(PWD)
GH_ORG_LC=robertkielty
REGISTRY ?= ghcr.io
GIT_SHA ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BRANCH ?= $(shell git rev-parse --abbrev-ref HEAD 2>/dev/null | tr '[:upper:]' '[:lower:]' | tr '/_' '--')
BUILD_DATE ?= $(shell date -u '+%a-%b-%d-%Y' | tr '[:lower:]' '[:upper:]')
TAG ?= $(BRANCH)-$(GIT_SHA)-$(BUILD_DATE)
IMAGE ?= $(REGISTRY)/$(GH_ORG_LC)/maintainerd:$(TAG)
IMAGE_LATEST ?= $(REGISTRY)/$(GH_ORG_LC)/maintainerd:latest
DOT_PROJECT_SYNC_IMAGE ?= $(REGISTRY)/$(GH_ORG_LC)/maintainerd-dot-project-sync:$(TAG)
DOT_PROJECT_SYNC_IMAGE_LATEST ?= $(REGISTRY)/$(GH_ORG_LC)/maintainerd-dot-project-sync:latest
SANITIZE_IMAGE ?= $(REGISTRY)/$(GH_ORG_LC)/maintainerd-sanitize:$(TAG)
SANITIZE_IMAGE_LATEST ?= $(REGISTRY)/$(GH_ORG_LC)/maintainerd-sanitize:latest
MIGRATE_IMAGE ?= $(REGISTRY)/$(GH_ORG_LC)/maintainerd-migrate:$(TAG)
MIGRATE_IMAGE_LATEST ?= $(REGISTRY)/$(GH_ORG_LC)/maintainerd-migrate:latest
ONBOARDING_BACKFILL_IMAGE ?= $(REGISTRY)/$(GH_ORG_LC)/maintainerd-onboarding-backfill:$(TAG)
ONBOARDING_BACKFILL_IMAGE_LATEST ?= $(REGISTRY)/$(GH_ORG_LC)/maintainerd-onboarding-backfill:latest
FOSSA_POLLER_IMAGE ?= $(REGISTRY)/$(GH_ORG_LC)/maintainerd-fossa-poller:$(TAG)
FOSSA_POLLER_IMAGE_LATEST ?= $(REGISTRY)/$(GH_ORG_LC)/maintainerd-fossa-poller:latest
GITHUB_PROFILE_SYNC_IMAGE ?= $(REGISTRY)/$(GH_ORG_LC)/maintainerd-github-profile-sync:$(TAG)
GITHUB_PROFILE_SYNC_IMAGE_LATEST ?= $(REGISTRY)/$(GH_ORG_LC)/maintainerd-github-profile-sync:latest
WEB_IMAGE ?= $(REGISTRY)/$(GH_ORG_LC)/maintainerd-web:$(TAG)
WEB_IMAGE_LATEST ?= $(REGISTRY)/$(GH_ORG_LC)/maintainerd-web:latest
WEB_BFF_IMAGE ?= $(REGISTRY)/$(GH_ORG_LC)/maintainerd-web-bff:$(TAG)
WEB_BFF_IMAGE_LATEST ?= $(REGISTRY)/$(GH_ORG_LC)/maintainerd-web-bff:latest
WEB_IMAGE_REPO ?= $(REGISTRY)/$(GH_ORG_LC)/maintainerd-web
WEB_BFF_IMAGE_REPO ?= $(REGISTRY)/$(GH_ORG_LC)/maintainerd-web-bff

# Docker buildkit progress (docker-only). Example: BUILD_PROGRESS=plain
BUILD_PROGRESS ?= auto
DOCKER_BUILD_EXTRA ?=
NPM_CONFIG_LOGLEVEL ?=
NPM_CONFIG_REGISTRY ?=
NO_BDD_OPEN ?= 1

ifeq ($(CONTAINER_TOOL),docker)
BUILD_PROGRESS_FLAG := --progress=$(BUILD_PROGRESS)
else
BUILD_PROGRESS_FLAG :=
endif
WHOAMI=$(shell whoami)
CONTAINER_TOOL ?= podman

# Helpful context string for logs
CTX_STR := $(if $(KUBECONTEXT),$(KUBECONTEXT),$(shell kubectl config current-context 2>/dev/null || echo current))

GOCACHE_DIR ?= $(TOPDIR)/.gocache
GOFMT_PATHS ?= $(shell go list -f '{{.Dir}}' ./...)

# GHCR auth (optional for push). If set, we will docker login before push.
GHCR_USER  ?= $(GH_ORG_LC)
GHCR_TOKEN ?=



# ---- Image ----
.PHONY: mntrd-image-build
mntrd-image-build:
	@echo "Building container image: $(IMAGE)"
	@$(CONTAINER_TOOL) build $(BUILD_PROGRESS_FLAG) $(DOCKER_BUILD_EXTRA) -t $(IMAGE) -f Dockerfile --target maintainerd .

.PHONY: dot-project-sync-image-build
dot-project-sync-image-build:
	@echo "Building dot-project sync image: $(DOT_PROJECT_SYNC_IMAGE)"
	@$(CONTAINER_TOOL) build $(BUILD_PROGRESS_FLAG) $(DOCKER_BUILD_EXTRA) -t $(DOT_PROJECT_SYNC_IMAGE) -f Dockerfile --target dot-project-sync .

.PHONY: sanitize-image-build
sanitize-image-build:
	@echo "Building sanitize image: $(SANITIZE_IMAGE)"
	@$(CONTAINER_TOOL) build $(BUILD_PROGRESS_FLAG) $(DOCKER_BUILD_EXTRA) -t $(SANITIZE_IMAGE) -f Dockerfile --target sanitize .

.PHONY: migrate-image-build
migrate-image-build:
	@echo "Building migrate image: $(MIGRATE_IMAGE)"
	@$(CONTAINER_TOOL) build $(BUILD_PROGRESS_FLAG) $(DOCKER_BUILD_EXTRA) -t $(MIGRATE_IMAGE) -f Dockerfile --target migrate .

.PHONY: onboarding-backfill-image-build
onboarding-backfill-image-build:
	@echo "Building onboarding backfill image: $(ONBOARDING_BACKFILL_IMAGE)"
	@$(CONTAINER_TOOL) build $(BUILD_PROGRESS_FLAG) $(DOCKER_BUILD_EXTRA) -t $(ONBOARDING_BACKFILL_IMAGE) -f Dockerfile --target onboarding-backfill .

.PHONY: fossa-poller-image-build
fossa-poller-image-build:
	@echo "Building fossa poller image: $(FOSSA_POLLER_IMAGE)"
	@$(CONTAINER_TOOL) build $(BUILD_PROGRESS_FLAG) $(DOCKER_BUILD_EXTRA) -t $(FOSSA_POLLER_IMAGE) -f Dockerfile --target fossa-poller .

.PHONY: github-profile-sync-image-build
github-profile-sync-image-build:
	@echo "Building GitHub profile sync image: $(GITHUB_PROFILE_SYNC_IMAGE)"
	@$(CONTAINER_TOOL) build $(BUILD_PROGRESS_FLAG) $(DOCKER_BUILD_EXTRA) -t $(GITHUB_PROFILE_SYNC_IMAGE) -f Dockerfile --target github-profile-sync .

.PHONY: web-image-build
web-image-build:
	@echo "Building web image: $(WEB_IMAGE)"
	@bash -c 'set -euo pipefail; \
	if ! command -v $(SOPS_CMD) >/dev/null 2>&1; then \
		echo "Missing $(SOPS_CMD); cannot read deploy/secrets/maintainerd-web-env.yaml for build-time NEXT_PUBLIC_BFF_BASE_URL."; \
		exit 1; \
	fi; \
	if [ ! -f deploy/secrets/maintainerd-web-env.yaml ]; then \
		echo "Missing deploy/secrets/maintainerd-web-env.yaml; cannot read NEXT_PUBLIC_BFF_BASE_URL for web build."; \
		exit 1; \
	fi; \
	bff="$$( $(SOPS_CMD) -d deploy/secrets/maintainerd-web-env.yaml | sed -n '\'' \
		/^stringData:/,/^[^[:space:]]/{ \
			s/^[[:space:]]*NEXT_PUBLIC_BFF_BASE_URL:[[:space:]]*//p; \
		} \
	'\'' | head -n 1 )"; \
	if [ -z "$$bff" ]; then \
		echo "NEXT_PUBLIC_BFF_BASE_URL missing in deploy/secrets/maintainerd-web-env.yaml; aborting web build."; \
		exit 1; \
	fi; \
	echo "Using NEXT_PUBLIC_BFF_BASE_URL=$$bff for web build."; \
	build_args="--build-arg NEXT_PUBLIC_BFF_BASE_URL=$$bff"; \
	[ -n "$(NPM_CONFIG_LOGLEVEL)" ] && build_args="$$build_args --build-arg NPM_CONFIG_LOGLEVEL=$(NPM_CONFIG_LOGLEVEL)"; \
	[ -n "$(NPM_CONFIG_REGISTRY)" ] && build_args="$$build_args --build-arg NPM_CONFIG_REGISTRY=$(NPM_CONFIG_REGISTRY)"; \
	$(CONTAINER_TOOL) build $(BUILD_PROGRESS_FLAG) $(DOCKER_BUILD_EXTRA) -t $(WEB_IMAGE) -f Dockerfile.web $$build_args .; \
	'

.PHONY: web-bff-image-build
web-bff-image-build:
	@echo "Building web-bff image: $(WEB_BFF_IMAGE)"
	@$(CONTAINER_TOOL) build $(BUILD_PROGRESS_FLAG) $(DOCKER_BUILD_EXTRA) -t $(WEB_BFF_IMAGE) -f Dockerfile.web-bff .

.PHONY: mntrd-image-push
mntrd-image-push: mntrd-image-build
	@echo "Ensuring $(CONTAINER_TOOL) is logged in to $(REGISTRY) (uses GHCR_TOKEN if set)"
	@if [ -n "$(GHCR_TOKEN)" ]; then \
		echo "Logging into $(REGISTRY) as $(GHCR_USER) using token from GHCR_TOKEN"; \
		echo "$(GHCR_TOKEN)" | $(CONTAINER_TOOL) login $(REGISTRY) -u "$(GHCR_USER)" --password-stdin; \
	else \
		echo "GHCR_TOKEN not set; attempting push with existing auth"; \
	fi
	@echo "Pushing image: $(IMAGE)"
	@$(CONTAINER_TOOL) push $(IMAGE)
	@echo "Tagging and pushing latest: $(IMAGE_LATEST)"
	@$(CONTAINER_TOOL) tag $(IMAGE) $(IMAGE_LATEST)
	@$(CONTAINER_TOOL) push $(IMAGE_LATEST)

.PHONY: mntrd-image-deploy
mntrd-image-deploy: mntrd-image-push
	@echo "Image pushed. Attempting rollout on context $(CTX_STR)."
	@CTX_FLAG="$(if $(KUBECONTEXT),--context $(KUBECONTEXT))" ; \
	if kubectl $$CTX_FLAG config current-context >/dev/null 2>&1; then \
		echo "Updating Deployment/maintainerd image to $(IMAGE) [ctx=$(CTX_STR)]"; \
		kubectl -n $(NAMESPACE) $$CTX_FLAG set image deploy/maintainerd '*=$(IMAGE)'; \
		echo "Rolling restart Deployment/maintainerd [ctx=$(CTX_STR)]"; \
		kubectl -n $(NAMESPACE) $$CTX_FLAG rollout restart deploy/maintainerd; \
		echo "Waiting for rollout to complete [ctx=$(CTX_STR)]"; \
		kubectl -n $(NAMESPACE) $$CTX_FLAG rollout status deploy/maintainerd --timeout=180s; \
	else \
		echo "kubectl context $(CTX_STR) unavailable; skipping rollout"; \
	fi

.PHONY: mntrd-image-set
mntrd-image-set:
	@if [ -z "$(TAG)" ]; then \
		echo "Usage: make mntrd-image-set TAG=<tag>"; exit 1; \
	fi
	@echo "Setting maintainerd image to $(IMAGE) [ns=$(NAMESPACE)]"
	@kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) \
		set image deploy/maintainerd '*=$(IMAGE)'

.PHONY: dot-project-sync-image-push
dot-project-sync-image-push: dot-project-sync-image-build
	@echo "Ensuring $(CONTAINER_TOOL) is logged in to $(REGISTRY) (uses GHCR_TOKEN if set)"
	@if [ -n "$(GHCR_TOKEN)" ]; then \
		echo "Logging into $(REGISTRY) as $(GHCR_USER) using token from GHCR_TOKEN"; \
		echo "$(GHCR_TOKEN)" | $(CONTAINER_TOOL) login $(REGISTRY) -u "$(GHCR_USER)" --password-stdin; \
	else \
		echo "GHCR_TOKEN not set; attempting push with existing auth"; \
	fi
	@echo "Pushing image: $(DOT_PROJECT_SYNC_IMAGE)"
	@$(CONTAINER_TOOL) push $(DOT_PROJECT_SYNC_IMAGE)
	@echo "Tagging and pushing latest: $(DOT_PROJECT_SYNC_IMAGE_LATEST)"
	@$(CONTAINER_TOOL) tag $(DOT_PROJECT_SYNC_IMAGE) $(DOT_PROJECT_SYNC_IMAGE_LATEST)
	@$(CONTAINER_TOOL) push $(DOT_PROJECT_SYNC_IMAGE_LATEST)

.PHONY: sanitize-image-push
sanitize-image-push: sanitize-image-build
	@echo "Ensuring $(CONTAINER_TOOL) is logged in to $(REGISTRY) (uses GHCR_TOKEN if set)"
	@if [ -n "$(GHCR_TOKEN)" ]; then \
		echo "Logging into $(REGISTRY) as $(GHCR_USER) using token from GHCR_TOKEN"; \
		echo "$(GHCR_TOKEN)" | $(CONTAINER_TOOL) login $(REGISTRY) -u "$(GHCR_USER)" --password-stdin; \
	else \
		echo "GHCR_TOKEN not set; attempting push with existing auth"; \
	fi
	@echo "Pushing image: $(SANITIZE_IMAGE)"
	@$(CONTAINER_TOOL) push $(SANITIZE_IMAGE)
	@echo "Tagging and pushing latest: $(SANITIZE_IMAGE_LATEST)"
	@$(CONTAINER_TOOL) tag $(SANITIZE_IMAGE) $(SANITIZE_IMAGE_LATEST)
	@$(CONTAINER_TOOL) push $(SANITIZE_IMAGE_LATEST)

.PHONY: migrate-image-push
migrate-image-push: migrate-image-build
	@echo "Ensuring $(CONTAINER_TOOL) is logged in to $(REGISTRY) (uses GHCR_TOKEN if set)"
	@if [ -n "$(GHCR_TOKEN)" ]; then \
		echo "Logging into $(REGISTRY) as $(GHCR_USER) using token from GHCR_TOKEN"; \
		echo "$(GHCR_TOKEN)" | $(CONTAINER_TOOL) login $(REGISTRY) -u "$(GHCR_USER)" --password-stdin; \
	else \
		echo "GHCR_TOKEN not set; attempting push with existing auth"; \
	fi
	@echo "Pushing image: $(MIGRATE_IMAGE)"
	@$(CONTAINER_TOOL) push $(MIGRATE_IMAGE)
	@echo "Tagging and pushing latest: $(MIGRATE_IMAGE_LATEST)"
	@$(CONTAINER_TOOL) tag $(MIGRATE_IMAGE) $(MIGRATE_IMAGE_LATEST)
	@$(CONTAINER_TOOL) push $(MIGRATE_IMAGE_LATEST)

.PHONY: onboarding-backfill-image-push
onboarding-backfill-image-push: onboarding-backfill-image-build
	@echo "Ensuring $(CONTAINER_TOOL) is logged in to $(REGISTRY) (uses GHCR_TOKEN if set)"
	@if [ -n "$(GHCR_TOKEN)" ]; then \
		echo "Logging into $(REGISTRY) as $(GHCR_USER) using token from GHCR_TOKEN"; \
		echo "$(GHCR_TOKEN)" | $(CONTAINER_TOOL) login $(REGISTRY) -u "$(GHCR_USER)" --password-stdin; \
	else \
		echo "GHCR_TOKEN not set; attempting push with existing auth"; \
	fi
	@echo "Pushing image: $(ONBOARDING_BACKFILL_IMAGE)"
	@$(CONTAINER_TOOL) push $(ONBOARDING_BACKFILL_IMAGE)
	@echo "Tagging and pushing latest: $(ONBOARDING_BACKFILL_IMAGE_LATEST)"
	@$(CONTAINER_TOOL) tag $(ONBOARDING_BACKFILL_IMAGE) $(ONBOARDING_BACKFILL_IMAGE_LATEST)
	@$(CONTAINER_TOOL) push $(ONBOARDING_BACKFILL_IMAGE_LATEST)

.PHONY: fossa-poller-image-push
fossa-poller-image-push: fossa-poller-image-build
	@echo "Ensuring $(CONTAINER_TOOL) is logged in to $(REGISTRY) (uses GHCR_TOKEN if set)"
	@if [ -n "$(GHCR_TOKEN)" ]; then \
		echo "Logging into $(REGISTRY) as $(GHCR_USER) using token from GHCR_TOKEN"; \
		echo "$(GHCR_TOKEN)" | $(CONTAINER_TOOL) login $(REGISTRY) -u "$(GHCR_USER)" --password-stdin; \
	else \
		echo "GHCR_TOKEN not set; attempting push with existing auth"; \
	fi
	@echo "Pushing image: $(FOSSA_POLLER_IMAGE)"
	@$(CONTAINER_TOOL) push $(FOSSA_POLLER_IMAGE)
	@echo "Tagging and pushing latest: $(FOSSA_POLLER_IMAGE_LATEST)"
	@$(CONTAINER_TOOL) tag $(FOSSA_POLLER_IMAGE) $(FOSSA_POLLER_IMAGE_LATEST)
	@$(CONTAINER_TOOL) push $(FOSSA_POLLER_IMAGE_LATEST)

.PHONY: github-profile-sync-image-push
github-profile-sync-image-push: github-profile-sync-image-build
	@echo "Ensuring $(CONTAINER_TOOL) is logged in to $(REGISTRY) (uses GHCR_TOKEN if set)"
	@if [ -n "$(GHCR_TOKEN)" ]; then \
		echo "Logging into $(REGISTRY) as $(GHCR_USER) using token from GHCR_TOKEN"; \
		echo "$(GHCR_TOKEN)" | $(CONTAINER_TOOL) login $(REGISTRY) -u "$(GHCR_USER)" --password-stdin; \
	else \
		echo "GHCR_TOKEN not set; attempting push with existing auth"; \
	fi
	@echo "Pushing image: $(GITHUB_PROFILE_SYNC_IMAGE)"
	@$(CONTAINER_TOOL) push $(GITHUB_PROFILE_SYNC_IMAGE)
	@echo "Tagging and pushing latest: $(GITHUB_PROFILE_SYNC_IMAGE_LATEST)"
	@$(CONTAINER_TOOL) tag $(GITHUB_PROFILE_SYNC_IMAGE) $(GITHUB_PROFILE_SYNC_IMAGE_LATEST)
	@$(CONTAINER_TOOL) push $(GITHUB_PROFILE_SYNC_IMAGE_LATEST)

.PHONY: web-image-push
web-image-push: web-image-build
	@echo "Ensuring $(CONTAINER_TOOL) is logged in to $(REGISTRY) (uses GHCR_TOKEN if set)"
	@if [ -n "$(GHCR_TOKEN)" ]; then \
		echo "Logging into $(REGISTRY) as $(GHCR_USER) using token from GHCR_TOKEN"; \
		echo "$(GHCR_TOKEN)" | $(CONTAINER_TOOL) login $(REGISTRY) -u "$(GHCR_USER)" --password-stdin; \
	else \
		echo "GHCR_TOKEN not set; attempting push with existing auth"; \
	fi
	@echo "Pushing image: $(WEB_IMAGE)"
	@$(CONTAINER_TOOL) push $(WEB_IMAGE)
	@echo "Tagging and pushing latest: $(WEB_IMAGE_LATEST)"
	@$(CONTAINER_TOOL) tag $(WEB_IMAGE) $(WEB_IMAGE_LATEST)
	@$(CONTAINER_TOOL) push $(WEB_IMAGE_LATEST)

.PHONY: web-bff-image-push
web-bff-image-push: web-bff-image-build
	@echo "Ensuring $(CONTAINER_TOOL) is logged in to $(REGISTRY) (uses GHCR_TOKEN if set)"
	@if [ -n "$(GHCR_TOKEN)" ]; then \
		echo "Logging into $(REGISTRY) as $(GHCR_USER) using token from GHCR_TOKEN"; \
		echo "$(GHCR_TOKEN)" | $(CONTAINER_TOOL) login $(REGISTRY) -u "$(GHCR_USER)" --password-stdin; \
	else \
		echo "GHCR_TOKEN not set; attempting push with existing auth"; \
	fi
	@echo "Pushing image: $(WEB_BFF_IMAGE)"
	@$(CONTAINER_TOOL) push $(WEB_BFF_IMAGE)
	@echo "Tagging and pushing latest: $(WEB_BFF_IMAGE_LATEST)"
	@$(CONTAINER_TOOL) tag $(WEB_BFF_IMAGE) $(WEB_BFF_IMAGE_LATEST)
	@$(CONTAINER_TOOL) push $(WEB_BFF_IMAGE_LATEST)

.PHONY: dot-project-sync-image-deploy
dot-project-sync-image-deploy: dot-project-sync-image-push
	@echo "Image pushed. Updating CronJob/dot-project-sync in $(NAMESPACE) [ctx=$(CTX_STR)]"
	@CTX_FLAG="$(if $(KUBECONTEXT),--context $(KUBECONTEXT))" ; \
	if ! kubectl $$CTX_FLAG config current-context >/dev/null 2>&1; then \
		echo "kubectl context $(CTX_STR) unavailable; skipping rollout"; exit 0; \
	fi ; \
	if ! kubectl -n $(NAMESPACE) $$CTX_FLAG get cronjob/dot-project-sync >/dev/null 2>&1; then \
		echo "CronJob/dot-project-sync not found in namespace $(NAMESPACE)."; \
		echo "Hint: apply deploy/manifests/dot-project-sync-cronjob.yaml or run 'make dot-project-sync-apply'."; \
		exit 1; \
	fi ; \
	kubectl -n $(NAMESPACE) $$CTX_FLAG set image cronjob/dot-project-sync '*=$(DOT_PROJECT_SYNC_IMAGE)'; \
	kubectl -n $(NAMESPACE) $$CTX_FLAG delete job -l job-name=dot-project-sync --ignore-not-found; \
	echo "Next scheduled run will pull $(DOT_PROJECT_SYNC_IMAGE)."

.PHONY: dot-project-sync-apply
dot-project-sync-apply:
	@echo "Applying dot-project sync CronJob in namespace $(NAMESPACE) [ctx=$(CTX_STR)]"
	@kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) apply -f deploy/manifests/dot-project-sync-cronjob.yaml

.PHONY: dot-project-sync-run
dot-project-sync-run:
	@bash -c 'set -euo pipefail; \
	job="dot-project-sync-manual-$$(date +%s)"; \
	echo "Creating dot-project sync job $$job in namespace $(NAMESPACE) [ctx=$(CTX_STR)]"; \
	kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) create job --from=cronjob/dot-project-sync $$job; \
	'

.PHONY: bootstrap-run
bootstrap-run: ensure-ns sops-apply-bootstrap-secrets apply-creds
	@bash -c 'set -euo pipefail; \
	echo "Verifying maintainerd-db-env exists in namespace $(NAMESPACE) [ctx=$(CTX_STR)]"; \
	kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) get secret maintainerd-db-env >/dev/null; \
	echo "Recreating bootstrap job in namespace $(NAMESPACE) [ctx=$(CTX_STR)]"; \
	kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) delete job/maintainerd-bootstrap --ignore-not-found; \
	kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) apply -f deploy/manifests/bootstrap-job.yaml; \
	echo "Waiting for bootstrap job to complete [ctx=$(CTX_STR)]"; \
	if ! kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) wait --for=condition=complete job/maintainerd-bootstrap --timeout=600s; then \
		echo "Bootstrap job failed or timed out. Recent logs:"; \
		kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) logs job/maintainerd-bootstrap --tail=200 || true; \
		exit 1; \
	fi; \
	kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) logs job/maintainerd-bootstrap --tail=200; \
	echo "Bootstrap job completed."; \
	'

.PHONY: bootstrap-run-dev
bootstrap-run-dev: ensure-ns apply-env apply-creds
	@bash -c 'set -euo pipefail; \
	echo "Verifying maintainerd-db-env exists in namespace $(NAMESPACE) [ctx=$(CTX_STR)]"; \
	kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) get secret maintainerd-db-env >/dev/null; \
	echo "Recreating bootstrap job in namespace $(NAMESPACE) [ctx=$(CTX_STR)]"; \
	kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) delete job/maintainerd-bootstrap --ignore-not-found; \
	kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) apply -f deploy/manifests/bootstrap-job.yaml; \
	echo "Waiting for bootstrap job to complete [ctx=$(CTX_STR)]"; \
	if ! kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) wait --for=condition=complete job/maintainerd-bootstrap --timeout=600s; then \
		echo "Bootstrap job failed or timed out. Recent logs:"; \
		kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) logs job/maintainerd-bootstrap --tail=200 || true; \
		exit 1; \
	fi; \
	kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) logs job/maintainerd-bootstrap --tail=200; \
	echo "Bootstrap job completed."; \
	'

.PHONY: migrate-schema
migrate-schema:
	@echo "Running schema migration job in namespace $(NAMESPACE) [ctx=$(CTX_STR)]"
	@kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) apply -f deploy/manifests/maintainerd-migrate-schema-job.yaml

.PHONY: onboarding-backfill-job
onboarding-backfill-job:
	@echo "Running onboarding backfill job in namespace $(NAMESPACE) [ctx=$(CTX_STR)]"
	@kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) apply -f deploy/manifests/maintainerd-onboarding-backfill-job.yaml

.PHONY: github-profile-sync-run
github-profile-sync-run:
	@bash -c 'set -euo pipefail; \
	echo "Recreating GitHub profile sync job in namespace $(NAMESPACE) [ctx=$(CTX_STR)]"; \
	kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) delete job github-profile-sync --ignore-not-found; \
	kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) apply -f deploy/manifests/github-profile-sync-job.yaml; \
	deadline=$$(( $$(date +%s) + 5400 )); \
	wait_status=1; \
	while [ "$$(date +%s)" -lt "$$deadline" ]; do \
		condition=$$(kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) get job/github-profile-sync -o jsonpath="{.status.conditions[?(@.status==\"True\")].type}"); \
		if echo "$$condition" | grep -q Complete; then wait_status=0; break; fi; \
		if echo "$$condition" | grep -q Failed; then wait_status=1; break; fi; \
		sleep 10; \
	done; \
	if [ "$$wait_status" -ne 0 ]; then echo "job did not reach Complete (timed out or Failed); collecting logs anyway"; fi; \
	kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) logs job/github-profile-sync --tail=-1; \
	exit $$wait_status; \
	'

.PHONY: fossa-poller-deploy
fossa-poller-deploy:
	@bash -c 'set -euo pipefail; \
	echo "Retiring legacy CronJob/maintainerd-fossa-poller in namespace $(NAMESPACE) [ctx=$(CTX_STR)]"; \
	kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) delete cronjob maintainerd-fossa-poller --ignore-not-found; \
	jobs="$$(kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) get jobs -o name | grep "^job.batch/maintainerd-fossa-poller-" || true)"; \
	if [ -n "$${jobs}" ]; then \
		echo "Deleting legacy fossa poller Jobs [ctx=$(CTX_STR)]"; \
		echo "$${jobs}" | xargs -r kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) delete --wait=true; \
	fi; \
	echo "Applying fossa poller deployment in namespace $(NAMESPACE) [ctx=$(CTX_STR)]"; \
	kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) apply -f deploy/manifests/maintainerd-fossa-poller-deployment.yaml; \
	kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) rollout status deployment/maintainerd-fossa-poller --timeout=180s; \
	'

.PHONY: fossa-poller-cronjob
fossa-poller-cronjob:
	@echo "fossa-poller-cronjob is deprecated; applying the deployment-backed poller instead [ctx=$(CTX_STR)]"
	@$(MAKE) fossa-poller-deploy

.PHONY: migrate-schema-safe
migrate-schema-safe:
	@bash -c 'set -euo pipefail; \
	echo "Scaling Deployment/maintainerd to 0 for schema migration [ctx=$(CTX_STR)]"; \
	kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) scale deploy/maintainerd --replicas=0; \
	echo "Resolving PVC attachment node for maintainerd-db [ctx=$(CTX_STR)]"; \
	pv="$$(kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) get pvc maintainerd-db -o jsonpath="{.spec.volumeName}")"; \
	node="$$(kubectl get volumeattachment -o jsonpath="{range .items[?(@.spec.source.persistentVolumeName==\"$${pv}\")]}{.spec.nodeName}{end}")"; \
	kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) delete job maintainerd-migrate-schema --ignore-not-found; \
	if [ -n "$${node}" ]; then \
		echo "Running schema migration job pinned to node $${node} [ctx=$(CTX_STR)]"; \
		kubectl create -f deploy/manifests/maintainerd-migrate-schema-job.yaml --dry-run=client -o json | \
		kubectl patch --local -f - -p "{\"spec\":{\"template\":{\"spec\":{\"nodeName\":\"$${node}\"}}}}" -o json | \
		kubectl apply -f -; \
	else \
		echo "No attachment node found; running migration job without pinning [ctx=$(CTX_STR)]"; \
		kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) apply -f deploy/manifests/maintainerd-migrate-schema-job.yaml; \
	fi; \
	kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) wait --for=condition=complete job/maintainerd-migrate-schema --timeout=300s; \
	echo "Scaling Deployment/maintainerd back to 1 [ctx=$(CTX_STR)]"; \
	kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) scale deploy/maintainerd --replicas=1; \
	'

.PHONY: mntrd-image
mntrd-image: mntrd-image-build
	@true

.PHONY: mntrd-image-run
mntrd-image-run: mntrd-image
	@$(CONTAINER_TOOL) run -ti --rm $(IMAGE)

# ---- Config ----
NAMESPACE ?= maintainerd
ENVSRC    ?= .envrc
ENVOUT    ?= bootstrap.env
KUBECONTEXT ?=
ESLINT_MAX_WARNINGS ?= 999


# Secret names (keep these stable across clusters)
ENV_SECRET_NAME   ?= maintainerd-bootstrap-env
CREDS_SECRET_NAME ?= workspace-credentials
WEB_ENV_SECRET_NAME ?= maintainerd-web-env
WEB_BFF_ENV_SECRET_NAME ?= maintainerd-web-bff-env
WEB_ENV_TEMPLATE ?= deploy/templates/maintainerd-web-env.yaml.tmpl
WEB_BFF_ENV_TEMPLATE ?= deploy/templates/maintainerd-web-bff-env.yaml.tmpl
SOPS_CMD ?= sops
SOPS_AGE_KEY ?=
SOPS_AGE_KEY_FILE ?=
SOPS_EXPECTED_AGE ?= $(shell age-keygen -y $(HOME)/.config/sops/age/keys.txt 2>/dev/null || true)

# Path to the JSON creds file on your machine
CREDS_FILE ?= ./cmd/bootstrap/credentials.json
CREDS_KEY  ?= credentials.json

# ---- Helpers ----
.PHONY: help
help:
	@echo "== Testing =="
	@echo "make test            -> run all tests"
	@echo "make test-verbose    -> run tests with verbose output"
	@echo "make test-coverage   -> run tests with coverage report"
	@echo "make test-race       -> run tests with race detector"
	@echo "make test-package    -> run tests for specific package (use PKG=...)"
	@echo "make ci-local        -> run all CI checks locally (fmt, vet, staticcheck, test)"
	@echo "make lint            -> run linters (requires golangci-lint)"
	@echo ""
	@echo "== Deployment =="
	@echo "Image tags: <branch>-<shortsha>-<DAY-MON-DD-YYYY> (UTC), plus latest"
	@echo "make secrets         -> dev-only: build $(ENVOUT) from $(ENVSRC) and apply both Secrets"
	@echo "make env             -> dev-only: build $(ENVOUT) from $(ENVSRC)"
	@echo "make apply-env       -> dev-only: create/update $(ENV_SECRET_NAME) from $(ENVOUT)"
	@echo "make apply-creds     -> create/update $(CREDS_SECRET_NAME) from $(CREDS_FILE)"
	@echo "make apply-web-env   -> create/update $(WEB_ENV_SECRET_NAME) from $(WEB_ENV_TEMPLATE)"
	@echo "make apply-web-bff-env -> create/update $(WEB_BFF_ENV_SECRET_NAME) from $(WEB_BFF_ENV_TEMPLATE)"
	@echo "make web-secrets     -> apply both web secrets from templates"
	@echo "make sops-encrypt-web-secrets -> encrypt web/db secrets in deploy/secrets (uses SOPS_AGE_KEY)"
	@echo "make sops-encrypt-server-secrets -> encrypt server secrets in deploy/secrets (uses SOPS_AGE_KEY)"
	@echo "make sops-encrypt-bootstrap-secrets -> encrypt bootstrap secrets in deploy/secrets (uses SOPS_AGE_KEY)"
	@echo "make sops-envsubst-encrypt -> envsubst templates into deploy/secrets then sops-encrypt (uses SOPS_AGE_KEY)"
	@echo "make sops-envsubst-encrypt-server -> envsubst server template into deploy/secrets then sops-encrypt (uses SOPS_AGE_KEY)"
	@echo "make sops-envsubst-encrypt-bootstrap -> envsubst bootstrap template into deploy/secrets then sops-encrypt (uses SOPS_AGE_KEY)"
	@echo "make sops-edit-web-secrets -> edit encrypted web secrets with sops"
	@echo "make sops-edit-server-secrets -> edit encrypted server secrets with sops"
	@echo "make sops-edit-bootstrap-secrets -> edit encrypted bootstrap secrets with sops"
	@echo "make sops-edit-web-bff-secrets -> edit only the web-bff secret with sops"
	@echo "make sops-apply-web-secrets -> apply web/db/server secrets from SOPS (uses local sops; set SOPS_CMD or SOPS_AGE_KEY_FILE if needed)"
	@echo "make sops-apply-server-secrets -> apply server secrets from SOPS (uses local sops; set SOPS_CMD or SOPS_AGE_KEY_FILE if needed)"
	@echo "make sops-apply-bootstrap-secrets -> apply bootstrap secrets from SOPS"
	@echo "make deploy-web -> apply web-bff/web services + deployments + ingress + cert"
	@echo "make web-image-set TAG=... -> set maintainerd-web image to a specific tag"
	@echo "make web-release TAG=... -> build+push web & web-bff with same tag, then set both images"
	@echo "make github-profile-sync-image-build -> build GitHub profile sync image $(GITHUB_PROFILE_SYNC_IMAGE) locally"
	@echo "make github-profile-sync-image-push -> build and push $(GITHUB_PROFILE_SYNC_IMAGE)"
	@echo "make dot-project-sync-image-build -> build dot-project sync image $(DOT_PROJECT_SYNC_IMAGE) locally"
	@echo "make dot-project-sync-image-push -> build and push $(DOT_PROJECT_SYNC_IMAGE)"
	@echo "make clean-env       -> remove $(ENVOUT)"
	@echo "make print           -> show which keys would be loaded (without values)"
	@echo "make mntrd-image-build  -> build maintainerd image $(IMAGE) locally"
	@echo "make mntrd-image-push   -> build and push $(IMAGE) (uses GHCR_TOKEN + GHCR_USER for ghcr login)"
	@echo "make mntrd-image-deploy -> build, push, and restart Deployment in $(NAMESPACE)"
	@echo "make dot-project-sync-apply -> apply the hourly dot-project sync CronJob"
	@echo "make dot-project-sync-run -> trigger a manual dot-project sync job"
	@echo "make fossa-poller-deploy -> apply Deployment for the long-running FOSSA poller"
	@echo "make github-profile-sync-run -> recreate one-off GitHub profile sync job and tail logs"
	@echo "make bootstrap-run   -> prod: recreate bootstrap job after applying SOPS bootstrap secrets"
	@echo "make bootstrap-run-dev -> dev: recreate bootstrap job after applying .envrc secrets"
	@echo "make migrate-schema  -> run one-off schema migration job"
	@echo "make migrate-schema-safe -> scale down, run migration pinned to attached node, scale back up"
	@echo "make ensure-ns       -> ensure namespace $(NAMESPACE) exists"
	@echo "make apply-ghcr-secret -> create/update docker-registry Secret 'ghcr-secret'"
	@echo "make manifests-apply -> kubectl apply -f deploy/manifests (prod-only)"
	@echo "make manifests-delete-> kubectl delete -f deploy/manifests (cleanup)"
	@echo "make cluster-up      -> ensure ns + secrets + ghcr secret + apply manifests"
	@echo "make maintainerd-delete -> delete Deployment/Service maintainerd"
	@echo "make maintainerd-restart -> rollout restart Deployment/maintainerd"
	@echo "make maintainerd-drain   -> scale Deployment/maintainerd to 0 and wait for pods to exit"
	@echo "make maintainerd-port-forward -> forward :2525 -> svc/maintainerd:2525"
	@echo "make cluster-down    -> delete manifests applied via deploy/manifests"
	@echo ""
	@echo "== Web =="
	@echo "make web-install     -> install web dependencies"
	@echo "make web-dev         -> run Next.js dev server"
	@echo "make web-bff-run     -> run the Go BFF locally"
	@echo "make test-web        -> run web BDD tests (Cucumber + Playwright)"
	@echo "make test-web-podman -> run web BDD tests in a Playwright container via Podman"
	@echo "make test-web-report -> generate HTML report from Cucumber JSON output"

# ---- Web ----
.PHONY: web-install
web-install:
	@cd web && npm install

.PHONY: web-dev
web-dev:
	@cd web && npm run dev

.PHONY: web-bff-run
web-bff-run:
	@go run ./cmd/web-bff

.PHONY: test-web
test-web:
	@bash -c 'set -euo pipefail; \
	TESTDATA_DIR="$${TESTDATA_DIR:-$$(pwd)/testdata}"; \
	HOST_LOG_DIR="$${HOST_LOG_DIR:-$$(pwd)/testdata}"; \
	mkdir -p "$$TESTDATA_DIR"; \
	bff_pid=""; web_pid=""; microcks_started=""; \
	cleanup() { \
		status=$$?; \
		if [ -n "$$bff_pid" ]; then kill -TERM -$$bff_pid >/dev/null 2>&1 || true; fi; \
		if [ -n "$$web_pid" ]; then kill -TERM -$$web_pid >/dev/null 2>&1 || true; fi; \
		if [ -n "$$microcks_started" ]; then \
			CONTAINER_TOOL="$${CONTAINER_TOOL:-podman}" timeout 30 scripts/microcks-down.sh >/dev/null 2>&1 || true; \
		fi; \
		if command -v lsof >/dev/null 2>&1; then \
			lsof -ti TCP:9001 -ti TCP:4001 2>/dev/null | xargs -r kill -9 >/dev/null 2>&1 || true; \
		fi; \
		if [ "$$TESTDATA_DIR" != "$$HOST_LOG_DIR" ]; then \
			mkdir -p "$$HOST_LOG_DIR"; \
			cp -f "$$TESTDATA_DIR"/web-*-test.log "$$HOST_LOG_DIR" 2>/dev/null || true; \
			cp -f "$$TESTDATA_DIR"/maintainerd_test.db "$$HOST_LOG_DIR" 2>/dev/null || true; \
			cp -f "$$TESTDATA_DIR"/web-bdd-report.json "$$HOST_LOG_DIR" 2>/dev/null || true; \
			cp -f "$$TESTDATA_DIR"/web-bdd-results.xml "$$HOST_LOG_DIR" 2>/dev/null || true; \
		fi; \
		if [ $$status -ne 0 ]; then \
			echo "test-web failed; grepping logs from $$TESTDATA_DIR"; \
			if [ -f "$$TESTDATA_DIR/web-bff-test.log" ]; then \
				echo "--- web-bff-test.log (errors) ---"; \
				if ! grep -nE "(error|failed|panic)" "$$TESTDATA_DIR/web-bff-test.log"; then \
					echo "(no error matches; tailing last 200 lines)"; \
					tail -n 200 "$$TESTDATA_DIR/web-bff-test.log" || true; \
				fi; \
			fi; \
			if [ -f "$$TESTDATA_DIR/web-app-test.log" ]; then \
				echo "--- web-app-test.log (errors) ---"; \
				if ! grep -nE "(error|failed|panic)" "$$TESTDATA_DIR/web-app-test.log"; then \
					echo "(no error matches; tailing last 200 lines)"; \
					tail -n 200 "$$TESTDATA_DIR/web-app-test.log" || true; \
				fi; \
			fi; \
			if [ -f "$$TESTDATA_DIR/web-build-test.log" ]; then \
				echo "--- web-build-test.log (errors) ---"; \
				if ! grep -nE "(error|failed|panic)" "$$TESTDATA_DIR/web-build-test.log"; then \
					echo "(no error matches; tailing last 200 lines)"; \
					tail -n 200 "$$TESTDATA_DIR/web-build-test.log" || true; \
				fi; \
			fi; \
		fi; \
		exit $$status; \
	}; \
	trap cleanup EXIT; \
	if command -v lsof >/dev/null 2>&1; then \
		pids="$$(lsof -ti TCP:9001 -ti TCP:4001 2>/dev/null || true)"; \
		if [ -n "$$pids" ]; then \
			echo "Ports 9001/4001 are in use (PIDs: $$pids). Stopping them..."; \
			kill $$pids 2>/dev/null || true; \
			sleep 1; \
		fi; \
	fi; \
	DB_DRIVER="$${MD_DB_DRIVER:-postgres}"; \
	DB_DSN="$${MD_DB_DSN:-}"; \
	if [ "$$DB_DRIVER" != "postgres" ]; then \
		echo "test-web only supports Postgres. Set MD_DB_DRIVER=postgres and MD_DB_DSN."; \
		exit 1; \
	fi; \
	if [ -z "$$DB_DSN" ]; then \
		echo "MD_DB_DSN is required for test-web."; \
		exit 1; \
	fi; \
	USE_MICROCKS="$${WEB_BDD_USE_MICROCKS:-false}"; \
	if [ "$$USE_MICROCKS" = "true" ]; then \
		if ! command -v "$${CONTAINER_TOOL:-podman}" >/dev/null 2>&1; then \
			echo "WEB_BDD_USE_MICROCKS=true but container tool $${CONTAINER_TOOL:-podman} is not available."; \
			exit 1; \
		fi; \
		microcks_env="$$(CONTAINER_TOOL="$${CONTAINER_TOOL:-podman}" scripts/microcks-up.sh)"; \
		eval "$$microcks_env"; \
		microcks_started=1; \
		export FOSSA_API_BASE; \
		export FOSSA_API_TOKEN="$${FOSSA_API_TOKEN:-dummy-token}"; \
	fi; \
	MD_DB_DRIVER=postgres MD_DB_DSN="$$DB_DSN" go run ./cmd/testdb-reset; \
	MD_DB_DRIVER=postgres MD_DB_DSN="$$DB_DSN" go run ./cmd/migrate; \
	go run ./cmd/web-bff-seed -driver postgres -dsn "$$DB_DSN"; \
	export MD_DB_DRIVER=postgres; \
	export MD_DB_DSN="$$DB_DSN"; \
	unset MD_DB_PATH; \
	BFF_ADDR=:9001 BFF_TEST_MODE=true BFF_ALLOW_LIVE_FOSSA="$$USE_MICROCKS" BFF_TEST_FOSSA_TEAM_ID=999001 SESSION_COOKIE_SECURE=false SESSION_COOKIE_DOMAIN= \
	WEB_APP_BASE_URL=http://localhost:4001 GITHUB_OAUTH_REDIRECT_URL=http://localhost:9001/auth/callback \
	GITHUB_OAUTH_CLIENT_ID=test GITHUB_OAUTH_CLIENT_SECRET=test FOSSA_API_BASE="$${FOSSA_API_BASE:-}" FOSSA_API_TOKEN="$${FOSSA_API_TOKEN:-}" \
	setsid go run ./cmd/web-bff </dev/null >"$$TESTDATA_DIR/web-bff-test.log" 2>&1 & \
	bff_pid=$$!; \
	mkdir -p "$$TESTDATA_DIR/tmp" web/tmp || true; \
	NEXT_PUBLIC_BFF_BASE_URL=http://localhost:9001 NEXT_DIST_DIR="$$TESTDATA_DIR/next-dist" TMPDIR="$$TESTDATA_DIR/tmp" NEXT_TEMP_DIR="$$TESTDATA_DIR/tmp" \
	NEXT_TELEMETRY_DISABLED=1 NPM_CONFIG_UPDATE_NOTIFIER=false TURBOPACK_ROOT="$$(pwd)/web" OUTPUT_FILE_TRACING_ROOT="$$(pwd)/web" \
	npm --prefix web run build > "$$TESTDATA_DIR/web-build-test.log" 2>&1; \
	PORT=4001 NEXT_PUBLIC_BFF_BASE_URL=http://localhost:9001 NEXT_DIST_DIR="$$TESTDATA_DIR/next-dist" TMPDIR="$$TESTDATA_DIR/tmp" NEXT_TEMP_DIR="$$TESTDATA_DIR/tmp" \
	NEXT_TELEMETRY_DISABLED=1 NPM_CONFIG_UPDATE_NOTIFIER=false TURBOPACK_ROOT="$$(pwd)/web" OUTPUT_FILE_TRACING_ROOT="$$(pwd)/web" \
	setsid npm --prefix web run start </dev/null > "$$TESTDATA_DIR/web-app-test.log" 2>&1 & \
	web_pid=$$!; \
	npx --prefix web wait-on http://localhost:9001/healthz http://localhost:4001 > /dev/null 2>&1; \
	BDD_FEATURE_PATH="$${BDD_FEATURE:-}"; \
	if [ -n "$$BDD_FEATURE_PATH" ]; then \
		cd web && WEB_BDD_FEATURE="$$BDD_FEATURE_PATH" WEB_BASE_URL=http://localhost:4001 BFF_BASE_URL=http://localhost:9001 TEST_STAFF_LOGIN=staff-tester TEST_MAINTAINER_LOGIN=antonio-example TEST_OTHER_MAINTAINER_LOGIN=renee-sample NEXT_TELEMETRY_DISABLED=1 NPM_CONFIG_UPDATE_NOTIFIER=false WEB_TEST_ARTIFACTS_DIR="$$TESTDATA_DIR/web-artifacts" npx cucumber-js --config ./cucumber.js; \
	else \
		WEB_BASE_URL=http://localhost:4001 BFF_BASE_URL=http://localhost:9001 TEST_STAFF_LOGIN=staff-tester TEST_MAINTAINER_LOGIN=antonio-example TEST_OTHER_MAINTAINER_LOGIN=renee-sample NEXT_TELEMETRY_DISABLED=1 NPM_CONFIG_UPDATE_NOTIFIER=false WEB_TEST_ARTIFACTS_DIR="$$TESTDATA_DIR/web-artifacts" npm --prefix web run test:bdd; \
	fi; \
	'

.PHONY: test-web-license-checker
test-web-license-checker:
	@bash -c 'set -euo pipefail; \
	TESTDATA_DIR="$${TESTDATA_DIR:-$$(pwd)/testdata}"; \
	HOST_LOG_DIR="$${HOST_LOG_DIR:-$$(pwd)/testdata}"; \
	mkdir -p "$$TESTDATA_DIR"; \
	bff_pid=""; web_pid=""; \
	cleanup() { \
		status=$$?; \
		if [ -n "$$bff_pid" ]; then kill -TERM -$$bff_pid >/dev/null 2>&1 || true; fi; \
		if [ -n "$$web_pid" ]; then kill -TERM -$$web_pid >/dev/null 2>&1 || true; fi; \
		if command -v lsof >/dev/null 2>&1; then \
			lsof -ti TCP:9001 -ti TCP:4001 2>/dev/null | xargs -r kill -9 >/dev/null 2>&1 || true; \
		fi; \
		if [ "$$TESTDATA_DIR" != "$$HOST_LOG_DIR" ]; then \
			mkdir -p "$$HOST_LOG_DIR"; \
			cp -f "$$TESTDATA_DIR"/web-*-test.log "$$HOST_LOG_DIR" 2>/dev/null || true; \
			cp -f "$$TESTDATA_DIR"/maintainerd_test.db "$$HOST_LOG_DIR" 2>/dev/null || true; \
			cp -f "$$TESTDATA_DIR"/web-bdd-report.json "$$HOST_LOG_DIR" 2>/dev/null || true; \
			cp -f "$$TESTDATA_DIR"/web-bdd-results.xml "$$HOST_LOG_DIR" 2>/dev/null || true; \
		fi; \
		if [ $$status -ne 0 ]; then \
			echo "test-web-license-checker failed; grepping logs from $$TESTDATA_DIR"; \
			if [ -f "$$TESTDATA_DIR/web-bff-test.log" ]; then \
				echo "--- web-bff-test.log (errors) ---"; \
				if ! grep -nE "(error|failed|panic)" "$$TESTDATA_DIR/web-bff-test.log"; then \
					echo "(no error matches; tailing last 200 lines)"; \
					tail -n 200 "$$TESTDATA_DIR/web-bff-test.log" || true; \
				fi; \
			fi; \
			if [ -f "$$TESTDATA_DIR/web-app-test.log" ]; then \
				echo "--- web-app-test.log (errors) ---"; \
				if ! grep -nE "(error|failed|panic)" "$$TESTDATA_DIR/web-app-test.log"; then \
					echo "(no error matches; tailing last 200 lines)"; \
					tail -n 200 "$$TESTDATA_DIR/web-app-test.log" || true; \
				fi; \
			fi; \
			if [ -f "$$TESTDATA_DIR/web-build-test.log" ]; then \
				echo "--- web-build-test.log (errors) ---"; \
				if ! grep -nE "(error|failed|panic)" "$$TESTDATA_DIR/web-build-test.log"; then \
					echo "(no error matches; tailing last 200 lines)"; \
					tail -n 200 "$$TESTDATA_DIR/web-build-test.log" || true; \
				fi; \
			fi; \
		fi; \
		exit $$status; \
	}; \
	trap cleanup EXIT; \
	if command -v lsof >/dev/null 2>&1; then \
		pids="$$(lsof -ti TCP:9001 -ti TCP:4001 2>/dev/null || true)"; \
		if [ -n "$$pids" ]; then \
			echo "Ports 9001/4001 are in use (PIDs: $$pids). Stopping them..."; \
			kill $$pids 2>/dev/null || true; \
			sleep 1; \
		fi; \
	fi; \
	DB_DRIVER="$${MD_DB_DRIVER:-postgres}"; \
	DB_DSN="$${MD_DB_DSN:-}"; \
	if [ "$$DB_DRIVER" != "postgres" ]; then \
		echo "test-web-license-checker only supports Postgres. Set MD_DB_DRIVER=postgres and MD_DB_DSN."; \
		exit 1; \
	fi; \
	if [ -z "$$DB_DSN" ]; then \
		echo "MD_DB_DSN is required for test-web-license-checker."; \
		exit 1; \
	fi; \
	MD_DB_DRIVER=postgres MD_DB_DSN="$$DB_DSN" go run ./cmd/testdb-reset; \
	MD_DB_DRIVER=postgres MD_DB_DSN="$$DB_DSN" go run ./cmd/migrate; \
	go run ./cmd/web-bff-seed -driver postgres -dsn "$$DB_DSN"; \
	export MD_DB_DRIVER=postgres; \
	export MD_DB_DSN="$$DB_DSN"; \
	unset MD_DB_PATH; \
	BFF_ADDR=:9001 BFF_TEST_MODE=true BFF_TEST_FOSSA_TEAM_ID=999001 SESSION_COOKIE_SECURE=false SESSION_COOKIE_DOMAIN= \
	WEB_APP_BASE_URL=http://localhost:4001 GITHUB_OAUTH_REDIRECT_URL=http://localhost:9001/auth/callback \
	GITHUB_OAUTH_CLIENT_ID=test GITHUB_OAUTH_CLIENT_SECRET=test \
	setsid go run ./cmd/web-bff </dev/null >"$$TESTDATA_DIR/web-bff-test.log" 2>&1 & \
	bff_pid=$$!; \
	mkdir -p "$$TESTDATA_DIR/tmp" web/tmp || true; \
	NEXT_PUBLIC_BFF_BASE_URL=http://localhost:9001 NEXT_DIST_DIR="$$TESTDATA_DIR/next-dist" TMPDIR="$$TESTDATA_DIR/tmp" NEXT_TEMP_DIR="$$TESTDATA_DIR/tmp" \
	NEXT_TELEMETRY_DISABLED=1 NPM_CONFIG_UPDATE_NOTIFIER=false TURBOPACK_ROOT="$$(pwd)/web" OUTPUT_FILE_TRACING_ROOT="$$(pwd)/web" \
	npm --prefix web run build > "$$TESTDATA_DIR/web-build-test.log" 2>&1; \
	PORT=4001 NEXT_PUBLIC_BFF_BASE_URL=http://localhost:9001 NEXT_DIST_DIR="$$TESTDATA_DIR/next-dist" TMPDIR="$$TESTDATA_DIR/tmp" NEXT_TEMP_DIR="$$TESTDATA_DIR/tmp" \
	NEXT_TELEMETRY_DISABLED=1 NPM_CONFIG_UPDATE_NOTIFIER=false TURBOPACK_ROOT="$$(pwd)/web" OUTPUT_FILE_TRACING_ROOT="$$(pwd)/web" \
	setsid npm --prefix web run start </dev/null > "$$TESTDATA_DIR/web-app-test.log" 2>&1 & \
	web_pid=$$!; \
	npx --prefix web wait-on http://localhost:9001/healthz http://localhost:4001 > /dev/null 2>&1; \
	cd web && WEB_BASE_URL=http://localhost:4001 BFF_BASE_URL=http://localhost:9001 TEST_STAFF_LOGIN=staff-tester \
	TEST_MAINTAINER_LOGIN=antonio-example TEST_OTHER_MAINTAINER_LOGIN=renee-sample \
	NEXT_TELEMETRY_DISABLED=1 NPM_CONFIG_UPDATE_NOTIFIER=false WEB_TEST_ARTIFACTS_DIR="$$TESTDATA_DIR/web-artifacts" \
	npx cucumber-js --config ./cucumber.js ../features/web/license_checker.feature; \
	'

.PHONY: web-bdd
web-bdd:
	@bash -c 'set -euo pipefail; \
	status=0; \
	$(MAKE) test-web || status=$$?; \
	$(MAKE) web-bdd-report || true; \
	exit $$status; \
	'

.PHONY: web-bdd-maintainer-profile
web-bdd-maintainer-profile:
	@bash -c 'set -euo pipefail; \
	status=0; \
	WEB_BDD_USE_MICROCKS=true BDD_FEATURE=../features/web/maintainer_profile.feature $(MAKE) test-web || status=$$?; \
	$(MAKE) web-bdd-report || true; \
	exit $$status; \
	'

.PHONY: web-bdd-report
web-bdd-report:
	@bash -c 'set -euo pipefail; \
	BASE_DIR="$${HOST_LOG_DIR:-testdata}"; \
	BASE_DIR_ABS="$$(cd "$$BASE_DIR" && pwd)"; \
	JSON_PATH="$$BASE_DIR_ABS/web-bdd-report.json"; \
	REPORT_DIR="$$BASE_DIR_ABS/web-bdd-report"; \
	if [ -f "$$JSON_PATH" ]; then \
		rm -rf "$$REPORT_DIR"; \
		(cd web && node -e "require(\"multiple-cucumber-html-reporter\").generate({ jsonDir: \"$$BASE_DIR_ABS\", reportPath: \"$$REPORT_DIR\", reportName: \"Maintainer D Web BDD\", pageTitle: \"Maintainer D Web BDD\", openReportInBrowser: false, disableLog: true, customData: { title: \"Run info\", data: [{ label: \"App\", value: \"maintainer-d\" }, { label: \"Platform\", value: \"Web\" }] } });"); \
		echo "BDD HTML report: $$REPORT_DIR/index.html"; \
		if [ -z "$(NO_BDD_OPEN)" ] && [ -z "$$CI" ]; then \
			if command -v xdg-open >/dev/null 2>&1; then xdg-open "$$REPORT_DIR/index.html" >/dev/null 2>&1 || true; fi; \
		fi; \
	else \
		echo "Missing $$JSON_PATH; run test-web first."; \
		exit 1; \
	fi; \
	'

.PHONY: test-web-report
test-web-report:
	@bash -c 'set -euo pipefail; \
	$(MAKE) test-web-podman || true; \
	BASE_DIR="$${HOST_LOG_DIR:-testdata}"; \
	BASE_DIR_ABS="$$(cd "$$BASE_DIR" && pwd)"; \
	JSON_PATH="$$BASE_DIR_ABS/web-bdd-report.json"; \
	REPORT_DIR="$$BASE_DIR_ABS/web-bdd-report"; \
	if [ -f "$$JSON_PATH" ]; then \
		rm -rf "$$REPORT_DIR"; \
		(cd web && node -e "require(\"multiple-cucumber-html-reporter\").generate({ jsonDir: \"$$BASE_DIR_ABS\", reportPath: \"$$REPORT_DIR\", reportName: \"Maintainer D Web BDD\", pageTitle: \"Maintainer D Web BDD\", openReportInBrowser: false, disableLog: true, customData: { title: \"Run info\", data: [{ label: \"App\", value: \"maintainer-d\" }, { label: \"Platform\", value: \"Web\" }] } });"); \
		echo "BDD HTML report: $$REPORT_DIR/index.html"; \
		if [ -z "$(NO_BDD_OPEN)" ] && [ -z "$$CI" ]; then \
			if command -v xdg-open >/dev/null 2>&1; then xdg-open "$$REPORT_DIR/index.html" >/dev/null 2>&1 || true; fi; \
		fi; \
	else \
		echo "Missing $$JSON_PATH; run test-web first."; \
		exit 1; \
	fi; \
	'

.PHONY: test-web-podman
test-web-podman:
	@echo "Running Playwright tests in a container so non-Ubuntu hosts can execute them."
	@if ! podman image exists maintainerd-web-test:local; then \
		echo "Building maintainerd-web-test:local (cached for future runs)."; \
		podman build -f testdata/web-test.Dockerfile -t maintainerd-web-test:local testdata; \
	fi; \
	podman run --rm -t --userns=keep-id \
		-v $(TOPDIR):/work:Z \
		-w /work \
		-e PLAYWRIGHT_BROWSERS_PATH=/ms-playwright \
		-e GOMODCACHE=/work/.modcache \
		-e GOCACHE=/work/.gocache \
		maintainerd-web-test:local \
		bash -lc 'PATH=/usr/local/go/bin:$$PATH TESTDATA_DIR=/work/testdata HOST_LOG_DIR=/work/testdata make test-web'

# Convert .envrc (export FOO=bar) to KEY=VALUE lines
# - drops comments/blank lines
# - strips a leading 'export' and surrounding whitespace
.PHONY: env
env: $(ENVOUT)

$(ENVOUT): $(ENVSRC)
	@echo "Generating $(ENVOUT) from $(ENVSRC)"
	@sed -E '/^[[:space:]]*#/d; /^[[:space:]]*$$/d; s/^[[:space:]]*export[[:space:]]+//' $(ENVSRC) > $(ENVOUT)

# Apply the Secret with all bootstrap env vars
.PHONY: apply-env
apply-env: $(ENVOUT)
	@echo "Applying secret $(ENV_SECRET_NAME) in namespace $(NAMESPACE) [ctx=$(CTX_STR)]"
	@kubectl -n $(NAMESPACE) create secret generic $(ENV_SECRET_NAME) \
		--from-env-file=$(ENVOUT) \
		$(if $(KUBECONTEXT),--context $(KUBECONTEXT)) \
		--dry-run=client -o yaml | kubectl -n $(NAMESPACE) apply -f -

# Apply the Secret that contains the credentials file
.PHONY: apply-creds
apply-creds:
	@[ -f "$(CREDS_FILE)" ] || (echo "Missing $(CREDS_FILE). Set CREDS_FILE=... or place the file."; exit 1)
	@echo "Applying secret $(CREDS_SECRET_NAME) in namespace $(NAMESPACE) [ctx=$(CTX_STR)]"
	@kubectl -n $(NAMESPACE) create secret generic $(CREDS_SECRET_NAME) \
		--from-file=$(CREDS_KEY)=$(CREDS_FILE) \
		$(if $(KUBECONTEXT),--context $(KUBECONTEXT)) \
		--dry-run=client -o yaml | kubectl -n $(NAMESPACE) apply -f -

# Apply the web env Secret from the template (env vars are substituted)
.PHONY: apply-web-env
apply-web-env:
	@echo "Applying secret $(WEB_ENV_SECRET_NAME) in namespace $(NAMESPACE) [ctx=$(CTX_STR)]"
	@envsubst < $(WEB_ENV_TEMPLATE) | kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) apply -f -

# Apply the web-bff env Secret from the template (env vars are substituted)
.PHONY: apply-web-bff-env
apply-web-bff-env:
	@echo "Applying secret $(WEB_BFF_ENV_SECRET_NAME) in namespace $(NAMESPACE) [ctx=$(CTX_STR)]"
	@envsubst < $(WEB_BFF_ENV_TEMPLATE) | kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) apply -f -

# Convenience combo target
.PHONY: web-secrets
web-secrets: apply-web-env apply-web-bff-env
	@echo "Web secrets applied: $(WEB_ENV_SECRET_NAME), $(WEB_BFF_ENV_SECRET_NAME) [ns=$(NAMESPACE)]"

.PHONY: sops-apply-web-secrets
sops-apply-web-secrets:
	@echo "Applying web secrets from SOPS [ns=$(NAMESPACE)]"
	@bash -c 'set -euo pipefail; \
	if [ ! -f "deploy/secrets/maintainerd-web-env.yaml" ] || [ ! -f "deploy/secrets/maintainerd-web-bff-env.yaml" ] || [ ! -f "deploy/secrets/maintainerd-db-env.yaml" ] || [ ! -f "deploy/secrets/maintainerd-server-env.yaml" ]; then \
		echo "Missing encrypted secrets. Expected deploy/secrets/maintainerd-web-env.yaml, deploy/secrets/maintainerd-web-bff-env.yaml, deploy/secrets/maintainerd-db-env.yaml, and deploy/secrets/maintainerd-server-env.yaml."; \
		echo "Create and encrypt them with sops before running this target."; exit 1; \
	fi; \
	if [ -n "$(SOPS_EXPECTED_AGE)" ]; then \
		for file in deploy/secrets/maintainerd-web-env.yaml deploy/secrets/maintainerd-web-bff-env.yaml deploy/secrets/maintainerd-db-env.yaml deploy/secrets/maintainerd-server-env.yaml; do \
			if ! grep -q "recipient: $(SOPS_EXPECTED_AGE)" "$$file"; then \
				echo "Encrypted recipients for $$file do not include expected key $(SOPS_EXPECTED_AGE)."; \
				echo "Re-encrypt with: make sops-encrypt-web-secrets SOPS_AGE_KEY=$(SOPS_EXPECTED_AGE)"; \
				exit 1; \
			fi; \
		done; \
	fi; \
	decrypt() { \
		file="$$1"; \
		if [ -n "$(SOPS_AGE_KEY_FILE)" ]; then \
			SOPS_AGE_KEY_FILE="$(SOPS_AGE_KEY_FILE)" $(SOPS_CMD) -d $$file; \
		else \
			$(SOPS_CMD) -d $$file; \
		fi; \
	}; \
	decrypt deploy/secrets/maintainerd-web-env.yaml | kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) apply -f -; \
	decrypt deploy/secrets/maintainerd-web-bff-env.yaml | kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) apply -f -; \
	decrypt deploy/secrets/maintainerd-db-env.yaml | kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) apply -f -; \
	decrypt deploy/secrets/maintainerd-server-env.yaml | kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) apply -f -; \
	echo "Web secrets applied from SOPS."'

.PHONY: sops-apply-bootstrap-secrets
sops-apply-bootstrap-secrets:
	@echo "Applying bootstrap secrets from SOPS [ns=$(NAMESPACE)]"
	@bash -c 'set -euo pipefail; \
	if [ ! -f "deploy/secrets/maintainerd-bootstrap-env.yaml" ]; then \
		echo "Missing encrypted secret deploy/secrets/maintainerd-bootstrap-env.yaml."; \
		echo "Create it from deploy/templates/maintainerd-bootstrap-env.yaml.tmpl and encrypt with sops."; \
		exit 1; \
	fi; \
	decrypt() { \
		file="$$1"; \
		if [ -n "$(SOPS_AGE_KEY_FILE)" ]; then \
			SOPS_AGE_KEY_FILE="$(SOPS_AGE_KEY_FILE)" $(SOPS_CMD) -d $$file; \
		else \
			$(SOPS_CMD) -d $$file; \
		fi; \
	}; \
	decrypt deploy/secrets/maintainerd-bootstrap-env.yaml | kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) apply -f -; \
	echo "Bootstrap secrets applied from SOPS."'

.PHONY: sops-apply-server-secrets
sops-apply-server-secrets:
	@echo "Applying server secrets from SOPS [ns=$(NAMESPACE)]"
	@bash -c 'set -euo pipefail; \
	if [ ! -f "deploy/secrets/maintainerd-server-env.yaml" ]; then \
		echo "Missing encrypted secret deploy/secrets/maintainerd-server-env.yaml."; \
		echo "Create it from deploy/templates/maintainerd-server-env.yaml.tmpl and encrypt with sops."; \
		exit 1; \
	fi; \
	decrypt() { \
		file="$$1"; \
		if [ -n "$(SOPS_AGE_KEY_FILE)" ]; then \
			SOPS_AGE_KEY_FILE="$(SOPS_AGE_KEY_FILE)" $(SOPS_CMD) -d $$file; \
		else \
			$(SOPS_CMD) -d $$file; \
		fi; \
	}; \
	decrypt deploy/secrets/maintainerd-server-env.yaml | kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) apply -f -; \
	echo "Server secrets applied from SOPS."'

.PHONY: sops-encrypt-web-secrets
sops-encrypt-web-secrets:
	@bash -c 'set -euo pipefail; \
	if [ -z "$(SOPS_AGE_KEY)" ]; then \
		echo "Set SOPS_AGE_KEY to the age public key used for encryption."; exit 1; \
	fi; \
	for file in deploy/secrets/maintainerd-web-env.yaml deploy/secrets/maintainerd-web-bff-env.yaml deploy/secrets/maintainerd-db-env.yaml deploy/secrets/maintainerd-server-env.yaml; do \
		if [ ! -f "$$file" ]; then \
			echo "Missing $$file"; exit 1; \
		fi; \
		$(SOPS_CMD) --age $(SOPS_AGE_KEY) -e -i $$file; \
	done; \
	echo "Encrypted web secrets with sops.";'

.PHONY: sops-encrypt-server-secrets
sops-encrypt-server-secrets:
	@bash -c 'set -euo pipefail; \
	if [ -z "$(SOPS_AGE_KEY)" ]; then \
		echo "Set SOPS_AGE_KEY to the age public key used for encryption."; exit 1; \
	fi; \
	if [ ! -f "deploy/secrets/maintainerd-server-env.yaml" ]; then \
		echo "Missing deploy/secrets/maintainerd-server-env.yaml"; exit 1; \
	fi; \
	$(SOPS_CMD) --age $(SOPS_AGE_KEY) -e -i deploy/secrets/maintainerd-server-env.yaml; \
	echo "Encrypted server secrets with sops.";'

.PHONY: sops-encrypt-bootstrap-secrets
sops-encrypt-bootstrap-secrets:
	@bash -c 'set -euo pipefail; \
	if [ -z "$(SOPS_AGE_KEY)" ]; then \
		echo "Set SOPS_AGE_KEY to the age public key used for encryption."; exit 1; \
	fi; \
	if [ ! -f "deploy/secrets/maintainerd-bootstrap-env.yaml" ]; then \
		echo "Missing deploy/secrets/maintainerd-bootstrap-env.yaml"; exit 1; \
	fi; \
	$(SOPS_CMD) --age $(SOPS_AGE_KEY) -e -i deploy/secrets/maintainerd-bootstrap-env.yaml; \
	echo "Encrypted bootstrap secrets with sops.";'

.PHONY: sops-envsubst-encrypt
sops-envsubst-encrypt:
	@bash -c 'set -euo pipefail; \
	if [ -z "$(SOPS_AGE_KEY)" ]; then \
		echo "Set SOPS_AGE_KEY to the age public key used for encryption."; exit 1; \
	fi; \
	for var in WEB_APP_BASE_URL GITHUB_OAUTH_REDIRECT_URL GITHUB_OAUTH_CLIENT_ID GITHUB_OAUTH_CLIENT_SECRET SESSION_COOKIE_NAME SESSION_COOKIE_DOMAIN SESSION_COOKIE_SECURE MD_DB_DRIVER MD_DB_DSN; do \
		if [ -z "$${!var:-}" ]; then \
			echo "Missing required env var: $$var"; exit 1; \
		fi; \
	done; \
	envsubst < deploy/templates/maintainerd-web-env.yaml.tmpl > deploy/secrets/maintainerd-web-env.yaml; \
	envsubst < deploy/templates/maintainerd-web-bff-env.yaml.tmpl > deploy/secrets/maintainerd-web-bff-env.yaml; \
	envsubst < deploy/templates/maintainerd-db-env.yaml.tmpl > deploy/secrets/maintainerd-db-env.yaml; \
	$(MAKE) sops-encrypt-web-secrets SOPS_AGE_KEY="$(SOPS_AGE_KEY)" SOPS_CMD="$(SOPS_CMD)";'

.PHONY: sops-envsubst-encrypt-server
sops-envsubst-encrypt-server:
	@bash -c 'set -euo pipefail; \
	if [ -z "$(SOPS_AGE_KEY)" ]; then \
		echo "Set SOPS_AGE_KEY to the age public key used for encryption."; exit 1; \
	fi; \
	for var in GITHUB_API_TOKEN GITHUB_WEBHOOK_SECRET FOSSA_API_TOKEN ORG REPO; do \
		if [ -z "$${!var:-}" ]; then \
			echo "Missing required env var: $$var"; exit 1; \
		fi; \
	done; \
	envsubst < deploy/templates/maintainerd-server-env.yaml.tmpl > deploy/secrets/maintainerd-server-env.yaml; \
	$(MAKE) sops-encrypt-server-secrets SOPS_AGE_KEY="$(SOPS_AGE_KEY)" SOPS_CMD="$(SOPS_CMD)";'

.PHONY: sops-envsubst-encrypt-bootstrap
sops-envsubst-encrypt-bootstrap:
	@bash -c 'set -euo pipefail; \
	if [ -z "$(SOPS_AGE_KEY)" ]; then \
		echo "Set SOPS_AGE_KEY to the age public key used for encryption."; exit 1; \
	fi; \
	for var in MD_WORKSHEET FOSSA_API_TOKEN WORKSPACE_CREDENTIALS_FILE; do \
		if [ -z "$${!var:-}" ]; then \
			echo "Missing required env var: $$var"; exit 1; \
		fi; \
	done; \
	envsubst < deploy/templates/maintainerd-bootstrap-env.yaml.tmpl > deploy/secrets/maintainerd-bootstrap-env.yaml; \
	$(MAKE) sops-encrypt-bootstrap-secrets SOPS_AGE_KEY="$(SOPS_AGE_KEY)" SOPS_CMD="$(SOPS_CMD)";'

.PHONY: sops-edit-web-secrets
sops-edit-web-secrets:
	@bash -c 'set -euo pipefail; \
	edit() { \
		file="$$1"; \
		if [ -n "$(SOPS_AGE_KEY_FILE)" ]; then \
			SOPS_AGE_KEY_FILE="$(SOPS_AGE_KEY_FILE)" $(SOPS_CMD) "$$file"; \
		else \
			$(SOPS_CMD) "$$file"; \
		fi; \
		status=$$?; \
		if [ $$status -ne 0 ] && [ $$status -ne 200 ]; then \
			exit $$status; \
		fi; \
	}; \
	edit deploy/secrets/maintainerd-web-env.yaml; \
	edit deploy/secrets/maintainerd-web-bff-env.yaml; \
	edit deploy/secrets/maintainerd-db-env.yaml;'

.PHONY: sops-edit-server-secrets
sops-edit-server-secrets:
	@bash -c 'set -euo pipefail; \
	if [ -n "$(SOPS_AGE_KEY_FILE)" ]; then \
		SOPS_AGE_KEY_FILE="$(SOPS_AGE_KEY_FILE)" $(SOPS_CMD) deploy/secrets/maintainerd-server-env.yaml; \
	else \
		$(SOPS_CMD) deploy/secrets/maintainerd-server-env.yaml; \
	fi;'

.PHONY: sops-edit-bootstrap-secrets
sops-edit-bootstrap-secrets:
	@bash -c 'set -euo pipefail; \
	if [ -n "$(SOPS_AGE_KEY_FILE)" ]; then \
		SOPS_AGE_KEY_FILE="$(SOPS_AGE_KEY_FILE)" $(SOPS_CMD) deploy/secrets/maintainerd-bootstrap-env.yaml; \
	else \
		$(SOPS_CMD) deploy/secrets/maintainerd-bootstrap-env.yaml; \
	fi;'

.PHONY: sops-edit-web-bff-secrets
sops-edit-web-bff-secrets:
	@bash -c 'set -euo pipefail; \
	if [ -n "$(SOPS_AGE_KEY_FILE)" ]; then \
		SOPS_AGE_KEY_FILE="$(SOPS_AGE_KEY_FILE)" $(SOPS_CMD) deploy/secrets/maintainerd-web-bff-env.yaml; \
	else \
		$(SOPS_CMD) deploy/secrets/maintainerd-web-bff-env.yaml; \
	fi;'

.PHONY: deploy-web
deploy-web:
	@echo "Deploying web-bff + web + ingress + cert [ns=$(NAMESPACE)]"
	@kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) apply -f deploy/manifests/web-bff-service.yaml
	@kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) apply -f deploy/manifests/web-bff-deployment.yaml
	@kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) apply -f deploy/manifests/web-service.yaml
	@kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) apply -f deploy/manifests/web-deployment.yaml
	@kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) apply -f deploy/manifests/ingress-maintainerd-web.yaml
	@kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) apply -f deploy/manifests/certificate-github-events.yaml

.PHONY: web-image-set
web-image-set:
	@if [ -z "$(TAG)" ]; then \
		echo "Usage: make web-image-set TAG=<tag>"; exit 1; \
	fi
	@echo "Setting maintainerd-web image to $(WEB_IMAGE_REPO):$(TAG) [ns=$(NAMESPACE)]"
	@kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) \
		set image deploy/maintainerd-web web=$(WEB_IMAGE_REPO):$(TAG)

.PHONY: web-bff-image-set
web-bff-image-set:
	@if [ -z "$(TAG)" ]; then \
		echo "Usage: make web-bff-image-set TAG=<tag>"; exit 1; \
	fi
	@echo "Setting maintainerd-web-bff image to $(WEB_BFF_IMAGE_REPO):$(TAG) [ns=$(NAMESPACE)]"
	@kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) \
		set image deploy/maintainerd-web-bff web-bff=$(WEB_BFF_IMAGE_REPO):$(TAG)

.PHONY: web-release
web-release:
	@if [ -z "$(TAG)" ]; then \
		echo "Usage: make web-release TAG=<tag>"; exit 1; \
	fi
	@$(MAKE) web-image-push TAG=$(TAG)
	@$(MAKE) web-bff-image-push TAG=$(TAG)
	@$(MAKE) web-image-set TAG=$(TAG)
	@$(MAKE) web-bff-image-set TAG=$(TAG)

.PHONY: fossa-poller-image-set
fossa-poller-image-set:
	@if [ -z "$(TAG)" ]; then \
		echo "Usage: make fossa-poller-image-set TAG=<tag>"; exit 1; \
	fi
	@echo "Setting Deployment/maintainerd-fossa-poller image to $(FOSSA_POLLER_IMAGE) [ns=$(NAMESPACE)]"
	@kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) \
		set image deployment/maintainerd-fossa-poller fossa-poller=$(FOSSA_POLLER_IMAGE)
	@kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) \
		rollout status deployment/maintainerd-fossa-poller --timeout=180s

.PHONY: dot-project-sync-image-set
dot-project-sync-image-set:
	@if [ -z "$(TAG)" ]; then \
		echo "Usage: make dot-project-sync-image-set TAG=<tag>"; exit 1; \
	fi
	@echo "Setting maintainerd-dot-project-sync image to $(DOT_PROJECT_SYNC_IMAGE) [ns=$(NAMESPACE)]"
	@kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) \
		set image cronjob/dot-project-sync '*=$(DOT_PROJECT_SYNC_IMAGE)'

.PHONY: sanitize-image-set
sanitize-image-set:
	@if [ -z "$(TAG)" ]; then \
		echo "Usage: make sanitize-image-set TAG=<tag>"; exit 1; \
	fi
	@echo "Setting maintainerd-sanitize image to $(SANITIZE_IMAGE) [ns=$(NAMESPACE)]"
	@kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) \
		set image cronjob/maintainer-sanitize '*=$(SANITIZE_IMAGE)'

.PHONY: onboarding-backfill-image-set
onboarding-backfill-image-set:
	@if [ -z "$(TAG)" ]; then \
		echo "Usage: make onboarding-backfill-image-set TAG=<tag>"; exit 1; \
	fi
	@echo "Setting maintainerd-onboarding-backfill image to $(ONBOARDING_BACKFILL_IMAGE) [ns=$(NAMESPACE)]"
	@kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) \
		set image cronjob/maintainerd-onboarding-backfill '*=$(ONBOARDING_BACKFILL_IMAGE)'

.PHONY: image-set
image-set: mntrd-image-set web-image-set web-bff-image-set fossa-poller-image-set dot-project-sync-image-set sanitize-image-set onboarding-backfill-image-set
	@true

# Convenience combo target
.PHONY: secrets
secrets: env apply-env apply-creds
	@echo "Secrets applied: $(ENV_SECRET_NAME), $(CREDS_SECRET_NAME) [ns=$(NAMESPACE)]"

# Show which keys would be loaded (without values)
.PHONY: print
print: env
	@echo "Keys in $(ENVOUT):"
	@cut -d= -f1 $(ENVOUT)

.PHONY: images-show
images-show:
	@echo "Image repositories:"
	@echo "  maintainerd        $(IMAGE)"
	@echo "  maintainerd-dot-project-sync $(DOT_PROJECT_SYNC_IMAGE)"
	@echo "  maintainerd-sanitize $(SANITIZE_IMAGE)"
	@echo "  maintainerd-migrate  $(MIGRATE_IMAGE)"
	@echo "  maintainerd-fossa-poller $(FOSSA_POLLER_IMAGE)"
	@echo "  maintainerd-github-profile-sync $(GITHUB_PROFILE_SYNC_IMAGE)"
	@echo "  maintainerd-web      $(WEB_IMAGE)"
	@echo "  maintainerd-web-bff  $(WEB_BFF_IMAGE)"

.PHONY: images-build
images-build: mntrd-image-build dot-project-sync-image-build sanitize-image-build migrate-image-build onboarding-backfill-image-build fossa-poller-image-build github-profile-sync-image-build web-image-build web-bff-image-build
	@echo "All images built."

.PHONY: mntrd-image-push-only
mntrd-image-push-only:
	@echo "Ensuring $(CONTAINER_TOOL) is logged in to $(REGISTRY) (uses GHCR_TOKEN if set)"
	@if [ -n "$(GHCR_TOKEN)" ]; then \
		echo "Logging into $(REGISTRY) as $(GHCR_USER) using token from GHCR_TOKEN"; \
		echo "$(GHCR_TOKEN)" | $(CONTAINER_TOOL) login $(REGISTRY) -u "$(GHCR_USER)" --password-stdin; \
	else \
		echo "GHCR_TOKEN not set; attempting push with existing auth"; \
	fi
	@echo "Pushing image: $(IMAGE)"
	@$(CONTAINER_TOOL) push $(IMAGE)
	@echo "Tagging and pushing latest: $(IMAGE_LATEST)"
	@$(CONTAINER_TOOL) tag $(IMAGE) $(IMAGE_LATEST)
	@$(CONTAINER_TOOL) push $(IMAGE_LATEST)

.PHONY: dot-project-sync-image-push-only
dot-project-sync-image-push-only:
	@echo "Ensuring $(CONTAINER_TOOL) is logged in to $(REGISTRY) (uses GHCR_TOKEN if set)"
	@if [ -n "$(GHCR_TOKEN)" ]; then \
		echo "Logging into $(REGISTRY) as $(GHCR_USER) using token from GHCR_TOKEN"; \
		echo "$(GHCR_TOKEN)" | $(CONTAINER_TOOL) login $(REGISTRY) -u "$(GHCR_USER)" --password-stdin; \
	else \
		echo "GHCR_TOKEN not set; attempting push with existing auth"; \
	fi
	@echo "Pushing image: $(DOT_PROJECT_SYNC_IMAGE)"
	@$(CONTAINER_TOOL) push $(DOT_PROJECT_SYNC_IMAGE)
	@echo "Tagging and pushing latest: $(DOT_PROJECT_SYNC_IMAGE_LATEST)"
	@$(CONTAINER_TOOL) tag $(DOT_PROJECT_SYNC_IMAGE) $(DOT_PROJECT_SYNC_IMAGE_LATEST)
	@$(CONTAINER_TOOL) push $(DOT_PROJECT_SYNC_IMAGE_LATEST)

.PHONY: sanitize-image-push-only
sanitize-image-push-only:
	@echo "Ensuring $(CONTAINER_TOOL) is logged in to $(REGISTRY) (uses GHCR_TOKEN if set)"
	@if [ -n "$(GHCR_TOKEN)" ]; then \
		echo "Logging into $(REGISTRY) as $(GHCR_USER) using token from GHCR_TOKEN"; \
		echo "$(GHCR_TOKEN)" | $(CONTAINER_TOOL) login $(REGISTRY) -u "$(GHCR_USER)" --password-stdin; \
	else \
		echo "GHCR_TOKEN not set; attempting push with existing auth"; \
	fi
	@echo "Pushing image: $(SANITIZE_IMAGE)"
	@$(CONTAINER_TOOL) push $(SANITIZE_IMAGE)
	@echo "Tagging and pushing latest: $(SANITIZE_IMAGE_LATEST)"
	@$(CONTAINER_TOOL) tag $(SANITIZE_IMAGE) $(SANITIZE_IMAGE_LATEST)
	@$(CONTAINER_TOOL) push $(SANITIZE_IMAGE_LATEST)

.PHONY: migrate-image-push-only
migrate-image-push-only:
	@echo "Ensuring $(CONTAINER_TOOL) is logged in to $(REGISTRY) (uses GHCR_TOKEN if set)"
	@if [ -n "$(GHCR_TOKEN)" ]; then \
		echo "Logging into $(REGISTRY) as $(GHCR_USER) using token from GHCR_TOKEN"; \
		echo "$(GHCR_TOKEN)" | $(CONTAINER_TOOL) login $(REGISTRY) -u "$(GHCR_USER)" --password-stdin; \
	else \
		echo "GHCR_TOKEN not set; attempting push with existing auth"; \
	fi
	@echo "Pushing image: $(MIGRATE_IMAGE)"
	@$(CONTAINER_TOOL) push $(MIGRATE_IMAGE)
	@echo "Tagging and pushing latest: $(MIGRATE_IMAGE_LATEST)"
	@$(CONTAINER_TOOL) tag $(MIGRATE_IMAGE) $(MIGRATE_IMAGE_LATEST)
	@$(CONTAINER_TOOL) push $(MIGRATE_IMAGE_LATEST)

.PHONY: onboarding-backfill-image-push-only
onboarding-backfill-image-push-only:
	@echo "Ensuring $(CONTAINER_TOOL) is logged in to $(REGISTRY) (uses GHCR_TOKEN if set)"
	@if [ -n "$(GHCR_TOKEN)" ]; then \
		echo "Logging into $(REGISTRY) as $(GHCR_USER) using token from GHCR_TOKEN"; \
		echo "$(GHCR_TOKEN)" | $(CONTAINER_TOOL) login $(REGISTRY) -u "$(GHCR_USER)" --password-stdin; \
	else \
		echo "GHCR_TOKEN not set; attempting push with existing auth"; \
	fi
	@echo "Pushing image: $(ONBOARDING_BACKFILL_IMAGE)"
	@$(CONTAINER_TOOL) push $(ONBOARDING_BACKFILL_IMAGE)
	@echo "Tagging and pushing latest: $(ONBOARDING_BACKFILL_IMAGE_LATEST)"
	@$(CONTAINER_TOOL) tag $(ONBOARDING_BACKFILL_IMAGE) $(ONBOARDING_BACKFILL_IMAGE_LATEST)
	@$(CONTAINER_TOOL) push $(ONBOARDING_BACKFILL_IMAGE_LATEST)

.PHONY: fossa-poller-image-push-only
fossa-poller-image-push-only:
	@echo "Ensuring $(CONTAINER_TOOL) is logged in to $(REGISTRY) (uses GHCR_TOKEN if set)"
	@if [ -n "$(GHCR_TOKEN)" ]; then \
		echo "Logging into $(REGISTRY) as $(GHCR_USER) using token from GHCR_TOKEN"; \
		echo "$(GHCR_TOKEN)" | $(CONTAINER_TOOL) login $(REGISTRY) -u "$(GHCR_USER)" --password-stdin; \
	else \
		echo "GHCR_TOKEN not set; attempting push with existing auth"; \
	fi
	@echo "Pushing image: $(FOSSA_POLLER_IMAGE)"
	@$(CONTAINER_TOOL) push $(FOSSA_POLLER_IMAGE)
	@echo "Tagging and pushing latest: $(FOSSA_POLLER_IMAGE_LATEST)"
	@$(CONTAINER_TOOL) tag $(FOSSA_POLLER_IMAGE) $(FOSSA_POLLER_IMAGE_LATEST)
	@$(CONTAINER_TOOL) push $(FOSSA_POLLER_IMAGE_LATEST)

.PHONY: github-profile-sync-image-push-only
github-profile-sync-image-push-only:
	@echo "Ensuring $(CONTAINER_TOOL) is logged in to $(REGISTRY) (uses GHCR_TOKEN if set)"
	@if [ -n "$(GHCR_TOKEN)" ]; then \
		echo "Logging into $(REGISTRY) as $(GHCR_USER) using token from GHCR_TOKEN"; \
		echo "$(GHCR_TOKEN)" | $(CONTAINER_TOOL) login $(REGISTRY) -u "$(GHCR_USER)" --password-stdin; \
	else \
		echo "GHCR_TOKEN not set; attempting push with existing auth"; \
	fi
	@echo "Pushing image: $(GITHUB_PROFILE_SYNC_IMAGE)"
	@$(CONTAINER_TOOL) push $(GITHUB_PROFILE_SYNC_IMAGE)
	@echo "Tagging and pushing latest: $(GITHUB_PROFILE_SYNC_IMAGE_LATEST)"
	@$(CONTAINER_TOOL) tag $(GITHUB_PROFILE_SYNC_IMAGE) $(GITHUB_PROFILE_SYNC_IMAGE_LATEST)
	@$(CONTAINER_TOOL) push $(GITHUB_PROFILE_SYNC_IMAGE_LATEST)

.PHONY: web-image-push-only
web-image-push-only:
	@echo "Ensuring $(CONTAINER_TOOL) is logged in to $(REGISTRY) (uses GHCR_TOKEN if set)"
	@if [ -n "$(GHCR_TOKEN)" ]; then \
		echo "Logging into $(REGISTRY) as $(GHCR_USER) using token from GHCR_TOKEN"; \
		echo "$(GHCR_TOKEN)" | $(CONTAINER_TOOL) login $(REGISTRY) -u "$(GHCR_USER)" --password-stdin; \
	else \
		echo "GHCR_TOKEN not set; attempting push with existing auth"; \
	fi
	@echo "Pushing image: $(WEB_IMAGE)"
	@$(CONTAINER_TOOL) push $(WEB_IMAGE)
	@echo "Tagging and pushing latest: $(WEB_IMAGE_LATEST)"
	@$(CONTAINER_TOOL) tag $(WEB_IMAGE) $(WEB_IMAGE_LATEST)
	@$(CONTAINER_TOOL) push $(WEB_IMAGE_LATEST)

.PHONY: web-bff-image-push-only
web-bff-image-push-only:
	@echo "Ensuring $(CONTAINER_TOOL) is logged in to $(REGISTRY) (uses GHCR_TOKEN if set)"
	@if [ -n "$(GHCR_TOKEN)" ]; then \
		echo "Logging into $(REGISTRY) as $(GHCR_USER) using token from GHCR_TOKEN"; \
		echo "$(GHCR_TOKEN)" | $(CONTAINER_TOOL) login $(REGISTRY) -u "$(GHCR_USER)" --password-stdin; \
	else \
		echo "GHCR_TOKEN not set; attempting push with existing auth"; \
	fi
	@echo "Pushing image: $(WEB_BFF_IMAGE)"
	@$(CONTAINER_TOOL) push $(WEB_BFF_IMAGE)
	@echo "Tagging and pushing latest: $(WEB_BFF_IMAGE_LATEST)"
	@$(CONTAINER_TOOL) tag $(WEB_BFF_IMAGE) $(WEB_BFF_IMAGE_LATEST)
	@$(CONTAINER_TOOL) push $(WEB_BFF_IMAGE_LATEST)

.PHONY: images-push
images-push: mntrd-image-push-only dot-project-sync-image-push-only sanitize-image-push-only migrate-image-push-only onboarding-backfill-image-push-only fossa-poller-image-push-only github-profile-sync-image-push-only web-image-push-only web-bff-image-push-only
	@echo "All images pushed (no build)."

.PHONY: clean-env
clean-env:
	@rm -f $(ENVOUT)
	@echo "Removed $(ENVOUT)"

.PHONY: githooks-install
githooks-install:
	@git config core.hooksPath .githooks
	@echo "Configured git hooks path to .githooks"

# ---- Cluster helpers ----
.PHONY: ensure-ns
ensure-ns:
	@echo "Ensuring namespace $(NAMESPACE) exists [ctx=$(CTX_STR)]"
	@kubectl $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) get ns $(NAMESPACE) >/dev/null 2>&1 \
		|| kubectl $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) create ns $(NAMESPACE)

.PHONY: apply-ghcr-secret
apply-ghcr-secret:
	@:${GHCR_USER:?Set GHCR_USER (e.g. your GitHub username)}
	@:${GHCR_TOKEN:?Set GHCR_TOKEN (a PAT with package:read)}
	@echo "Applying docker-registry secret 'ghcr-secret' in namespace $(NAMESPACE) [ctx=$(CTX_STR)]"
	@kubectl -n $(NAMESPACE) create secret docker-registry ghcr-secret \
		--docker-server=$(REGISTRY) \
		--docker-username=$(GHCR_USER) \
		--docker-password=$(GHCR_TOKEN) \
		$(if $(KUBECONTEXT),--context $(KUBECONTEXT)) \
		--dry-run=client -o yaml | kubectl -n $(NAMESPACE) apply -f -


# ---- Plain manifests (no Helm/Argo CD) ----
.PHONY: manifests-apply
manifests-apply:
	@echo "Applying manifests in deploy/manifests to namespace $(NAMESPACE) [ctx=$(CTX_STR)]"
	@kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) apply -f deploy/manifests

.PHONY: manifests-delete
manifests-delete:
	@echo "Deleting manifests in deploy/manifests from namespace $(NAMESPACE) [ctx=$(CTX_STR)]"
	@kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) delete -f deploy/manifests --ignore-not-found
.PHONY: cluster-up
cluster-up: ensure-ns apply-ghcr-secret secrets manifests-apply
	@echo "Maintainerd resources applied to $(NAMESPACE) [ctx=$(CTX_STR)]"

.PHONY: cluster-down
cluster-down: manifests-delete
	@echo "Maintainerd manifests removed from $(NAMESPACE) [ctx=$(CTX_STR)]"



.PHONY: maintainerd-delete
maintainerd-delete:
	@echo "Deleting Deployment/Service 'maintainerd' from $(NAMESPACE) [ctx=$(CTX_STR)]"
	@kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) delete deploy/maintainerd svc/maintainerd --ignore-not-found

.PHONY: maintainerd-restart
maintainerd-restart:
	@echo "Rolling out restart for Deployment/maintainerd [ctx=$(CTX_STR)]"
	@kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) rollout restart deploy/maintainerd

.PHONY: maintainerd-drain
maintainerd-drain:
	@echo "Scaling Deployment/maintainerd to 0 replicas [ctx=$(CTX_STR)]"
	@kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) scale deploy/maintainerd --replicas=0
	@echo "Waiting for maintainerd pods to terminate [ctx=$(CTX_STR)]"
	@kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) wait --for=delete pod -l app=maintainerd --timeout=120s 2>/dev/null || \
		echo "No maintainerd pods left to delete"

.PHONY: maintainerd-port-forward
maintainerd-port-forward:
	@echo "Port-forwarding localhost:2525 -> service/maintainerd:2525 [ctx=$(CTX_STR)]"
	@kubectl -n $(NAMESPACE) $(if $(KUBECONTEXT),--context $(KUBECONTEXT)) port-forward svc/maintainerd 2525:2525

# ---- Testing and CI ----
.PHONY: test
test:
	@echo "Running tests..."
	@go test ./...

.PHONY: test-verbose
test-verbose:
	@echo "Running tests with verbose output..."
	@go test -v ./...

.PHONY: test-coverage
test-coverage:
	@echo "Running tests with coverage..."
	@go test -coverprofile=coverage.out -covermode=atomic ./...
	@go tool cover -func=coverage.out | tail -n 1
	@echo "To view HTML coverage report: go tool cover -html=coverage.out"

.PHONY: test-race
test-race:
	@echo "Running tests with race detector..."
	@go test -race ./...

.PHONY: test-package
test-package:
	@if [ -z "$(PKG)" ]; then \
		echo "Usage: make test-package PKG=<package>"; \
		echo "Example: make test-package PKG=onboarding"; \
		exit 1; \
	fi
	@echo "Running tests for package: $(PKG)"
	@go test -v ./$(PKG)/...

.PHONY: ci-local
ci-local:
	@echo "Running local CI checks..."
	@mkdir -p $(GOCACHE_DIR) /tmp/golangci-lint
	@echo "→ Tool versions..."
	@bash -c 'set -euo pipefail; \
	echo "go: $$(go version)"; \
	if command -v staticcheck >/dev/null 2>&1; then echo "staticcheck: $$(staticcheck -version)"; else echo "staticcheck: not installed"; fi; \
	if command -v golangci-lint >/dev/null 2>&1; then echo "golangci-lint: $$(golangci-lint --version | head -n 1)"; else echo "golangci-lint: not installed"; fi; \
	if command -v node >/dev/null 2>&1; then echo "node: $$(node --version)"; else echo "node: not installed"; fi; \
	if command -v npm >/dev/null 2>&1; then echo "npm: $$(npm --version)"; else echo "npm: not installed"; fi; \
	if command -v npx >/dev/null 2>&1; then \
		( cd web && npx eslint --version 2>/dev/null | awk '\''{print "eslint: " $$1}'\'' ) || echo "eslint: not installed"; \
		( cd web && npx tsc --version 2>/dev/null | awk '\''{print "tsc: " $$2}'\'' ) || echo "tsc: not installed"; \
		( cd web && npx cucumber-js --version 2>/dev/null | awk '\''{print "cucumber-js: " $$1}'\'' ) || echo "cucumber-js: not installed"; \
	else \
		echo "npx: not installed"; \
	fi; \
	'
	@echo "→ Verifying dependencies..."
	@go mod verify
	@echo "→ Running go fmt..."
	@GOFILES="$$(find . -path './.modcache' -prune -o -path './.gocache' -prune -o -path './.git' -prune -o -name '*.go' -print)"; \
	if [ "$$(gofmt -s -l $$GOFILES | wc -l)" -gt 0 ]; then \
		echo "❌ Code needs formatting. Run: gofmt -w $$(echo $$GOFILES)"; \
		gofmt -s -l $$GOFILES; \
		exit 1; \
	fi
	@rm -rf web/work/testdata/next-dist web/testdata/next-dist web/tmp || true
	@bash -c 'set -euo pipefail; \
	if [ -z "$${MD_DB_DSN:-}" ]; then \
		echo "MD_DB_DSN is required for ci-local web BDD (Postgres only)."; \
		echo "Example: MD_DB_DSN=\"host=localhost port=5432 user=maintainerd password=maintainerd dbname=maintainerd_test sslmode=disable\""; \
		exit 1; \
	fi; \
	command -v staticcheck >/dev/null 2>&1 || { echo "staticcheck not installed. Run: go install honnef.co/go/tools/cmd/staticcheck@latest"; exit 1; }; \
	command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not installed. Run: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest"; exit 1; }; \
	NPROC=$$(nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo 4); \
	export GOCACHE=$(GOCACHE_DIR); \
	export GOLANGCI_LINT_CACHE=/tmp/golangci-lint; \
	GO_TEST_PACKAGES="$$(go list ./...)"; \
	echo "→ Go test packages: $$(printf "%s\n" "$$GO_TEST_PACKAGES" | wc -l)"; \
	echo "→ Running all checks sequentially..."; \
	go vet ./...; \
	echo "✓ go vet"; \
	npm --prefix web run lint -- --max-warnings=$(ESLINT_MAX_WARNINGS); \
	echo "✓ web lint"; \
	npm --prefix web run typecheck; \
	echo "✓ web typecheck"; \
	staticcheck ./...; \
	echo "✓ staticcheck"; \
	golangci-lint run ./...; \
	echo "✓ golangci-lint"; \
	go test -count=1 -race -p "$$NPROC" -coverprofile=coverage.out -covermode=atomic $$GO_TEST_PACKAGES; \
	echo "✓ go test -race"; \
	MD_DB_DRIVER=postgres MD_DB_DSN="$$MD_DB_DSN" WEB_BDD_USE_MICROCKS=true NO_BDD_OPEN=1 $(MAKE) web-bdd; \
	echo "✓ web BDD"; \
	echo "→ Coverage report:"; \
	go tool cover -func=coverage.out | tail -n 1; \
	echo "✅ All CI checks passed!"; \
	'

.PHONY: lint
lint:
	@echo "Running golangci-lint..."
	@mkdir -p $(GOCACHE_DIR) /tmp/golangci-lint
	@if command -v golangci-lint >/dev/null 2>&1; then \
		GOLANGCI_LINT_CACHE=/tmp/golangci-lint GOCACHE=$(GOCACHE_DIR) golangci-lint run ./...; \
	else \
		echo "golangci-lint not installed."; \
		echo "Install: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest"; \
		exit 1; \
	fi

.PHONY: fmt
fmt:
	@echo "Formatting code..."
	@go fmt ./...

.PHONY: vet
vet:
	@echo "Running go vet..."
	@go vet ./...
