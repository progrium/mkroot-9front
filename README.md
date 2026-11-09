# mkroot-9front

Builds [9front](https://9front.org) root trees as container images, for
[h9](https://github.com/progrium/hyper9) to run from
[h9reg](https://github.com/progrium/h9reg):

```sh
h9 --root ws://reg.example/progrium/9front rc
```

h9reg looks a name such as `progrium/9front` up as
`ghcr.io/progrium/mkroot-9front` first, so the image doesn't need its full
name.

## The image

`ghcr.io/progrium/mkroot-9front:RELEASE` (and `:latest`) is one image per
architecture, for platforms `plan9/386`, `plan9/amd64`, and `plan9/arm64`.
Each is one layer: the release's tree, as 9front installs it, with

- Plan 9 owners (`sys`, `adm`, `glenda`, …) as tar user and group names,
  with zero ids, and Plan 9 permission bits;
- the directories 9front's kernel supplies through `#/` (`bin`, `dev`, `env`,
  `mnt`, `n`, `net`, `proc`, `srv`, …), which h9 binds onto;
- without `dist`, which holds the installer's payloads.

The config's command is `/bin/rc`.

386 and amd64 come from the release ISOs. 9front ships arm64 as a disk
image instead, an installed system on an hjfs file system, so the arm64
tree is that disk's. It matches the ISOs' trees but for each
architecture's binaries, `cfg/plan9.ini`, `adm/users`, and the owners of
a few directories the installer gives to their users (`upas` owns
`mail`, for one).

## Building

```sh
go run ./cmd/mkroot                                   # build only
go run ./cmd/mkroot --push localhost:5000/progrium/mkroot-9front:11952
```

`pins` names the release and the sha256 of each release image as
9front.org serves it, gzipped. mkroot downloads them into `.cache`, checks
them, and unpacks each with readers of its own: `iso9660` reads an ISO's
names from the Joliet tree and its owners and modes from Plan 9's
extension, and for arm64, `qcow2` reads the disk image, `disk` finds the
`fs` partition in its Plan 9 partition, and `hjfs` reads the file system
there. A bump is a new `RELEASE` with its hashes emptied: mkroot then
prints each image's hash to pin.

The `publish` workflow builds every pull request, and pushes from `main`
to `ghcr.io/OWNER/mkroot-9front`, tagged with the release and `latest`.
