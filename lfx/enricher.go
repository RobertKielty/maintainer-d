package lfx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"maintainerd/dotproject"
	"maintainerd/model"
)

type ObservationStore interface {
	GetMaintainerMapByGitHubAccount() (map[string]model.Maintainer, error)
	ListMaintainersWithoutIdentityObservation(source string) ([]model.Maintainer, error)
	ListMaintainersActiveOnAnyProject(maintainerIDs []uint) (map[uint]bool, error)
	UpsertMaintainerIdentityObservation(observation *model.MaintainerIdentityObservation) (*model.MaintainerIdentityObservation, error)
	RetireStaleMaintainerIdentityObservations(source string, projectID *uint, sourceRef string, keepSourceUserIDs []string, observedBefore time.Time) (int64, error)
}

type UserSearcher interface {
	SearchUsers(ctx context.Context, query UserSearch) ([]User, error)
	GetUserIdentities(ctx context.Context, salesforceID string) ([]Identity, error)
}

type Enricher struct {
	Store      ObservationStore
	Client     UserSearcher
	EnrichAll  bool
	MaxLookups int
	Now        func() time.Time
	Progress   func(EnrichmentProgress)

	allMaintainersDone bool
}

type EnrichmentProgress struct {
	Total     int
	Processed int
	Current   string
	Summary   dotproject.EnrichmentSummary
}

func (e *Enricher) EnrichProject(ctx context.Context, project model.Project, result *dotproject.DiscoveryResult) (dotproject.EnrichmentSummary, error) {
	if e == nil || e.Store == nil || e.Client == nil {
		return dotproject.EnrichmentSummary{}, fmt.Errorf("lfx enricher store and client are required")
	}
	now := time.Now().UTC()
	if e.Now != nil {
		now = e.Now().UTC()
	}

	summary := dotproject.EnrichmentSummary{}
	candidates, err := e.candidates(project, result)
	if err != nil {
		return summary, err
	}
	total := len(candidates)
	e.reportProgress(total, 0, "", summary)
	processed := 0
	for _, candidate := range candidates {
		current := candidateLabel(candidate)
		if e.MaxLookups > 0 && summary.Attempted >= e.MaxLookups {
			summary.SkippedLimit++
			processed++
			e.reportProgress(total, processed, current, summary)
			continue
		}
		projectID := &project.ID
		if candidate.Global {
			projectID = nil
		}
		if err := e.enrichCandidate(ctx, projectID, candidate, now, &summary); err != nil {
			summary.Errored++
			processed++
			e.reportProgress(total, processed, current, summary)
			return summary, err
		}
		processed++
		e.reportProgress(total, processed, current, summary)
	}
	if e.EnrichAll && summary.SkippedLimit == 0 {
		e.allMaintainersDone = true
	}
	return summary, nil
}

func (e *Enricher) reportProgress(total, processed int, current string, summary dotproject.EnrichmentSummary) {
	if e == nil || e.Progress == nil {
		return
	}
	e.Progress(EnrichmentProgress{
		Total:     total,
		Processed: processed,
		Current:   current,
		Summary:   summary,
	})
}

type candidate struct {
	Maintainer *model.Maintainer
	GitHubUser string
	Email      string
	SourceRef  string
	Global     bool
}

