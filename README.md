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
architecture, for platforms `plan9/386` and `plan9/amd64`. Each is one layer:
the release ISO's tree, as 9front installs it, with

- Plan 9 owners (`sys`, `adm`, `glenda`, …) as tar user and group names,
  with zero ids, and Plan 9 permission bits;
- the directories 9front's kernel supplies through `#/` (`bin`, `dev`, `env`,
  `mnt`, `n`, `net`, `proc`, `srv`, …), which h9 binds onto;
- without `dist`, which holds the installer's payloads.

The config's command is `/bin/rc`. arm64 isn't built: its release is a disk
image, not an ISO.

## Building

```sh
go run ./cmd/mkroot                                   # build only
go run ./cmd/mkroot --push localhost:5000/progrium/mkroot-9front:11952
```

`pins` names the release and the sha256 of each ISO as 9front.org serves it.
mkroot downloads the ISOs into `.cache`, checks them, and unpacks each with
its own ISO 9660 reader (`iso9660`), which reads names from the Joliet tree
and owners and modes from Plan 9's extension. A bump is a new `RELEASE`
with its hashes emptied: mkroot then prints each ISO's hash to pin.

The `publish` workflow builds every pull request, and pushes from `main`
to `ghcr.io/OWNER/mkroot-9front`, tagged with the release and `latest`.
