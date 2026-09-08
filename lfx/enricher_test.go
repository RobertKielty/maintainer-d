package lfx

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"maintainerd/dotproject"
	"maintainerd/model"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type fakeObservationStore struct {
	maintainers map[string]model.Maintainer
	captured    *model.MaintainerIdentityObservation
}

func (f fakeObservationStore) GetMaintainerMapByGitHubAccount() (map[string]model.Maintainer, error) {
	result := make(map[string]model.Maintainer, len(f.maintainers))
	for key, maintainer := range f.maintainers {
		result[key] = maintainer
	}
	return result, nil
}

func (f fakeObservationStore) ListMaintainersWithoutIdentityObservation(string) ([]model.Maintainer, error) {
	return nil, nil
}

func (f fakeObservationStore) ListMaintainersActiveOnAnyProject(maintainerIDs []uint) (map[uint]bool, error) {
	active := make(map[uint]bool, len(maintainerIDs))
	for _, id := range maintainerIDs {
		active[id] = true
	}
	return active, nil
}

func (f fakeObservationStore) UpsertMaintainerIdentityObservation(observation *model.MaintainerIdentityObservation) (*model.MaintainerIdentityObservation, error) {
	if f.captured != nil {
		*f.captured = *observation
	}
	return observation, nil
}

func (f fakeObservationStore) RetireStaleMaintainerIdentityObservations(string, *uint, string, []string, time.Time) (int64, error) {
	return 0, nil
}

type fakeUserSearcher struct {
	calls int
}

func (f *fakeUserSearcher) SearchUsers(context.Context, UserSearch) ([]User, error) {
	f.calls++
	return nil, nil
}

func (f *fakeUserSearcher) GetUserIdentities(context.Context, string) ([]Identity, error) {
	f.calls++
	return nil, nil
}

// fakeSingleUserSearcher returns a fixed single-user match, with Raw bytes
// captured exactly as the real Client.SearchUsers/GetUserIdentities do.
type fakeSingleUserSearcher struct {
	user       User
	identities []Identity
}

func (f *fakeSingleUserSearcher) SearchUsers(context.Context, UserSearch) ([]User, error) {
	return []User{f.user}, nil
}

func (f *fakeSingleUserSearcher) GetUserIdentities(context.Context, string) ([]Identity, error) {
	return f.identities, nil
}

func TestWriteObservationPreservesRawBytesBeyondTypedFields(t *testing.T) {
	t.Parallel()

	// "Nickname" is not a field on User. It stands in for any real LFX
	// response field we haven't modeled yet - the point of this test is
	// that such fields survive into RawPayload instead of being dropped.
	userRaw := []byte(`{"ID":"sfid-synthetic-001","FirstName":"Test","LastName":"Fixture","Name":"Test Fixture","Email":"fixture@example.com","Username":"test-fixture-handle","Account":{"Name":"Example Org"},"Nickname":"unmodelled-field-value"}`)
	var user User
	require.NoError(t, unmarshalStrict(userRaw, &user))
	user.Raw = userRaw

	identityRaw := []byte(`{"ID":"identity-synthetic-001","UserSFID":"sfid-synthetic-001","Username":"test-fixture-handle","Source":"github","IsVerified":true,"Badge":"unmodelled-identity-field"}`)
	var identity Identity
	require.NoError(t, unmarshalStrict(identityRaw, &identity))
	identity.Raw = identityRaw

	var captured model.MaintainerIdentityObservation
	enricher := &Enricher{
		Store: fakeObservationStore{maintainers: map[string]model.Maintainer{}, captured: &captured},
		Client: &fakeSingleUserSearcher{
			user:       user,
			identities: []Identity{identity},
		},
	}

	var summary dotproject.EnrichmentSummary
	err := enricher.enrichCandidate(context.Background(), nil, candidate{
		GitHubUser: "test-fixture-handle",
		SourceRef:  "github:test-fixture-handle",
	}, time.Now().UTC(), &summary)
	require.NoError(t, err)

	assert.Contains(t, captured.RawPayload, "unmodelled-field-value", "raw bytes beyond the typed struct must survive into RawPayload")
	assert.Contains(t, captured.RawPayload, "unmodelled-identity-field")

	// Typed extraction must still work off the preserved raw bytes.
	assert.Equal(t, "sfid-synthetic-001", captured.SourceUserID)
	assert.Equal(t, "Test Fixture", captured.Name)
	assert.Equal(t, "fixture@example.com", captured.Email)
	assert.Equal(t, "test-fixture-handle", captured.LFID)
	assert.Equal(t, "Example Org", captured.CompanyName)
	assert.Equal(t, "exact", captured.Confidence)
}

