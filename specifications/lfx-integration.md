# LFX Maintainer Enrichment Plan

## Goal

Use LFX Platform as the preferred source for enriching `maintainer-d` maintainer records with externally verified identity data, while preserving source provenance and allowing other sources to contribute comparable evidence.

This plan follows the direction in [cncf/maintainer-d#119](https://github.com/cncf/maintainer-d/issues/119): move maintainer records away from manual entry and toward GitOps-driven discovery from `.project/maintainers.yaml`, supplemented by LFX/PCC, OpenProfile, GitHub, maintainers.cncf.io, and CNCF GitDM data.

## Current Context

`cmd/dot-project-sync` already scans known projects, discovers each project's `.project` repository, and caches `maintainers.yaml` content in `dot_project_sync_states`.

`model.Maintainer` currently stores the canonical view used by the application:

- `Name`
- `Email`
- `GitHubAccount`
- `GitHubEmail`
- `MaintainerStatus`
- `CompanyID`
- `Location`, `Country`, `Timezone`
- project associations through `MaintainerProject`

The current `Service`, `RemoteUser`, `RemoteTeam`, `RemoteTeamUser`, and `ServiceInvitation` models are operational integration models. They fit services where `maintainer-d` manages remote teams, invitations, and access state, such as FOSSA. LFX enrichment is different: LFX is mainly an identity and project/account metadata authority for this use case.

## Design Position

Treat LFX as a source of maintainer identity evidence, not as an operational `Service` in the FOSSA sense.

Do not model LFX maintainer enrichment primarily as `RemoteUser` or `RemoteTeamUser`. Those tables imply service membership/reconciliation behavior and use numeric remote IDs, while LFX identifiers include Salesforce IDs, identity UUIDs, LFID usernames, contact IDs, account IDs, and project IDs.

Add a small provenance model for third-party identity observations. LFX can be the first producer, but the same shape should support GitHub, OpenProfile, maintainers.cncf.io, and GitDM later.

## Proposed Data Model

Keep `maintainers` as the canonical application record. Add source-specific observations that can be compared and promoted into canonical fields.

Add a canonical LFX identifier to `Maintainer` so reports can directly distinguish maintainers with an LFX profile from maintainers who still need to create or connect one. The first-pass field should store the stable LFX user/contact ID returned by LFX user search, with LFID username still recorded on observations. A possible field is:

```go
LFXUserID string `gorm:"size:128;index"`
```

Do not treat the presence of an LFX ID as a maintainer registration gate by itself. It is reporting and enrichment data; the authoritative membership source for project maintainer registration is inclusion in the "project-maintainers" team in the maintainers.yaml file in a .project repo. Note that the default auto-add policy additionally requires corroboration by cncf/foundation project-maintainers.csv membership (`--check-foundation-csv`, default `true`; an entry with no CSV match is skipped, not auto-added).

Suggested new model:

```go
type MaintainerIdentityObservation struct {
    gorm.Model
    MaintainerID *uint `gorm:"index"`
    ProjectID    *uint `gorm:"index"`

    Source       string `gorm:"size:64;index"`  // lfx, github, openprofile, maintainers-cncf-io, gitdm, dot-project
    SourceRef    string `gorm:"size:512;index"` // stable URL or source-specific lookup key
    SourceUserID string `gorm:"size:128;index"` // LFX Salesforce user ID, GitHub login, etc.

    Name         string `gorm:"size:255"`
    Email        string `gorm:"size:254"`
    GitHubUser   string `gorm:"size:100"`
    LFID         string `gorm:"size:100"`
    CompanyName  string `gorm:"size:255"`
    CompanyRef   string `gorm:"size:128"` // LFX account ID when available

    MatchStatus  string `gorm:"size:32;index"` // matched, chosen, duplicate, unmatched, error
    MatchReason  string `gorm:"size:255"`
    Confidence   string `gorm:"size:32"`       // exact, strong, weak
    RawPayload    string `gorm:"type:jsonb"`
    ObservedAt    time.Time `gorm:"index"`
}
```

This lets the maintainer route show a unified view:

- canonical maintainer-d value
- observed values by source
- differences between sources
- source links or IDs used to support each value
- duplicate LFX profile groups (`chosen`/`duplicate` rows) that need staff review

Canonical fields should not be overwritten blindly. The first implementation should only fill missing canonical fields automatically when the match is exact or strong, and record an observation otherwise.

Confidence definitions (updated after `lfx/LFX-USER-API-NOTES.MD` finding 8 identified a live scoring
bug and this was fixed - see `lfx/enricher.go` `confidenceFor`):

- `exact`: `GetUserIdentities` confirms a linked GitHub identity whose source is `github` and whose username matches the maintainer GitHub handle case-insensitively. This holds regardless of the matched user's `Type`.
- `strong`: the searched email equals the returned user's email case-insensitively, **or** GitHub-handle search matched this user and the user's `Type` is `contact` (a claimed profile).
- `weak`: everything else, including a GitHub-handle match whose `Type` is `lead` (a stale, never-claimed Salesforce row) with no identity confirmation. `Type` is a demotion signal, not a hard gate - a `lead` record can still carry confirmed identities and score `exact`. Do not use weak matches for automatic canonical field updates.

### PR-Reviewed Provenance Confidence (dot-project / foundation-csv / legacy-ref)

The LFX confidence tiers above are unrelated to the confidence tiers for the three file-based
observation sources (`dot-project`, `foundation-csv`, `legacy-ref`). Those three are derived from
whether a human reviewed and approved the pull request that introduced the line where the GitHub
handle was found - not from an LFX identity match. The evidence is resolved by the `provenance`
package (blame -> commit -> pull request -> review state) and stored on the observation's
`SourceFilePath`, `SourceLine`, `SourceCommitSHA`, `SourceLineURL`, `SourcePRNumber`, `SourcePRURL`,
and `SourceReviewState` fields, and the confidence itself is derived by `provenance.Confidence`:

| Source | Review state | Confidence |
|---|---|---|
| `dot-project` (`project-maintainers` team membership) | approved | `exact` |
| `dot-project` | unreviewed / direct-push | `strong` |
| `dot-project` | unknown (unresolvable) | `strong` (one tier below `exact`) |
| `foundation-csv` | approved | `strong` |
| `foundation-csv` | unreviewed / direct-push | `medium` |
| `foundation-csv` | unknown (unresolvable) | `medium` (one tier below `strong`) |
| `foundation-csv`, no CSV lookup performed at all | - | `""` (asserts nothing) |
| `legacy-ref` | approved | `medium` |
| `legacy-ref` | unreviewed / direct-push | `weak` |
| `legacy-ref` | unknown (unresolvable) | `weak` (one tier below `medium`) |

`unknown` means the source could not be resolved (e.g. a gist-hosted legacy ref, or no GitHub
resolver configured) - it must never be reported as `direct-push` or `unreviewed`, since neither of
those would be true; an unresolvable source is not negative evidence.

## LFX Data Worth Recording

From LFX `user-service`:

- Salesforce user/contact `ID`
- LFID `Username`
- `FirstName`, `LastName`, `Name`
- primary `Email`
- additional `Emails`
- linked identities from `/v1/users/{salesforceID}/identities`
- identity `Source`, especially `github`
- identity UUID `ID`
- `IsVerified`
- account object when returned by user search

From LFX `project-service`:

- LFX project ID
- project slug
- repository ID
- repository URL
- whether a user appears in `/v1/projects/{projectID}/maintainers`
- maintainer `ID`, `IdentityID`, `Email`, `Username`

From LFX `member-service`, only when company/account data is needed:

- account ID
- account name
- account primary domain and domain aliases
- active membership status
- project membership details
- project contact roles such as `Technical Contact` or `Representative/Voting Contact`

For this use case, `member-service` is secondary. It helps enrich company affiliation and account membership, but it does not appear to be the authoritative API for GitHub repository maintainers.

## Minimal LFX Client Surface

Create a small internal package, for example `lfx`, with explicit clients for the endpoints we need. Do not generate a full client from the downloaded swagger files for the first version.

Required configuration:

- `LFX_BASE_URL=https://api-gw.platform.linuxfoundation.org`
- `LFX_AUTH_TOKEN` or the gateway token mechanism required by production
- `LFX_ACL` if the gateway requires `X-ACL`
- optional `LFX_USERNAME` and `LFX_EMAIL` request headers for audit context
- `LFX_RATE_LIMIT_RPS`, default conservative
- `LFX_ENRICH_ALL_MAINTAINERS`, default false initially

Minimal operations:

- `SearchUsers(ctx, query)` using `GET /v2/users/search`
- `GetUserIdentities(ctx, salesforceID)` using `GET /v1/users/{salesforceID}/identities`
- `SearchProjects(ctx, nameOrSlug)` using `GET /v1/projects/search` or `GET /v1/projects`
- `ListProjectMaintainers(ctx, projectID, filters)` using `GET /v1/maintainers?$filter=ProjectID eq {projectID}`
  (not `/v1/projects/{projectID}/maintainers` - that path does not exist; confirmed by a live 404
  and by the downloaded `user-service` OpenAPI spec, which instead documents a top-level
  `findMaintainers` operation at `/v1/maintainers`, filterable by `ProjectID`, `GithubID`, `Role`,
  `IsUserBot`, and `Repository`. See `lfx/LFX-USER-API-NOTES.MD` finding 12.)
- optional later: `LookupAccount(ctx, name, domain)` using `GET /v3/accounts/lookup`
- optional later: `ListAccountProjectRoles(ctx, accountID, projectID)` using `GET /v3/accounts/{accountID}/project/{projectID}/roles`

## Matching Strategy

Use exact identifiers before fuzzy human data.

1. GitHub handle from `.project/maintainers.yaml` or `Maintainer.GitHubAccount`.
2. Email from `Maintainer.Email` or `Maintainer.GitHubEmail`.
3. LFX user search by `githubID`.
4. LFX user search by `email`.
5. LFX identity lookup to confirm a `github` identity.
6. Project maintainer lookup by resolved LFX user ID and resolved LFX project ID.
7. Name/company comparison only as supporting evidence, not as a primary identifier.

When multiple LFX users match the same GitHub handle or email - a known upstream LFX data-quality
issue, since LFX has no 1:1 mapping between profile IDs and GitHub identities (229 duplicate-GithubID
groups were found across CNCF projects in one external audit) - record one `MaintainerIdentityObservation`
row per matched LFX profile, each with its own fetched identities and its own confidence. Rank the
candidates (confidence, then `contact` before `lead`, then higher identity count, then more recently
modified, then a stable ID tiebreak) and mark the top-ranked row `MatchStatus=chosen`; mark the rest
`MatchStatus=duplicate`. Only the `chosen` row drives canonical field updates, under the existing
fill-only-if-missing policy below. All rows are surfaced on the maintainer route so staff can see
every LFX profile bound to a maintainer.

When no LFX user matches, record `MatchStatus=unmatched` so future runs can back off instead of querying every hour forever.

## Dot-Project Sync Integration

Add enrichment after the existing discovery/cache step, not inside raw file discovery.

Initial mode:

- Parse cached `maintainers.yaml` for `project-maintainers` members.
- Match each handle to an existing maintainer by `GitHubAccount`.
- For unmatched handles, optionally create a maintainer record only when LFX or another source provides enough identity data.
- Enrich matched maintainers by writing `MaintainerIdentityObservation` rows.

Broader mode:

- Iterate all active maintainers in the DB.
- Prefer maintainers touched by current dot-project sync first.
- Then process the remaining maintainers in batches.
- Skip recently observed maintainers unless forced.

This supports the desired direction: `.project/maintainers.yaml` should drive discovery, but all maintainer records can be enriched over time.

## Rate Limits and Caching

Assume LFX should be treated as a relatively static source.

Use these defaults:

- cache successful LFX observations for 30 days
- cache unmatched lookups for 7 days
- cache error lookups for 1 day
- cap each dot-project sync run to a configurable number of LFX lookups
- batch by source priority: dot-project handles first, then existing maintainers with missing data, then stale observations
- record upstream status and failures in audit metadata

Avoid live LFX calls from web routes. The web UI should read persisted observations.

## Canonical Field Update Policy

Automatically fill missing fields only:

- `Name`, if maintainer-d has an empty or placeholder name and LFX has a strong match.
- `Email`, if maintainer-d has `EMAIL_MISSING` and LFX has a verified or primary email.
- `CompanyID`, if maintainer-d has no company and LFX account data gives a clear company.
- `LFXUserID`, if maintainer-d has no LFX ID and LFX has an exact or strong match.

Do not overwrite non-placeholder maintainer-d values without a staff-visible review workflow.

When LFX conflicts with existing values, keep the existing canonical value and surface the difference on the maintainer route.

## Maintainer Route UX

Add an identity/provenance section to the maintainer route:

- canonical maintainer-d fields
- source observations grouped by source
- match status and confidence
- LFX user/contact ID and LFID
- company/account observation
- conflicts such as `email differs from GitHub` or `company differs from maintainers.cncf.io`
- last observed timestamp

This makes LFX useful without pretending it is the only source of truth.

## Implementation Steps

1. Add `MaintainerIdentityObservation` and migrate Postgres.
2. Add store methods for upserting observations by source and source user/reference.
3. Add a small `lfx` package with typed request/response structs for the selected endpoints.
4. Add a dot-project enrichment interface so `dotproject.Syncer` can call an optional enricher after caching maintainer files.
5. Implement LFX enrichment for matched maintainers using GitHub handle and email lookup.
6. Add audit metadata to `DOT_PROJECT_SYNC_RUN`: attempted, matched, ambiguous, unmatched, errored, skipped_recent.
7. Add web API response fields for source observations on maintainer detail.
8. Add UI rendering for provenance and conflicts.
9. Add an opt-in mode to enrich all maintainers, with strict batching and stale-observation skipping.

## Open Questions

- What exact production authentication headers does the LFX API gateway require for machine-to-machine usage?
- Does `githubID` in `/v2/users/search` expect a GitHub login, a numeric GitHub ID, or an LFX identity ID?
- Should staff be able to promote a source observation into canonical fields manually from the maintainer route?
- Should `.project/maintainers.yaml` unmatched handles create placeholder maintainers immediately, or only after LFX/OpenProfile provides name and email?
- Should LFX project IDs/slugs be persisted on `Project`, or should they be observations until we trust the mapping?

## Recommendation

Start with LFX as a provenance-producing identity source, not a `Service`.

Use `Service` only if later LFX integration starts managing remote access, memberships, invitations, or project roles on behalf of maintainers. For the near-term enrichment workflow, a source observation model is more honest and more useful.
