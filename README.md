# osync

Copy on one machine, paste on another: text, screenshots, files and folders.
osync shares the clipboard between your own Linux and macOS machines over
[Tailscale](https://tailscale.com), so they do not need to be on the same
network.

## Install

On every machine that should share a clipboard:

```sh
curl -fsSL https://raw.githubusercontent.com/iluxav/osync/main/install.sh | bash
```

The script installs Tailscale if it is missing (on macOS, install the
Tailscale app and sign in first), downloads the binary for the machine's OS
and CPU to `~/.local/bin/osync` (linked from `/usr/local/bin` on macOS, so
`osync` works as a command), and starts it as a per-user service. Run it
again to upgrade.

All machines must be signed in to Tailscale with the same account.

## Use

Copy text, take a screenshot to the clipboard, or copy files in the file
manager, and paste on another machine. To see what osync is doing:

```sh
osync status
```

## What is synced

- Plain text up to 4 MB.
- Pictures that are on the clipboard as a picture and not as a file: a
  screenshot, or Copy Image in a browser. Up to 32 MB. They arrive as PNG.
- Files and folders copied in a file manager (GNOME Files, Finder), up to
  100 MB and 10,000 items per copy. A copy that is over the limit stays on its
  machine, and `osync status` says so.
- Only between devices owned by the same Tailscale user.
- Anything a password manager marks as concealed stays on the machine it was
  copied on.

Copied files are sent to every machine straight away, so that they are there
when you paste. Each machine keeps the last two copies it received in its
cache folder (`~/.cache/osync/files`, or `~/Library/Caches/osync/files` on
macOS); pasting copies them from there to where you want them. Links inside a
copied folder are left out.

A copy that comes in several forms is synced as one of them: files if it has
files, else text, else a picture. Cells copied in a spreadsheet arrive as
their text, not as the picture of them that the spreadsheet also offers.

Pictures need version 0.3 on both machines and files 0.2. An older machine
still gets text.

Linux needs a Wayland compositor with a data-control protocol (Hyprland, Sway,
KDE) and `wl-clipboard` 2.2 or newer. Files need a file manager that puts them
on the clipboard as `text/uri-list`, as GNOME Files does.

## Develop

```sh
make test     # unit tests and whole daemons syncing over loopback
make build    # ./osync for this machine
./install.sh  # build this checkout and install it
```

Pushing a tag such as `v0.2.0` makes the `build` workflow publish a release
with binaries for Linux and macOS on amd64 and arm64.