// unmarshalStrict is a small helper to decode fixture JSON into a typed
// struct while keeping the test's raw bytes and the decoded struct
// authored from the same literal, avoiding drift between them.
func unmarshalStrict(raw []byte, target any) error {
	return json.Unmarshal(raw, target)
}

func TestConfidenceFor(t *testing.T) {
	t.Parallel()

	user := User{Email: "fixture@example.com"}

	tests := []struct {
		name       string
		user       User
		identities []Identity
		githubUser string
		email      string
		matched    matchedBy
		want       string
	}{
		{
			name:       "verified github identity matches supplied handle",
			user:       user,
			identities: []Identity{{Source: "github", Username: "fixture-handle"}},
			githubUser: "fixture-handle",
			matched:    matchedByGitHubID,
			want:       "exact",
		},
		{
			name:       "matched by github-ID query, contact record, no verified identity",
			user:       User{Email: "fixture@example.com", Type: "contact"},
			githubUser: "fixture-handle",
			matched:    matchedByGitHubID,
			want:       "strong",
		},
		{
			name:       "matched by github-ID query, lead record, no verified identity demotes to weak",
			user:       User{Email: "fixture@example.com", Type: "lead"},
			githubUser: "fixture-handle",
			matched:    matchedByGitHubID,
			want:       "weak",
		},
		{
			name:    "matched by email query, email matches exactly",
			user:    user,
			email:   "fixture@example.com",
			matched: matchedByEmail,
			want:    "strong",
		},
		{
			name:       "email-fallback match while a handle was supplied must not inherit strong",
			user:       User{Email: "other@example.com"},
			githubUser: "fixture-handle",
			email:      "fixture@example.com",
			matched:    matchedByEmail,
			want:       "weak",
		},
		{
			name: "no match signal at all",
			user: user,
			want: "weak",
		},
		{
			name:       "lead record with a confirmed github identity still scores exact",
			user:       User{Email: "fixture@example.com", Type: "lead"},
			identities: []Identity{{Source: "github", Username: "fixture-handle"}},
			githubUser: "fixture-handle",
			matched:    matchedByGitHubID,
			want:       "exact",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := confidenceFor(tc.user, tc.identities, tc.githubUser, tc.email, tc.matched)
			assert.Equal(t, tc.want, got)
		})
	}
}

// fakeGitHubMissEmailHitSearcher simulates the real searchUsers fallback:
// the GitHub-ID query returns nothing, so it falls back to an email query
// that returns a user whose email does not corroborate the supplied handle.
type fakeGitHubMissEmailHitSearcher struct {
	emailMatch User
}

func (f *fakeGitHubMissEmailHitSearcher) SearchUsers(_ context.Context, query UserSearch) ([]User, error) {
	if query.GitHubID != "" {
		return nil, nil
	}
	if query.Email != "" {
		return []User{f.emailMatch}, nil
	}
	return nil, nil
}

func (f *fakeGitHubMissEmailHitSearcher) GetUserIdentities(context.Context, string) ([]Identity, error) {
	return nil, nil
}

func TestEnrichCandidateDoesNotGrantStrongOnEmailFallbackMatch(t *testing.T) {
	t.Parallel()

	var captured model.MaintainerIdentityObservation
	enricher := &Enricher{
		Store: fakeObservationStore{maintainers: map[string]model.Maintainer{}, captured: &captured},
		Client: &fakeGitHubMissEmailHitSearcher{
			// LFX's email search can match on a secondary/alternate email
			// that differs from the primary Email field the API returns -
			// this is the case the regression guards against: the matched
			// user's own Email doesn't corroborate the query email either.
			emailMatch: User{ID: "sfid-fixture-2", Email: "secondary@example.com", Username: "unrelated-handle"},
		},
	}

	var summary dotproject.EnrichmentSummary
	err := enricher.enrichCandidate(context.Background(), nil, candidate{
		GitHubUser: "fixture-handle",
		Email:      "fixture@example.com",
		SourceRef:  "github:fixture-handle",
	}, time.Now().UTC(), &summary)
	require.NoError(t, err)

	assert.Equal(t, "weak", captured.Confidence, "an email-only match must not inherit strong confidence just because a github handle was also supplied")
}

