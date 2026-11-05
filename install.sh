#!/bin/sh
# rexmod installer: the Changes panel, editor pane, review comments and ⌘P
# file picker for Rex, on a Mac where Rex is installed.
#
#   curl -fsSL https://raw.githubusercontent.com/alpcanaydin/rexmod/main/install.sh | sh
#   ./install.sh               install everything (from a release or a checkout)
#   ./install.sh --update-rex  also install the latest official Rex first
#   ./install.sh --no-patch    skip patching Rex (no panel)
#   ./install.sh --no-hooks    skip the agent hooks (Claude Code, Codex, OpenCode, Pi)
#
# Safe to run again; it updates what it installed before.
set -eu
ROOT="$(cd "$(dirname "$0")" && pwd)"
REPO="alpcanaydin/rexmod"
APP="${REX_APP:-/Applications/Rex Beta.app}"
PREFIX="$HOME/Library/Application Support/rexmod"
PATCH=1 HOOKS=1 UPDATE=0
for arg in "$@"; do
  case "$arg" in
    --no-patch) PATCH=0 ;;
    --no-hooks) HOOKS=0 ;;
    --update-rex) UPDATE=1 ;;
    -h|--help) sed -n '2,12p' "$0" 2>/dev/null || echo "usage: install.sh [--update-rex] [--no-patch] [--no-hooks]"; exit 0 ;;
    *) echo "unknown option: $arg" >&2; exit 2 ;;
  esac
done

say() { printf '\033[1m==>\033[0m %s\n' "$*"; }
note() { printf '    %s\n' "$*"; }
die() { printf '\033[31mrexmod:\033[0m %s\n' "$*" >&2; exit 1; }

# --- requirements -----------------------------------------------------------
[ "$(uname -s)" = Darwin ] && [ "$(uname -m)" = arm64 ] || die "needs a Mac with Apple silicon"
[ "$(sw_vers -productVersion | cut -d. -f1)" -ge 15 ] || die "needs macOS 15 or later"
[ -d "$APP" ] || die "Rex is not installed. Download it from https://rex.superlogical.com, open it once, then run this again."
if ! xcode-select -p >/dev/null 2>&1; then
  xcode-select --install >/dev/null 2>&1 || true
  die "needs the Command Line Tools (for git). A window to install them just opened; run this again when it finishes."
fi
# Piped from curl there is nothing next to this script: fetch the latest
# release (prebuilt, nothing to compile) and run its installer.
if [ ! -d "$ROOT/nvim" ] || [ ! -d "$ROOT/agents" ]; then
  say "Downloading the latest rexmod release"
  url=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" |
    sed -n 's/.*"browser_download_url": *"\([^"]*darwin-arm64\.tar\.gz\)".*/\1/p' | head -1)
  [ -n "$url" ] || die "couldn't find a release at https://github.com/$REPO/releases"
  TMP="$(mktemp -d)"
  curl -fsSL "$url" | tar -xz -C "$TMP" || die "download failed: $url"
  set -- "$TMP"/rexmod*/install.sh "$@"
  [ -f "$1" ] || die "the release archive has no installer"
  exec sh "$@"
fi
# A checkout has no prebuilt files: build them when the tools are there.
if [ ! -f "$ROOT/dist/rexmod" ] || [ ! -f "$ROOT/dist/insertdylib" ] || [ ! -f "$ROOT/dist/RexModPanel.dylib" ]; then
  if command -v go >/dev/null 2>&1 && xcrun -f swiftc >/dev/null 2>&1; then
    say "Building rexmod (Go and Swift)"
    "$ROOT/scripts/build-dist.sh" >/dev/null
  else
    die "no prebuilt files here. Use a release (https://github.com/$REPO/releases), or install Go and Xcode to build."
  fi
fi

BREW="$(command -v brew || true)"
[ -n "$BREW" ] || { [ -x /opt/homebrew/bin/brew ] && BREW=/opt/homebrew/bin/brew; }
need() { # tool formula
  command -v "$1" >/dev/null 2>&1 || [ -x "/opt/homebrew/bin/$1" ] && return 0
  [ -n "$BREW" ] || die "needs $2. Install Homebrew (https://brew.sh), then run this again."
  say "Installing $2"
  "$BREW" install "$2"
}
need nvim neovim
need fzf fzf
need node node # language servers installed with npm (TypeScript, JSON, YAML, CSS, Pyright…)
NVIM="$(command -v nvim || echo /opt/homebrew/bin/nvim)"

