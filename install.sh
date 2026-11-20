#!/usr/bin/env bash
# Sets up osync on this machine: Tailscale, the binary, the firewall rule, and
# a per-user service that starts with the desktop session.
#
# On any machine:
#
#   curl -fsSL https://raw.githubusercontent.com/iluxav/osync/main/install.sh | bash
#
# That downloads the latest release for this machine's OS and CPU. Set
# OSYNC_VERSION=v0.1.0 to pick a release. Run from a checkout that has Go
# installed (./install.sh), it builds the checked-out source instead.
# Safe to run again: it upgrades the binary and restarts the service.
set -euo pipefail

REPO=iluxav/osync
# Must match daemon.DefaultPort, and be the same on every machine.
PORT=24817
BIN_DIR="$HOME/.local/bin"
BIN="$BIN_DIR/osync"
# The directory this script sits in. Empty when it is piped into bash.
HERE=""
if [[ -n ${BASH_SOURCE[0]:-} && -f ${BASH_SOURCE[0]} ]]; then
  HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
fi

say() { printf '\n==> %s\n' "$*"; }
die() {
  printf 'install.sh: %s\n' "$*" >&2
  exit 1
}
have() { command -v "$1" >/dev/null 2>&1; }

detect_platform() {
  case "$(uname -s)" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) die "unsupported operating system: $(uname -s)" ;;
  esac
  case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) die "unsupported CPU architecture: $(uname -m)" ;;
  esac
}

# Prints the path of the tailscale client. On macOS the app keeps it inside
# its bundle and puts nothing on PATH.
tailscale_cli() {
  if have tailscale; then
    echo tailscale
  elif [[ -x /Applications/Tailscale.app/Contents/MacOS/Tailscale ]]; then
    echo /Applications/Tailscale.app/Contents/MacOS/Tailscale
  else
    return 1
  fi
}

install_tailscale() {
  if tailscale_cli >/dev/null; then
    return
  fi
  if [[ $os == darwin ]]; then
    die "Install Tailscale from https://tailscale.com/download/mac, sign in, then run this script again."
  fi
  say "Installing Tailscale"
  if have omarchy-install-service-tailscale; then
    # Omarchy's own installer: package, service, login, and the bar widget.
    omarchy-install-service-tailscale
  elif have pacman; then
    sudo pacman -S --needed --noconfirm tailscale
  else
    curl -fsSL https://tailscale.com/install.sh | sh
  fi
}

connect_tailscale() {
  local ts
  ts="$(tailscale_cli)" || die "Tailscale is still not installed."
  if "$ts" status >/dev/null 2>&1; then
    return
  fi
  if [[ $os == darwin ]]; then
    die "Tailscale is installed but not connected. Open the Tailscale app, sign in, then run this script again."
  fi
  say "Connecting Tailscale (follow the login link if one is printed)"
  sudo systemctl enable --now tailscaled.service
  sudo tailscale up
}

install_clipboard_tools() {
  if [[ $os != linux ]] || have wl-paste; then
    return
  fi
  say "Installing wl-clipboard"
  if have pacman; then
    sudo pacman -S --needed --noconfirm wl-clipboard
  else
    die "Install wl-clipboard with your package manager, then run this script again."
  fi
}

sha256() {
  if have sha256sum; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

# Downloads the release binary for this machine to $1 and checks it against
# the checksums published with the release.
download_binary() {
  local dest="$1" asset="osync-$os-$arch" base want got
  if [[ -n ${OSYNC_VERSION:-} ]]; then
    base="https://github.com/$REPO/releases/download/$OSYNC_VERSION"
  else
    base="https://github.com/$REPO/releases/latest/download"
  fi
  have curl || die "curl is needed to download osync."
  curl -fsSL -o "$dest" "$base/$asset" || die "Could not download $base/$asset"
  want="$(curl -fsSL "$base/checksums.txt" | awk -v f="$asset" '$2 == f {print $1}')" ||
    die "Could not download $base/checksums.txt"
  got="$(sha256 "$dest")"
  if [[ -z $want || $want != "$got" ]]; then
    rm -f "$dest"
    die "Checksum mismatch for $asset: the download is damaged. Run the script again."
  fi
  chmod +x "$dest"
}

install_binary() {
  say "Installing $BIN"
  mkdir -p "$BIN_DIR"
  local tmp="$BIN.new" version
  if [[ -n $HERE && -f $HERE/go.mod ]] && have go; then
    version="$(git -C "$HERE" describe --tags --always --dirty 2>/dev/null || echo dev)"
    (cd "$HERE" && go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$tmp" .)
  elif [[ -n $HERE && -f $HERE/dist/osync-$os-$arch ]]; then
    cp "$HERE/dist/osync-$os-$arch" "$tmp"
    chmod +x "$tmp"
  else
    download_binary "$tmp"
  fi
  # Renaming over the old binary works even while the daemon is running it.
  mv -f "$tmp" "$BIN"
  echo "installed osync $("$BIN" version)"
}

# Omarchy ships ufw denying all incoming connections, including those that
# arrive over Tailscale. Peers reach the daemon on tailscale0 only.
open_firewall() {
  if [[ $os != linux ]] || ! have ufw; then
    return
  fi
  if sudo ufw status | grep -q '^Status: active'; then
    say "Allowing osync through the firewall on the Tailscale interface"
    sudo ufw allow in on tailscale0 to any port "$PORT" proto tcp comment osync
  fi
}

# The clipboard belongs to the graphical session, so the daemon is a user
# service tied to that session rather than a system service.
install_service_linux() {
  say "Installing the systemd user service"
  local unit_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
  mkdir -p "$unit_dir"
  cat >"$unit_dir/osync.service" <<'EOF'
[Unit]
Description=osync clipboard sync
PartOf=graphical-session.target
After=graphical-session.target

[Service]
ExecStart=%h/.local/bin/osync daemon
Restart=on-failure
RestartSec=5

[Install]
WantedBy=graphical-session.target
EOF
  systemctl --user daemon-reload
  systemctl --user enable osync.service
  systemctl --user restart osync.service
}

install_service_macos() {
  say "Installing the launchd agent"
  local label=local.osync
  local plist="$HOME/Library/LaunchAgents/$label.plist"
  mkdir -p "$HOME/Library/LaunchAgents" "$HOME/Library/Logs"
  cat >"$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>$label</string>
  <key>ProgramArguments</key>
  <array>
    <string>$BIN</string>
    <string>daemon</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>LimitLoadToSessionType</key>
  <string>Aqua</string>
  <key>StandardErrorPath</key>
  <string>$HOME/Library/Logs/osync.log</string>
</dict>
</plist>
EOF
  launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
  launchctl bootstrap "gui/$(id -u)" "$plist"
}

main() {
  detect_platform
  install_tailscale
  connect_tailscale
  install_clipboard_tools
  install_binary
  open_firewall
  if [[ $os == linux ]]; then
    install_service_linux
  else
    install_service_macos
  fi

  say "Installed"
  sleep 2 # give the daemon a moment to find Tailscale and its peers
  "$BIN" status || true
  printf '\nRun this on your other machines too. Check on it any time with: osync status\n'
  case ":$PATH:" in
    *":$BIN_DIR:"*) ;;
    *) printf 'Note: %s is not on your PATH, so call it as %s\n' "$BIN_DIR" "$BIN" ;;
  esac
}

# This stays the last line: piped into bash, the whole script must be read
# before anything runs. OSYNC_INSTALL_NO_RUN=1 lets a test source the file for
# its functions.
[[ -n ${OSYNC_INSTALL_NO_RUN:-} ]] || main "$@"
