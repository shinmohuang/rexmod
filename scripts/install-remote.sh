#!/bin/sh
# Sets up rexmod on a remote Rex host (a Linux machine whose Rex server you reach
# with `rex hosts`), so the panel, the editor pane and the picker work for its
# sessions. Run it on the Mac, from a rexmod checkout:
#
#   scripts/install-remote.sh SSH_DEST [--no-hooks] [--no-nvim]
#
# Over ssh it installs, on the host:
#   ~/.local/bin/rexmod              rexmod built for the host (a running `rexmod serve`
#                                    sees the new binary and restarts on the next request)
#   $XDG_DATA_HOME/rexmod/           the editor plugin (nvim/), agents/, skills/, scripts/
#                                    (~/.local/share/rexmod unless your login shell sets it)
#   ~/.local/bin/nvim                Neovim NVIM_VERSION, unless the host has one recent enough
#   the editor config                as install.sh does: ~/.config/nvim when there's none, the
#                                    rexmod plugin spec in a lazy.nvim config, otherwise
#                                    ~/.config/rexmod-nvim (nvim_appname in ~/.config/rexmod)
#   ⌘P and Ctrl+h/j/k/l              scripts/rex-config.sh: keys in a session there go to that
#                                    host's Rex server, which reads its own init.lua
#   agent hooks                      scripts/install-hooks.sh (Last Turn, comment delivery)
#
# Safe to run again: it updates what it installed and leaves your own config alone.
set -eu

NVIM_VERSION="${NVIM_VERSION:-0.12.6}"
NVIM_MIN="0.11.2" # LazyVim's minimum

say() { printf '\033[1;35m==>\033[0m %s\n' "$*"; }
die() { printf 'install-remote: %s\n' "$*" >&2; exit 1; }

DEST=""
HOOKS=1
NVIM=1
for a in "$@"; do
  case "$a" in
    --no-hooks) HOOKS=0 ;;
    --no-nvim) NVIM=0 ;;
    -h|--help) sed -n '2,19p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    -*) die "unknown option $a" ;;
    *) DEST="$a" ;;
  esac
done
[ -n "$DEST" ] || die "usage: scripts/install-remote.sh SSH_DEST [--no-hooks] [--no-nvim]"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
[ -d "$ROOT/cmd/rexmod" ] && [ -d "$ROOT/editor/nvim" ] || die "run it from a rexmod checkout"
command -v go >/dev/null 2>&1 || die "needs Go to build rexmod for the host"

SSH="ssh -o BatchMode=yes -o ConnectTimeout=10"

say "Looking at $DEST"
UNAME="$($SSH "$DEST" uname -sm)" || die "can't reach $DEST over ssh"
case "$UNAME" in
  "Linux x86_64") GOARCH=amd64; NVIM_ARCH=x86_64 ;;
  "Linux aarch64"|"Linux arm64") GOARCH=arm64; NVIM_ARCH=arm64 ;;
  *) die "$DEST is $UNAME; this sets up Linux hosts (x86_64, arm64)" ;;
esac

STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT
say "Building rexmod for linux/$GOARCH"
VERSION="$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)"
(cd "$ROOT/cmd/rexmod" && CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build -trimpath \
  -ldflags "-s -w" -o "$STAGE/rexmod" .)
cp -R "$ROOT/nvim" "$ROOT/agents" "$ROOT/skills" "$STAGE/"
mkdir -p "$STAGE/scripts" "$STAGE/editor"
cp "$ROOT/scripts/install-hooks.sh" "$ROOT/scripts/rex-config.sh" "$STAGE/scripts/"
cp -R "$ROOT/editor/nvim" "$STAGE/editor/nvim"
echo "$VERSION" > "$STAGE/VERSION"

