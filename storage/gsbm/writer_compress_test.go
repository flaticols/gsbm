package gsbm

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
)

// pinnedMarshaler emits the same body as testdata/legacy_uncompressed.bin:
// tag=1 LengthDelim "abc" then tag=2 Varint 42. Size and write produce the
// same byte sequence so Marshal's full output is reproducible across
// versions — the load-bearing backward-compat property the legacy testdata
// file pins on the reader side, asserted here on the writer side.
type pinnedMarshaler struct{}

func (pinnedMarshaler) SizeGSBM() int {
	// tag1key + lenvarint + "abc" + tag2key + 42
	return 1 + 1 + 3 + 1 + 1
}

func (pinnedMarshaler) MarshalGSBM(w *Writer) error {
	w.WriteTag(1, WireLengthDelim)
	w.WriteString("abc")
	w.WriteTag(2, WireVarint)
	w.WriteUvarint(42)
	return w.Err()
}

// TestMarshalBackwardCompatPinned asserts that today's Marshal output is
// byte-for-byte identical to the pinned legacy blob. The plan calls this
// the load-bearing backward-compat assertion: existing callers who hold
// blobs written by prior versions must continue to see the same bytes
// when re-encoding equivalent values. A diff here means we've shifted
// the wire format under existing readers — investigate before changing.
func TestMarshalBackwardCompatPinned(t *testing.T) {
	path := filepath.Join("testdata", "legacy_uncompressed.bin")
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	got, err := Marshal(pinnedMarshaler{}, 0x1234)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Marshal bytes drifted from legacy pin:\n got %x\nwant %x", got, want)
	}
}

// TestMarshalWithOptionsZeroIsNoOp asserts MarshalWithOptions with the
// zero Options value produces bytes byte-for-byte identical to plain
// Marshal. The two entry points must be interchangeable when no opt-in
// flag is set — callers migrating from Marshal to MarshalWithOptions for
// later opt-in compression must not see any wire-level diff today.
func TestMarshalWithOptionsZeroIsNoOp(t *testing.T) {
	a, err := Marshal(pinnedMarshaler{}, 0x1234)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	b, err := MarshalWithOptions(pinnedMarshaler{}, 0x1234, Options{})
	if err != nil {
		t.Fatalf("MarshalWithOptions: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("MarshalWithOptions{} differs from Marshal:\n a=%x\n b=%x", a, b)
	}
}

// TestMarshalWithOptionsCompressFlagAndFrame asserts the zstd path sets
// the zstd compression method in the flags byte, records the compressed
// length in the header, and emits a body whose first four bytes are the
// zstd frame magic (28 B5 2F FD). Pinned to an explicit CompressionZstd
// because the default codec is now gzip.
func TestMarshalWithOptionsCompressFlagAndFrame(t *testing.T) {
	blob, err := MarshalWithOptions(pinnedMarshaler{}, 0xABCD, Options{Compression: CompressionZstd})
	if err != nil {
		t.Fatalf("MarshalWithOptions{Compression:CompressionZstd}: %v", err)
	}
	if len(blob) < HeaderSize+4 {
		t.Fatalf("blob too short: len=%d", len(blob))
	}
	if string(blob[0:4]) != Magic {
		t.Fatalf("magic mismatch: %x", blob[0:4])
	}
	if blob[4] != FmtVer2 {
		t.Fatalf("fmtVer = %d, want %d (no bump allowed)", blob[4], FmtVer2)
	}
	if blob[5] != FlagCompressed {
		t.Fatalf("flags = %#x, want %#x", blob[5], FlagCompressed)
	}
	schemaHint := binary.LittleEndian.Uint16(blob[6:8])
	if schemaHint != 0xABCD {
		t.Fatalf("schemaHint = %#x, want 0xABCD", schemaHint)
	}
	bodyLen := binary.LittleEndian.Uint32(blob[8:12])
	if int(bodyLen) != len(blob)-HeaderSize {
		t.Fatalf("bodyLen = %d, want %d (compressed on-disk length)", bodyLen, len(blob)-HeaderSize)
	}
	// zstd frame magic: 0xFD2FB528 little-endian on the wire.
	body := blob[HeaderSize:]
	if !bytes.HasPrefix(body, []byte{0x28, 0xB5, 0x2F, 0xFD}) {
		t.Fatalf("body does not start with zstd frame magic: %x", body[:4])
	}
}

// TestMarshalWithOptionsCompressRoundTrip exercises the full
// encode-compress / decompress-decode loop for each codec: a compressed
// blob must decode through NewReader+ReadHeader+primitive reads exactly
// the same way an uncompressed blob of the same payload does. Without
// this, compression would be write-only. Running both codecs in one table
// pins that the gzip path is symmetric with the zstd path.
func TestMarshalWithOptionsCompressRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method CompressionMethod
	}{
		{"zstd", CompressionZstd},
		{"gzip", CompressionGzip},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blob, err := MarshalWithOptions(pinnedMarshaler{}, 0x0042, Options{Compression: tc.method})
			if err != nil {
				t.Fatalf("MarshalWithOptions{Compression:%d}: %v", tc.method, err)
			}

			r := NewReader(blob)
			flags, schemaHint, _, err := r.ReadHeader()
			if err != nil {
				t.Fatalf("ReadHeader: %v", err)
			}
			if flags != uint8(tc.method) {
				t.Fatalf("flags = %#x, want %#x", flags, uint8(tc.method))
			}
			if schemaHint != 0x0042 {
				t.Fatalf("schemaHint = %#x, want 0x0042", schemaHint)
			}

			tag, wt, err := r.ReadTag()
			if err != nil {
				t.Fatalf("ReadTag #1: %v", err)
			}
			if tag != 1 || wt != WireLengthDelim {
				t.Fatalf("field #1 key: tag=%d wt=%d", tag, wt)
			}
			s, err := r.ReadString()
			if err != nil {
				t.Fatalf("ReadString: %v", err)
			}
			if s != "abc" {
				t.Fatalf("field #1 = %q, want %q", s, "abc")
			}

			tag, wt, err = r.ReadTag()
			if err != nil {
				t.Fatalf("ReadTag #2: %v", err)
			}
			if tag != 2 || wt != WireVarint {
				t.Fatalf("field #2 key: tag=%d wt=%d", tag, wt)
			}
			v, err := r.ReadUvarint()
			if err != nil {
				t.Fatalf("ReadUvarint: %v", err)
			}
			if v != 42 {
				t.Fatalf("field #2 = %d, want 42", v)
			}
			if r.HasMore() {
				t.Fatal("trailing bytes after compressed-blob decode")
			}
		})
	}
}

