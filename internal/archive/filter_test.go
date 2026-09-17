package archive

import (
	"strings"
	"testing"
)

func TestPlatformMetadataNames(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"__MACOSX/docs/._a.pdf", true},
		{"docs/__MACOSX/._a.pdf", true},
		{"docs/._a.pdf", true},
		{"._a.pdf", true},
		{".DS_Store", true},
		{"docs/.DS_Store", true},
		{"Thumbs.db", true},
		{"docs/a.pdf", false},
		{"docs/_a.pdf", false},
		{"macosx/a.pdf", false},
		{"thumbs.db", false},
	}
	for _, tt := range tests {
		if got := isPlatformMetadata(tt.name); got != tt.want {
			t.Errorf("isPlatformMetadata(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestSafeNames(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"a.pdf", true},
		{"docs/a.pdf", true},
		{"docs/./a.pdf", true},
		{"docs/../a.pdf", true}, // resolves inside the tree
		{"../a.pdf", false},
		{"docs/../../a.pdf", false},
		{"/etc/passwd", false},
		{`docs\a.pdf`, false},
		{"", false},
	}
	for _, tt := range tests {
		if got := safeName(tt.name); got != tt.want {
			t.Errorf("safeName(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestExtensionAllowlist(t *testing.T) {
	allowed := []string{".pdf", ".docx"}
	tests := []struct {
		name string
		list []string
		want bool
	}{
		{"a.pdf", allowed, true},
		{"a.PDF", allowed, true},
		{"a.txt", allowed, false},
		{"README", allowed, false},
		{"a.txt", nil, true},
		{"README", nil, true},
	}
	for _, tt := range tests {
		if got := extensionAllowed(tt.name, tt.list); got != tt.want {
			t.Errorf("extensionAllowed(%q, %v) = %v, want %v", tt.name, tt.list, got, tt.want)
		}
	}
}

func TestContentTypeDropsTheCharsetParameter(t *testing.T) {
	// mime.TypeByExtension returns "text/plain; charset=utf-8" for the text
	// types. The list endpoint matches content_type by exact equality, so a
	// stored type carrying a parameter is invisible to a search for the type
	// itself — which is the whole reason extraction sets it.
	for _, name := range []string{"notes/readme.txt", "data.csv", "page.html", "app.js", "q3.pdf", "data.json"} {
		got := contentType(name)
		if got == "" {
			continue // no mapping for this extension on this platform
		}
		if strings.ContainsRune(got, ';') {
			t.Errorf("contentType(%q) = %q, want no parameter", name, got)
		}
	}
	if got := contentType("notes/readme.txt"); got != "" && got != "text/plain" {
		t.Errorf(`contentType(".txt") = %q, want "text/plain"`, got)
	}
	if got := contentType("noextension"); got != "" {
		t.Errorf("contentType with no extension = %q, want empty", got)
	}
}
