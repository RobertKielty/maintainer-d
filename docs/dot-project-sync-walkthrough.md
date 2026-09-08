# `cmd/dot-project-sync` Code Walkthrough

This document describes how `cmd/dot-project-sync/main.go` drives `.project`
repository discovery, LFX Platform identity enrichment, maintainer auto-add
candidate generation, audit logging, and run-to-run persistence.

The command is a batch job. It scans all eligible projects, discovers files in
each project's GitHub `.project` repository, persists the discovery state, then
optionally enriches and auto-adds maintainers.

## Entry Point

`main()` performs the top-level setup:

1. Parse CLI flags with `parseFlags()`.
2. Build a timeout-bound context.
3. Validate required runtime config.
4. Open the Postgres-backed store.
5. Build GitHub and required LFX clients.
6. Load the CNCF foundation maintainer CSV if enabled.
7. Construct `dotproject.Syncer`.
8. Run `syncer.SyncAll(ctx)`.
9. Collect DB/cache metrics.
10. Write one audit event for the whole run.
11. Emit a final summary log line.

The command is intentionally Postgres-only:

```go
dbDriver := envOr("MD_DB_DRIVER", "postgres")
if dbDriver != "postgres" {
    log.Fatalf("dot-project-sync requires MD_DB_DRIVER=postgres, got %q", dbDriver)
}
```

Required inputs are:

- `MD_DB_DSN`
- `GITHUB_API_TOKEN`

Required LFX input:

- `LFX_AUTH_TOKEN`

Optional LFX inputs are:

- `LFX_ACL`
- `LFX_BASE_URL`
- `LFX_TIMEOUT`
- `LFX_REQUEST_DELAY` (default `250ms`, serialized client-side throttle)
- `LFX_MAX_LOOKUPS`
- `LFX_ENRICH_ALL_MAINTAINERS`
- `LFX_USERNAME`
- `LFX_EMAIL`
- `DOT_PROJECT_SYNC_ACTOR` (operator label included in audit metadata)

## Flags

`parseFlags()` controls maintainer auto-add behavior and the foundation CSV
source:

- `-check-foundation-csv`, default `true`
- `-auto-add-maintainers`, default `false`
- `-foundation-csv-owner`, default `cncf`
- `-foundation-csv-repo`, default `foundation`
- `-foundation-csv-ref`, default `main`
- `-foundation-csv-path`, default `project-maintainers.csv`

With the defaults, the command runs in dry-run mode for auto-add. It will report
would-create and would-link candidates but will not write maintainer/project
links.

## Foundation CSV Loading

`loadFoundationMaintainers()` downloads the configured foundation CSV from
GitHub at a pinned commit SHA:

1. Resolve the configured branch/ref to a commit SHA.
2. Fetch the CSV file at that commit.
3. Parse it with `dotproject.ParseFoundationMaintainersCSV`.
4. Store `CommitSHA` and `SourceURL` on the parsed index.

If the CSV load fails, `main()` logs the failure and continues:

```go
foundationIndex, foundationErr := loadFoundationMaintainers(ctx, client, cfg)
if foundationErr != nil {
    log.Printf("foundation csv load failed: %v", foundationErr)
}
```

The command still builds the auto-adder. If `-check-foundation-csv=true` and the
foundation index is nil, individual auto-add processing will return an error and
increment auto-add error counts, but the overall sync run still continues across
projects.

## Syncer Construction

`main()` builds this `dotproject.Syncer`:

```go
syncer := &dotproject.Syncer{
    Store: store,
    Discoverer: &dotproject.Discoverer{
        Client: &dotproject.GitHubRepositoryClient{Client: client},
    },
    AutoAdder: buildAutoAdder(store, cfg, foundationIndex, buildLFXIdentityResolver()),
    Enricher:  buildLFXEnricher(store),
}
```

There are three main collaborators:

- `Discoverer`: reads each GitHub `.project` repository.
- `Enricher`: writes LFX identity observations.
- `AutoAdder`: creates or reports maintainer add/link candidates.

LFX enrichment is compulsory. `buildRequiredLFXClient()` requires
`LFX_AUTH_TOKEN`; if it is missing, the command exits before scanning projects
and instructs the operator to refresh the token at
`https://app.lfx.dev/settings`.

## Project Loop

`dotproject.Syncer.SyncAll()` loads all projects and processes them one by one.

For each project:

1. Skip projects that should not sync, including archived/excluded projects.
2. Discover `.project` repository files.
3. Persist discovery metadata and cached file content.
4. Run LFX enrichment.
5. Run auto-add candidate processing.
6. Aggregate counters into one run summary.

Project-level errors do not abort the whole run. They increment
`summary.Errored`, record at most 10 error summary strings, and move on to the
next project.

## Discovery

`dotproject.Discoverer.Discover()` uses the GitHub API to inspect
`<project GitHub org>/.project`.

It discovers:

- repository existence
- default branch
- `project.yaml`
- `maintainers.yaml`
- `SECURITY.md`
- `CONTRIBUTING.md`
- `GOVERNANCE.md`

