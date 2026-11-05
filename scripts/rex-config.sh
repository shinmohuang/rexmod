#!/bin/sh
# Adds rexmod's keys to this machine's Rex config, in a block of its own that
# each run replaces (uninstall.sh takes it out):
#
#   Cmd+P       the file picker over the repo the tab's agent works in
#   Ctrl+h/j/k/l  move between panes (and Neovim's windows), unless the
#               config already binds focus_direction itself
#
# Rex reads the config of the server a session is on: keys in a session on a
# remote Rex host go to that host's server and its init.lua, so the host needs
# the block too (scripts/install-remote.sh runs this there).
#
#   rex-config.sh REXMOD [REX]
#
# REXMOD is the rexmod the picker runs; REX, the rex that reloads the config
# (default: rex on PATH). The config is where Rex looks for it: REX_CONFIG, or
# rex/init.lua in $XDG_CONFIG_HOME (~/.config).
set -eu
BIN="${1:?usage: rex-config.sh REXMOD [REX]}"
REX="${2:-$(command -v rex || true)}"

REXCFG="${REX_CONFIG:-${XDG_CONFIG_HOME:-$HOME/.config}/rex/init.lua}"
BEGIN="-- >>> rexmod (managed by install.sh; edits inside are replaced)"
END="-- <<< rexmod"
mkdir -p "$(dirname "$REXCFG")"
touch "$REXCFG"

# Take out the block of an earlier run. Written back through the file (not
# moved over it), so a config that is a link into dotfiles stays one.
tmp="$(mktemp)"
awk -v b="$BEGIN" -v e="$END" '
  $0 == b { skip = 1; next }
  skip { if ($0 == e) skip = 0; next }
  { print }
' "$REXCFG" > "$tmp"
cat "$tmp" > "$REXCFG"
rm -f "$tmp"

# A picker binding of the user's own stays theirs.
if grep -q 'pick.*--here' "$REXCFG"; then
  exit 0
fi
HAS_PANE_KEYS=0
grep -q "focus_direction" "$REXCFG" && HAS_PANE_KEYS=1

# BIN as a Lua string
LUA_BIN="$(printf '%s' "$BIN" | sed 's/\\/\\\\/g; s/"/\\"/g')"
# a blank line before the block, unless the config ends with one (or is empty)
SEP=""
if [ -s "$REXCFG" ] && [ -n "$(tail -n 1 "$REXCFG")" ]; then
  SEP="
"
fi
{
  printf '%s' "$SEP"
  printf '%s\n' "$BEGIN"
  cat <<EOF
-- Cmd+P: fuzzy file picker over the repo this tab's agent works in. rexmod runs as it is, with
-- no shell in between: Rex starts a command as a login one, and a profile that fails under sh
-- would stop it.
rex.bind("cmd+p", function(ctx)
  rex.call("session.new_layer", {
    session_id = ctx.session_id,
    bounds = { 0.15, 0.1, 0.7, 0.75 },
    focus = true,
    layout = { block = { flavor = "com.superlogical.terminal.shell", label = "files", options = {
      shell = "none",
      command = { "$LUA_BIN", "pick", "--here", "--session", ctx.session_id },
    } } },
  })
  return true
end)
EOF
  if [ "$HAS_PANE_KEYS" = 0 ]; then
    cat <<'EOF'

-- Ctrl+h/j/k/l move between panes; inside Neovim they move between its
-- windows first (the editor config hands over at its edge).
local function runs_vim(ctx)
  local ok, process = pcall(rex.call, "block.method", { session_id = ctx.session_id, block_id = ctx.block_id, method = "process" })
  local name = ok and process and process.foreground and process.foreground.name
  return name == "nvim" or name == "vim"
end
for key, direction in pairs({ h = "left", j = "down", k = "up", l = "right" }) do
  rex.bind("ctrl+" .. key, function(ctx)
    if runs_vim(ctx) then
      return false
    end
    rex.call("session.focus_direction", { session_id = ctx.session_id, direction = direction })
    return true
  end)
end
EOF
  fi
  printf '%s\n' "$END"
} >> "$REXCFG"
printf '==> Added ⌘P to %s\n' "$REXCFG"

if [ -n "$REX" ] && [ -x "$REX" ]; then
  "$REX" config reload >/dev/null 2>&1 || true
fi
