// Package iso9660 reads 9front's release ISOs: ISO 9660 with Joliet and
// Plan 9's system-use extension, as disk/mk9660 -9cj writes them.
//
// Names come from the Joliet tree, since the primary tree's are conformed
// to upper case. Owners and modes come from the primary tree's Plan 9
// extension. Both trees point at the same file data, so a file is matched
// across them by its extent. Directories have extents of their own in
// each tree, so a directory is matched by its depth and the lowest file
// extent below it. Plan 9 names longer than Joliet's 64 characters come
// from the extension, where it has them. Files with the same contents
// share one extent, so they share owners and modes too. This is hyper9's
// tools/isoroot.zig, with owners kept.
package iso9660

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	sector    = 2048
	flagDir   = 0x02
	flagMulti = 0x80
	// jolietMax is the longest Joliet name, less the room for ";1".
	jolietMax = 62
)

// File is one file or directory in the image.
type File struct {
	Path  string // slash-separated, without a leading slash
	Dir   bool
	Mode  uint32 // Plan 9 permission bits
	User  string
	Group string
	MTime time.Time
	Size  int64

	extent uint32
}

// Image is an opened ISO.
type Image struct {
	r       io.ReaderAt
	primary record
	joliet  record
	files   map[uint32]plan9 // primary tree files, by extent
	dirs    map[dirKey]plan9 // primary tree directories
}

type plan9 struct {
	name, uid, gid string
	mode           uint32
	ok             bool
}

type dirKey struct {
	depth  int
	extent uint32 // lowest file extent below the directory
}

// Open reads an image's volume descriptors and its primary tree.
func Open(r io.ReaderAt) (*Image, error) {
	im := &Image{r: r, files: map[uint32]plan9{}, dirs: map[dirKey]plan9{}}
	for s := int64(16); ; s++ {
		vd := make([]byte, sector)
		if _, err := r.ReadAt(vd, s*sector); err != nil {
			return nil, err
		}
		if string(vd[1:6]) != "CD001" {
			return nil, errors.New("not an ISO 9660 image")
		}
		switch vd[0] {
		case 1:
			im.primary = record(vd[156:190])
		case 2:
			// A supplementary descriptor is Joliet when its escape
			// sequence names UCS-2 (%/@, %/C or %/E).
			if vd[88] == '%' && vd[89] == '/' && bytes.IndexByte([]byte("@CE"), vd[90]) >= 0 {
				im.joliet = record(vd[156:190])
			}
		}
		if vd[0] == 255 {
			break
		}
	}
	if im.primary == nil {
		return nil, errors.New("no primary volume descriptor")
	}
	if im.joliet == nil {
		return nil, errors.New("no Joliet tree")
	}
	if _, err := im.walkPrimary(im.primary, 0); err != nil {
		return nil, err
	}
	return im, nil
}

// walkPrimary records the Plan 9 extension of everything under dir, and
// returns the lowest file extent below it (0 if none).
func (im *Image) walkPrimary(dir record, depth int) (uint32, error) {
	recs, err := im.records(dir)
	if err != nil {
		return 0, err
	}
	var low uint32
	lower := func(e uint32) {
		if e != 0 && (low == 0 || e < low) {
			low = e
		}
	}
	for _, rec := range recs {
		if rec.flags()&flagDir != 0 {
			e, err := im.walkPrimary(rec, depth+1)
			if err != nil {
				return 0, err
			}
			if e != 0 {
				im.dirs[dirKey{depth + 1, e}] = rec.plan9()
			}
			lower(e)
			continue
		}
		im.files[rec.extent()] = rec.plan9()
		lower(rec.extent())
	}
	return low, nil
}

// Walk calls fn for every file and directory, parents first, in name
// order.
func (im *Image) Walk(fn func(File) error) error {
	_, err := im.walk(im.joliet, "", 0, fn)
	return err
}

type node struct {
	rec      record
	name     string
	children []*node
	low      uint32
}

func (im *Image) walk(dir record, prefix string, depth int, fn func(File) error) (uint32, error) {
	// Directories need the lowest extent below them before they can be
	// matched, so read the whole tree first.
	root, err := im.load(dir, depth)
	if err != nil {
		return 0, err
	}
	return root.low, im.emit(root, prefix, depth, fn)
}

func (im *Image) load(dir record, depth int) (*node, error) {
	n := &node{rec: dir}
	recs, err := im.records(dir)
	if err != nil {
		return nil, err
	}
	lower := func(e uint32) {
		if e != 0 && (n.low == 0 || e < n.low) {
			n.low = e
		}
	}
	for _, rec := range recs {
		if rec.flags()&flagMulti != 0 {
			return nil, fmt.Errorf("%s: multi-extent files aren't supported", jolietName(rec.name()))
		}
		var c *node
		if rec.flags()&flagDir != 0 {
			if c, err = im.load(rec, depth+1); err != nil {
				return nil, err
			}
		} else {
			c = &node{rec: rec, low: rec.extent()}
		}
		c.rec = rec
		c.name = jolietName(rec.name())
		lower(c.low)
		n.children = append(n.children, c)
	}
	return n, nil
}

