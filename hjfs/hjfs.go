// Package hjfs reads 9front's hjfs file system, read-only: enough to
// unpack the root of 9front's arm64 release disk, which hjfs formats.
//
// The layout is 9front's sys/src/cmd/hjfs (dat.h and conv.c): 4096-byte
// blocks, each a type byte and 4095 bytes of data, little-endian. Block 0
// is the superblock, which names the block holding the root's directory
// entry. A directory's entry lists blocks of entries, nine to a block;
// a file's lists blocks of data. Both list them through 15 direct and 4
// indirect pointers.
package hjfs

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	block      = 4096
	rblock     = block - 1
	superMagic = 0x6E0DE51C
	nameLen    = 256
	nDirect    = 15
	nIndirect  = 4
	dentrySize = nameLen + 4*2 + 13 + (3+nDirect+nIndirect)*8
	dePerBlk   = rblock / dentrySize
	offPerBlk  = rblock / 12

	tSuper  = 1
	tDentry = 2
	tIndir  = 3

	dAlloc = 1 << 15
	qtDir  = 0x80
)

// File is one file or directory.
type File struct {
	Path  string
	Dir   bool
	Mode  uint32
	User  string
	Group string
	MTime time.Time
	Size  int64

	d *dentry
}

// FS is an opened hjfs file system.
type FS struct {
	r     io.ReaderAt
	root  uint64
	users map[int16]string
}

type dentry struct {
	name     string
	uid, gid int16
	mode     uint16
	typ      uint8
	size     uint64
	db       [nDirect]uint64
	ib       [nIndirect]uint64
	mtime    int64
}

// Open reads the superblock and the user table in /adm/users.
func Open(r io.ReaderAt) (*FS, error) {
	fs := &FS{r: r}
	typ, data, err := fs.block(0)
	if err != nil {
		return nil, err
	}
	le := binary.LittleEndian
	if typ != tSuper || le.Uint32(data) != superMagic {
		return nil, errors.New("hjfs: no superblock")
	}
	fs.root = le.Uint64(data[4+8+8+8:])
	fs.users = defaultUsers()
	if err := fs.loadUsers(); err != nil {
		return nil, err
	}
	return fs, nil
}

// The table hjfs uses when /adm/users is missing (auth.c's udef).
func defaultUsers() map[int16]string {
	return map[int16]string{-1: "adm", 0: "none", 1: "tor", 2: "glenda", 10000: "sys", 10001: "map", 10002: "doc", 10003: "upas", 10004: "font"}
}

func (fs *FS) block(n uint64) (byte, []byte, error) {
	b := make([]byte, block)
	if _, err := fs.r.ReadAt(b, int64(n)*block); err != nil {
		return 0, nil, fmt.Errorf("hjfs: block %d: %w", n, err)
	}
	return b[0], b[1:], nil
}

func (fs *FS) rootEntry() (*dentry, error) {
	typ, data, err := fs.block(fs.root)
	if err != nil {
		return nil, err
	}
	if typ != tDentry {
		return nil, errors.New("hjfs: root block isn't a directory block")
	}
	d := parseDentry(data)
	if d.mode&dAlloc == 0 {
		return nil, errors.New("hjfs: no root entry")
	}
	return d, nil
}

func parseDentry(b []byte) *dentry {
	le := binary.LittleEndian
	d := &dentry{}
	name := b[:nameLen]
	if i := bytes.IndexByte(name, 0); i >= 0 {
		name = name[:i]
	}
	d.name = string(name)
	p := nameLen
	d.uid = int16(le.Uint16(b[p:]))
	d.gid = int16(le.Uint16(b[p+4:]))
	d.mode = le.Uint16(b[p+6:])
	p += 8 + 8 + 4 // muid, mode, path, vers
	d.typ = b[p]
	p++
	d.size = le.Uint64(b[p:])
	p += 8
	for i := range d.db {
		d.db[i] = le.Uint64(b[p:])
		p += 8
	}
	for i := range d.ib {
		d.ib[i] = le.Uint64(b[p:])
		p += 8
	}
	p += 8 // atime
	d.mtime = int64(le.Uint64(b[p:]))
	return d
}

