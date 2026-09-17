package files_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mettjs/cairnmark/internal/files"
	"github.com/mettjs/cairnmark/internal/metadata"
	metamem "github.com/mettjs/cairnmark/internal/metadata/memory"
	"github.com/mettjs/cairnmark/internal/storage/memory"
)

func TestExtractProgressHookReportsAndCancels(t *testing.T) {
	ctx := context.Background()
	svc := files.New(memory.New(), metamem.New())
	arch := uploadArchive(t, svc, buildZip(t, docs...))

	var calls [][2]int
	sum, err := svc.Extract(ctx, arch.ID, files.ExtractOptions{
		OnProgress: func(done, total int) bool {
			calls = append(calls, [2]int{done, total})
			return done == 1 // stop after the first entry
		},
	})
	if !errors.Is(err, files.ErrExtractCancelled) {
		t.Fatalf("got %v want ErrExtractCancelled", err)
	}
	// The hook sees (0, total) before the loop and (n, total) after each
	// entry; the run stopped after the entry the hook cancelled on.
	if want := [][2]int{{0, 3}, {1, 3}}; len(calls) != 2 || calls[0] != want[0] || calls[1] != want[1] {
		t.Fatalf("progress calls: %v want %v", calls, want)
	}
	// The partial summary comes back with the error, and what was written stays.
	if sum == nil || sum.Extracted != 1 || sum.Entries != 4 {
		t.Fatalf("partial summary: %+v", sum)
	}
	if kids := children(t, svc, arch.ID, false); len(kids) != 1 || kids[0] == nil {
		t.Fatalf("children after cancel: %v", kids)
	}
	if f, _ := svc.Metadata(ctx, arch.ID); f.Metadata[metadata.TagArchive] != metadata.TagArchiveMarker {
		t.Fatal("a cancelled run that wrote a child must still mark the archive")
	}

	// A later run resumes past it.
	sum, err = svc.Extract(ctx, arch.ID, files.ExtractOptions{})
	if err != nil || sum.Extracted != 2 || sum.SkippedByReason[files.SkipAlreadyExtracted] != 1 {
		t.Fatalf("resume: %+v err=%v", sum, err)
	}
	if got := len(children(t, svc, arch.ID, false)); got != 3 {
		t.Fatalf("children after resume: %d", got)
	}
}

// flakyRepo fails the first archive-marking update, standing in for a run
// interrupted between its last child and the marker.
type flakyRepo struct {
	*metamem.Repo
	failMark bool
}

func (r *flakyRepo) UpdateMetadata(ctx context.Context, id string, tags map[string]any, merge bool) (*metadata.File, error) {
	if r.failMark {
		r.failMark = false
		return nil, errors.New("injected metadata failure")
	}
	return r.Repo.UpdateMetadata(ctx, id, tags, merge)
}

func TestExtractMarksArchiveWhenChildrenAlreadyExist(t *testing.T) {
	// A worker parked on shutdown can be interrupted after the last child
	// and before the marker. The resumed run writes nothing new — every
	// entry is already_extracted — and must still stamp the marker, or the
	// archive stays unlisted for good.
	ctx := context.Background()
	repo := &flakyRepo{Repo: metamem.New(), failMark: true}
	svc := files.New(memory.New(), repo)
	arch := uploadArchive(t, svc, buildZip(t, docs...))

	if _, err := svc.Extract(ctx, arch.ID, files.ExtractOptions{}); err == nil {
		t.Fatal("expected the injected failure")
	}
	if f, _ := svc.Metadata(ctx, arch.ID); f.Metadata[metadata.TagArchive] != nil {
		t.Fatal("marker present despite the failed update")
	}
	sum, err := svc.Extract(ctx, arch.ID, files.ExtractOptions{})
	if err != nil || sum.Extracted != 0 || sum.SkippedByReason[files.SkipAlreadyExtracted] != 3 {
		t.Fatalf("resume: %+v err=%v", sum, err)
	}
	if f, _ := svc.Metadata(ctx, arch.ID); f.Metadata[metadata.TagArchive] != metadata.TagArchiveMarker {
		t.Fatal("a resumed run over existing children left the archive unmarked")
	}
}

