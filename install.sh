#!/bin/sh
# Install the lease binary from GitHub releases.
#
#   curl -fsSL https://raw.githubusercontent.com/delaanthonio/lease/main/install.sh | sh
#
# Environment:
#   INSTALL_DIR  where to place the binary (default: ~/.local/bin, or
#                /usr/local/bin when run as root)
#   VERSION      a release tag to pin (default: latest release)
set -eu

repo="delaanthonio/lease"
name="lease"

os=$(uname -s)
arch=$(uname -m)
case "$os" in
  Darwin) os="darwin" ;;
  Linux) os="linux" ;;
  *) echo "install.sh: unsupported os: $os" >&2; exit 1 ;;
esac
case "$arch" in
  arm64|aarch64) arch="arm64" ;;
  x86_64|amd64) arch="amd64" ;;
  *) echo "install.sh: unsupported arch: $arch" >&2; exit 1 ;;
esac

if [ -n "${VERSION:-}" ]; then
  version="$VERSION"
else
  version=$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" |
    sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p')
  if [ -z "$version" ]; then
    echo "install.sh: could not determine the latest release" >&2
    exit 1
  fi
fi

install_dir="${INSTALL_DIR:-$HOME/.local/bin}"
if [ "$(id -u)" = "0" ] && [ -z "${INSTALL_DIR:-}" ]; then
  install_dir="/usr/local/bin"
fi
# A privileged install into a directory other users can write lets one of
# them replace the temporary binary before it is installed.
if [ "$(id -u)" = "0" ]; then
  mode=$(stat -c '%a' "$install_dir" 2>/dev/null || stat -f '%Lp' "$install_dir")
  group=${mode%?}
  case "$mode" in
    *[2367]) echo "install.sh: refusing privileged install into world-writable $install_dir" >&2; exit 1 ;;
  esac
  case "$group" in
    *[2367]) echo "install.sh: refusing privileged install into group-writable $install_dir" >&2; exit 1 ;;
  esac
fi
mkdir -p "$install_dir"

asset="${name}-${os}-${arch}"
url="https://github.com/$repo/releases/download/$version/$asset"
tmp=$(mktemp "$install_dir/.$name.XXXXXX")
trap 'rm -f "$tmp"' EXIT
curl -fsSL -o "$tmp" "$url"
chmod 0755 "$tmp"
mv "$tmp" "$install_dir/$name"
trap - EXIT

echo "installed $install_dir/$name ($version, ${os}/${arch})"
