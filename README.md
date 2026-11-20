# osync

Copy on one machine, paste on another. osync shares the clipboard between your
own Linux and macOS machines over [Tailscale](https://tailscale.com), so they
do not need to be on the same network.

## Install

On every machine that should share a clipboard:

```sh
curl -fsSL https://raw.githubusercontent.com/iluxav/osync/main/install.sh | bash
```

The script installs Tailscale if it is missing (on macOS, install the
Tailscale app and sign in first), downloads the binary for the machine's OS
and CPU to `~/.local/bin/osync`, and starts it as a per-user service. Run it
again to upgrade.

All machines must be signed in to Tailscale with the same account.

## Use

Copy text as usual. To see what osync is doing:

```sh
osync status
```

## What is synced

- Plain text up to 4 MB.
- Only between devices owned by the same Tailscale user.
- Anything a password manager marks as concealed stays on the machine it was
  copied on.

Linux needs a Wayland compositor with a data-control protocol (Hyprland, Sway,
KDE) and `wl-clipboard` 2.2 or newer.

## Develop

```sh
make test     # unit tests and whole daemons syncing over loopback
make build    # ./osync for this machine
./install.sh  # build this checkout and install it
```

Pushing a tag such as `v0.2.0` makes the `build` workflow publish a release
with binaries for Linux and macOS on amd64 and arm64.
