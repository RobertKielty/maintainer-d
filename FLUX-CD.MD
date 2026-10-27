# Flux CD Plan (maintainerd namespace, same repo)

> Updated 2026-08-25. The original plan below (goals, layout, apply order, notes) predates the current manifest set and was never implemented — no `deploy/flux/`, `deploy/kustomize/`, or `flux-system` namespace exist yet. The **Revised plan** section reflects the actual current manifest inventory and adds the Job-exclusion approach needed before this can be turned on.

## Goals
- Manage deployments for the `maintainerd` namespace with Flux.
- Keep everything in this repo.
- Start with Kustomize (no Helm yet).

## Proposed repo layout
```
FLUX-CD.MD
/deploy
  /manifests
    ... (existing YAMLs)
  /flux
    gotk-components.yaml            # flux system components (optional if already installed)
    gotk-sync.yaml                  # GitRepository + Kustomization for this repo
  /kustomize
    /base
      kustomization.yaml            # points to ../manifests
    /overlays
      /prod
        kustomization.yaml          # namespace, patches, image tags (optional)
        namespace.yaml              # maintainerd namespace if not already managed
```

## Revised plan (2026-08-25)

> **Phase 0 cleanup (2026-10-27, see #148):** one-shot Jobs moved out of `deploy/manifests/` into `deploy/jobs/`, so `deploy/manifests/` now contains only resources that are safe to reconcile continuously and the base Kustomization can point at the whole directory. The worksheet `bootstrap` Job (and its code) and the `maintainer-sanitize` CronJob were removed. The two `ingress-nginx*.yaml` files, which were captures of live Pod objects rather than manifests, were deleted.

The manifest set has grown significantly since this doc was first drafted — `cronjob-fossa-poller.yaml` no longer exists (fossa-poller is now a `Deployment`, not a `CronJob`), and several Jobs, CronJobs, ingress/cert-manager resources, and RBAC were added. The revised base below reflects `deploy/manifests/` as it exists today, split into what Flux should continuously reconcile vs. what stays manual.

`deploy/manifests/cronjob.yaml` (`CronJob/maintainer-sync`) was removed 2026-08-26 along with the dead `maintainer-d.cncf.io` CRD machinery it populated (kcp-experiment leftover, never read by anything). `deploy/manifests/sync-rbac.yaml` now defines only `ServiceAccount/maintainer-sync`, kept because `Deployment/maintainerd-fossa-poller` and the one-shot Jobs in `deploy/jobs/` still mount it.

### Base Kustomize (deploy/kustomize/base/kustomization.yaml)

Long-running resources, safe for continuous GitOps reconciliation:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  # Deployments
  - ../../manifests/deployment.yaml
  - ../../manifests/web-bff-deployment.yaml
  - ../../manifests/web-deployment.yaml
  - ../../manifests/maintainerd-fossa-poller-deployment.yaml
  # Services
  - ../../manifests/service.yaml
  - ../../manifests/web-bff-service.yaml
  - ../../manifests/web-service.yaml
  # Ingress + cert-manager (app-level resources, not the ingress-nginx controller itself)
  - ../../manifests/ingress-maintainerd-web.yaml
  - ../../manifests/ingress-github-events.yaml
  - ../../manifests/certificate-github-events.yaml
  - ../../manifests/clusterissuer-letsencrypt-http.yaml
  # RBAC (ServiceAccount only — maintainer-sync CronJob it was built for is gone;
  # SA retained because maintainerd-fossa-poller and deploy/jobs/ mount it)
  - ../../manifests/sync-rbac.yaml
  # Recurring CronJobs (self-healing scheduled work, safe to reconcile)
  - ../../manifests/dot-project-sync-cronjob.yaml
  - ../../manifests/github-profile-sync-cronjob.yaml
```

### Excluded from Flux entirely

**ingress-nginx** — third-party cluster infra, installed with `kubectl apply` of the upstream v1.13.2 static manifest (see `OPS.MD`). The former `ingress-nginx*.yaml` files in this repo were Pod captures and have been deleted. If the controller is ever brought under GitOps it should be a separate Flux `Kustomization`/`HelmRelease` dedicated to cluster infra.

### Job-exclusion approach

A Kubernetes `Job` spec is immutable once created and designed to run once to completion. Flux's `Kustomization` controller assumes continuous reconciliation: with `prune: true` it deletes anything it manages that drops out of the diff, and on every reconcile it re-applies what's still there. For a one-shot Job that means one of two bad outcomes:

- if the Job still exists when Flux reconciles, `kubectl apply` on an immutable Job spec **errors** (fields like `spec.template` can't change on an existing Job) — reported as an unhealthy `Kustomization` even though nothing's actually wrong; or
- if the Job is deleted after completion (normal cleanup), Flux's next reconcile just **recreates and re-runs it** — silently re-running a migration/backfill/add-staff job on every reconcile interval, which is actively dangerous for anything not idempotent.

So the following stay **outside** the Flux base, in `deploy/jobs/`, and keep being triggered by hand:

- `github-profile-sync-job.yaml`
- `maintainerd-add-staff-job.yaml`
- `maintainerd-migrate-schema-job.yaml`
- `maintainerd-onboarding-backfill-job.yaml`
- `maintainerd-remote-migration-job.yaml` (ConfigMap + Job)
- `maintainerd-invite-team-assignment-job.yaml` (ConfigMap + Job)

```bash
kubectl -n maintainerd --context context-cdv2c4jfn5q apply -f deploy/jobs/maintainerd-migrate-schema-job.yaml
kubectl -n maintainerd --context context-cdv2c4jfn5q delete job maintainerd-migrate-schema  # cleanup before next run
```

If Flux-managed one-shot Jobs are wanted later, the supported patterns are a `Kustomization` with `spec.wait: true` plus deliberate delete/recreate semantics, or moving the Job to a `HelmRelease` post-install hook. Both are bigger changes than the Deployment/CronJob move and should be a separate follow-up, not part of the first cut.

### Net effect of the first cut

- Flux would own: everything in `deploy/manifests/`, which is 4 Deployments, 3 Services, ingress + cert-manager app resources, RBAC and 2 recurring CronJobs.
- Manual `kubectl apply` keeps owning: the 6 one-shot Jobs in `deploy/jobs/`.

### Open decision: image tag strategy

Current manual flow generates a fresh `TAG` per build (branch+short-sha+date) and pushes/deploys `:latest`, decided at deploy time. Flux's GitOps model wants the desired image tag to live in git, so this needs to be resolved before (or alongside) turning Flux on:

- **Manual tag bump**: after building/pushing an image, edit the prod overlay's `images:` block with the new tag and commit — Flux applies the diff. Simple, no new controllers, but reintroduces a manual git-edit step into the deploy flow.
- **Flux Image Automation** (`ImageRepository` + `ImagePolicy` + `ImageUpdateAutomation`): Flux watches the registry and writes tag bumps back to git itself. Removes the manual step but adds three new CRDs/controllers and a bot-commit pattern to reason about.

Recommendation: ship the first cut with manual tag bumps in the prod overlay, confirm Flux reconciles cleanly end-to-end, then revisit Image Automation once that's proven — bolting automation on afterward is straightforward; building it into the base before Flux itself is trusted is not.

## Base Kustomize (deploy/kustomize/base/kustomization.yaml) — original draft, superseded above
```
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../../manifests/deployment.yaml
  - ../../manifests/service.yaml
  - ../../manifests/web-bff-deployment.yaml
  - ../../manifests/web-bff-service.yaml
  - ../../manifests/web-deployment.yaml
  - ../../manifests/web-service.yaml
  - ../../manifests/cronjob-fossa-poller.yaml
  - ../../manifests/cronjob-sanitize.yaml
  - ../../manifests/cronjob.yaml
  - ../../manifests/maintainerd-invite-team-assignment-job.yaml
  # add/remove manifests as needed
```

## Prod overlay (deploy/kustomize/overlays/prod/kustomization.yaml)
```
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: maintainerd
resources:
  - ../../base
# Optional patches or image overrides below
# images:
#   - name: ghcr.io/robertkielty/maintainerd
#     newTag: <tag>
```

## Flux CRDs to apply

### 1) GitRepository (flux-system namespace)
```
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: maintainerd
  namespace: flux-system
spec:
  interval: 1m
  url: https://github.com/cncf/maintainer-d
  ref:
    branch: main
  # optional: ignore paths
  # ignore: |
  #   /testdata/
```

### 2) Kustomization (flux-system namespace)
```
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: maintainerd
  namespace: flux-system
spec:
  interval: 5m
  path: ./deploy/kustomize/overlays/prod
  prune: true
  sourceRef:
    kind: GitRepository
    name: maintainerd
  targetNamespace: maintainerd
  wait: true
  timeout: 5m
```

## Optional: Image automation (later)
If you want Flux to update image tags automatically, we can add:
- ImageRepository
- ImagePolicy
- ImageUpdateAutomation

## Apply order
1. Install Flux components (if not already):
   - `deploy/flux/gotk-components.yaml`
2. Apply `GitRepository` and `Kustomization` in `deploy/flux/gotk-sync.yaml`.
3. Commit these files to the repo so Flux can reconcile.

## Notes
- Since you only want `maintainerd`, the `targetNamespace` is set.
- If you already manage namespaces outside Flux, remove `namespace.yaml` from overlays.
- This plan avoids Helm for now, but can evolve later.