# --- files ------------------------------------------------------------------
say "Installing rexmod into $PREFIX"
mkdir -p "$PREFIX/bin" "$PREFIX/scripts" "$HOME/.local/bin"
install -m 755 "$ROOT/dist/rexmod" "$ROOT/dist/insertdylib" "$PREFIX/bin/"
cp "$ROOT/dist/RexModPanel.dylib" "$PREFIX/RexModPanel.dylib.new"
mv -f "$PREFIX/RexModPanel.dylib.new" "$PREFIX/RexModPanel.dylib"
rm -rf "$PREFIX/nvim" "$PREFIX/agents" "$PREFIX/skills"
cp -R "$ROOT/nvim" "$PREFIX/nvim"
cp -R "$ROOT/agents" "$PREFIX/agents"
cp -R "$ROOT/skills" "$PREFIX/skills"
for s in patch.sh unpatch.sh update.sh install-hooks.sh fetch-flow-icons.sh install-watcher.sh rex-config.sh; do
  install -m 755 "$ROOT/scripts/$s" "$PREFIX/scripts/$s"
done
install -m 755 "$ROOT/uninstall.sh" "$PREFIX/scripts/uninstall.sh"
ln -sf "$PREFIX/bin/rexmod" "$HOME/.local/bin/rexmod"
case ":$PATH:" in *":$HOME/.local/bin:"*) ;; *) note "Add ~/.local/bin to your PATH to run rexmod from a shell (Rex itself doesn't need it)." ;; esac

if [ ! -f "$PREFIX/flow-icons/deep.json" ]; then
  say "Downloading Flow Icons (free pack, Open VSX)"
  "$PREFIX/scripts/fetch-flow-icons.sh" || note "icons failed to download; the panel falls back to SF Symbols"
fi

# --- editor -----------------------------------------------------------------
CFG="$HOME/.config/rexmod/config.json"
mkdir -p "$(dirname "$CFG")"
SPEC_FILE="lua/plugins/rexmod.lua"
BOOTSTRAP=""
if [ ! -e "$HOME/.config/nvim" ]; then
  say "Installing the editor config (LazyVim) into ~/.config/nvim"
  cp -R "$ROOT/editor/nvim" "$HOME/.config/nvim"
  echo '{}' > "$CFG"
  BOOTSTRAP="nvim"
elif [ -d "$HOME/.config/nvim/lua/plugins" ] && grep -rqs "lazy" "$HOME/.config/nvim/lua/config" "$HOME/.config/nvim/init.lua"; then
  if [ ! -f "$HOME/.config/nvim/$SPEC_FILE" ] || grep -qs "rexmod.nvim" "$HOME/.config/nvim/$SPEC_FILE"; then
    say "Adding the rexmod plugin to your Neovim config"
    cp "$ROOT/editor/nvim/$SPEC_FILE" "$HOME/.config/nvim/$SPEC_FILE"
  fi
  echo '{}' > "$CFG"
else
  say "Installing the editor config as ~/.config/rexmod-nvim (your ~/.config/nvim stays as it is)"
  rm -rf "$HOME/.config/rexmod-nvim"
  cp -R "$ROOT/editor/nvim" "$HOME/.config/rexmod-nvim"
  echo '{"nvim_appname": "rexmod-nvim"}' > "$CFG"
  BOOTSTRAP="rexmod-nvim"
fi
if [ -n "$BOOTSTRAP" ]; then
  say "Installing editor plugins (first run takes a minute)"
  NVIM_APPNAME="$BOOTSTRAP" "$NVIM" --headless "+Lazy! restore" +qa >/dev/null 2>&1 \
    || note "plugin install didn't finish; it completes the first time the editor opens"
fi

# --- Rex config: ⌘P picker (and Ctrl+h/j/k/l pane moves) -------------------
REX="$(command -v rex || echo "$HOME/Library/Application Support/com.superlogical.rex/RexServer/Versions/Current/Rex Server.app/Contents/MacOS/rex")"
"$PREFIX/scripts/rex-config.sh" "$PREFIX/bin/rexmod" "$REX"

# --- agent hooks: Claude Code, Codex, OpenCode, Pi ------------------------------
if [ "$HOOKS" = 1 ]; then
  say "Hooking into your coding agents (Last Turn, comment delivery)"
  "$PREFIX/scripts/install-hooks.sh"
  note "Installed another agent later? Run $PREFIX/scripts/install-hooks.sh again."
fi

# --- Rex ----------------------------------------------------------------------
if [ "$PATCH" = 1 ]; then
  if [ "$UPDATE" = 1 ]; then
    say "Updating Rex and patching it"
    "$PREFIX/scripts/update.sh"
  else
    say "Patching Rex"
    "$PREFIX/scripts/patch.sh"
  fi
  # Rex updates itself and drops the patch; re-apply it after each update.
  say "Installing the re-patch agent (keeps the panel across Rex updates)"
  "$PREFIX/scripts/install-watcher.sh"
fi

say "Done. Quit Rex (⌘Q) and open it again."
note "Panel: ⌘⇧E · files: ⌘P · comment in the editor: <Space>r (E edit, D delete on a note)"
note "Rex updates re-apply the panel by themselves (reopen Rex when told) · remove everything: $PREFIX/scripts/uninstall.sh"
