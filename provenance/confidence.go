package provenance

// Observation source identifiers. These match the Source column values
// written by their respective packages (dotproject.FoundationCSVSource,
// the new "dot-project" source, and the legacy-ref writer).
const (
	SourceDotProject    = "dot-project"
	SourceFoundationCSV = "foundation-csv"
	SourceLegacyRef     = "legacy-ref"
)

// Confidence tiers. "" deliberately asserts nothing - used when no lookup
// was performed at all, so the writer must not claim corroboration it
// never obtained.
const (
	ConfidenceExact  = "exact"
	ConfidenceStrong = "strong"
	ConfidenceMedium = "medium"
	ConfidenceWeak   = "weak"
)

// tierBelow maps a confidence tier to the next tier down, for the "unknown"
// review-state case (one tier below what the source would get if approved).
var tierBelow = map[string]string{
	ConfidenceExact:  ConfidenceStrong,
	ConfidenceStrong: ConfidenceMedium,
	ConfidenceMedium: ConfidenceWeak,
	ConfidenceWeak:   "",
}

// approvedConfidence is the confidence a source gets when its line was
// introduced by an approved PR review - the top of that source's range.
var approvedConfidence = map[string]string{
	SourceDotProject:    ConfidenceExact,
	SourceFoundationCSV: ConfidenceStrong,
	SourceLegacyRef:     ConfidenceMedium,
}

// unreviewedConfidence is the confidence when a PR exists (or the line was
// pushed directly) but was not approved - one tier below approved.
var unreviewedConfidence = map[string]string{
	SourceDotProject:    ConfidenceStrong,
	SourceFoundationCSV: ConfidenceMedium,
	SourceLegacyRef:     ConfidenceWeak,
}

// Confidence derives an observation's confidence tier from its source and
// the review state of the line that produced it, per the table in the
// PR-reviewed-provenance plan. lookupPerformed must be false only for the
// foundation-csv case where CheckFoundationCSV is disabled and no CSV
// lookup happened at all - that path must assert nothing.
func Confidence(source, reviewState string, lookupPerformed bool) string {
	if !lookupPerformed {
		return ""
	}
	switch reviewState {
	case ReviewStateApproved:
		return approvedConfidence[source]
	case ReviewStateUnreviewed, ReviewStateDirectPush:
		return unreviewedConfidence[source]
	case ReviewStateUnknown:
		return tierBelow[approvedConfidence[source]]
	default:
		return ""
	}
}
