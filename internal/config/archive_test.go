package config

import (
	"slices"
	"strings"
	"testing"
)

func TestArchiveDefaults(t *testing.T) {
	setRequired(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a := cfg.Archive
	if a.MaxEntries != 1000 || a.MaxTotalBytes != 1<<30 || a.MaxRatio != 100 || a.Extensions != nil {
		t.Fatalf("defaults: %+v", a)
	}
}

func TestArchiveOverrides(t *testing.T) {
	setRequired(t)
	t.Setenv("CAIRNMARK_ARCHIVE_MAX_ENTRIES", "0")
	t.Setenv("CAIRNMARK_ARCHIVE_MAX_TOTAL_BYTES", "5000")
	t.Setenv("CAIRNMARK_ARCHIVE_MAX_RATIO", "10")
	t.Setenv("CAIRNMARK_ARCHIVE_EXTENSIONS", "PDF, .docx ,, xlsx")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a := cfg.Archive
	if a.MaxEntries != 0 || a.MaxTotalBytes != 5000 || a.MaxRatio != 10 {
		t.Fatalf("overrides: %+v", a)
	}
	if want := []string{".pdf", ".docx", ".xlsx"}; !slices.Equal(a.Extensions, want) {
		t.Fatalf("extensions: got %v want %v", a.Extensions, want)
	}
}

func TestArchiveRejectsBadValues(t *testing.T) {
	setRequired(t)
	t.Setenv("CAIRNMARK_ARCHIVE_MAX_TOTAL_BYTES", "-1")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "CAIRNMARK_ARCHIVE_MAX_TOTAL_BYTES") {
		t.Fatalf("expected rejection of a negative cap, got %v", err)
	}
	t.Setenv("CAIRNMARK_ARCHIVE_MAX_TOTAL_BYTES", "1GiB")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "CAIRNMARK_ARCHIVE_MAX_TOTAL_BYTES") {
		t.Fatalf("expected parse error for a non-integer cap, got %v", err)
	}
}
