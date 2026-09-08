# LFX User Management API — notes

This file records what we have learned about the `user-service` API (the legacy-looking but
currently-live surface that `lfx/client.go` calls) so the knowledge survives the branch instead of
living only in shell history and chat scrollback.

**Standing rule:** consent to probe an account against this API is not consent to commit anything
from it. Every finding below is evidence-cited (`spec` = a quote from the downloaded OpenAPI
document, `observed` = a live probe run) but no real handle, name, email, company, or LFID appears
anywhere in this file. `scripts/check-pii.sh` only regex-matches email-shaped strings and known
secret patterns — it does not catch a name or handle, so this is a discipline, not a CI guarantee.

## 1. Reference resources

- **`lfx/swagger.json`** (gitignored, not committed — see `.gitignore`) — the authoritative
  artifact. Swagger 2.0, `info.title: "User Service API"`, `info.version: "1.0"`. Re-download it
  from the ReDoc page below (browser session, not the API token — see finding 1) and save it here
  as `lfx/swagger.json` to reproduce any of the findings below. Useful `jq` incantations once you
  have it:

  ```bash
  # Top-level paths and their operationIds
  jq '.paths | to_entries[] | {path: .key, ops: (.value | keys)}' lfx/swagger.json

  # A single definition
  jq '.definitions.user' lfx/swagger.json
  jq '.definitions.account' lfx/swagger.json
  jq '.definitions."list-metadata"' lfx/swagger.json

  # A single operation's full detail (params, description, responses)
  jq '.paths["/v1/maintainers"]' lfx/swagger.json
  ```

- **ReDoc pages** —
  prod `https://api-gw.platform.linuxfoundation.org/user-service/v1/api-docs`,
  dev `https://api-gw.dev.platform.linuxfoundation.org/user-service/v1/api-docs`.
  Browser session only; the raw `/openapi.json` and `/swagger.json` routes 403 even a valid bearer
  token (finding 1), so the "Download" button on the ReDoc page is the only reliable path.

- **`lfx-v2-meeting-service`'s `internal/infrastructure/userservice` package** —
  `https://pkg.go.dev/github.com/linuxfoundation/lfx-v2-meeting-service/internal/infrastructure/userservice`.
  A current V2 service's own client for this same v1 gateway (published 2026-08-27); the best
  available evidence that the surface is live production infrastructure rather than deprecated,
  and a reference implementation to diff our client against.

- **`lfx-skills` plugin references** — `references/repo-map.md`, `contract-ownership.md`,
  `data-privacy.md` under the `lfx-skills` plugin cache. Authoritative on V2 routing and on the PII
  rules that bind this work. Known blind spot: they document the V2 platform only and contain zero
  references to `user-service` — that gap is what originally pointed this investigation toward
  "must be deprecated," which the spec and the external corroboration above both contradict.

- **The LFX MCP server** (session-attached tools, e.g. `search_committee_members`) — no
  `user-service` tools exist there, so it does not substitute for this client, but its
  `organization: {id, name, website}` shape is the same Salesforce Account object as `User.Account`
  (finding 6) and is a useful independent confirmation of that shape.

- **`lfx/client_e2e_test.go`** — our own probes (`TestLFXLiveProbe`,
  `TestLFXProbeMaintainerIdentities`, `TestLFXLiveProbeProjectMaintainers`,
  `TestLFXProbeProjectMaintainers`). Skip-unless-`LFX_AUTH_TOKEN`, redacted-shape-only logging by
  default, opt-in raw dump via `LFX_PROBE_RAW=true` for local debugging. This is how we answer a
  behavioural question the spec doesn't settle — the tool of first resort for anything in section 2
  below that needs generalizing beyond one account.

## 2. Behavioural observations and bugs