For fetched files it records:

- GitHub blob URL
- raw URL
- ETag
- SHA-256 body hash
- body content

Not-found behavior is intentionally non-fatal for expected absence:

- Missing `.project` repo returns a result with `RepoExists=false`.
- Missing optional files return empty `FileDiscovery`.
- Other GitHub/API failures are returned as project errors.

The adoption status is then derived from the discovery result:

- `not_found`
- `repo_only`
- `partial`
- `adopted`
- `error`

## Run-To-Run Cache and Persistent State

There are two separate persistence concepts:

1. `.project` discovery state in `dot_project_sync_states`
2. identity lookup history in `maintainer_identity_observations`

### `.project` Discovery State

`dotproject.buildSyncState()` creates a `model.DotProjectSyncState` for each
project. `db.SQLStore.PersistDotProjectSync()` saves it in the same transaction
as the project metadata update.

The state table stores:

- whether the `.project` repo exists
- whether key files exist
- default branch
- maintainer filename
- schema version
- importer version
- ETags
- body hashes
- cached `maintainers.yaml` body
- last checked timestamp
- sync/parse errors

The project row itself is also updated with public-facing `.project` metadata:

- repo URL
- file URLs
- schema version
- maintainer count
- last synced timestamp
- adoption status

Important: this state is persisted between runs, but the current discovery path
still fetches GitHub data each run. The state currently acts as a durable record
and reporting/cache table, not as a conditional-request short-circuit. The final
audit metrics report how much content is cached.

`collectPostSyncMetrics()` summarizes this table after the run:

- database size
- `dot_project_sync_states` relation size
- number of cached maintainer file bodies
- total/average/max cached maintainer body bytes
- project coverage counts

### Identity Observation History

`maintainer_identity_observations` stores source-specific identity evidence from
foundation CSV and LFX Platform lookups.

The model stores:

- optional `maintainer_id`
- optional `project_id`
- source, such as `foundation-csv` or `lfx`
- source ref, such as `github:<handle>` or `maintainer-d:<id>`
- source user ID
- name
- email
- GitHub user
- LFID
- company name/ref
- match status
- match reason
- confidence
- raw JSON payload
- observed timestamp

`UpsertMaintainerIdentityObservation()` updates an existing observation rather
than creating duplicates when the source, maintainer/project identity, and source
user/ref match.

This table is used between runs in two important ways:

- `LFX_ENRICH_ALL_MAINTAINERS=true` uses `ListMaintainersWithoutIdentityObservation("lfx")` so global enrichment skips maintainers that already have an LFX observation.
- Auto-add summaries use latest LFX observations to enrich audit rows for would-create and would-link candidates.

For would-create rows where there is no maintainer yet, the lookup is by:

```text
source = 'lfx'
project_id = <project id>
source_ref = 'github:<normalized handle>'
```

For existing maintainers, the lookup is by:

```text
source = 'lfx'
maintainer_id = <maintainer id>
```

That is how previously observed LFX data can appear in a later audit event even
when auto-add is running in dry-run mode.

## LFX Enrichment

`buildLFXEnricher()` builds `lfx.Enricher` with:

- store
- LFX API client
- `EnrichAll`
- `MaxLookups`

Default lookup limits:

