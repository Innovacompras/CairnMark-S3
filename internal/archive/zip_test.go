package archive_test

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"testing"

	"github.com/mettjs/cairnmark/internal/archive"
)

// fixture assembles a zip in memory. Entries are added in order, so their
// indexes are predictable.
type fixture struct {
	t   *testing.T
	buf bytes.Buffer
	w   *zip.Writer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t}
	f.w = zip.NewWriter(&f.buf)
	return f
}

func (f *fixture) header(h *zip.FileHeader, data string) {
	f.t.Helper()
	w, err := f.w.CreateHeader(h)
	if err != nil {
		f.t.Fatalf("CreateHeader %s: %v", h.Name, err)
	}
	if _, err := io.WriteString(w, data); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) deflate(name, data string) {
	f.header(&zip.FileHeader{Name: name, Method: zip.Deflate}, data)
}

func (f *fixture) store(name, data string) {
	f.header(&zip.FileHeader{Name: name, Method: zip.Store}, data)
}

// raw writes data verbatim under a directory record the caller controls — the
// way to plant an unsupported method, a lying size, or a wrong CRC.
func (f *fixture) raw(h *zip.FileHeader, data string) {
	f.t.Helper()
	w, err := f.w.CreateRaw(h)
	if err != nil {
		f.t.Fatalf("CreateRaw %s: %v", h.Name, err)
	}
	if _, err := io.WriteString(w, data); err != nil {
		f.t.Fatal(err)
	}
}

// stored is a raw Store entry with a correct directory record.
func stored(name, data string) *zip.FileHeader {
	return &zip.FileHeader{
		Name: name, Method: zip.Store,
		CRC32:            crc32.ChecksumIEEE([]byte(data)),
		CompressedSize64: uint64(len(data)), UncompressedSize64: uint64(len(data)),
	}
}

func (f *fixture) bytes() []byte {
	f.t.Helper()
	if err := f.w.Close(); err != nil {
		f.t.Fatal(err)
	}
	return f.buf.Bytes()
}

func open(t *testing.T, data []byte, rules archive.Rules) archive.Walker {
	t.Helper()
	w, err := archive.OpenZip(bytes.NewReader(data), int64(len(data)), rules)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return w
}