func TestSubmitExtractionValidatesBeforeEnqueueing(t *testing.T) {
	ctx := context.Background()
	repo := metamem.New()
	svc := files.New(memory.New(), repo, files.WithArchiveLimits(files.ArchiveLimits{MaxTotalBytes: 30}))
	arch := uploadArchive(t, svc, buildZip(t, docs...))
	plain := uploadArchive(t, svc, []byte("%PDF-1.7 just a document"))

	// Every refusal a synchronous run could make is made at submission,
	// with the same error, and leaves no job behind.
	for _, tc := range []struct {
		name     string
		id       string
		selected []int
		want     error
	}{
		{"not an archive", plain.ID, nil, files.ErrNotArchive},
		{"selection out of range", arch.ID, []int{9}, files.ErrInvalidSelection},
		{"over the total cap", arch.ID, nil, files.ErrArchiveTooLarge},
		{"malformed id", "nope", nil, files.ErrInvalidID},
		{"unknown id", "11111111-1111-1111-1111-111111111111", nil, files.ErrNotFound},
	} {
		if _, err := svc.SubmitExtraction(ctx, tc.id, tc.selected); !errors.Is(err, tc.want) {
			t.Fatalf("%s: got %v want %v", tc.name, err, tc.want)
		}
	}
	if n, _ := repo.CountPendingJobs(ctx); n != 0 {
		t.Fatalf("a refused submission left %d jobs behind", n)
	}

	// A selection under the cap is accepted, and the selection is kept as
	// given — nil, empty and explicit stay distinct.
	job, err := svc.SubmitExtraction(ctx, arch.ID, []int{0})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if job.Status != files.JobPending || job.ArchiveID != arch.ID || len(job.Selection) != 1 || job.Selection[0] != 0 {
		t.Fatalf("job: %+v", job)
	}
	got, err := svc.ExtractionJob(ctx, job.ID)
	if err != nil || got.ID != job.ID || got.Status != files.JobPending {
		t.Fatalf("ExtractionJob: %+v err=%v", got, err)
	}
}

func TestSubmitExtractionRefusesASecondActiveJob(t *testing.T) {
	ctx := context.Background()
	svc := files.New(memory.New(), metamem.New())
	arch := uploadArchive(t, svc, buildZip(t, docs...))

	first, err := svc.SubmitExtraction(ctx, arch.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.SubmitExtraction(ctx, arch.ID, []int{0})
	var inProgress *files.ExtractInProgressError
	if !errors.As(err, &inProgress) || !errors.Is(err, files.ErrExtractInProgress) || inProgress.JobID != first.ID {
		t.Fatalf("second submit: got %v (job %+v)", err, inProgress)
	}

	// Cancelling the pending job frees the archive at once.
	cancelled, err := svc.CancelExtraction(ctx, first.ID)
	if err != nil || cancelled.Status != files.JobCancelled {
		t.Fatalf("cancel: %+v err=%v", cancelled, err)
	}
	if _, err := svc.SubmitExtraction(ctx, arch.ID, nil); err != nil {
		t.Fatalf("submit after cancel: %v", err)
	}
	// Cancelling again is a no-op reporting the terminal state.
	if again, err := svc.CancelExtraction(ctx, first.ID); err != nil || again.Status != files.JobCancelled {
		t.Fatalf("cancel twice: %+v err=%v", again, err)
	}
}

func TestExtractionJobLookupsRejectBadIDs(t *testing.T) {
	ctx := context.Background()
	svc := files.New(memory.New(), metamem.New())
	const unknown = "11111111-1111-1111-1111-111111111111"
	if _, err := svc.ExtractionJob(ctx, "nope"); !errors.Is(err, files.ErrInvalidID) {
		t.Fatalf("malformed: %v", err)
	}
	if _, err := svc.ExtractionJob(ctx, unknown); !errors.Is(err, files.ErrJobNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := svc.CancelExtraction(ctx, "nope"); !errors.Is(err, files.ErrInvalidID) {
		t.Fatalf("cancel malformed: %v", err)
	}
	if _, err := svc.CancelExtraction(ctx, unknown); !errors.Is(err, files.ErrJobNotFound) {
		t.Fatalf("cancel unknown: %v", err)
	}
}

// racingRepo answers the first misses submissions as "refused, and nobody is
// active" — the window in which the job that refused the insert finished
// before it could be named.
type racingRepo struct {
	*metamem.Repo
	misses int
}

func (r *racingRepo) CreateJob(ctx context.Context, j *metadata.Job) (bool, string, error) {
	if r.misses > 0 {
		r.misses--
		return false, "", nil
	}
	return r.Repo.CreateJob(ctx, j)
}

func TestSubmitExtractionRetriesWhenTheActiveJobJustFinished(t *testing.T) {
	ctx := context.Background()
	repo := &racingRepo{Repo: metamem.New(), misses: 1}
	svc := files.New(memory.New(), repo)
	arch := uploadArchive(t, svc, buildZip(t, docs...))

	// One miss: the slot is free by the second insert, and the caller never
	// sees a 409 it could do nothing about.
	job, err := svc.SubmitExtraction(ctx, arch.ID, nil)
	if err != nil || job.Status != files.JobPending {
		t.Fatalf("submit after a miss: %+v err=%v", job, err)
	}
	if _, err := svc.CancelExtraction(ctx, job.ID); err != nil {
		t.Fatal(err)
	}

	// Two in a row: the archive is contended, and the refusal stands —
	// without a job id, since none was ever seen.
	repo.misses = 2
	_, err = svc.SubmitExtraction(ctx, arch.ID, nil)
	var inProgress *files.ExtractInProgressError
	if !errors.As(err, &inProgress) || inProgress.JobID != "" {
		t.Fatalf("two misses: got %v want an in-progress refusal naming no job", err)
	}
	if n, _ := repo.CountPendingJobs(ctx); n != 0 {
		t.Fatalf("a refused submission left %d jobs behind", n)
	}
}
