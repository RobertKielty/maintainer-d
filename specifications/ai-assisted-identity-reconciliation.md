# AI-Assisted Maintainer Identity Reconciliation — Design & Phased Plan

Status: draft for review. No application code written yet.

## 0. Grounding: what already exists

Before proposing anything new, this is what the codebase already has (so the design
extends it rather than duplicating it):

- **`model.MaintainerIdentityObservation`** (`model/main.go:196-217`) already models
  "an identity observation from a particular source": `Source`, `SourceRef`,
  `SourceUserID`, raw fields (`Name`/`Email`/`GitHubUser`/`LFID`/`CompanyName`/`CompanyRef`),
  `MatchStatus` (matched/chosen/duplicate/unmatched/error), `MatchReason`, `Confidence`
  (exact/strong/medium/weak), `RawPayload` (JSONB), `ObservedAt`. This is concept (1) in the
  prompt's "important design principles" list — **already built**, but it upserts in
  place per `(Source, MaintainerID/ProjectID, SourceUserID/SourceRef)` key, so it holds
  only the *latest* observation, not a history.
- **`lfx/enricher.go`** already does LFX-based candidate matching (by GitHub handle or
  email), classifies `matched/chosen/duplicate/unmatched/error`, and assigns
  `exact/strong/medium/weak` confidence, writing straight into
  `MaintainerIdentityObservation`. It is the closest existing precedent for "candidate
  identity matching" and "OpenProfile verification" — but it is unclear whether it hits
  OpenProfile.dev specifically or a more general LFX identity search; treat this as the
  seed of `get_openprofile`, not a finished OpenProfile client.
- **`lfx/` already has a working LFX data client today** — `lfx/client.go`'s `SearchUsers` and
  `GetUserIdentities` call the real `user-service` v2/v1 endpoints (confirmed live; see
  `lfx/LFX-USER-API-NOTES.MD`), and `lfx/enricher.go` uses them for candidate matching. This is not
  net-new work; it is the seed of `get_openprofile` (see below). Since this was written, the
  `improve-lfx-client` branch closed most of the named gaps: `lfx.User` now models `Type` and
  `GithubID` (persisted as `SourceUserType`/`SourceGitHubID`), the `lead`-vs-`contact` confidence
  bug is fixed (a bare GitHub-ID match on a `lead` profile demotes to weak), and multi-profile
  matches record one row per profile as `chosen`/`duplicate`. Remaining gaps: `Emails` (the
  secondary-email array) is still unmodeled, and confidence scoring does not consult the
  in-payload `GithubID` directly (it still round-trips via `GetUserIdentities`).
  `cmd/lfx-client/` is a separate, narrower tool that only scrapes LFX's OpenAPI specs to local
  files - it is not the data client.
- **`dotproject/auto_add.go` (`AutoMaintainerAdder`)** already reads observations and
  **auto-creates/auto-links maintainers from `.project/maintainers.yaml` without human
  approval.** This directly conflicts with this design's human-in-the-loop principle and
  needs to be reconciled explicitly (see §11, Phase 1).
- **`specifications/maintainer_identity_with_names_preserved.md`** is a real, already-written
  spec for the *existing* 122 duplicate-maintainer problem inside Maintainer-D's own DB
  (root cause: legacy email-first matching). It proposes a 4-class taxonomy
  (`SAFE_MERGE`, `REVIEW_REQUIRED`, `CORRUPT_DATA`, `NEAR_DUPLICATE_EMAIL`) and a staged
  cleanup plan. This is functionally "Phase 0/1" of the in-DB half of this project and
  should be adopted as-is rather than re-invented — see §11.
- **No maintainer-merge operation exists.** `db.MergeCompanies` (company-only) is the only
  merge primitive in `db/store.go`/`store_impl.go`. A `MergeMaintainers`-equivalent command
  is new work.
- **`refparse.ExtractGitHubHandles` / `MaintainerRefContains`** (`refparse/maintainer_refs.go`)
  already parses legacy maintainer files for GitHub handles — reusable for source ingestion.