func readEntry(t *testing.T, w archive.Walker, i int) string {
	t.Helper()
	rc, err := w.Open(i)
	if err != nil {
		t.Fatalf("Open(%d): %v", i, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read entry %d: %v", i, err)
	}
	return string(b)
}

func TestClassifyEntries(t *testing.T) {
	f := newFixture(t)
	f.deflate("reports/q3.pdf", "%PDF-1.7 quarterly")                                                                           // 0
	f.store("reports/data.json", `{"ok":true}`)                                                                                 // 1
	f.header(&zip.FileHeader{Name: "reports/"}, "")                                                                             // 2 directory
	f.deflate("__MACOSX/reports/._q3.pdf", "resource fork")                                                                     // 3
	f.deflate("reports/.DS_Store", "finder")                                                                                    // 4
	f.deflate("Thumbs.db", "explorer")                                                                                          // 5
	f.header(&zip.FileHeader{Name: "secret.txt", Method: zip.Deflate, Flags: 0x1}, "cipher")                                    // 6 encrypted
	f.raw(&zip.FileHeader{Name: "old.bz2", Method: 12, CompressedSize64: 2, UncompressedSize64: 2}, "bz")                       // 7 bzip2
	f.deflate("../escape.txt", "slip")                                                                                          // 8
	f.deflate(`win\path.txt`, "backslash")                                                                                      // 9
	f.raw(&zip.FileHeader{Name: "huge.bin", Method: zip.Store, CompressedSize64: 5000, UncompressedSize64: 5000}, "x")          // 10
	f.raw(&zip.FileHeader{Name: "bomb.bin", Method: zip.Deflate, CompressedSize64: 10, UncompressedSize64: 4000}, "0123456789") // 11 ratio 400
	f.deflate("notes.md", "not on the allowlist")                                                                               // 12
	f.deflate("reports/q3.pdf", "duplicate name, distinct index")                                                               // 13
	f.deflate("empty.txt", "")                                                                                                  // 14 zero bytes: kept
	link := &zip.FileHeader{Name: "link.pdf"}
	link.SetMode(fs.ModeSymlink | 0o777)
	f.header(link, "reports/q3.pdf") // 15

	rules := archive.Rules{
		MaxEntryBytes: 4096, MaxRatio: 100,
		Extensions: []string{".pdf", ".json", ".txt", ".bin", ".bz2"},
	}
	w := open(t, f.bytes(), rules)
	entries := w.Entries()
	if len(entries) != 16 {
		t.Fatalf("entries: got %d want 16", len(entries))
	}

	want := map[int]archive.SkipReason{
		0: "", 1: "", 2: archive.SkipDirectory,
		3: archive.SkipPlatformMetadata, 4: archive.SkipPlatformMetadata, 5: archive.SkipPlatformMetadata,
		6: archive.SkipEncrypted, 7: archive.SkipUnsupportedMethod,
		8: archive.SkipUnsafeName, 9: archive.SkipUnsafeName,
		10: archive.SkipTooLarge, 11: archive.SkipRatioExceeded, 12: archive.SkipExtension,
		13: "", 14: "", 15: archive.SkipNonRegular,
	}
	for i, e := range entries {
		if e.Index != i {
			t.Errorf("entry %d: Index = %d", i, e.Index)
		}
		if e.Skip != want[i] {
			t.Errorf("entry %d (%s): skip = %q want %q", i, e.Name, e.Skip, want[i])
		}
	}
	if entries[0].ContentType != "application/pdf" || entries[1].ContentType != "application/json" {
		t.Errorf("content types from extension: %q %q", entries[0].ContentType, entries[1].ContentType)
	}
	if entries[14].Size != 0 || !entries[14].Selectable() {
		t.Errorf("a zero-byte entry is a legitimate file and must be kept: %+v", entries[14])
	}

	// Duplicate names are distinct entries with distinct content, and content
	// round-trips for both compression methods.
	if got := readEntry(t, w, 0); got != "%PDF-1.7 quarterly" {
		t.Errorf("entry 0: %q", got)
	}
	if got := readEntry(t, w, 1); got != `{"ok":true}` {
		t.Errorf("entry 1: %q", got)
	}
	if got := readEntry(t, w, 13); got != "duplicate name, distinct index" {
		t.Errorf("entry 13: %q", got)
	}
	if got := readEntry(t, w, 14); got != "" {
		t.Errorf("entry 14: %q", got)
	}

	// Skipped and out-of-range entries cannot be opened.
	if _, err := w.Open(6); err == nil {
		t.Error("Open on a skipped entry should fail")
	}
	if _, err := w.Open(16); err == nil {
		t.Error("Open out of range should fail")
	}
}

func TestOpenSurfacesCRCVerdictBeforeTheLastByte(t *testing.T) {
	const body = "eleven bytes"
	bad := stored("bad.txt", body)
	bad.CRC32 = 0xDEADBEEF
	f := newFixture(t)
	f.raw(stored("good.txt", body), body) // 0
	f.raw(bad, body)                      // 1
	data := f.bytes()
	w := open(t, data, archive.Rules{})

	// A consumer that reads exactly the declared size and never asks again —
	// io.ReadFull here, an S3 multipart uploader in production — still sees a
	// corrupt entry fail, and receives none of its bytes...
	rc, err := w.Open(1)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	n, err := io.ReadFull(rc, make([]byte, len(body)))
	if !errors.Is(err, zip.ErrChecksum) || n != 0 {
		t.Fatalf("corrupt entry via ReadFull: n=%d err=%v, want 0 and zip.ErrChecksum", n, err)
	}

	// ...which is exactly what archive/zip alone does not give it.
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := zr.File[1].Open()
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if _, err := io.ReadFull(plain, make([]byte, len(body))); err != nil {
		t.Fatalf("stdlib ReadFull unexpectedly reported the bad CRC itself: %v", err)
	}

	// A sound entry delivers exactly its bytes, then a clean EOF.
	good, err := w.Open(0)
	if err != nil {
		t.Fatal(err)
	}
	defer good.Close()
	buf := make([]byte, len(body))
	if _, err := io.ReadFull(good, buf); err != nil || string(buf) != body {
		t.Fatalf("good entry: %q err=%v", buf, err)
	}
	if n, err := good.Read(buf); n != 0 || err != io.EOF {
		t.Fatalf("after the last byte: n=%d err=%v, want 0 and io.EOF", n, err)
	}
}

func TestOpenPrefixedArchive(t *testing.T) {
	// A self-extracting archive is a program with a zip appended. The zip's
	// structure hangs off its tail, so it still parses and reads.
	f := newFixture(t)
	f.deflate("payload.txt", "still a zip")
	stub := bytes.Repeat([]byte("MZ\x90\x00 pretend extractor stub "), 200)
	data := append(stub, f.bytes()...)

	w := open(t, data, archive.Rules{})
	if got := readEntry(t, w, 0); got != "still a zip" {
		t.Fatalf("entry 0: %q", got)
	}
}

func TestOpenRejectsNonArchives(t *testing.T) {
	f := newFixture(t)
	f.deflate("a.txt", "a")
	z := f.bytes()
	junk := make([]byte, 4096)
	if _, err := rand.Read(junk); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"random bytes", junk},
		{"a pdf", []byte("%PDF-1.7 not a zip at all")},
		{"truncated zip", z[:len(z)-10]},
	} {
		_, err := archive.OpenZip(bytes.NewReader(tc.data), int64(len(tc.data)), archive.Rules{})
		if !errors.Is(err, archive.ErrNotArchive) {
			t.Errorf("%s: got %v want ErrNotArchive", tc.name, err)
		}
	}
}

