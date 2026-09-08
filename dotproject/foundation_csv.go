package dotproject

import (
	"encoding/csv"
	"fmt"
	"io"
	"strings"
)

const FoundationCSVSource = "foundation-csv"

type FoundationMaintainerRecord struct {
	Project    string
	Name       string
	Company    string
	GitHub     string
	LineNumber int
	Raw        map[string]string
}

type FoundationMaintainerIndex struct {
	Records  []FoundationMaintainerRecord
	byKey    map[string]FoundationMaintainerRecord
	byGitHub map[string][]FoundationMaintainerRecord

	SourceURL string
	CommitSHA string
	// Branch is the configured branch the CSV is read from - distinct from
	// CommitSHA, which pins blame to a snapshot, and from any ref parsed out
	// of SourceURL, which also encodes CommitSHA once a snapshot is pinned.
	// PR-selection disambiguation needs the branch name, since a PR's base
	// can never equal a bare commit SHA.
	Branch string
}

func ParseFoundationMaintainersCSV(r io.Reader) (*FoundationMaintainerIndex, error) {
	reader := csv.NewReader(r)
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true

	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("read foundation csv header: %w", err)
	}
	columns := make(map[string]int, len(header))
	for i, name := range header {
		columns[normalizedHeader(name)] = i
	}

	projectCol, ok := columns["project"]
	if !ok {
		return nil, fmt.Errorf("foundation csv missing Project column")
	}
	nameCol, ok := columns["maintainer name"]
	if !ok {
		return nil, fmt.Errorf("foundation csv missing Maintainer Name column")
	}
	companyCol, ok := columns["company"]
	if !ok {
		return nil, fmt.Errorf("foundation csv missing Company column")
	}
	githubCol, ok := columns["github name"]
	if !ok {
		return nil, fmt.Errorf("foundation csv missing Github Name column")
	}

	index := &FoundationMaintainerIndex{
		byKey:    make(map[string]FoundationMaintainerRecord),
		byGitHub: make(map[string][]FoundationMaintainerRecord),
	}
	currentProject := ""
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read foundation csv row: %w", err)
		}
		// Provenance (permalink + blame) must point at the line holding the
		// GitHub handle: a quoted multiline field earlier in the record would
		// make the record's first line blame the wrong commit. Ragged rows
		// (FieldsPerRecord is -1) too short to hold the handle fall back to
		// the record's first line; they are skipped below anyway.
		posField := 0
		if githubCol < len(row) {
			posField = githubCol
		}
		lineNumber, _ := reader.FieldPos(posField)
		if project := strings.TrimSpace(csvValue(row, projectCol)); project != "" {
			currentProject = project
		}
		github := strings.TrimSpace(csvValue(row, githubCol))
		if currentProject == "" || github == "" {
			continue
		}
		raw := make(map[string]string, len(header))
		for i, name := range header {
			raw[strings.TrimSpace(name)] = strings.TrimSpace(csvValue(row, i))
		}
		record := FoundationMaintainerRecord{
			Project:    currentProject,
			Name:       strings.TrimSpace(csvValue(row, nameCol)),
			Company:    strings.TrimSpace(csvValue(row, companyCol)),
			GitHub:     github,
			LineNumber: lineNumber,
			Raw:        raw,
		}
		index.Records = append(index.Records, record)
		index.byKey[foundationKey(record.Project, record.GitHub)] = record
		index.byGitHub[NormalizeGitHubHandle(record.GitHub)] = append(index.byGitHub[NormalizeGitHubHandle(record.GitHub)], record)
	}
	return index, nil
}

func (i *FoundationMaintainerIndex) LineURL(record FoundationMaintainerRecord) string {
	if i == nil || strings.TrimSpace(i.SourceURL) == "" || record.LineNumber <= 0 {
		return ""
	}
	return fmt.Sprintf("%s#L%d", strings.TrimSpace(i.SourceURL), record.LineNumber)
}

func (i *FoundationMaintainerIndex) Lookup(projectName, githubHandle string) (FoundationMaintainerRecord, bool) {
	if i == nil {
		return FoundationMaintainerRecord{}, false
	}
	record, ok := i.byKey[foundationKey(projectName, githubHandle)]
	return record, ok
}

func (i *FoundationMaintainerIndex) HasGitHub(githubHandle string) bool {
	if i == nil {
		return false
	}
	return len(i.byGitHub[NormalizeGitHubHandle(githubHandle)]) > 0
}

func foundationKey(projectName, githubHandle string) string {
	return strings.TrimSpace(projectName) + "\x00" + NormalizeGitHubHandle(githubHandle)
}

func normalizedHeader(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func csvValue(row []string, index int) string {
	if index < 0 || index >= len(row) {
		return ""
	}
	return row[index]
}
