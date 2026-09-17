package config

import (
	"fmt"
	"strings"
)

// Archive bounds archive extraction (POST /files/{id}/extract).
// Zero disables each cap, matching MaxUploadBytes. Per-entry size is not here:
// an extracted entry is an upload, so it reuses CAIRNMARK_MAX_UPLOAD_BYTES —
// the upload cap alone would not cover it, since entries enter below the
// handler that enforces it.
type Archive struct {
	MaxEntries    int      // entries an archive may hold; both archive endpoints refuse larger ones
	MaxTotalBytes int64    // uncompressed bytes one extraction may write
	MaxRatio      int64    // per-entry uncompressed ÷ compressed; the zip-bomb guard
	Extensions    []string // allowlist, lower-case with the leading dot; empty allows every extension

	// MaxDirectoryBytes bounds the central directory's byte span, the only cap
	// that can be applied before archive/zip parses it. Zero does not disable
	// it — it derives the bound from MaxEntries, which is the right answer
	// almost always — because this is what bounds the allocation a crafted
	// directory provokes. Set it only for an archive with unusually long paths.
	MaxDirectoryBytes int64
}

func loadArchive() (Archive, error) {
	a := Archive{
		MaxEntries: 1000, MaxTotalBytes: 1 << 30, MaxRatio: 100,
		MaxDirectoryBytes: 0, // 0 = derived from MaxEntries; see the field
	}
	entries := int64(a.MaxEntries)
	for _, v := range []struct {
		key string
		dst *int64
	}{
		{"CAIRNMARK_ARCHIVE_MAX_ENTRIES", &entries},
		{"CAIRNMARK_ARCHIVE_MAX_TOTAL_BYTES", &a.MaxTotalBytes},
		{"CAIRNMARK_ARCHIVE_MAX_RATIO", &a.MaxRatio},
		{"CAIRNMARK_ARCHIVE_MAX_DIRECTORY_BYTES", &a.MaxDirectoryBytes},
	} {
		if err := getint64(v.key, v.dst); err != nil {
			return Archive{}, err
		}
		if *v.dst < 0 {
			return Archive{}, fmt.Errorf("config: %s must be >= 0 (0 disables the cap), got %d", v.key, *v.dst)
		}
	}
	a.MaxEntries = int(entries)
	a.Extensions = parseExtensions(getenv("CAIRNMARK_ARCHIVE_EXTENSIONS", ""))
	return a, nil
}

// parseExtensions normalises a comma-separated allowlist — trimmed, lower-case,
// dot-prefixed — so "PDF, .docx" and ".pdf,.docx" mean the same thing.
func parseExtensions(v string) []string {
	var out []string
	for _, e := range strings.Split(v, ",") {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		out = append(out, e)
	}
	return out
}