func TestOpenZip64(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 65 535-entry archive")
	}
	const n = 65535 // the record count that forces zip64 end-of-directory records
	f := newFixture(t)
	for i := range n {
		f.store(fmt.Sprintf("e%d", i), "x")
	}
	w := open(t, f.bytes(), archive.Rules{})
	if got := len(w.Entries()); got != n {
		t.Fatalf("entries: got %d want %d", got, n)
	}
	if got := readEntry(t, w, n-1); got != "x" {
		t.Fatalf("last entry: %q", got)
	}
}

// patchDeclaredEntryCount rewrites the end-of-central-directory record's entry
// count. The fixture carries no archive comment, so that record is the last 22
// bytes and the count is the uint16 at offset 10.
func patchDeclaredEntryCount(t *testing.T, data []byte, n uint16) {
	t.Helper()
	eocd := data[len(data)-22:]
	if binary.LittleEndian.Uint32(eocd) != 0x06054b50 {
		t.Fatal("fixture does not end in an end-of-central-directory record")
	}
	binary.LittleEndian.PutUint16(eocd[10:], n)
}

func TestDirectoryGuardRejectsALyingEntryCount(t *testing.T) {
	// The whole reason the guard measures bytes rather than the declared count:
	// archive/zip uses that count as a preallocation hint, reads records until
	// one is malformed, and compares the total only modulo 65536. An archive
	// can therefore claim 10 entries, carry thousands, and be parsed in full
	// before anything objects — so a guard that trusted the count would admit
	// exactly the input it exists to refuse.
	const n = 2000
	f := newFixture(t)
	for i := range n {
		f.header(stored(fmt.Sprintf("padding/entry-%05d.txt", i), "x"), "x")
	}
	data := f.bytes()
	patchDeclaredEntryCount(t, data, 10)

	if got := binary.LittleEndian.Uint16(data[len(data)-22+10:]); got != 10 {
		t.Fatalf("fixture should declare 10 entries, declares %d", got)
	}
	_, err := archive.OpenZip(bytes.NewReader(data), int64(len(data)),
		archive.Rules{MaxDirectoryBytes: 4 << 10})
	if !errors.Is(err, archive.ErrDirectoryTooLarge) {
		t.Fatalf("got %v want ErrDirectoryTooLarge", err)
	}
}

