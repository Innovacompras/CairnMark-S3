package files

// ExtractSummary is the bounded report of one extraction. It never lists the
// files it created: a large archive's results could not fit one response, so
// callers enumerate the children with a tag filter on the archive id instead.
//
// The JSON tags are the wire shape — the api layer serialises it as is, and a
// job stores the same bytes — so there is one shape, not two.
type ExtractSummary struct {
	ArchiveID       string             `json:"archive_id"`
	Entries         int                `json:"entries"` // entries in the archive's directory
	Extracted       int                `json:"extracted"`
	Skipped         int                `json:"skipped"`
	SkippedByReason map[SkipReason]int `json:"skipped_by_reason"`
	SampleSkipped   []SkippedEntry     `json:"sample_skipped"` // the first sampleSkippedMax skipped entries
}

// SkippedEntry names one skipped entry and why.
type SkippedEntry struct {
	Index  int        `json:"index"`
	Name   string     `json:"name"`
	Reason SkipReason `json:"reason"`
}

// sampleSkippedMax keeps the response size independent of archive size.
const sampleSkippedMax = 20

func (sum *ExtractSummary) skip(e ArchiveEntry, why SkipReason) {
	sum.Skipped++
	sum.SkippedByReason[why]++
	if len(sum.SampleSkipped) < sampleSkippedMax {
		sum.SampleSkipped = append(sum.SampleSkipped, SkippedEntry{Index: e.Index, Name: e.Name, Reason: why})
	}
}