func (e *Enricher) candidates(project model.Project, result *dotproject.DiscoveryResult) ([]candidate, error) {
	seen := make(map[string]struct{})
	candidates := make([]candidate, 0)
	if !e.EnrichAll && result != nil && result.MaintainersFile.Exists {
		byHandle, err := e.Store.GetMaintainerMapByGitHubAccount()
		if err != nil {
			return nil, err
		}
		handles, status, _ := dotproject.ParseProjectMaintainerHandles(result.MaintainersFile.Body)
		if status != dotproject.ParseStatusParsed {
			return candidates, nil
		}
		for _, handle := range handles {
			key := strings.ToLower(strings.TrimSpace(handle))
			if key == "" {
				continue
			}
			maintainer, ok := byHandle[key]
			if ok {
				candidates = appendCandidate(candidates, seen, candidate{
					Maintainer: &maintainer,
					GitHubUser: maintainer.GitHubAccount,
					Email:      preferredEmail(maintainer),
					SourceRef:  "github:" + key,
				})
				continue
			}
			candidates = appendCandidate(candidates, seen, candidate{
				GitHubUser: key,
				SourceRef:  "github:" + key,
			})
		}
	}

	if e.EnrichAll && !e.allMaintainersDone {
		maintainers, err := e.Store.ListMaintainersWithoutIdentityObservation("lfx")
		if err != nil {
			return nil, err
		}
		maintainerIDs := make([]uint, 0, len(maintainers))
		for _, maintainer := range maintainers {
			maintainerIDs = append(maintainerIDs, maintainer.ID)
		}
		activeOnAnyProject, err := e.Store.ListMaintainersActiveOnAnyProject(maintainerIDs)
		if err != nil {
			return nil, err
		}
		for _, maintainer := range maintainers {
			if !activeOnAnyProject[maintainer.ID] {
				continue
			}
			candidates = appendCandidate(candidates, seen, candidate{
				Maintainer: &maintainer,
				GitHubUser: maintainer.GitHubAccount,
				Email:      preferredEmail(maintainer),
				SourceRef:  "maintainer-d:" + fmt.Sprintf("%d", maintainer.ID),
				Global:     true,
			})
		}
	}

	_ = project
	return candidates, nil
}

func appendCandidate(candidates []candidate, seen map[string]struct{}, candidate candidate) []candidate {
	key := candidate.SourceRef
	if candidate.Maintainer != nil {
		key = fmt.Sprintf("maintainer:%d", candidate.Maintainer.ID)
	}
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" {
		return candidates
	}
	if _, ok := seen[key]; ok {
		return candidates
	}
	seen[key] = struct{}{}
	return append(candidates, candidate)
}

func candidateLabel(candidate candidate) string {
	if candidate.Maintainer != nil {
		if github := strings.TrimSpace(candidate.Maintainer.GitHubAccount); github != "" {
			return github
		}
		if email := strings.TrimSpace(candidate.Maintainer.Email); email != "" {
			return email
		}
		return fmt.Sprintf("maintainer:%d", candidate.Maintainer.ID)
	}
	if github := strings.TrimSpace(candidate.GitHubUser); github != "" {
		return github
	}
	if email := strings.TrimSpace(candidate.Email); email != "" {
		return email
	}
	return strings.TrimSpace(candidate.SourceRef)
}

func (e *Enricher) enrichCandidate(ctx context.Context, projectID *uint, candidate candidate, now time.Time, summary *dotproject.EnrichmentSummary) error {
	githubUser := normalized(candidate.GitHubUser)
	email := normalized(candidate.Email)
	if githubUser == "" || githubUser == "github_missing" {
		githubUser = ""
	}
	if email == "" || email == "email_missing" || email == "github_email_missing" {
		email = ""
	}
	if githubUser == "" && email == "" {
		summary.Unmatched++
		return e.writeObservation(projectID, candidate, nil, nil, now, "unmatched", "no github handle or email available", "")
	}

	summary.Attempted++
	users, matched, err := e.searchUsers(ctx, githubUser, email)
	if err != nil {
		// A done run context is not this candidate's failure, and recording
		// it would overwrite the candidate's previously good row (the upsert
		// replaces every column). Propagate so the caller stops the run
		// cleanly instead of writing a spurious error row per candidate.
		if ctx.Err() != nil {
			return PlatformAccessError(err)
		}
		if writeErr := e.writeObservation(projectID, candidate, nil, nil, now, "error", err.Error(), ""); writeErr != nil {
			return fmt.Errorf("%w; failed to record LFX error observation: %v", PlatformAccessError(err), writeErr)
		}
		return PlatformAccessError(err)
	}
	switch len(users) {
	case 0:
		summary.Unmatched++
		if err := e.writeObservation(projectID, candidate, nil, nil, now, "unmatched", "no LFX user matched github handle, email, or username", ""); err != nil {
			return err
		}
		// The search genuinely ran and found nothing this time, so every
		// profile previously recorded for this candidate is stale (LFX
		// resolved a duplicate, or the identity backing it changed).
		e.retireStaleProfiles(projectID, candidate, nil, now)
		return nil
	case 1:
		summary.Matched++
		user := users[0]
		identities, err := e.Client.GetUserIdentities(ctx, user.ID)
		if err != nil {
			// Same run-context guard as above: never trade a good row for a
			// deadline artifact.
			if ctx.Err() != nil {
				return PlatformAccessError(err)
			}
			// The search already identified the profile, so record it on the
			// error row: without SourceUserID the UI's per-profile grouping
			// cannot tie the failure to the profile, leaving an older
			// successful row looking current.
			if writeErr := e.writeObservation(projectID, candidate, &user, nil, now, "error", err.Error(), ""); writeErr != nil {
				return fmt.Errorf("%w; failed to record LFX error observation: %v", PlatformAccessError(err), writeErr)
			}
			return PlatformAccessError(err)
		}
		confidence := confidenceFor(user, identities, githubUser, email, matched)
		if err := e.writeObservation(projectID, candidate, &user, identities, now, "matched", "single LFX user match", confidence); err != nil {
			return err
		}
		// Only one profile matches now; retire any other profile rows a
		// previous, more ambiguous lookup left behind.
		e.retireStaleProfiles(projectID, candidate, []string{strings.TrimSpace(user.ID)}, now)
		return nil
	default:
		summary.Ambiguous++
		return e.enrichMultipleMatches(ctx, projectID, candidate, users, githubUser, email, matched, now, summary)
	}
}