func TestDirectoryGuardAdmitsLongPathsUnderTheCap(t *testing.T) {
	// The bound is a byte span divided by the 46-byte minimum record, so it
	// over-counts entries. It must still admit an archive whose records are
	// realistically long rather than minimal.
	const n = 200
	f := newFixture(t)
	for i := range n {
		f.header(stored(fmt.Sprintf(
			"reports/2026/q3/regional/north/division-%03d/summary-%06d.pdf", i, i), "x"), "x")
	}
	data := f.bytes()

	w := open(t, data, archive.Rules{MaxDirectoryBytes: int64(n) * 512})
	if got := len(w.Entries()); got != n {
		t.Fatalf("entries: got %d want %d", got, n)
	}
}

func TestDirectoryGuardSurvivesAnArchiveComment(t *testing.T) {
	// The end record sits before the comment, so locating it means checking the
	// comment length accounts for exactly the trailing bytes — a scan that
	// simply took the last signature would read a bogus directory offset.
	f := newFixture(t)
	f.header(stored("a.txt", "one"), "one")
	f.header(stored("b.txt", "two"), "two")
	if err := f.w.SetComment("built by the fixture, and long enough to matter"); err != nil {
		t.Fatal(err)
	}
	data := f.bytes()

	w := open(t, data, archive.Rules{})
	if got := len(w.Entries()); got != 2 {
		t.Fatalf("entries: got %d want 2", got)
	}
	if got := readEntry(t, w, 1); got != "two" {
		t.Fatalf("entry 1: %q", got)
	}
}

func TestDirectoryGuardAgreesWithStdlibOnADecoySignature(t *testing.T) {
	// A comment carrying the end-of-directory signature defeats archive/zip's
	// own backscan, so stdlib calls such an archive invalid. The guard runs
	// first and must reach the same verdict — a pre-parse check that accepted
	// what the parser then rejects would be a divergence, and one that rejected
	// what it accepts would lose valid archives.
	f := newFixture(t)
	f.header(stored("a.txt", "one"), "one")
	if err := f.w.SetComment("decoy PK\x05\x06 aaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	data := f.bytes()

	if _, err := zip.NewReader(bytes.NewReader(data), int64(len(data))); err == nil {
		t.Skip("stdlib now reads a decoy comment; the premise of this test is gone")
	}
	_, err := archive.OpenZip(bytes.NewReader(data), int64(len(data)), archive.Rules{})
	if !errors.Is(err, archive.ErrNotArchive) {
		t.Fatalf("got %v want ErrNotArchive, matching stdlib", err)
	}
}

// failingReaderAt fails every read, as a store that has gone away would.
type failingReaderAt struct{ err error }

func (f failingReaderAt) ReadAt([]byte, int64) (int, error) { return 0, f.err }

func TestOpenReportsAStoreFailureAsItself(t *testing.T) {
	// A read that fails is not evidence about the archive: the guard must
	// pass the store's error up rather than call bytes it never saw "not a
	// zip", or an outage would be reported as a client error.
	boom := errors.New("s3: connection refused")
	_, err := archive.OpenZip(failingReaderAt{boom}, 1<<20, archive.Rules{})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v want the store's error", err)
	}
	if errors.Is(err, archive.ErrNotArchive) {
		t.Fatal("a store failure was reported as not-an-archive")
	}
}