// fakeUsernameOnlySearcher simulates an LFX/PCC record with no GithubID
// field populated and no email, but whose LF Username (the openprofile.dev
// slug) happens to match the GitHub handle - the case searchUsers' username
// fallback exists to catch.
type fakeUsernameOnlySearcher struct {
	usernameMatch User
}

func (f *fakeUsernameOnlySearcher) SearchUsers(_ context.Context, query UserSearch) ([]User, error) {
	if query.Username != "" {
		return []User{f.usernameMatch}, nil
	}
	return nil, nil
}

func (f *fakeUsernameOnlySearcher) GetUserIdentities(context.Context, string) ([]Identity, error) {
	return nil, nil
}

func TestEnrichCandidateFallsBackToUsernameSearch(t *testing.T) {
	t.Parallel()

	var captured model.MaintainerIdentityObservation
	enricher := &Enricher{
		Store: fakeObservationStore{maintainers: map[string]model.Maintainer{}, captured: &captured},
		Client: &fakeUsernameOnlySearcher{
			usernameMatch: User{
				ID:        "sfid-fixture-3",
				Username:  "fixture-handle",
				FirstName: "Fixture",
				LastName:  "Person",
				Type:      "contact",
			},
		},
	}

	var summary dotproject.EnrichmentSummary
	err := enricher.enrichCandidate(context.Background(), nil, candidate{
		GitHubUser: "fixture-handle",
		SourceRef:  "github:fixture-handle",
	}, time.Now().UTC(), &summary)
	require.NoError(t, err)

	assert.Equal(t, "matched", captured.MatchStatus, "a username-only match should still resolve the maintainer, not leave them unmatched")
	assert.Equal(t, "weak", captured.Confidence, "a username-only match is a coincidental string match, not a verified linkage, so it must not inherit strong")
	assert.Equal(t, "Fixture Person", captured.Name, "the matched record's name should be lifted even though the match came from the username fallback")
}

func TestEnricherSkipsInvalidProjectMaintainersFile(t *testing.T) {
	t.Parallel()

	client := &fakeUserSearcher{}
	enricher := &Enricher{
		Store: fakeObservationStore{
			maintainers: map[string]model.Maintainer{},
		},
		Client: client,
	}

	summary, err := enricher.EnrichProject(context.Background(), model.Project{Model: gorm.Model{ID: 1}, Name: "Kubernetes"}, &dotproject.DiscoveryResult{
		MaintainersFile: dotproject.FileDiscovery{
			Exists: true,
			Body:   "maintainers: []",
		},
	})
	require.NoError(t, err)

	assert.Equal(t, dotproject.EnrichmentSummary{}, summary)
	assert.Equal(t, 0, client.calls)
}

// capturingObservationStore appends every upserted observation, unlike
// fakeObservationStore which only keeps the last one - needed to assert on
// the full set of rows a multi-profile match writes.
type retireCall struct {
	sourceRef string
	keep      []string
}

type capturingObservationStore struct {
	maintainers map[string]model.Maintainer
	observed    *[]model.MaintainerIdentityObservation
	retired     *[]retireCall
}

func (f capturingObservationStore) GetMaintainerMapByGitHubAccount() (map[string]model.Maintainer, error) {
	return f.maintainers, nil
}

func (f capturingObservationStore) ListMaintainersWithoutIdentityObservation(string) ([]model.Maintainer, error) {
	return nil, nil
}

func (f capturingObservationStore) ListMaintainersActiveOnAnyProject(maintainerIDs []uint) (map[uint]bool, error) {
	return nil, nil
}

func (f capturingObservationStore) UpsertMaintainerIdentityObservation(observation *model.MaintainerIdentityObservation) (*model.MaintainerIdentityObservation, error) {
	*f.observed = append(*f.observed, *observation)
	return observation, nil
}