// retireStaleProfiles deletes previously recorded LFX profile rows for this
// candidate that the current lookup did not return. It never aborts
// enrichment: a retirement failure is logged-equivalent (returned error is
// deliberately dropped) since the stale row is a display nuisance, not data
// loss, and must not turn a successful lookup into a failed one.
func (e *Enricher) retireStaleProfiles(projectID *uint, candidate candidate, keepSourceUserIDs []string, now time.Time) {
	sourceRef := strings.TrimSpace(candidate.SourceRef)
	if sourceRef == "" {
		return
	}
	_, _ = e.Store.RetireStaleMaintainerIdentityObservations("lfx", projectID, sourceRef, keepSourceUserIDs, now) //nolint:errcheck // deliberately dropped, see doc comment above
}

// scoredCandidate pairs an LFX user match with the identities and confidence
// fetched for it, so rankCandidates can pick the best one without re-fetching.
type scoredCandidate struct {
	user        User
	identities  []Identity
	confidence  string
	identityErr error
}

// enrichMultipleMatches writes one observation row per LFX profile matched
// for a single GitHub handle/email, rather than collapsing them into a
// single opaque blob. Each candidate's own identities are fetched so its
// IdentityCount and confidence are independently correct; the best-ranked
// candidate is marked "chosen" (and drives canonical field promotion via the
// caller's fill-only-if-missing policy), the rest "duplicate". An
// identity-fetch failure for one candidate is recorded as its own row and
// must not abort the rest of the group.
func (e *Enricher) enrichMultipleMatches(ctx context.Context, projectID *uint, candidate candidate, users []User, githubUser, email string, matched matchedBy, now time.Time, summary *dotproject.EnrichmentSummary) error {
	scored := make([]scoredCandidate, 0, len(users))
	for _, user := range users {
		identities, err := e.Client.GetUserIdentities(ctx, user.ID)
		sc := scoredCandidate{user: user, identities: identities, identityErr: err}
		if err != nil {
			classified := PlatformAccessError(err)
			// A done run context dooms every remaining fetch and is not a
			// per-profile failure: recording it would overwrite this
			// profile's previously good row via the upsert. Propagate so the
			// caller stops the run cleanly.
			if ctx.Err() != nil {
				return classified
			}
			// Per-profile tolerance only covers nonfatal failures (timeouts,
			// 5xx, transport). A fatal classification (dead token, rate
			// limit) would hit every remaining request too, so stop fetching
			// immediately - after recording this profile's failure as its own
			// row - instead of burning a doomed request per remaining profile.
			var fatal dotproject.FatalSyncError
			if errors.As(classified, &fatal) {
				// No summary.Errored++ here: EnrichProject counts every
				// propagated error, so a local increment would report one
				// failed profile as two LFX errors. The local count is only
				// for tolerated failures that return nil.
				if werr := e.writeObservation(projectID, candidate, &user, nil, now, "error", err.Error(), ""); werr != nil {
					return fmt.Errorf("%w; failed to record LFX error observation for duplicate profile: %v", classified, werr)
				}
				return classified
			}
		} else {
			sc.confidence = confidenceFor(user, identities, githubUser, email, matched)
		}
		scored = append(scored, sc)
	}
	rankCandidates(scored)

	total := len(scored)
	keepSourceUserIDs := make([]string, 0, len(scored))
	for i, sc := range scored {
		keepSourceUserIDs = append(keepSourceUserIDs, strings.TrimSpace(sc.user.ID))
		if sc.identityErr != nil {
			// A nonfatal failure (fatal ones short-circuited during fetching)
			// is recorded as this profile's own row and must not abort the
			// group, but it still has to count as an error or the run reports
			// lfx_errored=0 while error rows accumulate.
			summary.Errored++
			if err := e.writeObservation(projectID, candidate, &sc.user, nil, now, "error", sc.identityErr.Error(), ""); err != nil {
				return fmt.Errorf("failed to record LFX error observation for duplicate profile: %w", err)
			}
			continue
		}
		status := "duplicate"
		if i == 0 {
			status = "chosen"
		}
		reason := fmt.Sprintf("%d of %d LFX profiles for this GitHub handle", i+1, total)
		if err := e.writeObservation(projectID, candidate, &sc.user, sc.identities, now, status, reason, sc.confidence); err != nil {
			return err
		}
	}
	// This lookup's full profile set is now known (every scored candidate,
	// including ones whose identity fetch merely failed this run - only a
	// profile LFX no longer returns at all should be retired).
	e.retireStaleProfiles(projectID, candidate, keepSourceUserIDs, now)
	return nil
}