say "Copying it to $DEST"
# The setup below runs on the host, from the unpacked stage.
cat > "$STAGE/setup.sh" <<'EOF'
set -eu
STAGE="$(cd "$(dirname "$0")" && pwd)"
# XDG_DATA_HOME as the login shell has it: Rex starts the editor's panes with
# that environment, and rexmod looks for its files there.
DATA_HOME="$("${SHELL:-/bin/sh}" -l -c 'printf %s "$XDG_DATA_HOME"' 2>/dev/null </dev/null || true)"
case "$DATA_HOME" in /*) ;; *) DATA_HOME="$HOME/.local/share" ;; esac
PREFIX="$DATA_HOME/rexmod"
BIN="$HOME/.local/bin"
say() { printf '\033[1;35m==>\033[0m %s\n' "$*"; }
note() { printf '    %s\n' "$*"; }
mkdir -p "$BIN" "$PREFIX" "$HOME/.config/rexmod"

# rexmod: replace the file in one step, so a running serve sees one new binary
cp "$STAGE/rexmod" "$BIN/rexmod.new"
chmod 755 "$BIN/rexmod.new"
mv -f "$BIN/rexmod.new" "$BIN/rexmod"
rm -rf "$PREFIX/nvim" "$PREFIX/agents" "$PREFIX/skills" "$PREFIX/scripts"
cp -R "$STAGE/nvim" "$STAGE/agents" "$STAGE/skills" "$STAGE/scripts" "$PREFIX/"
cp "$STAGE/VERSION" "$PREFIX/VERSION"
say "rexmod $(cat "$STAGE/VERSION") in $BIN/rexmod"

# Neovim
PATH="$BIN:$PATH"
newer() { # version a >= b
  [ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | head -1)" = "$2" ]
}
have=""
if command -v nvim >/dev/null 2>&1; then
  have="$(nvim --version 2>/dev/null | head -1 | sed 's/^NVIM v//; s/[-+].*//')"
fi
if [ "$WANT_NVIM" = 1 ] && { [ -z "$have" ] || ! newer "$have" "$NVIM_MIN"; }; then
  say "Installing Neovim $NVIM_VERSION into $PREFIX/neovim"
  url="https://github.com/neovim/neovim/releases/download/v$NVIM_VERSION/nvim-linux-$NVIM_ARCH.tar.gz"
  tmp="$(mktemp -d)"
  if command -v curl >/dev/null 2>&1; then curl -fsSL "$url" -o "$tmp/nvim.tgz"; else wget -qO "$tmp/nvim.tgz" "$url"; fi
  tar -xzf "$tmp/nvim.tgz" -C "$tmp"
  rm -rf "$PREFIX/neovim"
  mv "$tmp/nvim-linux-$NVIM_ARCH" "$PREFIX/neovim"
  rm -rf "$tmp"
  ln -sf "$PREFIX/neovim/bin/nvim" "$BIN/nvim"
elif [ -n "$have" ]; then
  say "Neovim $have: $(command -v nvim)"
fi
command -v fzf >/dev/null 2>&1 || note "No fzf here: the file picker needs it (https://github.com/junegunn/fzf)."
command -v git >/dev/null 2>&1 || note "No git here: rexmod needs it."

# the editor config, as install.sh does on the Mac
CFG="$HOME/.config/rexmod/config.json"
SPEC_FILE="lua/plugins/rexmod.lua"
BOOTSTRAP=""
set_appname() { # keep the rest of an existing config
  if [ -s "$CFG" ] && command -v python3 >/dev/null 2>&1; then
    python3 - "$CFG" "$1" <<'PY'
import json, sys
path, app = sys.argv[1], sys.argv[2]
try:
    cfg = json.load(open(path))
except Exception:
    cfg = {}
if app:
    cfg["nvim_appname"] = app
else:
    cfg.pop("nvim_appname", None)
json.dump(cfg, open(path, "w"), indent=2)
PY
  elif [ -n "$1" ]; then
    printf '{"nvim_appname": "%s"}\n' "$1" > "$CFG"
  else
    [ -s "$CFG" ] || echo '{}' > "$CFG"
  fi
}
if [ ! -e "$HOME/.config/nvim" ]; then
  say "Installing the editor config (LazyVim) into ~/.config/nvim"
  cp -R "$STAGE/editor/nvim" "$HOME/.config/nvim"
  set_appname ""
  BOOTSTRAP="nvim"