func (im *Image) emit(n *node, prefix string, depth int, fn func(File) error) error {
	for _, c := range n.children {
		var m plan9
		isDir := c.rec.flags()&flagDir != 0
		if isDir {
			m = im.dirs[dirKey{depth + 1, c.low}]
		} else {
			m = im.files[c.rec.extent()]
		}
		// The extension's name replaces a Joliet name cut short at
		// Joliet's limit. Only then: files with the same contents share
		// an extent, and with it one file's extension, so the name in it
		// may be another file's ("CW" shares "CWxyz"'s).
		if m.ok && utf8.RuneCountInString(c.name) >= jolietMax && len(m.name) > len(c.name) && strings.HasPrefix(m.name, c.name) {
			c.name = m.name
		}
	}
	sort.Slice(n.children, func(i, j int) bool { return n.children[i].name < n.children[j].name })
	for _, c := range n.children {
		if c.name == "" || c.name == "." || c.name == ".." || strings.Contains(c.name, "/") {
			return fmt.Errorf("bad name %q in %q", c.name, prefix)
		}
		isDir := c.rec.flags()&flagDir != 0
		f := File{Path: c.name, Dir: isDir, MTime: c.rec.time(), User: "sys", Group: "sys", Mode: 0o664, extent: c.rec.extent()}
		if prefix != "" {
			f.Path = prefix + "/" + c.name
		}
		m := im.files[c.rec.extent()]
		if isDir {
			m = im.dirs[dirKey{depth + 1, c.low}]
			f.Mode = 0o775
		} else {
			f.Size = int64(c.rec.size())
		}
		if m.ok {
			f.Mode = m.mode & 0o777
			if m.uid != "" {
				f.User = m.uid
			}
			if m.gid != "" {
				f.Group = m.gid
			}
		}
		if err := fn(f); err != nil {
			return err
		}
		if isDir {
			if err := im.emit(c, f.Path, depth+1, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

// Open returns a file's contents.
func (im *Image) Open(f File) io.Reader {
	return io.NewSectionReader(im.r, int64(f.extent)*sector, f.Size)
}

// records lists the records of the directory dir points at, without "."
// and "..".
func (im *Image) records(dir record) ([]record, error) {
	data := make([]byte, dir.size())
	if _, err := im.r.ReadAt(data, int64(dir.extent())*sector); err != nil {
		return nil, err
	}
	var out []record
	for i := 0; i < len(data); {
		l := int(data[i])
		// Records don't cross sectors; a zero length pads to the next.
		if l == 0 {
			i = (i/sector + 1) * sector
			continue
		}
		if l < 34 || i+l > len(data) {
			return nil, errors.New("bad directory record")
		}
		rec := record(data[i : i+l])
		if !rec.isDot() {
			out = append(out, rec)
		}
		i += l
	}
	return out, nil
}

// record is a directory record, as ISO 9660 lays it out.
type record []byte

func (r record) extent() uint32 { return binary.LittleEndian.Uint32(r[2:6]) }
func (r record) size() uint32   { return binary.LittleEndian.Uint32(r[10:14]) }
func (r record) flags() byte    { return r[25] }
func (r record) name() []byte   { return r[33 : 33+int(r[32])] }

// isDot reports "." and "..", which are the bytes 0 and 1.
func (r record) isDot() bool { return r[32] == 1 && r[33] <= 1 }

// time is the recording time: years since 1900, month, day, hour,
// minute, second, and the offset from GMT in 15-minute steps.
func (r record) time() time.Time {
	d := r[18:25]
	zone := time.FixedZone("", int(int8(d[6]))*15*60)
	return time.Date(1900+int(d[0]), time.Month(d[1]), int(d[2]), int(d[3]), int(d[4]), int(d[5]), 0, zone).UTC()
}

// plan9 reads Plan 9's extension: name, uid and gid, each a length byte
// and the bytes, then a 32-bit mode at the next even offset, as 9660srv
// reads it.
func (r record) plan9() plan9 {
	o := 33 + int(r[32])
	if o&1 != 0 {
		o++
	}
	var f [3]string
	for i := range f {
		if o >= len(r) {
			return plan9{}
		}
		l := int(r[o])
		if o+1+l > len(r) {
			return plan9{}
		}
		f[i] = string(r[o+1 : o+1+l])
		o += 1 + l
	}
	if o&1 != 0 {
		o++
	}
	if o+4 > len(r) {
		return plan9{}
	}
	return plan9{name: f[0], uid: f[1], gid: f[2], mode: binary.LittleEndian.Uint32(r[o : o+4]), ok: true}
}

// jolietName decodes a Joliet name, UCS-2 big-endian, without its ";1"
// version.
func jolietName(raw []byte) string {
	u := make([]uint16, len(raw)/2)
	for i := range u {
		u[i] = binary.BigEndian.Uint16(raw[2*i:])
	}
	s := string(utf16.Decode(u))
	if k := strings.LastIndexByte(s, ';'); k >= 0 {
		s = s[:k]
	}
	return s
}
