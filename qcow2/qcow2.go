// Package qcow2 reads QEMU disk images: enough of the format to read the
// disk 9front's arm64 release ships as, read-only and without backing
// files, snapshots, or encryption.
package qcow2

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Image is an opened qcow2 file. It reads as the raw disk.
type Image struct {
	r           io.ReaderAt
	size        int64
	clusterBits uint
	l1          []uint64
	zstd        bool

	mu    sync.Mutex
	l2    map[uint64][]uint64 // L2 tables by host offset
	cache map[int64][]byte    // decompressed clusters by guest cluster
}

const (
	copied     = 1 << 63
	compressed = 1 << 62
	zeroFlag   = 1
	offMask    = 0x00fffffffffffe00
)

// Open reads an image's header and L1 table.
func Open(r io.ReaderAt) (*Image, error) {
	h := make([]byte, 112)
	if _, err := r.ReadAt(h, 0); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	be := binary.BigEndian
	if string(h[:4]) != "QFI\xfb" {
		return nil, errors.New("qcow2: not a qcow2 image")
	}
	version := be.Uint32(h[4:])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("qcow2: version %d", version)
	}
	if be.Uint64(h[8:]) != 0 {
		return nil, errors.New("qcow2: backing files aren't supported")
	}
	if be.Uint32(h[32:]) != 0 {
		return nil, errors.New("qcow2: encrypted images aren't supported")
	}
	im := &Image{
		r:           r,
		clusterBits: uint(be.Uint32(h[20:])),
		size:        int64(be.Uint64(h[24:])),
		l2:          map[uint64][]uint64{},
		cache:       map[int64][]byte{},
	}
	if im.clusterBits < 9 || im.clusterBits > 21 {
		return nil, fmt.Errorf("qcow2: cluster bits %d", im.clusterBits)
	}
	if version == 3 {
		incompat := be.Uint64(h[72:])
		if hlen := be.Uint32(h[100:]); hlen > 104 {
			switch h[104] {
			case 0:
			case 1:
				im.zstd = true
			default:
				return nil, fmt.Errorf("qcow2: compression type %d", h[104])
			}
		}
		if incompat&^(1<<3) != 0 { // only the compression-type bit
			return nil, fmt.Errorf("qcow2: incompatible features %#x", incompat)
		}
	}
	n := be.Uint32(h[36:])
	l1 := make([]byte, 8*int(n))
	if _, err := r.ReadAt(l1, int64(be.Uint64(h[40:]))); err != nil {
		return nil, fmt.Errorf("qcow2: L1 table: %w", err)
	}
	im.l1 = make([]uint64, n)
	for i := range im.l1 {
		im.l1[i] = be.Uint64(l1[8*i:])
	}
	return im, nil
}

// Size is the disk's size in bytes.
func (im *Image) Size() int64 { return im.size }

func (im *Image) clusterSize() int64 { return 1 << im.clusterBits }

// ReadAt reads the raw disk.
func (im *Image) ReadAt(p []byte, off int64) (int, error) {
	n := 0
	for n < len(p) {
		pos := off + int64(n)
		if pos >= im.size {
			return n, io.EOF
		}
		c, err := im.cluster(pos >> im.clusterBits)
		if err != nil {
			return n, err
		}
		in := pos & (im.clusterSize() - 1)
		m := copy(p[n:], c[in:])
		if rem := im.size - pos; int64(m) > rem {
			m = int(rem)
		}
		n += m
	}
	return n, nil
}

// cluster returns one guest cluster's bytes.
func (im *Image) cluster(idx int64) ([]byte, error) {
	im.mu.Lock()
	if c, ok := im.cache[idx]; ok {
		im.mu.Unlock()
		return c, nil
	}
	im.mu.Unlock()
	cs := im.clusterSize()
	perL2 := cs / 8
	l1i := idx / perL2
	out := make([]byte, cs)
	if l1i >= int64(len(im.l1)) {
		return out, nil
	}
	l2off := im.l1[l1i] & offMask
	if l2off == 0 {
		return out, nil
	}
	l2, err := im.table(l2off)
	if err != nil {
		return nil, err
	}
	e := l2[idx%perL2]
	switch {
	case e&compressed != 0:
		x := 62 - (im.clusterBits - 8)
		hostOff := int64(e & (1<<x - 1))
		sectors := int64((e>>x)&(1<<(im.clusterBits-8)-1)) + 1
		n := sectors*512 - hostOff&511
		buf := make([]byte, n)
		if m, err := im.r.ReadAt(buf, hostOff); err != nil && !(errors.Is(err, io.EOF) && m > 0) {
			return nil, err
		} else {
			buf = buf[:m]
		}
		var zr io.Reader
		if im.zstd {
			d, err := zstd.NewReader(bytes.NewReader(buf))
			if err != nil {
				return nil, err
			}
			defer d.Close()
			zr = d
		} else {
			zr = flate.NewReader(bytes.NewReader(buf))
		}
		if _, err := io.ReadFull(zr, out); err != nil {
			return nil, fmt.Errorf("qcow2: cluster %d: %w", idx, err)
		}
	case e&zeroFlag != 0 || e&offMask == 0:
	default:
		if _, err := im.r.ReadAt(out, int64(e&offMask)); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
	}
	im.mu.Lock()
	if len(im.cache) > 256 {
		im.cache = map[int64][]byte{}
	}
	im.cache[idx] = out
	im.mu.Unlock()
	return out, nil
}

func (im *Image) table(off uint64) ([]uint64, error) {
	im.mu.Lock()
	t, ok := im.l2[off]
	im.mu.Unlock()
	if ok {
		return t, nil
	}
	raw := make([]byte, im.clusterSize())
	if _, err := im.r.ReadAt(raw, int64(off)); err != nil {
		return nil, fmt.Errorf("qcow2: L2 table: %w", err)
	}
	t = make([]uint64, len(raw)/8)
	for i := range t {
		t[i] = binary.BigEndian.Uint64(raw[8*i:])
	}
	im.mu.Lock()
	im.l2[off] = t
	im.mu.Unlock()
	return t, nil
}