elif [ -d "$HOME/.config/nvim/lua/plugins" ] && grep -rqs "lazy" "$HOME/.config/nvim/lua/config" "$HOME/.config/nvim/init.lua"; then
  if [ ! -f "$HOME/.config/nvim/$SPEC_FILE" ] || grep -qs "rexmod.nvim" "$HOME/.config/nvim/$SPEC_FILE"; then
    say "Adding the rexmod plugin to your Neovim config"
    cp "$STAGE/editor/nvim/$SPEC_FILE" "$HOME/.config/nvim/$SPEC_FILE"
  fi
  set_appname ""
else
  say "Installing the editor config as ~/.config/rexmod-nvim (your ~/.config/nvim stays as it is)"
  rm -rf "$HOME/.config/rexmod-nvim"
  cp -R "$STAGE/editor/nvim" "$HOME/.config/rexmod-nvim"
  set_appname "rexmod-nvim"
  BOOTSTRAP="rexmod-nvim"
fi
if [ -n "$BOOTSTRAP" ] && command -v nvim >/dev/null 2>&1; then
  say "Installing editor plugins (first run takes a minute)"
  NVIM_APPNAME="$BOOTSTRAP" nvim --headless "+Lazy! restore" +qa >/dev/null 2>&1 \
    || note "plugin install didn't finish; it completes the first time the editor opens"
fi

# A local copy of the editor's data (rexmod keeps one when the home is on a
# network filesystem) would keep the old plugins: drop it, and the next open
# copies the data afresh.
COPY_ROOT=""
if command -v python3 >/dev/null 2>&1 && [ -s "$CFG" ]; then
  COPY_ROOT="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("editor_data_dir",""))' "$CFG" 2>/dev/null || true)"
fi
case "$COPY_ROOT" in
  off) COPY_ROOT="" ;;
  "~/"*) COPY_ROOT="$HOME/${COPY_ROOT#\~/}" ;;
  /*) ;;
  "") COPY_ROOT="/var/tmp/rexmod-$(id -u)" ;;
  *) COPY_ROOT="" ;; # a relative path: rexmod takes it as off too
esac
if [ -n "$COPY_ROOT" ] && [ -d "$COPY_ROOT/data" ]; then
  say "Dropping the local copy of the editor's data in $COPY_ROOT (the next open makes a new one)"
  rm -rf "$COPY_ROOT/data" "$COPY_ROOT/cache" "$COPY_ROOT/.copy.failed"
fi

# Keys in a session on this host go to its Rex server and its own init.lua:
# the picker and pane keys go there too. Where Rex looks for it as the login
# shell (which starts the server) sees it.
REXCFG="$("${SHELL:-/bin/sh}" -l -c 'printf %s "${REX_CONFIG:-${XDG_CONFIG_HOME:-$HOME/.config}/rex/init.lua}"' 2>/dev/null </dev/null || true)"
case "$REXCFG" in /*) ;; *) REXCFG="$HOME/.config/rex/init.lua" ;; esac
REX_CONFIG="$REXCFG" "$PREFIX/scripts/rex-config.sh" "$BIN/rexmod" "$(command -v rex || true)"

if [ "$WANT_HOOKS" = 1 ]; then
  say "Hooking into the coding agents here (Last Turn, comment delivery)"
  "$PREFIX/scripts/install-hooks.sh"
fi
EOF

REMOTE_STAGE="$($SSH "$DEST" 'mktemp -d')"
tar -C "$STAGE" -cf - . | $SSH "$DEST" "tar -C '$REMOTE_STAGE' -xf -"
$SSH "$DEST" "WANT_HOOKS=$HOOKS WANT_NVIM=$NVIM NVIM_VERSION=$NVIM_VERSION NVIM_MIN=$NVIM_MIN NVIM_ARCH=$NVIM_ARCH sh '$REMOTE_STAGE/setup.sh'; s=\$?; rm -rf '$REMOTE_STAGE'; exit \$s"
say "Done. Open a file from the panel in a session on that host to start the editor there."
