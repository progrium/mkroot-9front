// Command mkroot builds 9front root images from 9front's release images
// and pushes them as one multi-platform image:
//
//	go run ./cmd/mkroot --push ghcr.io/progrium/mkroot-9front:11952
//
// For each architecture it downloads the release image (and checks it
// against pins), unpacks its tree into a tar layer with Plan 9 owners and
// modes, and makes an image of that one layer for platform plan9/ARCH.
// 386 and amd64 come from the release ISOs. 9front ships no arm64 ISO,
// so arm64 comes from the release's arm64 disk image: a qcow2 disk whose
// Plan 9 partition holds an hjfs file system.
package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/progrium/mkroot-9front/disk"
	"github.com/progrium/mkroot-9front/hjfs"
	"github.com/progrium/mkroot-9front/iso9660"
	"github.com/progrium/mkroot-9front/qcow2"
)

const releaseURL = "https://9front.org/iso"

// formats says which release image each architecture is built from.
var formats = map[string]string{"386": "iso", "amd64": "iso", "arm64": "qcow2"}

// mountPoints are the top-level directories 9front's kernel supplies
// through #/ rather than the disk, so the ISO lacks them. h9 binds onto
// them, so the image must have them. The list is hyper9's tools/mkroot.zig.
var mountPoints = []string{
	"bin", "boot", "dev", "env", "fd", "mnt", "mnt/temp", "mnt/term", "mnt/term/cwd", "mnt/wsys", "n",
	"net", "net.alt", "proc", "root", "shr", "srv", "tmp",
}

// exclude are top-level directories left out: dist holds the installer's
// payloads, a git repository of the release's source among them.
// The arm64 disk has dist too.
var exclude = []string{"dist"}

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(s string) error { *l = append(*l, s); return nil }

func main() {
	var push listFlag
	pinsFile := flag.String("pins", "pins", "file naming the release and its images' hashes")
	archs := flag.String("arch", "386,amd64,arm64", "architectures to build, comma-separated")
	cache := flag.String("cache", ".cache", "directory for downloaded images and built layers")
	flag.Var(&push, "push", "reference to push the image to; repeatable")
	flag.Parse()
	log.SetFlags(0)
	log.SetPrefix("mkroot: ")

	pins, err := readPins(*pinsFile)
	if err != nil {
		log.Fatal(err)
	}
	release := pins["RELEASE"]
	if release == "" {
		log.Fatalf("%s: no RELEASE", *pinsFile)
	}
	if err := os.MkdirAll(*cache, 0o755); err != nil {
		log.Fatal(err)
	}
	idx := v1.ImageIndex(empty.Index)
	for _, arch := range strings.Split(*archs, ",") {
		img, err := build(*cache, release, arch, pins["SHA256_"+arch])
		if err != nil {
			log.Fatalf("%s: %v", arch, err)
		}
		idx = mutate.AppendManifests(idx, mutate.IndexAddendum{
			Add:        img,
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "plan9", Architecture: arch}},
		})
	}
	d, err := idx.Digest()
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("9front %s: %s", release, d)
	for _, ref := range push {
		tag, err := name.ParseReference(ref)
		if err != nil {
			log.Fatal(err)
		}
		if err := remote.WriteIndex(tag, idx, remote.WithAuthFromKeychain(authn.DefaultKeychain)); err != nil {
			log.Fatalf("push %s: %v", ref, err)
		}
		log.Printf("pushed %s", ref)
	}
}

func readPins(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	pins := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s: bad line %q", path, line)
		}
		v, _, _ = strings.Cut(v, "#")
		pins[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return pins, sc.Err()
}

// build makes the image for one architecture.
func build(cache, release, arch, sum string) (v1.Image, error) {
	format, ok := formats[arch]
	if !ok {
		return nil, fmt.Errorf("9front ships no release image for %s", arch)
	}
	base := fmt.Sprintf("9front-%s.%s", release, arch)
	gz := filepath.Join(cache, base+"."+format+".gz")
	if err := fetch(releaseURL+"/"+filepath.Base(gz), gz); err != nil {
		return nil, err
	}
	if err := check(gz, sum); err != nil {
		return nil, err
	}
	raw := filepath.Join(cache, base+"."+format)
	if err := gunzip(gz, raw); err != nil {
		return nil, err
	}
	layerPath := filepath.Join(cache, base+".tar")
	f, err := os.Open(raw)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var walk walkFunc
	switch format {
	case "iso":
		walk, err = isoTree(f)
	case "qcow2":
		walk, err = diskTree(f)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", raw, err)
	}
	newest, n, err := unpack(walk, layerPath)
	if err != nil {
		return nil, err
	}
	log.Printf("%s: %d files", base, n)
	layer, err := tarball.LayerFromFile(layerPath)
	if err != nil {
		return nil, err
	}
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		return nil, err
	}
	return mutate.ConfigFile(img, &v1.ConfigFile{
		OS:           "plan9",
		Architecture: arch,
		Created:      v1.Time{Time: newest},
		Config: v1.Config{
			Cmd: []string{"/bin/rc"},
			Labels: map[string]string{
				"org.opencontainers.image.source":  "https://github.com/progrium/mkroot-9front",
				"org.opencontainers.image.version": release,
				"org.opencontainers.image.title":   "9front " + release + " root",
			},
		},
	})
}