// rankCandidates orders scored candidates best-first: higher confidence,
// then contact over lead, then more linked identities, then a more recently
// modified LFX record, then SourceUserID as a stable final tiebreak so
// ordering is never ambiguous. Candidates whose identity fetch failed sort
// last without disturbing the relative order of the rest.
func rankCandidates(scored []scoredCandidate) {
	sort.SliceStable(scored, func(i, j int) bool {
		a, b := scored[i], scored[j]
		if (a.identityErr != nil) != (b.identityErr != nil) {
			return a.identityErr == nil
		}
		if a.identityErr != nil && b.identityErr != nil {
			return false
		}
		if ra, rb := confidenceRank(a.confidence), confidenceRank(b.confidence); ra != rb {
			return ra > rb
		}
		if ac, bc := isContactType(a.user.Type), isContactType(b.user.Type); ac != bc {
			return ac
		}
		if len(a.identities) != len(b.identities) {
			return len(a.identities) > len(b.identities)
		}
		// An unparseable timestamp maps to the zero time so the comparison
		// stays a total order: falling back to ID only for mixed
		// valid/invalid pairs made the less-func non-transitive, and a
		// non-transitive comparator lets sort pick the wrong "chosen" row.
		at := lfxTimestampOrZero(a.user.LastModifiedDate)
		bt := lfxTimestampOrZero(b.user.LastModifiedDate)
		if !at.Equal(bt) {
			return at.After(bt)
		}
		return a.user.ID < b.user.ID
	})
}

// matchedBy records which query actually produced a result, so confidence
// can be based on how a user was found rather than re-derived from which
// inputs happened to be supplied.
type matchedBy int

const (
	matchedByNone matchedBy = iota
	matchedByGitHubID
	matchedByEmail
	matchedByUsername
)

