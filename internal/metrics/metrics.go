// Package metrics owns the Prometheus instrumentation: the metric definitions
// and the /metrics exposition handler. Layers record observations through the
// functions here; nothing in this package depends on the rest of the system,
// so any layer may import it (the composition root wires GC observations in,
// keeping gc itself instrumentation-free).
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	httpRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cairnmark_http_requests_total",
		Help: "HTTP requests served, by method, route pattern, and status code.",
	}, []string{"method", "pattern", "status"})

	httpDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "cairnmark_http_request_duration_seconds",
		Help:    "HTTP request latency, by method and route pattern.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "pattern"})

	gcSweeps = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cairnmark_gc_sweeps_total",
		Help: "GC reconciliation sweeps, by result (ok | error).",
	}, []string{"result"})

	gcReclaimed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cairnmark_gc_reclaimed_total",
		Help: "Items reclaimed by GC, by kind (purged | orphans | expired_keys | expired_jobs).",
	}, []string{"kind"})

	jobRuns = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cairnmark_extraction_job_runs_total",
		Help: "Extraction job runs, by resting state (succeeded | failed | cancelled | pending — parked on shutdown or lost to a reaper, to be resumed).",
	}, []string{"status"})

	jobDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "cairnmark_extraction_job_duration_seconds",
		Help:    "Wall time of one extraction job run, from claim to its resting state.",
		Buckets: prometheus.ExponentialBuckets(1, 2, 12), // 1s … ~34min
	})

	jobsPending = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "cairnmark_extraction_jobs_pending",
		Help: "Extraction jobs waiting for a worker, sampled on the worker's ticker.",
	})

	jobsReaped = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cairnmark_extraction_jobs_reaped_total",
		Help: "Running extraction jobs returned to the queue because their worker stopped reporting. A non-zero rate means workers are dying or CAIRNMARK_JOB_HEARTBEAT_TTL is too tight — it is the only signal of either.",
	})
)

// Handler serves the Prometheus exposition endpoint (GET /metrics).
func Handler() http.Handler { return promhttp.Handler() }

// ObserveRequest records one served HTTP request. pattern must be the matched
// route pattern, never the raw URL — path parameters like file ids would blow
// up label cardinality.
func ObserveRequest(method, pattern string, status int, elapsed time.Duration) {
	httpRequests.WithLabelValues(method, pattern, strconv.Itoa(status)).Inc()
	httpDuration.WithLabelValues(method, pattern).Observe(elapsed.Seconds())
}

// ObserveGCSweep records the outcome of one reconciliation sweep.
func ObserveGCSweep(purged, orphans, expiredKeys, expiredJobs int, err error) {
	if err != nil {
		gcSweeps.WithLabelValues("error").Inc()
		return
	}
	gcSweeps.WithLabelValues("ok").Inc()
	gcReclaimed.WithLabelValues("purged").Add(float64(purged))
	gcReclaimed.WithLabelValues("orphans").Add(float64(orphans))
	gcReclaimed.WithLabelValues("expired_keys").Add(float64(expiredKeys))
	gcReclaimed.WithLabelValues("expired_jobs").Add(float64(expiredJobs))
}

// ObserveJobRun records one extraction job run reaching a resting state.
func ObserveJobRun(status string, elapsed time.Duration) {
	jobRuns.WithLabelValues(status).Inc()
	jobDuration.Observe(elapsed.Seconds())
}

// ObserveJobSweep records one reaper pass: stranded jobs returned to the
// queue, and the queue depth it saw.
func ObserveJobSweep(reaped, pending int, err error) {
	if err != nil {
		return
	}
	jobsReaped.Add(float64(reaped))
	jobsPending.Set(float64(pending))
}
