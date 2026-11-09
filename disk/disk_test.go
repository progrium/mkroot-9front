package disk

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

func TestPart(t *testing.T) {
	d := make([]byte, 64*sector)
	e := d[446+16:] // second MBR entry
	e[4] = 0x39
	binary.LittleEndian.PutUint32(e[8:], 8)
	binary.LittleEndian.PutUint32(e[12:], 40)
	d[510], d[511] = 0x55, 0xAA
	copy(d[9*sector:], "part nvram 2 3\npart fs 3 40\n")
	copy(d[(8+3)*sector:], "fs here")

	p, err := Part(bytes.NewReader(d), "fs")
	if err != nil {
		t.Fatal(err)
	}
	if p.Size() != 37*sector {
		t.Errorf("size %d, want %d", p.Size(), 37*sector)
	}
	b := make([]byte, 7)
	if _, err := io.ReadFull(p, b); err != nil || string(b) != "fs here" {
		t.Errorf("read %q, %v", b, err)
	}
	if _, err := Part(bytes.NewReader(d), "cache"); err == nil {
		t.Error("found a cache partition that isn't there")
	}
}