// TestMarshalWithOptionsGzipFlagAndFrame is the gzip mirror of
// TestMarshalWithOptionsCompressFlagAndFrame: it asserts the gzip path
// writes CompressionGzip (0x02) into the flags byte without bumping
// fmtVer, records the compressed on-disk length, and emits a body that
// starts with the gzip magic (1F 8B).
func TestMarshalWithOptionsGzipFlagAndFrame(t *testing.T) {
	blob, err := MarshalWithOptions(pinnedMarshaler{}, 0xABCD, Options{Compression: CompressionGzip})
	if err != nil {
		t.Fatalf("MarshalWithOptions{Compression:CompressionGzip}: %v", err)
	}
	if len(blob) < HeaderSize+2 {
		t.Fatalf("blob too short: len=%d", len(blob))
	}
	if string(blob[0:4]) != Magic {
		t.Fatalf("magic mismatch: %x", blob[0:4])
	}
	if blob[4] != FmtVer2 {
		t.Fatalf("fmtVer = %d, want %d (no bump allowed)", blob[4], FmtVer2)
	}
	if blob[5] != uint8(CompressionGzip) {
		t.Fatalf("flags = %#x, want %#x", blob[5], uint8(CompressionGzip))
	}
	bodyLen := binary.LittleEndian.Uint32(blob[8:12])
	if int(bodyLen) != len(blob)-HeaderSize {
		t.Fatalf("bodyLen = %d, want %d (compressed on-disk length)", bodyLen, len(blob)-HeaderSize)
	}
	// gzip magic: 0x1F 0x8B.
	body := blob[HeaderSize:]
	if !bytes.HasPrefix(body, []byte{0x1F, 0x8B}) {
		t.Fatalf("body does not start with gzip magic: %x", body[:2])
	}
}

// TestCompressBoolDefaultsToGzip pins the chosen default-codec behavior:
// the deprecated Options.Compress bool now selects gzip, not zstd. This
// is the soft-compat shift the change accepts — a regression here would
// silently revert the default and reintroduce zstd's larger resident
// footprint for callers using the legacy flag.
func TestCompressBoolDefaultsToGzip(t *testing.T) {
	blob, err := MarshalWithOptions(pinnedMarshaler{}, 0, Options{Compress: true})
	if err != nil {
		t.Fatalf("MarshalWithOptions{Compress:true}: %v", err)
	}
	if blob[5] != uint8(CompressionGzip) {
		t.Fatalf("flags = %#x, want gzip %#x (Compress:true must default to gzip)", blob[5], uint8(CompressionGzip))
	}
}

