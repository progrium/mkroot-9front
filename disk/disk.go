// Package disk finds a Plan 9 partition on a disk image: the MBR entry of
// type 0x39 holds a prep table in its second sector, which names the
// partitions inside it, such as fs and nvram.
package disk

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const sector = 512

// Part returns the named partition of the disk's Plan 9 partition.
func Part(r io.ReaderAt, name string) (*io.SectionReader, error) {
	mbr := make([]byte, sector)
	if _, err := r.ReadAt(mbr, 0); err != nil {
		return nil, fmt.Errorf("read mbr: %w", err)
	}
	if mbr[510] != 0x55 || mbr[511] != 0xAA {
		return nil, fmt.Errorf("no mbr signature")
	}
	for i := 0; i < 4; i++ {
		e := mbr[446+16*i:][:16]
		if e[4] != 0x39 {
			continue
		}
		start := int64(binary.LittleEndian.Uint32(e[8:]))
		size := int64(binary.LittleEndian.Uint32(e[12:]))
		prep := make([]byte, sector)
		if _, err := r.ReadAt(prep, (start+1)*sector); err != nil {
			return nil, fmt.Errorf("read prep table: %w", err)
		}
		if i := bytes.IndexByte(prep, 0); i >= 0 {
			prep = prep[:i]
		}
		for _, line := range strings.Split(string(prep), "\n") {
			f := strings.Fields(line)
			if len(f) != 4 || f[0] != "part" || f[1] != name {
				continue
			}
			lo, err1 := strconv.ParseInt(f[2], 10, 64)
			hi, err2 := strconv.ParseInt(f[3], 10, 64)
			if err1 != nil || err2 != nil || lo >= hi || hi > size {
				return nil, fmt.Errorf("bad prep entry %q", line)
			}
			return io.NewSectionReader(r, (start+lo)*sector, (hi-lo)*sector), nil
		}
		return nil, fmt.Errorf("no %s partition in prep table %q", name, prep)
	}
	return nil, fmt.Errorf("no plan9 partition")
}
