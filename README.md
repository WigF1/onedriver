[![Tests](https://github.com/WigF1/onedriver/actions/workflows/ci.yml/badge.svg)](https://github.com/WigF1/onedriver/actions/workflows/ci.yml)

# onedriver

**onedriver is a native Linux filesystem for Microsoft OneDrive.**

onedriver mounts your OneDrive as a folder. It is not a sync client: files are
downloaded on demand, when a program actually reads them, so you get instant
access to everything in your OneDrive without copying it all to your computer.
Changes you make are uploaded, and changes made elsewhere show up locally.

This is a maintained fork of [jstaf/onedriver](https://github.com/jstaf/onedriver),
focused on Fedora with GNOME. It fixes a number of long-standing upstream issues
(see [what's different](#whats-different-from-upstream)) and includes fixes
ported from [leifrossau/onedriver](https://github.com/leifrossau/onedriver).

## Features

- **Files are only downloaded when you use them.** Browsing folders doesn't
  download files: file managers that peek at a file's first few KB to work out
  its type get just that, through a small range request.
- **Changes go both ways.** Local changes are uploaded; changes made on other
  devices or in OneDrive on the web show up locally. If a file changes in both
  places before your change is uploaded, your local version wins.
- **Works offline.** Files you've opened before stay available without a
  network connection. The mount becomes read-only while offline and switches
  back automatically when the connection returns; during an outage, file
  operations answer from the cache instead of hanging.
- **Fast.** Folder listings and drive quota are cached and refreshed in the
  background, so file managers don't wait on the network.
- **Has a user interface** (`onedriver-launcher`) for adding and removing
  OneDrive accounts, and runs as a systemd user service.

## What's different from upstream

| Area | Upstream issue | Change |
|---|---|---|
| Nautilus freezes | #327 | Free-space queries (`statfs`) are answered from a cache instead of a 2–4 s Graph request each |
| Browsing downloads files | #24, #405, #275 | Read-only opens no longer download; file-type sniffing uses range requests |
| Slow first visit to folders | #335 | Folder listings are prefetched in the background |
| Directory listings | #494, #466, #215 | Stable, sorted listings; no duplicate, missing or partial entries |
| Sign-in | #493, #478, #490, #457, #378 | Reliable token renewal; an expired login keeps the mount up (read-only) instead of crashing it; login window fixes |
| Atomic saves (KeePassXC, LibreOffice) | #492, #439, #217 | One upload per save, no leftover temp or lock files |
| Remote edits | #343, #473 | Edits and deletions made elsewhere are applied reliably |
| Network outages | #472 | Operations fail fast from the cache instead of hanging applications |
| Other | #470, #425, #201, #356, #390 | No idle CPU spikes, empty files upload, mtime updates on write, cached files open offline, Turkish İ in names |

Graph API throttling (HTTP 429) is honoured, and the fork has an end-to-end test
suite that mounts onedriver against a fake Graph API, so fixes are tested
without a Microsoft account.

## Install on Fedora

Download the RPM from the latest
[release](https://github.com/WigF1/onedriver/releases/latest) and install it:

```bash
sudo dnf install ./onedriver-*.rpm
```

or straight from the release URL:

```bash
sudo dnf install https://github.com/WigF1/onedriver/releases/download/vX.Y.Z/onedriver-X.Y.Z-1.fcNN.x86_64.rpm
```

If you previously installed upstream onedriver from COPR, remove it first
(`sudo dnf remove onedriver` and `sudo dnf copr disable jstaf/onedriver`), so a
`dnf upgrade` doesn't replace this version. Your login and cache in
`~/.cache/onedriver` and settings in `~/.config/onedriver` are kept.

To build and install from source instead, see
[Building from source](#building-from-source).

## Getting started

Open **onedriver** from your applications and click **+** to add a OneDrive
account and choose where to mount it. Or from a terminal:

```bash
mkdir -p ~/OneDrive
onedriver ~/OneDrive      # opens the Microsoft login window the first time
```

`onedriver --help` and `man onedriver` list all options.

### Start automatically on login

The launcher can do this for you. To set it up from the command line:

```bash
mkdir -p ~/OneDrive
SERVICE_NAME=$(systemd-escape --template onedriver@.service --path ~/OneDrive)
systemctl --user daemon-reload
systemctl --user enable --now "$SERVICE_NAME"

# logs
journalctl --user -u "$SERVICE_NAME" --since today
```

### Configuration

Settings are read from `~/.config/onedriver/config.yml` (see
[config-example.yml](pkg/resources/config-example.yml)), for example the log
level and the cache location. The default log level is `debug`; `log: info`
keeps the journal much quieter.

## Login expired

If renewing the login fails because of the network or a server problem,
onedriver keeps the old token and retries in the background with a growing
delay. If Microsoft definitively rejects the stored login (`invalid_grant`, for
example after a password change), onedriver never opens a login window by
itself and never exits: the mount stays up and serves files read-only from its
cache. To log in again, run:

```bash
onedriver --auth-only ~/OneDrive      # add -n to log in with your normal web browser
```

The running mount picks up the new login within about 10 seconds. Started from a
terminal, onedriver asks for the login at startup as before.

If the embedded login window stays blank or doesn't show the Microsoft
Authenticator number: run it with `ONEDRIVER_DEBUG_AUTH=1` to see the page's
console output, make sure `gstreamer1-plugins-good` is installed, or use `-n` to
log in with your normal browser. `ONEDRIVER_USER_AGENT` overrides the browser
user agent sent to Microsoft's login page, and
`ONEDRIVER_NO_WEBKIT_WORKAROUNDS=1` disables the `WEBKIT_DISABLE_DMABUF_RENDERER`
workaround for blank windows on Wayland/NVIDIA.

## Troubleshooting

- **Logs:** `journalctl --user -u onedriver@home-$USER-OneDrive.service --since today`
  (the unit name is the escaped mount path, see above). `log: trace` in the
  config file logs every filesystem operation.
- **"Read-only file system":** onedriver is offline (or the login expired);
  writes work again once it's back online.
- **Thumbnails:** file managers download image and video files to make
  thumbnails. To avoid that, turn thumbnails off for the mount's file manager
  (in Nautilus: `gsettings set org.gnome.nautilus.preferences show-image-thumbnails 'never'`).
- **Mount hangs or "Transport endpoint is not connected":** onedriver stopped
  without unmounting. Close file managers using the folder, then
  `fusermount3 -uz ~/OneDrive` and start the service again.
- **Start from scratch:** `onedriver -w` deletes all cached data (you'll need
  to log in again).

## Known limitations

- OneDrive has no symbolic links; creating one returns `ENOSYS`.
- The OneDrive Recycle Bin isn't exposed by the API: empty or restore it in
  OneDrive on the web. Deleting a file in the mount moves it to the OneDrive
  Recycle Bin.
- Files being uploaded are held in memory, so very large files (several GB)
  are better copied with a tool like [rclone](https://rclone.org/).
- onedriver is not a backup tool. Use [restic](https://restic.net/) or
  [borg](https://www.borgbackup.org/) for backups.

## Building from source

Install the build dependencies, then build:

```bash
sudo dnf install golang gcc pkgconf-pkg-config webkit2gtk4.1-devel json-glib-devel git
make                 # builds ./onedriver and ./onedriver-launcher
sudo make install    # installs to /usr/bin and /etc/systemd/user
```

`sudo make uninstall` removes it again. To build an RPM locally, see
[RELEASING.md](RELEASING.md).

### Tests

```bash
# no OneDrive account needed: unit tests + the end-to-end suite, which mounts
# onedriver through FUSE against a fake Graph API (needs /dev/fuse)
make test-e2e

# full upstream suite against a real account; writes to /onedriver_tests on
# your OneDrive, and the offline tests need sudo
make test-init
make test
```

## Credits and license

onedriver was created by [Jeff Stafford](https://github.com/jstaf); several
fixes in this fork come from [Leif Rossau's fork](https://github.com/leifrossau/onedriver).
Licensed under the [GNU GPL v3](LICENSE) or later.