// TestExplicitCompressionWinsOverCompressBool asserts the resolution
// precedence: an explicit Compression field overrides the deprecated
// Compress bool, so callers can still pin zstd while the bool is set.
func TestExplicitCompressionWinsOverCompressBool(t *testing.T) {
	blob, err := MarshalWithOptions(pinnedMarshaler{}, 0, Options{Compress: true, Compression: CompressionZstd})
	if err != nil {
		t.Fatalf("MarshalWithOptions: %v", err)
	}
	if blob[5] != uint8(CompressionZstd) {
		t.Fatalf("flags = %#x, want zstd %#x (explicit Compression must win)", blob[5], uint8(CompressionZstd))
	}
}

// TestMarshalWithOptionsUnknownCodecRejected asserts the encoder refuses a
// reserved CompressionMethod (3–7) rather than writing a flags byte no
// decoder can interpret.
func TestMarshalWithOptionsUnknownCodecRejected(t *testing.T) {
	_, err := MarshalWithOptions(pinnedMarshaler{}, 0, Options{Compression: CompressionMethod(5)})
	if !errors.Is(err, ErrUnsupportedCompression) {
		t.Fatalf("err = %v, want ErrUnsupportedCompression", err)
	}
}

// TestMarshalWithOptionsCompressBodyIsValidZstd cross-checks the body
// using a freshly-constructed zstd decoder (not the pooled one) so a bug
// in the encoder pool would not be papered over by a matching bug in the
// decoder pool.
func TestMarshalWithOptionsCompressBodyIsValidZstd(t *testing.T) {
	blob, err := MarshalWithOptions(pinnedMarshaler{}, 0, Options{Compression: CompressionZstd})
	if err != nil {
		t.Fatalf("MarshalWithOptions{Compression:CompressionZstd}: %v", err)
	}
	body := blob[HeaderSize:]

	dec, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatalf("zstd.NewReader: %v", err)
	}
	defer dec.Close()
	raw, err := dec.DecodeAll(body, nil)
	if err != nil {
		t.Fatalf("DecodeAll: %v", err)
	}
	want := []byte{0x0A, 0x03, 0x61, 0x62, 0x63, 0x10, 0x2A}
	if !bytes.Equal(raw, want) {
		t.Fatalf("decompressed body = %x, want %x", raw, want)
	}
}

// TestMarshalWithOptionsCompressBodyIsValidGzip is the gzip mirror: it
// inflates the body with a freshly-constructed gzip.Reader (not the
// pooled one) so an encoder-pool bug cannot be hidden by a matching
// decoder-pool bug, and asserts the inflated bytes equal the canonical
// uncompressed body of pinnedMarshaler.
func TestMarshalWithOptionsCompressBodyIsValidGzip(t *testing.T) {
	blob, err := MarshalWithOptions(pinnedMarshaler{}, 0, Options{Compression: CompressionGzip})
	if err != nil {
		t.Fatalf("MarshalWithOptions{Compression:CompressionGzip}: %v", err)
	}
	body := blob[HeaderSize:]

	gr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer gr.Close()
	raw, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("gzip ReadAll: %v", err)
	}
	want := []byte{0x0A, 0x03, 0x61, 0x62, 0x63, 0x10, 0x2A}
	if !bytes.Equal(raw, want) {
		t.Fatalf("decompressed body = %x, want %x", raw, want)
	}
}

// TestMarshalWithOptionsErrorPropagation asserts that a MarshalGSBM
// failure surfaces from MarshalWithOptions, on both the compressed and
// uncompressed paths. The error must reach the caller — silent swallow
// would corrupt user data.
func TestMarshalWithOptionsErrorPropagation(t *testing.T) {
	sentinel := errors.New("boom")
	em := &errOnWrite{err: sentinel}

	for _, opts := range []Options{{}, {Compression: CompressionZstd}, {Compression: CompressionGzip}} {
		em.calls = 0
		_, err := MarshalWithOptions(em, 0, opts)
		if !errors.Is(err, sentinel) {
			t.Fatalf("opts=%+v: err=%v, want %v", opts, err, sentinel)
		}
	}
}

// errOnWrite is a Marshaler whose write pass fails after the size pass
// succeeds — the size pass returns the declared size cleanly. This
// exercises the error path of the second (real-mode) Writer in both
// marshalUncompressed and marshalCompressed.
type errOnWrite struct {
	calls int
	err   error
}

func (e *errOnWrite) SizeGSBM() int { return 0 }

func (e *errOnWrite) MarshalGSBM(w *Writer) error {
	e.calls++
	// First call is the size pass; second is the write pass. Fail on the
	// write pass so the size pass sees an error-free path.
	if e.calls >= 2 {
		w.setErr(e.err)
		return e.err
	}
	return nil
}