Each entry is tagged **[LFX]** (a platform-side quirk/bug to be aware of, not ours to fix) or
**[maintainer-d]** (a real bug in this repo's client/enricher code).

1. **[LFX] The docs route is ACL'd separately from the data routes.**
   `GET /user-service/v1/openapi.json` returns `403 ... does not have access to resource or path`
   for a token that successfully calls `/v2/users/search`. Getting the spec requires a browser
   session, not the API token. *(observed)*

2. **[LFX] The spec's own URLs are wrong, by its own admission.** `info.description`:
   *"URL in the swagger spec is not correct, Please replace `/v1` in the URL with
   `/user-service/v1` to make it work."* `basePath` is `/` and `host` is `null`. Every path in the
   document must be prefixed with `/user-service` by hand. Our client (`lfx/client.go`) already
   does this correctly. *(spec)*

3. **[LFX] Search parameters OR together — they do not narrow.** `searchUsersV2`'s description:
   *"If you search using parameters like email, linkedinID, twitterID, githubID and username then
   it will exact match (`=` operator) with OR relation"*, and firstname/lastname use LIKE, also OR.
   This is the opposite of the intuitive reading. **maintainer-d impact:**
   `enricher.searchUsers` tries GitHub-ID then falls back to email precisely to avoid combining
   them, which is correct given this — but a future "pass both to narrow the match" change would
   silently *widen* the result set instead of narrowing it. *(spec)*

4. **[LFX] Results are scoped by the *caller's* organization.** *"If the user who is calling this
   endpoint belongs to a specific organization then this endpoint will return all users from the
   same organization."* Results are therefore a function of whose token is used, not a pure
   function of the query. **maintainer-d impact:** the production service account and a staff
   member's personal token can legitimately return different result sets for an identical query —
   worth stating explicitly before anyone debugs a "works on my machine" discrepancy. *(spec)*

5. **[LFX] `$filter` fails silently and open; deprecated on v1 in favour of v2.**
   *"If filter fails you won't get an error, it will return everything"* and *"Filter can not be
   used con other request parameters, it will ignore all others."* A malformed filter degrades to
   an unfiltered full listing rather than an error — a dangerous default for any caller that
   assumes a filtered response is actually filtered. Also: `$filter` is explicitly marked
   deprecated on `/v1/users/search` in favour of `/v2/users/search`. *(spec; the silent-open-failure
   claim specifically is spec prose only so far — `TestLFXProbeProjectMaintainers`'s deliberate
   malformed-filter request is designed to verify it empirically against `/v1/maintainers`)*

6. **[maintainer-d] `User.Account` is a documented four-field object, and we guess at five
   nonexistent ones.** Spec: `#/definitions/account` = `{ID, LogoURL, Name, Website}`. Observed live
   with three populated fields (`ID` an 18-character Salesforce ID, `Name`, `Website`).
   `accountCompanyName` (`lfx/enricher.go:361-377`) probes six key spellings
   (`Company`/`company`/`CompanyName`/`companyName`/`Name`/`name`) plus a nested-map fallback. Only
   `Name` exists — five of the six branches are dead code guarding against a shape the spec rules
   out, and `Account.ID` (the stable Salesforce org identifier that would fix free-text company
   drift) is discarded entirely. *(spec + observed)*

7. **[maintainer-d] `User.GithubID` is inherited via `allOf` and easy to miss.** It comes from
   `#/definitions/contact-details` (alongside `TwitterID`, `LinkedInID`, `Phone`, `MobilePhone`)
   rather than `#/definitions/user` itself. Observed populated on a real record. `lfx.User` now
   models it and the enricher persists it as `SourceGitHubID`. The remaining gap is narrower than
   originally noted: `confidenceFor` does not consult the field directly — handle corroboration
   still comes from the `GetUserIdentities` round-trip (linked `github` identity) plus the
   matched-by-GitHub-ID search path, so the in-payload signal could in principle short-circuit
   that second call but currently does not. *(spec + observed)*

8. **[maintainer-d] `User.Type` is `lead` | `contact`, and this is the "old profile" explanation —
   now reproduced across real maintainer handles, and traced to a live confidence-scoring bug.**
   Originally observed as `Type="lead"` on a single self-lookup; a `lead` is a stale, never-claimed
   pre-conversion Salesforce row, and it having no linked identities explained the "identities are
   from an old profile" symptom that kicked off this investigation. A later multi-handle run of
   `TestLFXProbeMaintainerIdentities` against real, independently-known-duplicated project
   maintainer handles (chosen from an external CNCF repo's own duplicate-`GithubID` report, no
   real data copied here) reproduced this on ≥2 more accounts: single v2 `SearchUsers` matches with
   `Type="lead"` and 0 linked identities. This generalizes the finding beyond one data point.

   That same run also surfaced the opposite fan-out case: one handle returned **6** matches through
   v2 `SearchUsers`, all `Type="lead"`, most with 0 identities (one had 1) — v2's exact-match
   semantics do not collapse duplicate `GithubID` records into one the way an early hypothesis
   assumed; that collapse only happened to occur for a different single-handle test case, likely
   because the other duplicate records' stored `GithubID` values weren't byte-identical (case or
   whitespace differences that v1's substring search tolerates but v2's exact match does not).

   *(Historical note: at the time of this probe, `Enricher.enrich` routed `len(users) > 1` to a
   single `MatchStatus=ambiguous` row with confidence forced to `weak`, and `confidenceFor` never
   read `user.Type` — so a lone `lead` record with zero supporting identity evidence scored
   `"strong"` on a bare GitHub-ID match. Both have since been fixed: the multi-match case now
   records one observation row per profile with the best-ranked one marked `chosen` and the rest
   `duplicate` (`enrichMultipleMatches`), and `confidenceFor` demotes a bare GitHub-ID match on a
   non-`contact` record to `weak`. The confidence-scoring bug described here was confirmed live
   against real handles in this same probe run before the fix.)*

   **Counter-evidence against a pure `Type` filter:** one probed handle was `Type="lead"` *with* 3
   linked identities — so "`lead` implies zero identities" is not an absolute rule, only a strong
   correlation. Gating `confidenceFor` on `Type=="contact"` (the fix that was ultimately adopted)
   is a meaningful improvement over checking nothing, but is not a perfect filter on its own — a
   verified GitHub identity still overrides the type gate and scores `exact`.

   **External corroboration:** the CNCF's own `cncf/maintainer-audit-temp` repo
   (`scripts/update_project_maintainers.py`, `lookup_pcc_user()`) independently hits this same
   duplicate-`GithubID`-per-person problem hard enough to need its own dedup pass: it queries the
   *v1* `/users/search` endpoint (substring match, not v2's exact match), filters client-side to
   exact `GithubID` matches, and when more than one survives, picks the freshest by
   `LastModifiedDate` (then `CreatedDate`, then `ID`), logging the rest as rejected. Its own report
   surfaced 229 distinct duplicate-`GithubID` groups in one run against real CNCF project
   maintainers — this is not an edge case. Its report has no `Type` field, so it cannot say whether
   its rejected records are disproportionately `lead`s; that remains open. Reported verbally at LFX
   Office Hours as a known, unresolved platform-side data-quality gap (no 1:1 mapping between LFX
   profile IDs and GitHub identities).
   *(spec + observed, reproduced across multiple handles; external corroboration from a
   separate CNCF codebase)*

9. **[maintainer-d] `User.Username` is the LFID, and it was absent (not merely empty) from the
   observed payload.** Contrast `Bio`/`Address`, which appeared with `len 0` — `Username` did not
   appear at all. Spec: *"Linux Foundation ID for this user."*
   `writeRawObservation` sets `observation.LFID = user.Username`, which is therefore likely empty
   for real data. Related: the spec's `/v1/me` description says it *"will return user data only if
   that user has `Username`"* — LFID absence has real consequences platform-side too, not just in
   our own code. *(spec + observed)*

10. **[maintainer-d] `User.Emails` is an array of rich objects, not the single `Email` scalar we
    model.** `#/definitions/email` carries
    `{Active, EmailAddress, EmailType, ID, IsDeleted, IsPrimary, IsVerified, SubscribedSystem, ...}`
    plus per-system fields (`Github`, `Git`, `Gerrit`, `Jira`, `Slack`, `Jenkins`, `Confluence`,
    `BugZilla`, `PiperMail`). Observed live with 7 keys populated on one entry. Matching on the
    single `Email` field can miss a person whose query address is a verified secondary email — a
    plausible contributor to false `weak` confidence results. *(spec + observed)*

11. **[LFX] `GetUserIdentities` has no v2 equivalent, and its own description is unreliable.**
    `/v1/users/{salesforceID}/identities` is the only identities resource in the document.
    `salesforceID` is a **required path parameter** — despite the operation's own description
    reading *"a list of identity for the current user,"* the signature is clearly designed for
    arbitrary target lookup, not just self-lookup. It also accepts `X-ADMIN`/`X-STAFF` headers, so
    what a non-privileged token may read about *another* user is unresolved and worth a targeted
    probe. Treat the endpoint's prose description as unreliable where it conflicts with its
    signature. *(spec)*

12. **[LFX + maintainer-d] `GET /v1/maintainers` exists — this is the endpoint the original spec
    was groping for.** `operationId: findMaintainers`, returning `maintainer-list` of
    `{ID, Name, Email, GithubID, IsUserBot, Role, ProjectID, Repository}`, with `ProjectID`,
    `GithubID`, `Role`, `IsUserBot`, and `Repository` all `$filter`-available. This falsifies the
    prior conclusion that "no `/projects/{id}/maintainers` resource exists in V2 — do not
    implement": the resource exists, just at a different path
    (`/v1/maintainers?$filter=ProjectID eq <id>`, not `/v1/projects/{id}/maintainers`). Also
    record the spec-authoring bug alongside it: the `maintainer` definition's own `description` and
    `title` read *"Properties to update an user preference"* / *"Update User Preference"* — an
    obvious copy-paste error, and a caution against trusting this document's prose metadata in
    general. See `specifications/lfx-integration.md`'s corrected `ListProjectMaintainers` row.
    *(spec; `TestLFXProbeProjectMaintainers` is the live probe for this endpoint)*

13. **[LFX, unresolved] `X-ACL` is declared required; we only send it when configured — and it does
    not generalize the same way across every route.** `securityDefinitions.lf-auth` reads *"Requires
    X-ACL headers and bearer token"* and `#/parameters/x-acl` is `required: true`.
    `Client.addHeaders` (`lfx/client.go:220-222`) sets it only when `c.ACL` is non-empty.
    `/v2/users/search` and `/v1/users/{salesforceID}/identities` both succeeded without it, so the
    gateway evidently synthesizes `X-ACL` from the bearer token for those routes. But
    `GET /v1/maintainers` (with the correct `/user-service` prefix — see finding 15) returned a
    clean `403 Forbidden` on the author's personal token, for both a well-formed `ProjectID` filter
    and a deliberately malformed one, with identical `Metadata.TotalSize=0` — i.e. this route
    enforces something (plausibly a real `X-ACL` scope, or an admin/staff-only gate) that the
    author's token doesn't carry, unlike the other two routes. Whether that is a missing `X-ACL`
    header specifically, or a genuinely staff/admin-restricted endpoint, is still unresolved — would
    need a service-account token or an explicit `X-ACL` value to distinguish. **maintainer-d
    consequence:** the `TestLFXProbeProjectMaintainers/malformed_filter` sub-test did not actually
    exercise finding 5's "bad filter returns everything" claim, since the 403 happens before any
    filter logic runs — that claim is still spec-prose-only for this route. *(observed)*

14. **[maintainer-d] Pagination is real and we ignore it.** `pageSize` defaults to 100 (min 1);
    responses carry `Metadata: {Offset, PageSize, TotalSize}`. `SearchUsers`
    (`lfx/client.go:92-94`) decodes only `Data` and drops `Metadata`, so a truncated result set is
    currently indistinguishable from a complete one. Low impact for single-user lookups; matters
    for `/v1/maintainers`, where a project roster can plausibly exceed 100 entries.
    `TestLFXProbeProjectMaintainers` logs `Metadata.TotalSize` against the returned count precisely
    to check whether this bites in practice. *(spec)*

15. **[maintainer-d] A missing `/user-service` prefix produces a misleading 404, not a clean
    "endpoint doesn't exist" signal.** The first `TestLFXProbeProjectMaintainers` run against
    `GET /v1/maintainers` (no `/user-service` prefix) came back `404 Not Found` with a body starting
    `p...` (an HTML/plaintext gateway error page, not JSON) — indistinguishable at a glance from "no
    such resource." `TestLFXLiveProbeProjectMaintainers`'s original D5 evidence had the exact same
    bug (built the request as `base+"/v1/projects/{id}/maintainers"`), so **that 404 was not valid
    evidence for D5** — it may have been hitting a bad gateway route rather than a genuinely absent
    resource. Both tests are now fixed to include `/user-service` (matching finding 2 and how
    `client.go` itself builds every real request path); re-running is what would produce trustworthy
    evidence either way. *(observed; self-caught bug in this repo's own probe tests, not an LFX
    finding)*
