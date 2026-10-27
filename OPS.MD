# Operations Guide (Goal-Oriented)

This guide is organized by outcomes. Use it as a checklist when operating maintainer-d in production or development.

## Deploy the maintainerd server

Goal: Deploy the API server and supporting resources.

1) Apply namespace, registry secret, and SOPS secrets:
```
make ensure-ns
make apply-ghcr-secret
make sops-apply-web-secrets
```

2) Apply Kubernetes manifests:
```
make manifests-apply
```
`deploy/manifests/` contains only long-lived resources, so re-applying it is safe. One-shot Jobs live in `deploy/jobs/` and are never applied by `manifests-apply` (see "Run a one-shot job" below).

3) Verify:
```
kubectl -n maintainerd get pvc,deploy,svc,ing
```

## Run schema migrations

Goal: Apply database schema changes safely without re-importing data.

1) Build and push the migrate image:
```
make migrate-image-build
make migrate-image-push
```

2) Apply the migrate job manifest:
```
kubectl -n maintainerd apply -f deploy/jobs/maintainerd-migrate-schema-job.yaml
```

3) Wait for completion:
```
kubectl -n maintainerd wait --for=condition=complete job/maintainerd-migrate-schema --timeout=5m
```

4) Check logs:
```
kubectl -n maintainerd logs job/maintainerd-migrate-schema
```

5) Cleanup (optional):
```
kubectl -n maintainerd delete job/maintainerd-migrate-schema
```

## Run a one-shot job

Goal: Run a migration, backfill or other one-off task without touching the long-lived resources.

One-shot Jobs live in `deploy/jobs/`. Apply exactly one file, on purpose; never apply the directory wholesale. A Job's spec is immutable, so delete any previous run first:
```
kubectl -n maintainerd delete job <job-name> --ignore-not-found
kubectl -n maintainerd apply -f deploy/jobs/<file>.yaml
kubectl -n maintainerd logs -f job/<job-name>
```

Makefile shortcuts exist for some of them: `make migrate-schema`, `make onboarding-backfill-job`, `make github-profile-sync-run`.

## Ingress controller

ingress-nginx runs in the `ingress-nginx` namespace as `Deployment/ingress-nginx-controller` (v1.13.2). It was installed with `kubectl apply` of the upstream static deploy manifest, not Helm, and is not managed from this repo.

## Take a copy of the data in the database

Goal: Export a snapshot of the production Postgres database.

Run a one-off `pg_dump` from a pod:
```
PGPASSWORD="<password>" \
kubectl -n maintainerd run pgdump --rm -i --restart=Never \
  --image=postgres:16-alpine \
  --env="PGPASSWORD=$PGPASSWORD" -- \
  pg_dump "host=<db-host> port=5432 user=<db-user> dbname=maintainerd sslmode=require" \
  --format=custom --no-owner --no-privileges > maintainerd-prod.dump
```

## Deploy the web app for staff

Goal: Deploy web + BFF to production.

1) Build/push images:
```
make web-image-build
make web-image-push
make web-bff-image-build
make web-bff-image-push
```

2) Apply manifests:
```
make deploy-web
```

3) (Optional) pin the web image to a tag:
```
make web-image-set TAG=<tag>
```

## Manage secrets (dev vs production)

### Dev secrets (local .envrc)
- `.envrc` is dev-only.
- Generate and apply the dev env secret:
```
make env
make apply-env
```

### Production secrets (SOPS)
We store encrypted secrets in `deploy/secrets/`.

1) Create plaintext secrets (once) and encrypt:
```
mkdir -p deploy/secrets
cp deploy/templates/maintainerd-web-env.yaml.tmpl deploy/secrets/maintainerd-web-env.yaml
cp deploy/templates/maintainerd-web-bff-env.yaml.tmpl deploy/secrets/maintainerd-web-bff-env.yaml
cp deploy/templates/maintainerd-db-env.yaml.tmpl deploy/secrets/maintainerd-db-env.yaml
cp deploy/templates/maintainerd-bootstrap-env.yaml.tmpl deploy/secrets/maintainerd-bootstrap-env.yaml
# fill in values, then:
make sops-encrypt-web-secrets SOPS_AGE_KEY=age1...
make sops-encrypt-bootstrap-secrets SOPS_AGE_KEY=age1...
```

2) Apply encrypted secrets:
```
make sops-apply-web-secrets SOPS_AGE_KEY_FILE=~/.config/sops/age/keys.txt
make sops-apply-bootstrap-secrets SOPS_AGE_KEY_FILE=~/.config/sops/age/keys.txt
```

## License
[![FOSSA Status](https://app.fossa.com/api/projects/git%2Bgithub.com%2FRobertKielty%2Fmaintainerd.svg?type=large)](https://app.fossa.com/projects/git%2Bgithub.com%2FRobertKielty%2Fmaintainerd?ref=badge_large)
