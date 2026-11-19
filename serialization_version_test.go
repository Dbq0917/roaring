package roaring

// Tests for the versioned serialization trailer: the mechanism that keeps
// new files readable by old readers (forward compatibility) and old files
// readable by new readers (backward compatibility), at a constant small
// size overhead and with no change to the hot read path.

import (
	"bytes"
	"encoding/binary"
	"io/ioutil"
	"path/filepath"
	"testing"
)

// versionedTestBitmap builds a bitmap exercising all three container kinds.
func versionedTestBitmap() *Bitmap {
	rb := NewBitmap()
	for i := uint32(0); i < 100; i++ { // array container
		rb.Add(i * 1000)
	}
	for i := uint32(0); i < 5000; i++ { // bitmap container
		rb.Add(1<<16*2 + i)
	}
	rb.AddRange(1<<16*5, 1<<16*5+10000) // run container after RunOptimize
	rb.RunOptimize()
	return rb
}

func legacyBytesOf(rb *Bitmap) []byte {
	buf := &bytes.Buffer{}
	if _, err := rb.highlowcontainer.writeToLegacy(buf); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func TestVersionedFormatRoundTrip(t *testing.T) {
	rb := versionedTestBitmap()

	buf := &bytes.Buffer{}
	if _, err := rb.WriteTo(buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if uint64(buf.Len()) != rb.GetSerializedSizeInBytes() {
		t.Fatalf("GetSerializedSizeInBytes=%d does not match %d written bytes",
			rb.GetSerializedSizeInBytes(), buf.Len())
	}
	serialized := append([]byte{}, buf.Bytes()...)

	viaStream := NewBitmap()
	if _, err := viaStream.ReadFrom(buf); err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if !viaStream.Equals(rb) {
		t.Fatal("stream round trip changed the bitmap")
	}
	if viaStream.FormatVersion() != formatVersionCurrent {
		t.Fatalf("FormatVersion=%d, want %d", viaStream.FormatVersion(), formatVersionCurrent)
	}

	viaBuffer := NewBitmap()
	if _, err := viaBuffer.FromBuffer(serialized); err != nil {
		t.Fatalf("FromBuffer: %v", err)
	}
	if !viaBuffer.Equals(rb) {
		t.Fatal("buffer round trip changed the bitmap")
	}
}

// TestVersionedFormatPrefixProperty is the forward-compatibility guarantee:
// a current file is exactly a legacy file plus a constant-size trailer, so a
// legacy reader consumes the prefix it understands and ignores the rest.
// This is what makes rolling back to an older binary safe.
func TestVersionedFormatPrefixProperty(t *testing.T) {
	rb := versionedTestBitmap()

	legacy := legacyBytesOf(rb)
	current, err := rb.ToBytes()
	if err != nil {
		t.Fatalf("ToBytes: %v", err)
	}

	if got := len(current) - len(legacy); got != trailerHeaderSize {
		t.Fatalf("size overhead is %d bytes, want a constant %d", got, trailerHeaderSize)
	}
	if !bytes.Equal(current[:len(legacy)], legacy) {
		t.Fatal("current format is not a byte-exact extension of the legacy format")
	}

	// What a legacy reader sees when given a current file: the prefix.
	seenByLegacyReader := NewBitmap()
	if _, err := seenByLegacyReader.FromBuffer(current[:len(legacy)]); err != nil {
		t.Fatalf("legacy parse of current file: %v", err)
	}
	if !seenByLegacyReader.Equals(rb) {
		t.Fatal("legacy reader would not recover the full bitmap from a current file")
	}
}

func TestVersionedFormatReadsLegacyData(t *testing.T) {
	// Legacy fixtures on disk (written before format versioning existed).
	for _, name := range []string{"bitmapwithoutruns.bin", "bitmapwithruns.bin"} {
		data, err := ioutil.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatalf("read fixture: %v", err)
		}
		rb := NewBitmap()
		if _, err := rb.FromBuffer(data); err != nil {
			t.Fatalf("FromBuffer(%s): %v", name, err)
		}
		if rb.FormatVersion() != 0 {
			t.Fatalf("%s: FormatVersion=%d, want 0 for legacy data", name, rb.FormatVersion())
		}
		if rb.GetCardinality() == 0 {
			t.Fatalf("%s: unexpected empty bitmap", name)
		}
	}

	// In-memory legacy encoding must read back as version 0 as well.
	rb := versionedTestBitmap()
	legacy := legacyBytesOf(rb)
	back := NewBitmap()
	if _, err := back.ReadFrom(bytes.NewReader(legacy)); err != nil {
		t.Fatalf("ReadFrom(legacy): %v", err)
	}
	if !back.Equals(rb) || back.FormatVersion() != 0 {
		t.Fatalf("legacy stream: equals=%v version=%d", back.Equals(rb), back.FormatVersion())
	}
}

// TestVersionedFormatSkipsUnknownExtension ensures the next format bump does
// not repeat the original incident: a reader must skip extension payloads it
// does not understand instead of rejecting the file.
func TestVersionedFormatSkipsUnknownExtension(t *testing.T) {
	rb := versionedTestBitmap()
	legacy := legacyBytesOf(rb)

	ext := []byte("some future field")
	future := append([]byte{}, legacy...)
	var hdr [trailerHeaderSize]byte
	binary.LittleEndian.PutUint32(hdr[0:], uint32(trailerMagic))
	binary.LittleEndian.PutUint16(hdr[4:], uint16(formatVersionCurrent+1))
	binary.LittleEndian.PutUint32(hdr[8:], uint32(len(ext)))
	future = append(future, hdr[:]...)
	future = append(future, ext...)

	back := NewBitmap()
	if _, err := back.FromBuffer(future); err != nil {
		t.Fatalf("FromBuffer(future version): %v", err)
	}
	if !back.Equals(rb) {
		t.Fatal("unknown extension payload broke the base data")
	}
	if back.FormatVersion() != formatVersionCurrent+1 {
		t.Fatalf("FormatVersion=%d, want %d", back.FormatVersion(), formatVersionCurrent+1)
	}

	backStream := NewBitmap()
	if _, err := backStream.ReadFrom(bytes.NewReader(future)); err != nil {
		t.Fatalf("ReadFrom(future version): %v", err)
	}
	if !backStream.Equals(rb) {
		t.Fatal("stream read of unknown extension broke the base data")
	}
}

func TestVersionedFormatCorruptTrailer(t *testing.T) {
	rb := versionedTestBitmap()
	legacy := legacyBytesOf(rb)

	// Trailer claims an extension that is not there.
	corrupt := append([]byte{}, legacy...)
	var hdr [trailerHeaderSize]byte
	binary.LittleEndian.PutUint32(hdr[0:], uint32(trailerMagic))
	binary.LittleEndian.PutUint16(hdr[4:], uint16(formatVersionCurrent))
	binary.LittleEndian.PutUint32(hdr[8:], uint32(1<<20))
	corrupt = append(corrupt, hdr[:]...)

	if _, err := NewBitmap().FromBuffer(corrupt); err == nil {
		t.Fatal("FromBuffer accepted a trailer whose extension overruns the buffer")
	}
	if _, err := NewBitmap().ReadFrom(bytes.NewReader(corrupt)); err == nil {
		t.Fatal("ReadFrom accepted a truncated trailer extension")
	}

	// Truncated trailer header on a stream.
	truncated := append([]byte{}, legacy...)
	truncated = append(truncated, hdr[:8]...)
	if _, err := NewBitmap().ReadFrom(bytes.NewReader(truncated)); err == nil {
		t.Fatal("ReadFrom accepted a truncated trailer header")
	}
}

func BenchmarkDeserializationVersioned(b *testing.B) {
	rb := NewBitmap()
	for i := uint32(0); i < 1000000; i += 3 {
		rb.Add(i)
	}
	legacy := legacyBytesOf(rb)
	current, err := rb.ToBytes()
	if err != nil {
		b.Fatalf("ToBytes: %v", err)
	}

	b.Run("legacy", func(b *testing.B) {
		b.SetBytes(int64(len(legacy)))
		rb := NewBitmap()
		for i := 0; i < b.N; i++ {
			if _, err := rb.FromBuffer(legacy); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("versioned", func(b *testing.B) {
		b.SetBytes(int64(len(current)))
		rb := NewBitmap()
		for i := 0; i < b.N; i++ {
			if _, err := rb.FromBuffer(current); err != nil {
				b.Fatal(err)
			}
		}
	})
}
