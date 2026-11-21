# osync

Copy on one machine, paste on another: text, files and folders. osync shares
the clipboard between your own Linux and macOS machines over
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

Copy text as usual, or copy files in the file manager, and paste on another
machine. To see what osync is doing:

```sh
osync status
```

## What is synced

- Plain text up to 4 MB.
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

Files go between machines on version 0.2 or newer. A machine still on 0.1
gets text only, and `osync status` on the others says that it needs updating.

Linux needs a Wayland compositor with a data-control protocol (Hyprland, Sway,
KDE) and `wl-clipboard` 2.2 or newer. Files need a file manager that puts them
on the clipboard as `text/uri-list`, as GNOME Files does.

On macOS, a file is noticed when the clipboard's text changes, which is the
file's name. Copying a file and then, with nothing in between, another one of
the same name is not noticed.

## Develop

```sh
make test     # unit tests and whole daemons syncing over loopback
make build    # ./osync for this machine
./install.sh  # build this checkout and install it
```

Pushing a tag such as `v0.2.0` makes the `build` workflow publish a release
with binaries for Linux and macOS on amd64 and arm64.