func (e *Enricher) searchUsers(ctx context.Context, githubUser, email string) ([]User, matchedBy, error) {
	if githubUser != "" {
		users, err := e.Client.SearchUsers(ctx, UserSearch{GitHubID: githubUser, PageSize: 100})
		if err != nil {
			return nil, matchedByNone, err
		}
		if len(users) > 0 {
			return users, matchedByGitHubID, nil
		}
	}
	if email != "" {
		users, err := e.Client.SearchUsers(ctx, UserSearch{Email: email, PageSize: 100})
		if err != nil {
			return nil, matchedByNone, err
		}
		if len(users) > 0 {
			return users, matchedByEmail, nil
		}
	}
	// Some LFX/PCC records have no GithubID field populated at all, but the
	// person set their LF Username (the openprofile.dev slug) to match their
	// GitHub handle. Try that as a last resort before giving up - it's a
	// coincidental string match, not a verified linkage, so confidenceFor
	// scores it "weak" unless a confirmed github identity rescues it.
	if githubUser != "" {
		users, err := e.Client.SearchUsers(ctx, UserSearch{Username: githubUser, PageSize: 100})
		if err != nil {
			return nil, matchedByNone, err
		}
		if len(users) > 0 {
			return users, matchedByUsername, nil
		}
	}
	return nil, matchedByNone, nil
}

// observationPayload is the on-disk shape written to
// MaintainerIdentityObservation.RawPayload. It preserves the raw bytes LFX
// returned (User.Raw / Identity.Raw) rather than re-marshaling the typed
// structs, so fields we haven't modeled yet survive and can be mined later.
// The "user"/"identities" keys must stay stable: writeRawObservation
// re-unmarshals this exact shape to extract SourceUserID/Name/Email/LFID/
// CompanyName.
type observationPayload struct {
	User       json.RawMessage   `json:"user"`
	Identities []json.RawMessage `json:"identities,omitempty"`
}

func (e *Enricher) writeObservation(projectID *uint, candidate candidate, user *User, identities []Identity, now time.Time, status, reason, confidence string) error {
	rawPayload := ""
	if user != nil {
		userRaw := user.Raw
		if len(userRaw) == 0 {
			// Raw is empty for synthesized data (e.g. tests) that construct
			// a User directly instead of decoding one from the API. Fall
			// back to marshaling the typed struct so callers still get a
			// usable payload.
			body, err := json.Marshal(user)
			if err != nil {
				return err
			}
			userRaw = body
		}
		identitiesRaw := make([]json.RawMessage, 0, len(identities))
		for _, identity := range identities {
			identityRaw := identity.Raw
			if len(identityRaw) == 0 {
				body, err := json.Marshal(identity)
				if err != nil {
					return err
				}
				identityRaw = body
			}
			identitiesRaw = append(identitiesRaw, identityRaw)
		}
		body, err := json.Marshal(observationPayload{User: userRaw, Identities: identitiesRaw})
		if err != nil {
			return err
		}
		rawPayload = string(body)
	}
	return e.writeRawObservation(projectID, candidate, now, status, reason, confidence, rawPayload)
}

func (e *Enricher) writeRawObservation(projectID *uint, candidate candidate, now time.Time, status, reason, confidence, rawPayload string) error {
	var maintainerID *uint
	if candidate.Maintainer != nil {
		id := candidate.Maintainer.ID
		maintainerID = &id
	}
	observation := &model.MaintainerIdentityObservation{
		MaintainerID: maintainerID,
		ProjectID:    projectID,
		Source:       "lfx",
		SourceRef:    candidate.SourceRef,
		GitHubUser:   strings.TrimSpace(candidate.GitHubUser),
		Email:        strings.TrimSpace(candidate.Email),
		MatchStatus:  status,
		MatchReason:  reason,
		Confidence:   confidence,
		RawPayload:   rawPayload,
		ObservedAt:   now,
	}
	if rawPayload != "" {
		var payload observationPayload
		var user User
		if err := json.Unmarshal([]byte(rawPayload), &payload); err == nil {
			if err := json.Unmarshal(payload.User, &user); err == nil {
				observation.SourceUserID = strings.TrimSpace(user.ID)
				observation.Name = firstNonEmpty(user.Name, strings.TrimSpace(user.FirstName+" "+user.LastName))
				observation.Email = firstNonEmpty(user.Email, observation.Email)
				observation.LFID = user.Username
				observation.CompanyName = accountCompanyName(user.Account)
				observation.SourceUserType = strings.TrimSpace(user.Type)
				observation.SourceGitHubID = strings.TrimSpace(user.GithubID)
				if modifiedAt, err := parseLFXTimestamp(user.LastModifiedDate); err == nil {
					observation.SourceLastModifiedAt = &modifiedAt
				}
			}
			// An error row's payload carries the user but the identities
			// request failed, so no count was measured - persisting 0 would
			// claim "measured zero identities". Leave it nil (unmeasured).
			if status != "error" {
				identityCount := len(payload.Identities)
				observation.IdentityCount = &identityCount
			}
		}
	}
	_, err := e.Store.UpsertMaintainerIdentityObservation(observation)
	return err
}

