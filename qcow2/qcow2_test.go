package qcow2

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"testing"
)

// TestReadAt reads a four-cluster image of 512-byte clusters: one stored
// as is, one deflated, one marked zero, and one unallocated.
func TestReadAt(t *testing.T) {
	const cs = 512
	be := binary.BigEndian
	f := make([]byte, 4*cs)
	copy(f, "QFI\xfb")
	be.PutUint32(f[4:], 2)
	be.PutUint32(f[20:], 9)    // cluster bits
	be.PutUint64(f[24:], 4*cs) // size
	be.PutUint32(f[36:], 1)    // L1 entries
	be.PutUint64(f[40:], cs)   // L1 offset
	be.PutUint64(f[cs:], 2*cs|copied)

	raw := bytes.Repeat([]byte("raw!"), cs/4)
	copy(f[3*cs:], raw)
	packed := bytes.Repeat([]byte("zip?"), cs/4)
	var z bytes.Buffer
	w, _ := flate.NewWriter(&z, flate.BestCompression)
	w.Write(packed)
	w.Close()
	zoff := uint64(len(f))
	f = append(f, z.Bytes()...)
	f = append(f, make([]byte, cs-z.Len())...)

	l2 := f[2*cs:]
	be.PutUint64(l2[0:], 3*cs|copied)
	be.PutUint64(l2[8:], compressed|zoff) // one sector
	be.PutUint64(l2[16:], zeroFlag)

	im, err := Open(bytes.NewReader(f))
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4*cs)
	if n, err := im.ReadAt(got, 0); n != len(got) || err != nil {
		t.Fatalf("read %d bytes: %v", n, err)
	}
	want := append(append(raw, packed...), make([]byte, 2*cs)...)
	if !bytes.Equal(got, want) {
		t.Fatal("contents differ")
	}
	part := make([]byte, 8)
	if _, err := im.ReadAt(part, cs-4); err != nil || string(part) != "raw!zip?" {
		t.Errorf("straddling read: %q, %v", part, err)
	}
}
