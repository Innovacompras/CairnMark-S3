package config

import (
	"fmt"
	"time"
)

// Jobs configures the extraction-job worker (internal/jobs) and the
// retention of finished jobs. One global knob each — no per-job, per-archive
// or per-caller policy.
type Jobs struct {
	// Concurrency is how many jobs one replica runs at once. Default 1, on
	// purpose: extraction is IO-bound on the object store, the jobs table
	// already keeps two workers off one archive, and every in-flight run pins
	// a read window plus a multipart part of memory. Raise it with evidence.
	Concurrency int

	// HeartbeatTTL is how long a running job may go without a heartbeat
	// before its worker is presumed dead and the job is returned to the
	// queue. Two-sided, like the lease TTL it replaces: too long and a
	// crashed job blocks its archive for that long; too short and a healthy
	// run is stolen mid-flight. Zero is not "disabled" — it would reap every
	// job instantly — so it is refused.
	HeartbeatTTL time.Duration

	// Retention is how long a finished job stays readable through
	// GET /jobs/{id} before the GC sweep deletes it. Non-positive disables
	// the sweep, like CAIRNMARK_IDEMPOTENCY_TTL.
	Retention time.Duration
}

func loadJobs() (Jobs, error) {
	j := Jobs{Concurrency: 1, HeartbeatTTL: 5 * time.Minute, Retention: 24 * time.Hour}
	concurrency := int64(j.Concurrency)
	if err := getint64("CAIRNMARK_JOB_CONCURRENCY", &concurrency); err != nil {
		return Jobs{}, err
	}
	if concurrency < 1 {
		return Jobs{}, fmt.Errorf("config: CAIRNMARK_JOB_CONCURRENCY must be >= 1, got %d", concurrency)
	}
	j.Concurrency = int(concurrency)
	if err := getduration("CAIRNMARK_JOB_HEARTBEAT_TTL", &j.HeartbeatTTL); err != nil {
		return Jobs{}, err
	}
	if j.HeartbeatTTL <= 0 {
		return Jobs{}, fmt.Errorf(
			"config: CAIRNMARK_JOB_HEARTBEAT_TTL must be > 0 (it bounds crash recovery, it does not disable it), got %s",
			j.HeartbeatTTL)
	}
	if err := getduration("CAIRNMARK_JOB_RETENTION", &j.Retention); err != nil {
		return Jobs{}, err
	}
	return j, nil
}