// fetch downloads url to path, unless path exists.
func fetch(url, path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	log.Printf("fetching %s", url)
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	tmp := path + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// check compares a file's sha256 with its pin. With no pin it prints the
// hash to pin and fails.
func check(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	switch {
	case want == "":
		return fmt.Errorf("%s has no pin; its sha256 is %s", filepath.Base(path), got)
	case got != want:
		return fmt.Errorf("%s: sha256 %s, pinned %s", filepath.Base(path), got, want)
	}
	return nil
}

func gunzip(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	zr, err := gzip.NewReader(in)
	if err != nil {
		return err
	}
	tmp := dst + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, zr); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// entry is one file or directory of a release image's tree.
type entry struct {
	Path        string
	Dir         bool
	Mode        uint32
	User, Group string
	MTime       time.Time
	Size        int64
	open        func() io.Reader
}

// walkFunc calls fn for each entry of a tree, parents before children.
type walkFunc func(fn func(entry) error) error

// isoTree reads a release ISO.
func isoTree(r io.ReaderAt) (walkFunc, error) {
	im, err := iso9660.Open(r)
	if err != nil {
		return nil, err
	}
	return func(fn func(entry) error) error {
		return im.Walk(func(f iso9660.File) error {
			return fn(entry{f.Path, f.Dir, f.Mode, f.User, f.Group, f.MTime, f.Size, func() io.Reader { return im.Open(f) }})
		})
	}, nil
}

// diskTree reads the fs partition of a release disk image.
func diskTree(r io.ReaderAt) (walkFunc, error) {
	im, err := qcow2.Open(r)
	if err != nil {
		return nil, err
	}
	part, err := disk.Part(im, "fs")
	if err != nil {
		return nil, err
	}
	fs, err := hjfs.Open(part)
	if err != nil {
		return nil, err
	}
	return func(fn func(entry) error) error {
		return fs.Walk(func(f hjfs.File) error {
			return fn(entry{f.Path, f.Dir, f.Mode, f.User, f.Group, f.MTime, f.Size, func() io.Reader { return fs.Open(f) }})
		})
	}, nil
}

// unpack writes a tree as a tar. Owners are Plan 9 names with zero ids,
// as h9reg expects of a Plan 9 layer. It returns the newest modification
// time and the number of entries.
func unpack(walk walkFunc, tarPath string) (time.Time, int, error) {
	var newest time.Time
	out, err := os.Create(tarPath)
	if err != nil {
		return newest, 0, err
	}
	defer out.Close()
	bw := bufio.NewWriterSize(out, 1<<20)
	tw := tar.NewWriter(bw)
	n := 0
	seen := map[string]bool{}
	err = walk(func(file entry) error {
		top, _, _ := strings.Cut(file.Path, "/")
		if slices.Contains(exclude, top) {
			return nil
		}
		seen[file.Path] = true
		hdr := &tar.Header{
			Name:    file.Path,
			Mode:    int64(file.Mode),
			Uname:   file.User,
			Gname:   file.Group,
			ModTime: file.MTime,
			Format:  tar.FormatPAX,
		}
		if file.Dir {
			hdr.Typeflag = tar.TypeDir
			hdr.Name += "/"
		} else {
			hdr.Typeflag = tar.TypeReg
			hdr.Size = file.Size
		}
		if file.MTime.After(newest) {
			newest = file.MTime
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !file.Dir {
			if _, err := io.Copy(tw, file.open()); err != nil {
				return fmt.Errorf("%s: %w", file.Path, err)
			}
		}
		n++
		return nil
	})
	if err != nil {
		return newest, 0, err
	}
	for _, mp := range mountPoints {
		if seen[mp] {
			continue
		}
		hdr := &tar.Header{Name: mp + "/", Typeflag: tar.TypeDir, Mode: 0o775, Uname: "sys", Gname: "sys", ModTime: newest, Format: tar.FormatPAX}
		if err := tw.WriteHeader(hdr); err != nil {
			return newest, 0, err
		}
		n++
	}
	if err := tw.Close(); err != nil {
		return newest, 0, err
	}
	if err := bw.Flush(); err != nil {
		return newest, 0, err
	}
	if n == 0 {
		return newest, 0, errors.New("empty tree")
	}
	return newest, n, out.Close()
}