func (f capturingObservationStore) RetireStaleMaintainerIdentityObservations(_ string, _ *uint, sourceRef string, keep []string, _ time.Time) (int64, error) {
	if f.retired != nil {
		*f.retired = append(*f.retired, retireCall{sourceRef: sourceRef, keep: keep})
	}
	return 0, nil
}

// fakeMultiUserSearcher returns a fixed set of LFX profiles for any
// GitHub-ID search, and looks up identities/errors per user ID - simulating
// a GitHub handle bound to several duplicate LFX profiles.
type fakeMultiUserSearcher struct {
	users       []User
	identities  map[string][]Identity
	identityErr map[string]error
}

func (f *fakeMultiUserSearcher) SearchUsers(_ context.Context, query UserSearch) ([]User, error) {
	if query.GitHubID != "" {
		return f.users, nil
	}
	return nil, nil
}

func (f *fakeMultiUserSearcher) GetUserIdentities(_ context.Context, salesforceID string) ([]Identity, error) {
	if err, ok := f.identityErr[salesforceID]; ok {
		return nil, err
	}
	return f.identities[salesforceID], nil
}

func TestEnrichCandidateWritesOneRowPerDuplicateLFXProfile(t *testing.T) {
	t.Parallel()

	users := []User{
		{ID: "sfid-a", Type: "lead", LastModifiedDate: "2026-01-01T00:00:00Z"},
		{ID: "sfid-b", Type: "contact", LastModifiedDate: "2026-01-02T00:00:00Z"},
		{ID: "sfid-c", Type: "lead", LastModifiedDate: "2026-01-03T00:00:00Z"},
	}
	var observed []model.MaintainerIdentityObservation
	enricher := &Enricher{
		Store: capturingObservationStore{maintainers: map[string]model.Maintainer{}, observed: &observed},
		Client: &fakeMultiUserSearcher{
			users: users,
			identities: map[string][]Identity{
				"sfid-b": {{Source: "github", Username: "test-fixture-handle"}},
			},
		},
	}

	var summary dotproject.EnrichmentSummary
	err := enricher.enrichCandidate(context.Background(), nil, candidate{
		GitHubUser: "test-fixture-handle",
		SourceRef:  "github:test-fixture-handle",
	}, time.Now().UTC(), &summary)
	require.NoError(t, err)

	require.Len(t, observed, 3, "one observation row must be written per duplicate LFX profile")

	chosen := 0
	bySourceUserID := make(map[string]model.MaintainerIdentityObservation, len(observed))
	for _, obs := range observed {
		bySourceUserID[obs.SourceUserID] = obs
		if obs.MatchStatus == "chosen" {
			chosen++
		}
		assert.Contains(t, obs.MatchReason, "3 LFX profiles")
	}
	assert.Equal(t, 1, chosen, "exactly one profile in the group must be marked chosen")
	assert.Equal(t, "chosen", bySourceUserID["sfid-b"].MatchStatus, "the contact record with a confirmed identity should outrank the two leads")
	assert.Equal(t, "exact", bySourceUserID["sfid-b"].Confidence)
	assert.Equal(t, "duplicate", bySourceUserID["sfid-a"].MatchStatus)
	assert.Equal(t, "duplicate", bySourceUserID["sfid-c"].MatchStatus)
}

func TestEnrichCandidateRetiresProfilesAbsentFromMultiMatchLookup(t *testing.T) {
	t.Parallel()

	users := []User{
		{ID: "sfid-a", Type: "lead", LastModifiedDate: "2026-01-01T00:00:00Z"},
		{ID: "sfid-b", Type: "contact", LastModifiedDate: "2026-01-02T00:00:00Z"},
	}
	var observed []model.MaintainerIdentityObservation
	var retired []retireCall
	enricher := &Enricher{
		Store: capturingObservationStore{maintainers: map[string]model.Maintainer{}, observed: &observed, retired: &retired},
		Client: &fakeMultiUserSearcher{
			users: users,
		},
	}

	var summary dotproject.EnrichmentSummary
	err := enricher.enrichCandidate(context.Background(), nil, candidate{
		GitHubUser: "test-fixture-handle",
		SourceRef:  "github:test-fixture-handle",
	}, time.Now().UTC(), &summary)
	require.NoError(t, err)

	require.Len(t, retired, 1, "a multi-profile lookup must retire rows absent from its own result set")
	assert.Equal(t, "github:test-fixture-handle", retired[0].sourceRef)
	assert.ElementsMatch(t, []string{"sfid-a", "sfid-b"}, retired[0].keep,
		"every profile this lookup returned must be kept, not retired")
}

