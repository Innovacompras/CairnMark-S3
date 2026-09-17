package config

import (
	"strings"
	"testing"
	"time"
)

func TestJobsDefaults(t *testing.T) {
	setRequired(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	j := cfg.Jobs
	if j.Concurrency != 1 || j.HeartbeatTTL != 5*time.Minute || j.Retention != 24*time.Hour {
		t.Fatalf("defaults: %+v", j)
	}
}

func TestJobsOverrides(t *testing.T) {
	setRequired(t)
	t.Setenv("CAIRNMARK_JOB_CONCURRENCY", "4")
	t.Setenv("CAIRNMARK_JOB_HEARTBEAT_TTL", "90s")
	t.Setenv("CAIRNMARK_JOB_RETENTION", "0")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	j := cfg.Jobs
	if j.Concurrency != 4 || j.HeartbeatTTL != 90*time.Second || j.Retention != 0 {
		t.Fatalf("overrides: %+v", j)
	}
}

func TestJobsRejectsBadValues(t *testing.T) {
	setRequired(t)
	for _, tc := range []struct{ key, value string }{
		{"CAIRNMARK_JOB_CONCURRENCY", "0"},
		{"CAIRNMARK_JOB_CONCURRENCY", "-1"},
		{"CAIRNMARK_JOB_CONCURRENCY", "two"},
		// Zero would reap every running job instantly and hand it to a
		// second worker — the double write the heartbeat exists to prevent.
		{"CAIRNMARK_JOB_HEARTBEAT_TTL", "0"},
		{"CAIRNMARK_JOB_HEARTBEAT_TTL", "-5m"},
		{"CAIRNMARK_JOB_HEARTBEAT_TTL", "soon"},
		{"CAIRNMARK_JOB_RETENTION", "1 day"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("expected rejection naming %s, got %v", tc.key, err)
			}
		})
	}
}
