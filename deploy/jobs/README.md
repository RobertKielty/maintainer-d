# One-shot Jobs

Each file here is a Kubernetes `Job` (sometimes with a `ConfigMap`) that runs once, by hand, for a specific task such as a schema migration or a backfill.

**Never apply this directory wholesale.** Jobs carry `ttlSecondsAfterFinished`, so once a finished Job is cleaned up, re-applying the file runs it again. That is why these files live outside `deploy/manifests/`, which `make manifests-apply` (and any future GitOps reconciler) applies in full.

Run one Job:

```bash
kubectl -n maintainerd delete job <job-name> --ignore-not-found   # Job specs are immutable
kubectl -n maintainerd apply -f deploy/jobs/<file>.yaml
kubectl -n maintainerd logs -f job/<job-name>
```

Makefile shortcuts: `make migrate-schema`, `make migrate-schema-safe`, `make onboarding-backfill-job`, `make github-profile-sync-run`.