func TestEnrichCandidateRetiresProfilesAbsentFromSingleMatchLookup(t *testing.T) {
	t.Parallel()

	var observed []model.MaintainerIdentityObservation
	var retired []retireCall
	enricher := &Enricher{
		Store: capturingObservationStore{maintainers: map[string]model.Maintainer{}, observed: &observed, retired: &retired},
		Client: &fakeSingleUserSearcher{
			user: User{ID: "sfid-only", Type: "contact"},
		},
	}

	var summary dotproject.EnrichmentSummary
	err := enricher.enrichCandidate(context.Background(), nil, candidate{
		GitHubUser: "test-fixture-handle",
		SourceRef:  "github:test-fixture-handle",
	}, time.Now().UTC(), &summary)
	require.NoError(t, err)

	require.Len(t, retired, 1, "resolving down to a single profile must retire any other profile a previous ambiguous lookup left behind")
	assert.Equal(t, []string{"sfid-only"}, retired[0].keep)
}

func TestEnrichCandidateToleratesPartialIdentityFetchFailure(t *testing.T) {
	t.Parallel()

	users := []User{
		{ID: "sfid-ok", Type: "contact"},
		{ID: "sfid-broken", Type: "contact"},
	}
	var observed []model.MaintainerIdentityObservation
	enricher := &Enricher{
		Store: capturingObservationStore{maintainers: map[string]model.Maintainer{}, observed: &observed},
		Client: &fakeMultiUserSearcher{
			users:       users,
			identityErr: map[string]error{"sfid-broken": assert.AnError},
		},
	}

	var summary dotproject.EnrichmentSummary
	err := enricher.enrichCandidate(context.Background(), nil, candidate{
		GitHubUser: "test-fixture-handle",
		SourceRef:  "github:test-fixture-handle",
	}, time.Now().UTC(), &summary)
	require.NoError(t, err, "a partial identity-fetch failure must not abort the rest of the group")

	require.Len(t, observed, 2)
	bySourceUserID := make(map[string]model.MaintainerIdentityObservation, len(observed))
	for _, obs := range observed {
		bySourceUserID[obs.SourceUserID] = obs
	}
	assert.Equal(t, "error", bySourceUserID["sfid-broken"].MatchStatus)
	assert.Contains(t, []string{"chosen", "duplicate"}, bySourceUserID["sfid-ok"].MatchStatus)
	assert.Equal(t, 1, summary.Errored, "a tolerated per-profile failure must still count as an error, not vanish from the run summary")

	// The error row's identities request failed, so no count was measured:
	// nil means unmeasured, 0 would falsely claim "measured zero identities".
	assert.Nil(t, bySourceUserID["sfid-broken"].IdentityCount)
	require.NotNil(t, bySourceUserID["sfid-ok"].IdentityCount, "a successful fetch must record its measured count")
	assert.Equal(t, 0, *bySourceUserID["sfid-ok"].IdentityCount)
}

func TestRankCandidatesIsDeterministicOnFullTie(t *testing.T) {
	t.Parallel()

	tie := User{Type: "contact", LastModifiedDate: "2026-01-01T00:00:00Z"}
	scored := []scoredCandidate{
		{user: User{ID: "sfid-zzz", Type: tie.Type, LastModifiedDate: tie.LastModifiedDate}, confidence: "strong"},
		{user: User{ID: "sfid-aaa", Type: tie.Type, LastModifiedDate: tie.LastModifiedDate}, confidence: "strong"},
	}

	rankCandidates(scored)

	assert.Equal(t, "sfid-aaa", scored[0].user.ID, "a full tie must fall back to a stable SourceUserID ordering")
	assert.Equal(t, "sfid-zzz", scored[1].user.ID)
}

