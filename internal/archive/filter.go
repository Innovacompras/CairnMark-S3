package archive

import (
	"archive/zip"
	"math"
	"mime"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// flagEncrypted is general-purpose bit 0 of a zip entry: the content is
// encrypted (traditional PKWARE, or AES via method 99), which archive/zip
// cannot read.
const flagEncrypted = 0x1

// classify builds the Entry for a directory record, deciding from the record
// alone — no content is read — whether it can be extracted and, if not, why.
func classify(index int, f *zip.File, rules Rules) Entry {
	return Entry{
		Index:       index,
		Name:        f.Name,
		Size:        int64(min(f.UncompressedSize64, math.MaxInt64)),
		CRC32:       f.CRC32,
		ContentType: contentType(f.Name),
		Skip:        skipReason(f, rules),
	}
}

// contentType maps the entry's extension to a media type, dropping the charset
// parameter mime.TypeByExtension attaches to the text types (".txt" comes back
// as "text/plain; charset=utf-8"). The list endpoint matches content_type by
// exact equality, so a stored type carrying a parameter is invisible to a search
// for the type itself — the blindness extraction sets ContentType to avoid.
func contentType(name string) string {
	t := mime.TypeByExtension(path.Ext(name))
	if t == "" {
		return ""
	}
	media, _, err := mime.ParseMediaType(t)
	if err != nil {
		return t // unparseable: better the raw type than none
	}
	return media
}

// skipReason applies the rules in a fixed order so an entry that trips several
// reports the same one every time: the first match wins.
func skipReason(f *zip.File, rules Rules) SkipReason {
	mode := f.Mode()
	switch {
	case mode.IsDir():
		return SkipDirectory
	case !mode.IsRegular():
		return SkipNonRegular
	case isPlatformMetadata(f.Name):
		return SkipPlatformMetadata
	case f.Flags&flagEncrypted != 0:
		return SkipEncrypted
	case f.Method != zip.Store && f.Method != zip.Deflate:
		return SkipUnsupportedMethod
	case !safeName(f.Name):
		return SkipUnsafeName
	case tooLarge(f.UncompressedSize64, rules.MaxEntryBytes):
		return SkipTooLarge
	case ratioExceeded(f, rules.MaxRatio):
		return SkipRatioExceeded
	case !extensionAllowed(f.Name, rules.Extensions):
		return SkipExtension
	}
	return ""
}

// isPlatformMetadata matches the resource-fork and thumbnail litter desktop
// archivers add. A Finder zip carries a __MACOSX twin for every file, so
// without this rule the entry count doubles with junk.
func isPlatformMetadata(name string) bool {
	base := path.Base(name)
	if strings.HasPrefix(base, "._") || base == ".DS_Store" || base == "Thumbs.db" {
		return true
	}
	return slices.Contains(strings.Split(name, "/"), "__MACOSX")
}

// safeName is the check archive/zip performs under GODEBUG=zipinsecurepath=0,
// applied locally so it does not depend on the environment. Storage keys are
// UUIDs and nothing here touches a filesystem path, so this is belt and
// braces — but a name that cannot be local is not worth storing.
func safeName(name string) bool {
	return filepath.IsLocal(name) && !strings.Contains(name, `\`)
}

func tooLarge(size uint64, limit int64) bool {
	if size > math.MaxInt64 {
		return true
	}
	return limit > 0 && int64(size) > limit
}

// ratioExceeded flags a deflate entry whose declared expansion is implausible
// for a document — the zip-bomb signature. Deflate tops out near 1032:1, so the
// cap is a tradeoff and not a bright line: at the default of 100 the guard also
// refuses genuinely repetitive documents — a CSV of repeated values, an
// application log, an XML export — which compress well past 100:1 and are then
// skipped as ratio_exceeded with no error, only a counter. Raise
// CAIRNMARK_ARCHIVE_MAX_RATIO where that is the corpus; the closer it gets to
// 1032 the less the guard does.
func ratioExceeded(f *zip.File, limit int64) bool {
	if limit <= 0 || f.Method == zip.Store || f.UncompressedSize64 == 0 {
		return false
	}
	if f.CompressedSize64 == 0 {
		return true
	}
	return f.UncompressedSize64/f.CompressedSize64 > uint64(limit)
}

func extensionAllowed(name string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	return slices.Contains(allowed, strings.ToLower(path.Ext(name)))
}