func confidenceFor(user User, identities []Identity, githubUser, email string, matched matchedBy) string {
	for _, identity := range identities {
		if strings.EqualFold(strings.TrimSpace(identity.Source), "github") && githubUser != "" && strings.EqualFold(identity.Username, githubUser) {
			return "exact"
		}
	}
	if email != "" && strings.EqualFold(user.Email, email) {
		return "strong"
	}
	// Granting "strong" here must be based on how the user was actually
	// found (matched == matchedByGitHubID), not merely on whether a handle
	// was supplied to the query - searchUsers falls back to an email-only
	// search when the GitHub-ID search returns nothing, so a purely
	// email-matched user must not inherit "strong" confidence from an
	// unrelated handle that was passed in alongside the email.
	//
	// A bare GitHub-ID match is only promoted to "strong" when the LFX
	// record is a "contact" (a claimed profile). A "lead" is a stale,
	// never-claimed Salesforce row - see LFX-USER-API-NOTES.MD finding 8 -
	// and demotes to "weak" unless the identity-confirmed path above
	// already rescued it. This is a demotion signal, not a hard gate: a
	// lead can still carry confirmed identities, which is exactly what
	// the loop above already checks first.
	if githubUser != "" && matched == matchedByGitHubID && strings.EqualFold(strings.TrimSpace(user.Type), "contact") {
		return "strong"
	}
	return "weak"
}

// confidenceRank orders confidence tiers so the ranking helper can compare
// them numerically; higher is better.
func confidenceRank(confidence string) int {
	switch confidence {
	case "exact":
		return 3
	case "strong":
		return 2
	case "weak":
		return 1
	default:
		return 0
	}
}

func isContactType(userType string) bool {
	return strings.EqualFold(strings.TrimSpace(userType), "contact")
}

// parseLFXTimestamp parses the LastModifiedDate LFX returns from
// /user-service/v2/users/search, which has been observed as RFC3339
// (optionally with fractional seconds).
// lfxTimestampOrZero collapses an unparseable LastModifiedDate to the zero
// time, which sorts after every valid timestamp under "newer first". This
// keeps rankCandidates' less-func a total order.
func lfxTimestampOrZero(value string) time.Time {
	t, err := parseLFXTimestamp(value)
	if err != nil {
		return time.Time{}
	}
	return t
}

func parseLFXTimestamp(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339Nano, value)
}

func preferredEmail(maintainer model.Maintainer) string {
	if email := strings.TrimSpace(maintainer.Email); email != "" && email != "EMAIL_MISSING" {
		return email
	}
	if email := strings.TrimSpace(maintainer.GitHubEmail); email != "" && email != "GITHUB_EMAIL_MISSING" && email != "GITHUB_MISSING" {
		return email
	}
	return ""
}

func normalized(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func accountCompanyName(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	return firstNonEmpty(
		mapStringValue(payload, "Company"),
		mapStringValue(payload, "company"),
		mapStringValue(payload, "CompanyName"),
		mapStringValue(payload, "companyName"),
		mapStringValue(payload, "Name"),
		mapStringValue(payload, "name"),
	)
}

func mapStringValue(payload map[string]any, key string) string {
	value, ok := payload[key]
	if !ok {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case map[string]any:
		return firstNonEmpty(
			mapStringValue(typed, "Name"),
			mapStringValue(typed, "name"),
			mapStringValue(typed, "CompanyName"),
			mapStringValue(typed, "companyName"),
		)
	default:
		return ""
	}
}