// Mixed valid/invalid LastModifiedDate values used to fall back to ID
// comparison only for the mixed pairs, making the less-func non-transitive -
// and a non-transitive comparator lets sort place the wrong profile first.
// Invalid timestamps must sort after every valid one, deterministically.
func TestRankCandidatesTreatsInvalidTimestampAsOldest(t *testing.T) {
	t.Parallel()

	scored := []scoredCandidate{
		{user: User{ID: "sfid-invalid", Type: "contact", LastModifiedDate: "not-a-timestamp"}, confidence: "strong"},
		{user: User{ID: "sfid-old", Type: "contact", LastModifiedDate: "2024-01-01T00:00:00Z"}, confidence: "strong"},
		{user: User{ID: "sfid-new", Type: "contact", LastModifiedDate: "2026-01-01T00:00:00Z"}, confidence: "strong"},
	}

	rankCandidates(scored)

	assert.Equal(t, "sfid-new", scored[0].user.ID)
	assert.Equal(t, "sfid-old", scored[1].user.ID)
	assert.Equal(t, "sfid-invalid", scored[2].user.ID, "an unparseable timestamp must rank below every valid one")
}

func TestEnrichCandidatePropagatesFatalIdentityFetchFailure(t *testing.T) {
	t.Parallel()

	users := []User{
		{ID: "sfid-ok", Type: "contact"},
		{ID: "sfid-unauthorized", Type: "contact"},
	}
	var observed []model.MaintainerIdentityObservation
	enricher := &Enricher{
		Store: capturingObservationStore{maintainers: map[string]model.Maintainer{}, observed: &observed},
		Client: &fakeMultiUserSearcher{
			users: users,
			identityErr: map[string]error{
				"sfid-unauthorized": &HTTPStatusError{StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized"},
			},
		},
	}

	var summary dotproject.EnrichmentSummary
	err := enricher.enrichCandidate(context.Background(), nil, candidate{
		GitHubUser: "test-fixture-handle",
		SourceRef:  "github:test-fixture-handle",
	}, time.Now().UTC(), &summary)
	require.Error(t, err, "a dead token hits every remaining request, so it must abort the run even though the profile's error row was written")
	var fatal dotproject.FatalSyncError
	assert.True(t, errors.As(err, &fatal), "a 401 identity fetch must classify as FatalSyncError")

	found := false
	for _, obs := range observed {
		if obs.SourceUserID == "sfid-unauthorized" {
			assert.Equal(t, "error", obs.MatchStatus)
			found = true
		}
	}
	assert.True(t, found, "the failing profile's error row must still be recorded before the fatal error propagates")
	assert.Equal(t, 0, summary.Errored, "a propagated fatal failure is counted by EnrichProject; counting it here too would report one failed profile as two LFX errors")
}

// A run-context expiry mid-candidate is the run's failure, not the
// profile's: no error observation may be written (the upsert would replace a
// previously good row with the deadline artifact) and the error must reach
// the caller so the run stops cleanly.
func TestEnrichCandidateStopsWithoutErrorRowsWhenRunContextIsDone(t *testing.T) {
	t.Parallel()

	cases := map[string][]User{
		"single profile": {{ID: "sfid-solo", Type: "contact"}},
		"multiple profiles": {
			{ID: "sfid-a", Type: "contact"},
			{ID: "sfid-b", Type: "lead"},
		},
	}
	for name, users := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			identityErr := make(map[string]error, len(users))
			for _, user := range users {
				identityErr[user.ID] = context.Canceled
			}
			var observed []model.MaintainerIdentityObservation
			enricher := &Enricher{
				Store:  capturingObservationStore{maintainers: map[string]model.Maintainer{}, observed: &observed},
				Client: &fakeMultiUserSearcher{users: users, identityErr: identityErr},
			}

			var summary dotproject.EnrichmentSummary
			err := enricher.enrichCandidate(ctx, nil, candidate{
				GitHubUser: "test-fixture-handle",
				SourceRef:  "github:test-fixture-handle",
			}, time.Now().UTC(), &summary)
			require.Error(t, err, "a done run context must propagate, not be tolerated per profile")
			assert.Empty(t, observed, "no observation may be written for a run-context expiry - it would overwrite a previously good row")
		})
	}
}
