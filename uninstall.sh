#!/bin/sh
# Removes rexmod: restores the official Rex, takes out the agent hooks,
# the ⌘P block in Rex's config and the Neovim plugin spec, then deletes the
# installed files. An editor config it installed (~/.config/nvim or
# ~/.config/rexmod-nvim) stays; delete it yourself if you don't want it.
set -eu
PREFIX="$HOME/Library/Application Support/rexmod"
APP="${REX_APP:-/Applications/Rex Beta.app}"
say() { printf '\033[1m==>\033[0m %s\n' "$*"; }

if [ -x "$PREFIX/bin/insertdylib" ] && "$PREFIX/bin/insertdylib" --check "$APP/Contents/MacOS/Rex" "$PREFIX/RexModPanel.dylib" >/dev/null 2>&1; then
  if [ -d "$PREFIX/backup/Rex Beta.app" ]; then
    say "Restoring the official Rex"
    "$PREFIX/scripts/unpatch.sh"
  else
    say "No backup of the official Rex; download it again from https://rex.superlogical.com"
  fi
fi

if [ -f "$HOME/Library/LaunchAgents/dev.rexmod.repatch.plist" ]; then
  say "Removing the re-patch agent"
  launchctl bootout "gui/$(id -u)/dev.rexmod.repatch" 2>/dev/null || true
  rm -f "$HOME/Library/LaunchAgents/dev.rexmod.repatch.plist"
fi

if [ -x "$PREFIX/scripts/install-hooks.sh" ]; then
  say "Removing the agent hooks (Claude Code, Codex, OpenCode, Pi)"
  "$PREFIX/scripts/install-hooks.sh" --remove
fi

REXCFG="${REX_CONFIG:-${XDG_CONFIG_HOME:-$HOME/.config}/rex/init.lua}"
if [ -f "$REXCFG" ]; then
  say "Removing the rexmod block from Rex's config"
  /usr/bin/python3 - "$REXCFG" <<'EOF'
import re, sys
p = sys.argv[1]
t = open(p).read()
t = re.sub(r"\n?-- >>> rexmod.*?-- <<< rexmod\n?", "\n", t, flags=re.S)
open(p, "w").write(t)
EOF
fi

SPEC="$HOME/.config/nvim/lua/plugins/rexmod.lua"
if grep -qs "rexmod.nvim" "$SPEC"; then
  say "Removing the rexmod Neovim plugin spec"
  rm -f "$SPEC"
fi

say "Deleting installed files"
[ -L "$HOME/.local/bin/rexmod" ] && rm -f "$HOME/.local/bin/rexmod"
rm -rf "$HOME/.config/rexmod" "$HOME/Library/Caches/rexmod"
rm -rf "$PREFIX"
say "Done. Quit Rex (⌘Q) and open it again."