- normal mode: 100 lookups per project (the counter resets for each project's `EnrichProject` call, so a full run can issue up to 100 × the number of synced projects)
- `LFX_ENRICH_ALL_MAINTAINERS=true`: unlimited unless `LFX_MAX_LOOKUPS` is set

Default request pacing:

- `LFX_REQUEST_DELAY=250ms`, meaning at most roughly 4 LFX API requests per second from one job.
- Increase this value, for example `1s`, if LFX starts returning transient failures or rate-limit-like responses.

`lfx.Enricher.EnrichProject()` builds candidates from two sources:

1. Current project's `.project` `project-maintainers` handles, unless
   `EnrichAll` is enabled.
2. All active maintainers without an LFX observation, if `EnrichAll` is enabled.

For each candidate, `enrichCandidate()`:

1. Normalizes GitHub and email inputs.
2. If both are missing, writes an unmatched observation.
3. Searches LFX by GitHub handle, then by email.
4. Writes one observation with status:
   - `matched`
   - `unmatched`
   - `ambiguous`
   - `error`

LFX matched observations include platform fields such as:

- source user ID
- LFID
- name
- email
- company name, when present in the LFX account payload
- raw payload

## Auto-Add LFX Resolver

Auto-add has its own narrower LFX lookup path through `lfxIdentityResolver`.
This resolver is used when considering a new maintainer candidate. It searches
for a single LFX user by GitHub handle, falls back to email when available, and
returns `dotproject.LFXIdentityResult`.

Key behavior:

- Zero or multiple users is not treated as an error. It returns
  `Confidence: "unmatched"` with a reason.
- `SearchUsers` API failures are returned as errors.
`SearchUsers` and `GetUserIdentities` API failures are fatal LFX Platform access
errors. The returned error includes the OpenProfile developer settings URL so
operators know where to refresh the short-lived token.

## Where LFX Errors Stop the Job

The LFX Platform is required for this job. Platform access errors stop the sync
instead of being reported only as counters.

### 1. Missing Token Fails Startup

`buildRequiredLFXClient()` requires `LFX_AUTH_TOKEN`:

```go
if token == "" {
    return nil, fmt.Errorf("%s", lfxTokenHelp)
}
```

The error message tells the operator to update the token at
`https://app.lfx.dev/settings`.

### 2. Enricher Errors Are Fatal

In `dotproject.Syncer.syncProject()`:

```go
if s.Enricher != nil {
    enrichment, err = s.Enricher.EnrichProject(ctx, project, result)
    if err != nil {
        if enrichment.Errored == 0 {
            enrichment.Errored++
        }
        return status, enrichment, AutoAddSummary{}, FatalSyncError{Err: err}
    }
}
```

`SyncAll()` returns immediately when it sees `FatalSyncError`, so later projects
are not processed.

### 3. Per-Candidate LFX Enrichment Errors Are Written and Returned

In `lfx.Enricher.enrichCandidate()`:

```go
users, err := e.searchUsers(ctx, githubUser, email)
if err != nil {
    if writeErr := e.writeObservation(projectID, candidate, nil, nil, now, "error", err.Error(), ""); writeErr != nil {
        return fmt.Errorf("%w; failed to record LFX error observation: %v", PlatformAccessError(err), writeErr)
    }
    return PlatformAccessError(err)
}
```

Search failures become `maintainer_identity_observations` rows with
`MatchStatus="error"` when possible, and the wrapped platform access error then
stops the job.

### 4. LFX Identity Fetch Errors Are Fatal

Both LFX paths return errors from `GetUserIdentities()`.

```go
identities, err := e.Client.GetUserIdentities(ctx, user.ID)
if err != nil {
    ...
    return PlatformAccessError(err)
}
```

This is important because short-lived or expired LFX access tokens can fail on
either search or identity endpoints.

### 5. Auto-Add Resolver Errors Are Fatal

In `AutoMaintainerAdder.ProcessProject()`, resolver errors increment
`summary.LFXErrored` and are returned:

```go
resolved, err := a.LFX.ResolveMaintainerIdentity(ctx, normalized, "")
if err != nil {
    summary.LFXErrored++
    return summary, err
} else {
    identity = resolved
    ...
}
```

`Syncer` wraps this as `FatalSyncError`, causing the overall job to stop.

### 6. Audit Logging Failures Are Counted or Logged, Not Fatal

Per-candidate observation/audit failures increment `AuditFailures`. The final
run audit event failure is logged:

```go
if err := store.LogAuditEvent(logger, buildAuditEvent(summary, metrics, metricsErr, cfg)); err != nil {
    log.Printf("dot-project sync audit log failed: %v", err)
}
```

The command still exits successfully if the sync itself completed.

## Auto-Add Flow

`buildAutoAdder()` wires:

- DB store
- foundation CSV index
- LFX resolver
- `CheckFoundationCSV`
- `AutoAddMaintainers`

`AutoMaintainerAdder.ProcessProject()` only considers handles in the
`project-maintainers` team from `.project` `maintainers.yaml`.

For each handle:

1. Normalize and skip placeholders.
2. Check whether the maintainer already exists by GitHub handle.
3. If foundation CSV gating is enabled, require a matching foundation CSV row.
4. Write a foundation CSV identity observation.
5. If the maintainer exists but is not linked to the project:
   - report would-link in dry-run mode, or
   - link the maintainer in write mode.
6. If the maintainer does not exist:
   - optionally resolve LFX identity
   - report would-create in dry-run mode, or
   - create and link in write mode.

Dry-run audit rows include:

- project ID/name
- GitHub handle
- LFX ID when available
- name
- company
- email

The summary first uses immediate LFX resolver data, then overlays any saved LFX
observation for the same project and GitHub handle. That gives the final audit
event the best known LFX Platform data for dry-run candidates.

## Final Audit Event

`buildAuditEvent()` writes one `DOT_PROJECT_SYNC_RUN` audit log row. Its metadata
contains:

- discovery totals
- adoption status counts
- LFX enrichment counters
- auto-add counters
- auto-add runtime flag state
- foundation CSV source configuration
- would-create/would-link handle tables
- truncated project error summaries
- DB/cache metrics
- DB metrics error, if metric collection failed

This audit event is the data source behind the local UI audit popup.

## Operational Implications

- A failure in GitHub discovery for one project does not stop the whole run.
- A failure in LFX enrichment stops the job.
- LFX search failures are preserved as `error` observations when possible.
- LFX identity-detail failures stop the job.
- `.project` file metadata and maintainer file bodies are persisted between
  runs in `dot_project_sync_states`.
- LFX/foundation identity evidence is persisted between runs in
  `maintainer_identity_observations`.
- Current GitHub discovery records ETags and body hashes, but does not currently
  use the saved ETags to avoid GitHub fetches.
- Global LFX enrichment uses saved observations as a cache boundary so maintainers
  with an existing LFX observation are skipped.