- **`cmd/sanitize/`** already does a crude, one-directional project-membership check
  (is this maintainer's handle/name present in the legacy ref?) — a real precedent for
  "project membership reconciliation," but deterministic and unidirectional only.
- **Manual dedup scripts** (`scripts/duplicate-github-maintainers-local.sh`,
  `maintainer-dupe-review.py`, `company-dupe-merge.py`, etc.) are one-off, exact/fuzzy-match,
  operator-run SQL/Python — no ML, not wired into the app. They're useful as reference
  heuristics for the deterministic candidate-matching layer (§1, §6) but not
  infrastructure to build on top of.
- **`AuditLog`** already exists as a first-class model and is used by the FOSSA poller and
  onboarding flows — reuse this rather than inventing a parallel audit mechanism.
- **`db/bootstrap.go`** worksheet ingestion is already gated by
  `MD_BOOTSTRAP_ALLOW_MAINTAINER_WRITES` (disabled by default) — the worksheet's writes to
  `Maintainer` are already off in normal operation; this reduces the urgency of "the
  worksheet keeps corrupting the DB" and reframes the worksheet as a read-only comparison
  source for reconciliation, not a write path to disable.

Everything below is written against this baseline, not a green field.

---

## 1. Problem decomposition

| Capability | Deterministic or LLM? | Existing precedent |
|---|---|---|
| Source ingestion (worksheet, legacy ref, `.project/maintainers.yaml`, OpenProfile, DB) | Deterministic | `db/bootstrap.go`, `refparse`, `dotproject/*`, none for OpenProfile |
| Observation normalisation (case-fold email/GitHub, sentinel handling) | Deterministic | `UpdateMaintainerDetails` sentinel logic in `store_impl.go` |
| Candidate identity matching (same person across sources) | **Deterministic first** (exact GitHub/email match, normalized-name blocking), LLM only for the residual ambiguous set | `lfx/enricher.go`, dedup scripts |
| Duplicate detection (within Maintainer-D DB) | Deterministic | `maintainer_identity_with_names_preserved.md` taxonomy |
| Conflict detection (same person, different field values) | Deterministic (diff) — LLM explains, doesn't decide | none today |
| Project membership reconciliation (worksheet vs. legacy ref vs. `.project/maintainers.yaml`) | Deterministic set comparison | `cmd/sanitize` (partial) |
| OpenProfile verification | Deterministic API call + rules; LLM only to summarize gaps | `lfx/enricher.go` (unconfirmed scope) |
| Human review | Human, tooled | none — this is the actual gap the whole feature exists to fill |
| Database updates | Deterministic, command-based, gated on human approval | `AuditLog`, `ServiceInvitation`/FOSSA command pattern |
| `.project/maintainers.yaml` generation | Deterministic templating from canonical identities | none |
| Auditing and rollback | Deterministic | `AuditLog` (extend, don't replace) |

The LLM's real job is narrow: **rank/explain ambiguous candidates that survive
deterministic filtering, and turn a reconciled/approved data set into prose explanations
and structured proposals.** It is never the source of a match decision, a merge, or a
database write.

---

## 2. Proposed domain model

New tables, additive to the existing schema (nothing here replaces
`MaintainerIdentityObservation` — it becomes one of several source-observation shapes it
already models well enough to reuse directly for worksheet/OpenProfile/legacy-ref rows).

```
SourceRecord                 -- a raw pull from one source at one point in time
  id, source (enum: worksheet|legacy_ref|dot_project_yaml|openprofile|db),
  project_id (nullable), external_ref, fetched_at, raw_payload (jsonb)

  -- Reuses MaintainerIdentityObservation's shape for the per-person row;
  -- SourceRecord is the *batch/fetch* envelope, observations are the per-row facts.

CanonicalIdentity             -- concept (3): a reconciled "this is one real person"
  id, primary_maintainer_id (fk Maintainer, nullable until first merge),
  display_name, created_at, superseded_by_id (nullable, self-fk for merge chains)

CandidateMatch                -- concept (5 precursor): a possible identity pairing
  id, canonical_identity_id (nullable if not yet grouped),
  left_ref (maintainer_id | observation_id), right_ref (maintainer_id | observation_id),
  match_reason (enum: exact_github|exact_email|normalized_name|llm_ranked),
  confidence (exact|strong|weak), score (float, nullable),
  status (pending|accepted|rejected), decided_by, decided_at

MatchEvidence                 -- concept (2's evidentiary backing)
  id, candidate_match_id, evidence_type (field_equal|field_conflict|source_corroboration),
  field_name, left_value, right_value, source

ReconciliationSession         -- concept (project-scoped or dedup-cluster-scoped unit of work)
  id, scope (enum: project|cluster), project_id (nullable),
  opened_by, opened_at, closed_at, status (open|applied|abandoned)

ProposedAction                 -- concept (5): a not-yet-applied change
  id, reconciliation_session_id, action_type (enum: merge_identity|update_field|
      add_project_membership|remove_project_membership|generate_maintainers_yaml),
  payload (jsonb, typed per action_type), rationale (text, LLM- or rule-generated),
  evidence_refs (array of match_evidence ids), status (proposed|approved|rejected|applied)

HumanDecision                 -- concept (6)
  id, proposed_action_id, decided_by, decision (approve|reject|edit),
  edited_payload (jsonb, nullable), decided_at, comment

AppliedChange                  -- concept (7's DB half): what a HumanDecision actually did
  id, proposed_action_id, applied_at, applied_by, command_name,
  before_snapshot (jsonb), after_snapshot (jsonb), audit_log_id (fk AuditLog)

ExportedFile                   -- concept (7's file half)
  id, project_id, reconciliation_session_id, file_path (".project/maintainers.yaml"),
  content_hash, content, generated_at, committed (bool), commit_ref (nullable)
```

Relationships: `ReconciliationSession 1—N ProposedAction 1—1 HumanDecision (0..1) 1—1
AppliedChange (0..1)`. `CandidateMatch N—N MatchEvidence`. `CanonicalIdentity 1—N
CandidateMatch` once grouped. `Maintainer` gains no new columns — it stays the canonical
row; everything upstream of a decision lives in these new tables, consistent with "do not
assume `Maintainer` should contain every raw observation."

`ReconciliationResult` (existing, unused) should be **retired**, not extended — its shape
doesn't match this model closely enough to be worth adapting, and it has no callers.

---

## 3. Trust and source precedence model

Not a single ordering — precedence is **per-field, per-context**:

| Field | Precedence order | Rationale |
|---|---|---|
| Name, work email, company affiliation | OpenProfile > legacy ref / `.project/maintainers.yaml` > worksheet > DB | Maintainer-controlled PII should come from the maintainer, per product policy |
| GitHub handle | `.project/maintainers.yaml` / legacy ref > OpenProfile > worksheet | Project files are the operational truth for "who can approve PRs"; a stale OpenProfile GitHub handle is a real failure mode |
| Project membership (is X a maintainer of P) | `.project/maintainers.yaml` > legacy ref > worksheet > DB | Project-owned files are authoritative for membership; the DB and worksheet are historically stale |
| Email *ownership* (is this the same person) | Whichever source has verified the email (OpenProfile is verified by construction; worksheet/legacy-ref emails are unverified) | Ownership claims need stronger evidence than mere presence |
| Historical/DB-only records with no corroborating source | Lowest trust, always | Acknowledged in `maintainer_identity_with_names_preserved.md` as containing known-dirty legacy imports |

Encode this as a small table-driven policy (`(field, source) -> rank`), not a hardcoded
`if/else` chain, so it's auditable and adjustable without a redeploy of core logic.

---

## 4. Reconciliation workflow

1. Operator opens a `ReconciliationSession` (project-scoped or dedup-cluster-scoped).
2. System pulls `SourceRecord`s for that scope: worksheet rows (already fetched by
   `db/bootstrap.go`'s sheet reader, reused read-only here), legacy ref (`refparse`),
   `.project/maintainers.yaml` (`dotproject` package), OpenProfile (new client), DB rows.
3. Deterministic matcher (§1, §6) produces `CandidateMatch` rows: exact GitHub/email first,
   then normalized-name blocking. Everything with `confidence=exact` and no conflicting
   evidence is pre-accepted as a suggestion, not auto-applied.
4. For matches left `ambiguous` or `weak`, the assistant is asked to rank/explain — it
   reads `MatchEvidence`, returns a ranked list with rationale, never a decision.
5. UI presents: evidence panel per candidate, conflicting fields side-by-side, and a set of
   `ProposedAction`s.
6. Operator edits/approves/rejects each `ProposedAction` individually → `HumanDecision`.
7. Approved decisions are converted to typed commands (§6) → `AppliedChange` + `AuditLog`.
8. `generate_maintainers_yaml` renders an `ExportedFile` from the now-canonical identities
   and memberships for that project; operator reviews the diff against the existing file
   before it's committed anywhere (commit itself is out of scope for v1 — produce the file,
   human copies/PRs it, matching today's contractor workflow's final step).
9. Full session (all proposals, decisions, evidence) is retained for audit.

**Worked example — migrating one project off a legacy maintainer file:**
Operator opens a session scoped to project `foo`. System loads: worksheet rows for `foo`,
`foo`'s `LegacyMaintainerRef` content via `refparse`, no existing `.project/maintainers.yaml`
(new adoption). Deterministic match finds 6 maintainers with exact GitHub-handle agreement
between worksheet and legacy ref; 1 maintainer only in the worksheet (candidate: add
membership); 1 GitHub handle in the legacy ref with no worksheet row and no DB match
(candidate: needs OpenProfile lookup or manual entry). Assistant explains the ambiguous
one, citing evidence. Operator approves 6 adds, edits 1 (corrects a typo'd handle),
rejects 1 (stale — no longer a maintainer). System applies via `propose_project_membership_change`
→ approved → `AppliedChange`, then runs `generate_maintainers_yaml`. Operator reviews and
downloads the file.

---

## 5. Conversational interface design

**Hybrid, not free-form chat.** The prompt's own list of example questions
("which maintainers appear in the worksheet but not the repo?") maps directly to
read-only tool calls with structured answers — a chat box is a *thin* layer over
evidence panels and proposed-action cards, not the primary interaction.

Recommended layout: reconciliation-session view = evidence panel (per candidate) + diff
view (conflicting fields) + proposed-action list with inline approve/reject/edit +
a chat sidebar for ad hoc questions, which itself returns structured cards, not paragraphs.

Example:
> **Operator:** "Which repo maintainers don't have a matching OpenProfile?"
> **Assistant (structured):**
> ```
> 3 maintainers in .project/maintainers.yaml with no OpenProfile match:
> - @alice-dev  (exact GitHub match attempted, 0 results)
> - @bob99      (ambiguous: 2 OpenProfile accounts share this handle pattern — needs review)
> - @c-chen     (email domain suggests possible match, confidence: weak)
> [View evidence] [Propose OpenProfile lookup retry] [Mark as known-non-OpenProfile]
> ```
No prose-only answers for anything that implies a decision — always a card with an action
or an explicit "no action available" state.

---

## 6. Tool and command design

Split strictly into **read-only tools** (model may call directly) and **mutation
commands** (model can never call — only the application, after a `HumanDecision`, invokes
these).

Read-only (model-callable):
- `get_project_identity_observations(project_id)` — wraps existing
  `ListMaintainerIdentityObservations`. No auth beyond session scope. Fully auditable
  (log the call + args).
- `find_candidate_maintainer_matches(scope)` — runs the deterministic matcher, returns
  `CandidateMatch`+`MatchEvidence`. Read-only, but expensive — rate-limit per session.
- `get_openprofile(github_handle | email)` — new OpenProfile/LFX client call. Must not leak
  PII into logs beyond what's already visible to the operator; cache negative results
  briefly to avoid hammering the LFX API.
- `compare_project_membership_sources(project_id)` — deterministic set diff across sources.
- `list_pending_reconciliation_actions(session_id)` — read `ProposedAction` where
  `status=proposed`.
- `validate_maintainers_yaml(content)` — schema/lint check only, no write.

Proposal tools (model-callable, but produce a `ProposedAction` row — not a DB mutation):
- `propose_identity_merge(candidate_match_id, rationale)`
- `propose_field_update(maintainer_id, field, new_value, evidence_refs)`
- `propose_project_membership_change(project_id, maintainer_ref, add|remove, evidence_refs)`
- `generate_maintainers_yaml(project_id, reconciliation_session_id)` — produces an
  `ExportedFile` row, still requires operator sign-off before being treated as final.

Every proposal tool: validates its payload against the target action's schema, requires
an authenticated session with write-adjacent scope (even though it writes only to the
proposal tables, not `Maintainer`), and is unconditionally audit-logged.

Mutation commands (application-only, never model-invocable):
- `ApplyMergeIdentity`, `ApplyFieldUpdate`, `ApplyMembershipChange` — each takes a
  `HumanDecision` id, re-validates the underlying `ProposedAction` hasn't gone stale
  (source data changed since proposal), applies via existing store methods
  (`UpsertMaintainerWithIdentity`, a new `MergeMaintainers` modeled on `MergeCompanies`),
  writes `AppliedChange` + `AuditLog`.

This mirrors the existing FOSSA-poller pattern (typed Go commands, `AuditLog` write) but
adds the missing human-approval gate that `AutoMaintainerAdder` currently skips.

---

## 7. Local LLM architecture

For v1 (low volume — a handful of operators, batch sizes in the dozens of ambiguous
candidates per session): **Ollama for local dev, llama.cpp-backed serving (or Ollama again)
for a first single-pod Kubernetes deployment.** vLLM and llm-d are both overkill at this
volume — vLLM's throughput advantages matter at concurrent-request scale this feature
will not reach for a long time; llm-d's distributed-serving value only shows up once
you're running multiple GPU nodes for concurrency, which a single-digit-operator internal
tool doesn't justify.

| | Local dev | First k8s deploy | CPU-only | GPU | Higher concurrency | Future distributed |
|---|---|---|---|---|---|---|
| Recommended | Ollama | Ollama or llama.cpp server, CPU-only pod | llama.cpp (GGUF, quantized) | still fine on llama.cpp/Ollama at this scale | vLLM if it ever comes to that | llm-d, only then |

Model characteristics, not a specific vendor: an instruct model in the 7-14B class,
quantized (Q4/Q5 GGUF) for CPU-friendliness, with demonstrated structured-output/JSON-mode
and tool-calling support (this rules out very old or purely completion-tuned checkpoints).
Context window needs are modest — a reconciliation prompt (schema + a handful of candidate
records + evidence) comfortably fits in 8-16K tokens; no need to chase 128K+ context.
Latency at this batch size (ranking/explaining a few ambiguous candidates) is not
latency-sensitive — a few seconds per candidate is acceptable, favoring quantized CPU
inference over provisioning a GPU node for an internal tool.

Privacy: local inference means PII (names/emails) stays inside the cluster, matching the
"sensitive identity data should not be sent to external model providers by default"
requirement — Claude remains an experimentation-only backend, opt-in, never the default for
production reconciliation of real PII.

Access pattern: a narrow internal HTTP service (`llm-gateway` or similar), OpenAI-compatible
API surface, so the Go application's AI interface can point at Claude, a local Ollama/llama.cpp
endpoint, or a future OpenAI-compatible endpoint via the same client code — no compiling a
model runtime into the Go binary.

llm-d becomes justified only if/when concurrent reconciliation load (many operators,
many simultaneous sessions, or a move to real-time chat-first UX) starts queuing on a single
model server — not something the first several phases will hit.

---

## 8. Training, retrieval, and evaluation strategy

**No fine-tuning for v1.** Use: schema-aware system prompt (entity definitions, trust
model from §3, tool list from §6) + few-shot examples curated by hand from the
*already-approved* dedup decisions in `maintainer_identity_with_names_preserved.md`'s
manual review process + retrieval over schema/policy docs (this document, the trust-model
table, `.project/maintainers.yaml` schema) for grounding, not over raw DB rows.

Labelled-dataset path: every `HumanDecision` (approve/reject/edit) on a `ProposedAction`
that itself carried `match_reason=llm_ranked` is a labelled example. Only decisions with
`decision=approve` and **no subsequent reversal audit event** within some cooldown window
become training candidates — `reject` and `edit` decisions are kept as *negative* examples
for eval, not silently dropped, but never enter a fine-tuning set as positive examples.
This directly prevents rejected/incorrect proposals from contaminating training data.
Fine-tuning itself is deferred until this labelled set is large and clean enough to
matter — likely not before Phase 6.

---

## 9. Safety and privacy design

- PII minimisation: tool calls to the LLM pass only the fields relevant to the decision at
  hand (name/email/github/company), never raw `RawPayload` blobs, never unrelated
  maintainers' data.
- Local inference by default for anything touching real maintainer PII (§7); Claude only
  for early experimentation against synthetic/anonymized fixtures.
- Logging: log tool calls and their arguments for audit (§6), but redact/hash email
  addresses in long-term logs where the audit trail doesn't need the raw value beyond
  `AppliedChange.before/after_snapshot`, which already carries it under normal DB access
  controls.
- Prompt/model-input retention: no indefinite raw-prompt storage; retain only what's needed
  to reconstruct a `ProposedAction`'s rationale (evidence refs + rationale text), not the
  full prompt/response transcript, past a short debugging window.
- Access controls: reconciliation sessions and proposal tools require the same
  authenticated web-bff session/authorisation the rest of the app already uses — no new
  auth surface.
- Tenant/project isolation: sessions are scoped to a project or an explicit dedup cluster;
  tools must reject cross-scope reads (`get_project_identity_observations` for a project the
  session isn't scoped to).
- Prompt injection from repository-controlled files: legacy maintainer refs and
  `.project/maintainers.yaml` are **untrusted text**. Never interpolate raw file content
  into a system/instruction position — only as clearly-delimited data the model is told to
  extract from, and outputs are validated against expected schemas (`validate_maintainers_yaml`)
  before being trusted, exactly as `refparse` already does deterministically today.
- Malicious YAML: parse with a safe loader, size/depth limits, and the existing
  `validate_maintainers_yaml` schema check before ever proposing it as an `ExportedFile`.
- Hallucinated identities / incorrect merges: enforced structurally, not by prompting —
  the model cannot call mutation commands (§6), every merge requires a `HumanDecision`, and
  `ApplyMergeIdentity` re-validates the underlying evidence hasn't gone stale before applying.
- OpenProfile terms/policy: confirm LFX API terms permit this caching/matching use case
  before building `get_openprofile` broadly — open question, see §13.

---

## 10. Evaluation plan

Pilot dataset: 10-20 real CNCF projects spanning "has `.project/maintainers.yaml` already"
and "still on legacy ref only," plus the known 122-duplicate cluster from
`maintainer_identity_with_names_preserved.md` as a fixed regression set with human-verified
ground truth.

Metrics and pilot acceptance thresholds:

| Metric | Pilot threshold |
|---|---|
| False merge rate (accepted merges later reversed) | **0 tolerated** in pilot — any false merge blocks graduation to Phase 4 |
| Duplicate detection recall (of the known 122-cluster set) | ≥ 90% surfaced as candidates (doesn't need to auto-resolve, just surface) |
| Candidate ranking accuracy (LLM's top suggestion matches human's eventual decision) | ≥ 80% on ambiguous-only subset |
| YAML generation validity (passes `validate_maintainers_yaml`) | 100% — this is deterministic-templated, should never fail |
| Human acceptance rate of LLM rationale as "correct/useful" (operator survey per session) | ≥ 70% |
| Time saved per project vs. manual Claude-chat workflow | tracked, no hard gate for pilot |
| Manual corrections per session | tracked, trend should decrease across pilot |

False merges are weighted far above missed matches — a missed match just means another
manual pass; a false merge corrupts identity data the way the *current* bug already did.

---

## 11. Phased delivery plan

- **Phase 0 — Document current manual workflow.** Write down the contractor's actual
  worksheet→Claude→YAML steps precisely (this document's §4 example is a start).
  *Deliverable:* short runbook. *Risk:* low. *Deferred:* everything else.

- **Phase 1 — Deterministic reconciliation report + fix the existing gate.**
  Ship the read-only comparison (§1, §4 steps 1-3) with **no LLM involvement** — worksheet
  vs. legacy ref vs. `.project/maintainers.yaml` vs. DB, surfaced as a report. In parallel,
  execute the already-written `maintainer_identity_with_names_preserved.md` plan (GitHub-
  before-email matching, sentinel blocking, partial unique index) since it's a prerequisite
  for trustworthy candidate matching later. Also: **gate `AutoMaintainerAdder`** behind this
  phase's review surface, or at minimum log/flag every auto-decision it makes so Phase 2's
  human review has visibility into what it already silently did.
  *Dependencies:* none new. *Success criteria:* report matches manual spreadsheet review on
  a sample of 5 projects.

- **Phase 2 — Human-reviewed duplicate resolution.** Build `CandidateMatch`/`MatchEvidence`/
  `ProposedAction`/`HumanDecision` tables and the approve/reject UI, driven purely by
  deterministic matching (no LLM yet). Apply via `ApplyMergeIdentity` etc.
  *Deliverable:* first real mutation path with audit trail. *Risk:* schema churn if §2's
  model needs revision under real data — budget for one iteration.

- **Phase 3 — AI explanations and candidate ranking.** Introduce the LLM, Claude-backed
  initially, for the ambiguous residual only (§1, §5). No new mutation surface — same
  commands as Phase 2, just better-ranked/explained proposals.
  *Success criteria:* candidate ranking accuracy threshold from §10 on a pilot subset.

- **Phase 4 — Guided `.project/maintainers.yaml` generation.** `generate_maintainers_yaml`
  end-to-end for the worked example in §4. *Deferred until now:* touching real repo files —
  Phase 4 still stops at "produce a reviewable file," no auto-commit/PR.

- **Phase 5 — Local model deployment.** Stand up the Ollama/llama.cpp gateway (§7),
  cut over from Claude for any session touching real PII. *Risk:* structured-output quality
  regression vs. Claude — hold Phase 3's accuracy threshold as the gate to cut over.

- **Phase 6 — Feedback dataset and possible fine-tuning.** Only after Phase 2-5 have
  produced enough clean, non-contaminated labelled decisions (§8). Fine-tuning is a
  "maybe," not a commitment.

- **Phase 7 — Broader rollout.** Beyond the pilot project set, plus OpenProfile
  verification (§6's `get_openprofile`) once the LFX client actually exists and terms are
  confirmed (§13) — this can start earlier than Phase 7 in parallel if the LFX blocker
  clears sooner, since OpenProfile access is largely independent of the reconciliation UI.

The first useful release (Phase 1-2) depends on **no** LLM and **no** OpenProfile client —
both are real blockers today (§0) and shouldn't gate early value.

---

## 12. Proposed first vertical slice

Refine the candidate slice slightly: rather than "one project, all five sources including
OpenProfile," start with **worksheet + legacy ref + `.project/maintainers.yaml` + DB only**
(drop OpenProfile from the first slice) — because the OpenProfile client doesn't exist yet
(§0) and building it is independent, non-trivial work that would otherwise block the whole
slice. Once that four-source slice works end-to-end (Phase 1-2's report → proposals →
approved merges/membership changes → generated YAML), add OpenProfile as a fifth source
in the same session shape, no redesign needed — `SourceRecord`/`MaintainerIdentityObservation`
already generalize to it.

## 13. Open design decisions

1. **Does `lfx/enricher.go` actually reach OpenProfile.dev, or a different LFX identity
   store?** Matters because §6's `get_openprofile` and §3's trust model assume it's the
   maintainer-controlled profile. Options: confirm via LFX team / API docs, or treat
   current enrichment as a placeholder pending confirmation. Default: verify before Phase 7;
   don't block Phase 1-2 on this.
2. **What should happen to `AutoMaintainerAdder`'s existing auto-apply behavior?** Options:
   disable it, log-only mode, or leave it and treat its output as another observation
   source for the new review flow. Default: log-only during Phase 1, revisit once Phase 2's
   review UI exists. Evidence that would change this: if `AutoMaintainerAdder`'s current
   error rate turns out to be low in practice, less urgency to gate it.
3. **Is a maintainer allowed to have more than one canonical identity temporarily during
   review, or must `CanonicalIdentity` be 1:1 with `Maintainer` at all times?** Matters for
   schema design in §2. Default: allow N pending `CandidateMatch` rows against one
   `Maintainer` with no forced 1:1 until a merge is applied.
4. **Where does the "operator" identity come from for `HumanDecision.decided_by` /
   `AuditLog`?** Default: reuse the existing web-bff session/auth user, no new identity
   concept.
5. **Should `generate_maintainers_yaml` ever auto-open a PR, or only produce a downloadable
   file?** Default: file only for v1 (matches §4/§11). Evidence to revisit: pilot feedback
   that manual copy/PR is the remaining bottleneck.
6. **LFX API terms for caching maintainer PII fetched via OpenProfile** — matters for §9.
   Default: assume caching is fine for operational use but confirm explicitly with the LFX
   team before Phase 7, don't guess.
7. **Should the four-category taxonomy in `maintainer_identity_with_names_preserved.md` be
   reused verbatim for cross-source reconciliation, or does cross-source matching need a
   fifth category (e.g. `SOURCE_CONFLICT` for cases where two authoritative sources
   disagree, not just two DB rows)?** Default: add `SOURCE_CONFLICT` explicitly — the
   existing four were designed for in-DB dedup only.
8. **Volume/SLA for the LFX API** — matters for §7 caching/rate-limit design. Default:
   assume low-volume/no published SLA; cache aggressively; revisit if LFX team provides one.

## 14. Clarifying questions

1. Does `lfx/enricher.go` call OpenProfile.dev specifically, or a different LFX identity
   endpoint — and is there existing internal documentation on which LFX API surfaces
   maintainer profile data?
2. Who are the intended operators of this feature — is it still exclusively the contractor
   doing manual reconciliation today, or should CNCF project maintainers themselves ever get
   a self-service view of their own conflicting records?
3. Is there an existing or planned webhook/notification when OpenProfile data changes, or
   would this feature need to poll?
4. For the pilot (§10), which ~10-20 projects would you actually want used, and is the
   122-duplicate cluster from the existing spec available as a fixed test fixture already,
   or does it need to be snapshotted first?
5. Is `AutoMaintainerAdder`'s current auto-apply behavior something you're already aware
   of/comfortable with, or is gating it (Open Decision #2) itself a wanted deliverable
   regardless of this larger feature?
6. Do you want `.project/maintainers.yaml` generation to ever open a PR automatically in
   any phase, or is "produce a file for manual PR" the permanent end state for this tool?
7. What's the expected concurrency — how many operators would use this at once — since
   that's the main variable that would justify vLLM/llm-d earlier than §7 recommends?
8. Is there a legal/compliance owner (CNCF, LF) who needs to sign off on caching OpenProfile
   PII before `get_openprofile` is built, or is that purely an engineering decision?
9. Should Claude ever be usable in production for real (non-synthetic) maintainer PII under
   any circumstances (e.g. with a BAA/DPA in place), or is local-only a hard requirement
   regardless of contractual terms?
10. Is there a target date or event (e.g. a CNCF projects-team milestone) driving this, or
    is the phased plan in §11 free to run at whatever pace the pilot data supports?