// blockOf returns the disk block holding a file's n'th block, or 0 for a
// hole, as fs1.c's getblk finds it.
func (fs *FS) blockOf(d *dentry, n uint64) (uint64, error) {
	if n < nDirect {
		return d.db[n], nil
	}
	n -= nDirect
	l := uint64(1)
	i := 0
	for ; i < nIndirect; i++ {
		l *= offPerBlk
		if n < l {
			break
		}
		n -= l
	}
	if i == nIndirect {
		return 0, errors.New("hjfs: block offset too large")
	}
	loc := d.ib[i]
	for j := 0; j <= i; j++ {
		if loc == 0 {
			return 0, nil
		}
		typ, data, err := fs.block(loc)
		if err != nil {
			return 0, err
		}
		if typ != tIndir {
			return 0, fmt.Errorf("hjfs: block %d isn't an indirect block", loc)
		}
		l /= offPerBlk
		loc = binary.LittleEndian.Uint64(data[8*(n/l):])
		n %= l
	}
	return loc, nil
}

func (fs *FS) children(d *dentry) ([]*dentry, error) {
	var out []*dentry
	for i := uint64(0); i < d.size; i++ {
		b, err := fs.blockOf(d, i)
		if err != nil {
			return nil, err
		}
		if b == 0 {
			continue
		}
		typ, data, err := fs.block(b)
		if err != nil {
			return nil, err
		}
		if typ != tDentry {
			return nil, fmt.Errorf("hjfs: block %d of %q isn't a directory block", b, d.name)
		}
		for j := 0; j < dePerBlk; j++ {
			c := parseDentry(data[j*dentrySize:])
			if c.mode&dAlloc != 0 {
				out = append(out, c)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// Walk calls fn for every file and directory, parents first, in name
// order.
func (fs *FS) Walk(fn func(File) error) error {
	root, err := fs.rootEntry()
	if err != nil {
		return err
	}
	return fs.walk(root, "", fn)
}

func (fs *FS) walk(dir *dentry, prefix string, fn func(File) error) error {
	kids, err := fs.children(dir)
	if err != nil {
		return err
	}
	for _, c := range kids {
		if c.name == "" || c.name == "." || c.name == ".." || strings.Contains(c.name, "/") {
			return fmt.Errorf("hjfs: bad name %q in %q", c.name, prefix)
		}
		f := fs.file(c, prefix)
		if err := fn(f); err != nil {
			return err
		}
		if f.Dir {
			if err := fs.walk(c, f.Path, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

func (fs *FS) file(d *dentry, prefix string) File {
	f := File{
		Path:  d.name,
		Dir:   d.typ&qtDir != 0,
		Mode:  uint32(d.mode & 0o777),
		User:  fs.userName(d.uid),
		Group: fs.userName(d.gid),
		MTime: time.Unix(d.mtime, 0).UTC(),
		d:     d,
	}
	if prefix != "" {
		f.Path = prefix + "/" + d.name
	}
	if !f.Dir {
		f.Size = int64(d.size)
	}
	return f
}

func (fs *FS) userName(id int16) string {
	if n, ok := fs.users[id]; ok {
		return n
	}
	return strconv.Itoa(int(id))
}

// Open returns a file's contents.
func (fs *FS) Open(f File) io.Reader {
	return &reader{fs: fs, d: f.d, size: f.Size}
}

type reader struct {
	fs   *FS
	d    *dentry
	size int64
	off  int64
	buf  []byte
}

func (r *reader) Read(p []byte) (int, error) {
	if len(r.buf) == 0 {
		if r.off >= r.size {
			return 0, io.EOF
		}
		n := uint64(r.off / rblock)
		b, err := r.fs.blockOf(r.d, n)
		if err != nil {
			return 0, err
		}
		data := make([]byte, rblock)
		if b != 0 {
			_, d, err := r.fs.block(b)
			if err != nil {
				return 0, err
			}
			data = d
		}
		want := min(int64(rblock), r.size-r.off)
		r.buf = data[:want]
		r.off += want
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

// loadUsers reads /adm/users: lines of id:name:leader:members.
func (fs *FS) loadUsers() error {
	root, err := fs.rootEntry()
	if err != nil {
		return err
	}
	d := root
	for _, name := range []string{"adm", "users"} {
		kids, err := fs.children(d)
		if err != nil {
			return err
		}
		var next *dentry
		for _, c := range kids {
			if c.name == name {
				next = c
			}
		}
		if next == nil {
			return nil // hjfs then uses its defaults too
		}
		d = next
	}
	sc := bufio.NewScanner(fs.Open(File{d: d, Size: int64(d.size)}))
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		f := strings.Split(line, ":")
		if len(f) < 4 {
			continue
		}
		id, err := strconv.Atoi(f[0])
		if err != nil || f[1] == "" {
			continue
		}
		fs.users[int16(id)] = f[1]
	}
	return sc.Err()
}
