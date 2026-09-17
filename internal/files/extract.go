package files

import (
	"context"
	"fmt"
	"math"
)

// ExtractOptions configures one extraction run.
type ExtractOptions struct {
	// Entries restricts the run to those directory indexes (an index outside
	// the directory is ErrInvalidSelection); nil means every selectable
	// entry, and an empty, non-nil slice selects nothing.
	Entries []int

	// OnProgress, when set, is called once before the first entry with
	// (0, total) and again after each entry. Returning true stops the run
	// after the entry just written — Extract then returns the partial summary
	// with ErrExtractCancelled. The hook is what lets a job report progress
	// and honour cancellation without this package knowing what a job is.
	OnProgress func(done, total int) (cancel bool)
}

func (o ExtractOptions) tick(done, total int) bool {
	return o.OnProgress != nil && o.OnProgress(done, total)
}

// Extract stores every selected entry of the archive id as an ordinary file,
// tagged with the archive it came from.
//
// A run is resumable by construction: entries that already have a child row
// are skipped as SkipAlreadyExtracted, so a run interrupted part-way — or
// simply repeated — completes the remainder instead of duplicating. A child
// the user deleted stays deleted (SkipPreviouslyDeleted). On any error, or a
// cancellation through OnProgress, the children written so far remain, and
// the next run picks up after them. Serialising concurrent runs on one
// archive is the job layer's concern: nothing here prevents two direct
// callers from writing the same archive at once.
func (s *Service) Extract(ctx context.Context, id string, o ExtractOptions) (*ExtractSummary, error) {
	if !validID(id) {
		return nil, ErrInvalidID
	}
	a, sum, todo, err := s.planExtraction(ctx, id, o.Entries)
	if err != nil {
		return nil, err
	}

	total := len(todo)
	cancelled := o.tick(0, total)
	for i := 0; i < total && !cancelled; i++ {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("files: extract %s: %w", id, err)
		}
		e := todo[i]
		why, err := s.extractEntry(ctx, a, e)
		if err != nil {
			return nil, err
		}
		if why != "" {
			sum.skip(e, why)
		} else {
			sum.Extracted++
		}
		cancelled = o.tick(i+1, total)
	}

	// Only a run that leaves live children behind marks the archive: the
	// marker answers "list my archives", and a selection of none — or a
	// directory of nothing but litter — has produced no children to find.
	// Children from an earlier run count too: a run interrupted between its
	// last child and the marker resumes as a no-op, and would otherwise leave
	// the archive unlisted for good.
	if sum.Extracted > 0 || sum.SkippedByReason[SkipAlreadyExtracted] > 0 {
		if err := s.markArchive(ctx, a.file); err != nil {
			return nil, err
		}
	}
	if cancelled {
		return sum, ErrExtractCancelled
	}
	return sum, nil
}

// planExtraction decides the whole run before anything is written — opens the
// archive, checks the selection, builds the skip-set — so the total cap can
// be checked against exactly what would be written. It is what a job's
// submission runs too: every refusal a synchronous call could make (not an
// archive, bad selection, over a cap) is made before the job exists.
func (s *Service) planExtraction(ctx context.Context, id string, selected []int) (*opened, *ExtractSummary, []ArchiveEntry, error) {
	a, err := s.openArchive(ctx, id)
	if err != nil {
		return nil, nil, nil, err
	}
	entries := a.walker.Entries()
	chosen, err := selection(selected, len(entries))
	if err != nil {
		return nil, nil, nil, err
	}
	prior, err := s.priorChildren(ctx, id)
	if err != nil {
		return nil, nil, nil, err
	}

	// Both collections non-nil, so the wire shape is an object and an array
	// even when nothing was skipped — never null.
	sum := &ExtractSummary{
		ArchiveID: id, Entries: len(entries),
		SkippedByReason: map[SkipReason]int{}, SampleSkipped: []SkippedEntry{},
	}
	var todo []ArchiveEntry
	var total int64
	for _, e := range entries {
		switch {
		case e.Skip != "":
			sum.skip(e, e.Skip)
		case chosen != nil && !chosen[e.Index]:
			sum.skip(e, SkipNotSelected)
		case prior[e.Index] != "":
			sum.skip(e, prior[e.Index])
		default:
			todo = append(todo, e)
			total = addClamped(total, e.Size)
		}
	}
	if s.limits.MaxTotalBytes > 0 && total > s.limits.MaxTotalBytes {
		return nil, nil, nil, fmt.Errorf("%w: the selected entries total %d bytes, the cap is %d",
			ErrArchiveTooLarge, total, s.limits.MaxTotalBytes)
	}
	return a, sum, todo, nil
}

// addClamped sums declared uncompressed sizes without wrapping. A directory
// record can claim any size up to math.MaxInt64 and nothing validates it
// against the object, so a plain += lets two entries wrap the total negative —
// under any cap, and the 413 the cap exists to return never fires.
func addClamped(a, b int64) int64 {
	if sum := a + b; sum >= a {
		return sum
	}
	return math.MaxInt64
}

// selection turns the caller's index list into a set, or nil for "all".
func selection(selected []int, n int) (map[int]bool, error) {
	if selected == nil {
		return nil, nil
	}
	set := make(map[int]bool, len(selected))
	for _, i := range selected {
		if i < 0 || i >= n {
			return nil, fmt.Errorf("%w: index %d (the archive has %d entries)", ErrInvalidSelection, i, n)
		}
		set[i] = true
	}
	return set, nil
}
