package provenance

import "testing"

func TestConfidenceTable(t *testing.T) {
	cases := []struct {
		name            string
		source          string
		reviewState     string
		lookupPerformed bool
		want            string
	}{
		{"dot-project approved", SourceDotProject, ReviewStateApproved, true, ConfidenceExact},
		{"dot-project unreviewed", SourceDotProject, ReviewStateUnreviewed, true, ConfidenceStrong},
		{"dot-project direct-push", SourceDotProject, ReviewStateDirectPush, true, ConfidenceStrong},
		{"dot-project unknown", SourceDotProject, ReviewStateUnknown, true, ConfidenceStrong},

		{"foundation-csv approved", SourceFoundationCSV, ReviewStateApproved, true, ConfidenceStrong},
		{"foundation-csv unreviewed", SourceFoundationCSV, ReviewStateUnreviewed, true, ConfidenceMedium},
		{"foundation-csv direct-push", SourceFoundationCSV, ReviewStateDirectPush, true, ConfidenceMedium},
		{"foundation-csv unknown", SourceFoundationCSV, ReviewStateUnknown, true, ConfidenceMedium},
		{"foundation-csv no lookup performed", SourceFoundationCSV, ReviewStateApproved, false, ""},

		{"legacy-ref approved", SourceLegacyRef, ReviewStateApproved, true, ConfidenceMedium},
		{"legacy-ref unreviewed", SourceLegacyRef, ReviewStateUnreviewed, true, ConfidenceWeak},
		{"legacy-ref direct-push", SourceLegacyRef, ReviewStateDirectPush, true, ConfidenceWeak},
		{"legacy-ref unknown", SourceLegacyRef, ReviewStateUnknown, true, ConfidenceWeak},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Confidence(tc.source, tc.reviewState, tc.lookupPerformed)
			if got != tc.want {
				t.Errorf("Confidence(%q, %q, %v) = %q, want %q", tc.source, tc.reviewState, tc.lookupPerformed, got, tc.want)
			}
		})
	}
}
