package jobs

import "time"

// Options tune the worker. Zero values take the defaults below.
type Options struct {
	// Concurrency is how many jobs this replica runs at once. Default 1, on
	// purpose: extraction is IO-bound on the object store, the jobs table
	// already keeps two workers off one archive, and every in-flight run pins
	// a read window plus a multipart part of memory. Raise it with evidence.
	Concurrency int

	// HeartbeatTTL is how long a running job may go without a heartbeat
	// before its worker is presumed dead and the reaper returns the job to
	// pending. Two-sided: too long and a crashed job blocks its archive for
	// that long; too short and a healthy run is stolen mid-flight.
	HeartbeatTTL time.Duration

	// PollInterval is how often an idle runner looks for work. A second or
	// two is invisible next to a run measured in minutes; LISTEN/NOTIFY is
	// deliberately not used — it does not survive a reconnect, so it would
	// need this poll as a backstop anyway.
	PollInterval time.Duration

	// ReapInterval is how often the reaper looks for running jobs that have
	// outlived HeartbeatTTL without reporting, and samples the queue depth.
	// A tenth of the TTL by default, never more often than PollInterval: a
	// crashed worker's job waits at most 1.1 TTL for pickup, and the two
	// queries a pass costs stay off the every-second path.
	ReapInterval time.Duration

	// FlushInterval is how often a running job writes progress and its
	// heartbeat and reads the cancel flag back — one round trip for all
	// three. It bounds how stale progress can be, how long a cancel takes to
	// be noticed, and (with HeartbeatTTL) how far a heartbeat can drift.
	FlushInterval time.Duration
}

// Defaults for the zero Options.
const (
	DefaultHeartbeatTTL  = 5 * time.Minute
	defaultPollInterval  = time.Second
	defaultFlushInterval = 3 * time.Second
)
