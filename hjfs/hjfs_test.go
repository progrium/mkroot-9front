package hjfs

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

// image builds a small hjfs image in memory.
type image struct{ blocks [][]byte }

func (im *image) alloc(typ byte) (uint64, []byte) {
	b := make([]byte, block)
	b[0] = typ
	im.blocks = append(im.blocks, b)
	return uint64(len(im.blocks) - 1), b[1:]
}

type ent struct {
	name     string
	uid, gid int16
	mode     uint16
	dir      bool
	size     uint64
	db       []uint64
	ib       []uint64
	mtime    int64
}

func putDentry(b []byte, e ent) {
	le := binary.LittleEndian
	copy(b, e.name)
	p := nameLen
	le.PutUint16(b[p:], uint16(e.uid))
	le.PutUint16(b[p+4:], uint16(e.gid))
	le.PutUint16(b[p+6:], e.mode|dAlloc)
	p += 20
	if e.dir {
		b[p] = qtDir
	}
	p++
	le.PutUint64(b[p:], e.size)
	p += 8
	for i, v := range e.db {
		le.PutUint64(b[p+8*i:], v)
	}
	p += 8 * nDirect
	for i, v := range e.ib {
		le.PutUint64(b[p+8*i:], v)
	}
	p += 8*nIndirect + 8
	le.PutUint64(b[p:], uint64(e.mtime))
}

// file writes data blocks and returns the entry's block pointers, with
// one level of indirection past the direct blocks.
func (im *image) file(data []byte) (db, ib []uint64) {
	var locs []uint64
	for len(data) > 0 {
		n, b := im.alloc(0)
		data = data[copy(b, data):]
		locs = append(locs, n)
	}
	if len(locs) <= nDirect {
		return locs, nil
	}
	n, b := im.alloc(tIndir)
	for i, l := range locs[nDirect:] {
		binary.LittleEndian.PutUint64(b[8*i:], l)
	}
	return locs[:nDirect], []uint64{n}
}

func (im *image) dir(ents ...ent) uint64 {
	n, b := im.alloc(tDentry)
	for i, e := range ents {
		putDentry(b[i*dentrySize:], e)
	}
	return n
}

func (im *image) ReadAt(p []byte, off int64) (int, error) {
	all := bytes.Join(im.blocks, nil)
	if off >= int64(len(all)) {
		return 0, io.EOF
	}
	n := copy(p, all[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestWalk(t *testing.T) {
	im := &image{}
	_, sb := im.alloc(tSuper)
	rootBlk, _ := im.alloc(tDentry)

	users := []byte("-1:adm:adm:\n10000:sys::\n42:jeff:jeff:\n")
	udb, _ := im.file(users)
	adm := im.dir(ent{name: "users", uid: -1, gid: -1, mode: 0o664, size: uint64(len(users)), db: udb})

	big := make([]byte, 20*rblock+7)
	for i := range big {
		big[i] = byte(i * 7)
	}
	bdb, bib := im.file(big)
	small := []byte("hello")
	sdb, _ := im.file(small)
	top := im.dir(
		ent{name: "big", uid: 42, gid: 10000, mode: 0o644, size: uint64(len(big)), db: bdb, ib: bib, mtime: 1000},
		ent{name: "adm", uid: -1, gid: -1, mode: 0o775, dir: true, size: 1, db: []uint64{adm}},
		ent{name: "hello", uid: 7, gid: 10000, mode: 0o444, size: uint64(len(small)), db: sdb},
	)
	putDentry(im.blocks[rootBlk][1:], ent{name: "/", mode: 0o775, dir: true, size: 1, db: []uint64{top}})
	le := binary.LittleEndian
	le.PutUint32(sb, superMagic)
	le.PutUint64(sb[28:], rootBlk)

	fs, err := Open(im)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	contents := map[string][]byte{}
	err = fs.Walk(func(f File) error {
		got = append(got, f.Path+" "+f.User+" "+f.Group)
		if !f.Dir {
			b, err := io.ReadAll(fs.Open(f))
			if err != nil {
				return err
			}
			contents[f.Path] = b
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"adm adm adm", "adm/users adm adm", "big jeff sys", "hello 7 sys"}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d: got %q, want %q", i, got[i], want[i])
		}
	}
	if !bytes.Equal(contents["big"], big) {
		t.Errorf("big: contents differ (%d bytes, want %d)", len(contents["big"]), len(big))
	}
	if string(contents["hello"]) != "hello" {
		t.Errorf("hello: %q", contents["hello"])
	}
}
