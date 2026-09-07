package dotproject

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseFoundationMaintainersCSV_CarriesProjectForward(t *testing.T) {
	t.Parallel()

	index, err := ParseFoundationMaintainersCSV(strings.NewReader(`,Project,Maintainer Name,Company,Github Name,OWNERS
Graduated,Kubernetes,Alice Example,Acme,AliceExample,https://example.com/owners
,,Bob Example,Acme,bob-example,
Graduated,Prometheus,Carol Example,Grafana,carol,
`))
	require.NoError(t, err)

	alice, ok := index.Lookup("Kubernetes", "aliceexample")
	require.True(t, ok)
	assert.Equal(t, "Alice Example", alice.Name)
	assert.Equal(t, "AliceExample", alice.GitHub)
	assert.Equal(t, 2, alice.LineNumber)

	bob, ok := index.Lookup("Kubernetes", "Bob-Example")
	require.True(t, ok)
	assert.Equal(t, "Bob Example", bob.Name)
	assert.Equal(t, 3, bob.LineNumber)

	index.SourceURL = "https://github.com/cncf/foundation/blob/abc/project-maintainers.csv"
	assert.Equal(t, "https://github.com/cncf/foundation/blob/abc/project-maintainers.csv#L3", index.LineURL(bob))

	_, ok = index.Lookup("Prometheus", "bob-example")
	assert.False(t, ok)
	_, ok = index.Lookup("kubernetes", "aliceexample")
	assert.False(t, ok, "project matching is exact after trimming")
}

func TestParseFoundationMaintainersCSV_LineNumberPointsAtGitHubField(t *testing.T) {
	t.Parallel()

	// The quoted name spans lines 2-3, putting the GitHub handle on line 3.
	// Provenance must blame and permalink the handle's line, not the
	// record's first line - otherwise the stored PR/review evidence
	// describes the wrong maintainer field.
	index, err := ParseFoundationMaintainersCSV(strings.NewReader(",Project,Maintainer Name,Company,Github Name\nGraduated,Kubernetes,\"Alice\nExample\",Acme,AliceExample\n"))
	require.NoError(t, err)

	alice, ok := index.Lookup("Kubernetes", "aliceexample")
	require.True(t, ok)
	assert.Equal(t, 3, alice.LineNumber)
}

func TestParseFoundationMaintainersCSV_RaggedRowDoesNotPanic(t *testing.T) {
	t.Parallel()

	// A row shorter than the Github Name column must not panic in FieldPos;
	// it has no handle, so it is skipped.
	index, err := ParseFoundationMaintainersCSV(strings.NewReader(",Project,Maintainer Name,Company,Github Name\nGraduated,Kubernetes\nGraduated,Kubernetes,Alice Example,Acme,AliceExample\n"))
	require.NoError(t, err)

	alice, ok := index.Lookup("Kubernetes", "aliceexample")
	require.True(t, ok)
	assert.Equal(t, 3, alice.LineNumber)
	assert.Len(t, index.Records, 1)
}
