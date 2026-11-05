-- Reads the active Rex terminal theme, so nvim follows Rex.
-- Rex does not answer the OSC 4 and OSC 11 color queries with its theme, so read its settings instead:
-- the theme IDs come from the app's defaults, the colors from the themes.json in the app bundle.
-- An imported theme is not in themes.json. Its colors sit in the defaults key Workspace.customThemes.
-- On a remote Rex host there are no such settings: the Mac's rexmod hands the theme over with
-- `rexmod open`, which keeps it in the file $REXMOD_REX_THEME (or ~/.cache/rexmod/rex-theme.json).
local M = {}

local function snapshot_path()
  if vim.env.REXMOD_REX_THEME and vim.env.REXMOD_REX_THEME ~= "" then
    return vim.env.REXMOD_REX_THEME
  end
  local cache = vim.env.XDG_CACHE_HOME
  if not cache or cache == "" then
    cache = vim.env.HOME .. "/.cache"
  end
  return cache .. "/rexmod/rex-theme.json"
end

-- The theme the Mac handed over, or nil when this is the Mac (or nothing was handed over).
local function read_snapshot()
  if vim.fn.has("mac") == 1 then
    return nil
  end
  local ok, lines = pcall(vim.fn.readfile, snapshot_path())
  if not ok then
    return nil
  end
  local decoded, snap = pcall(vim.json.decode, table.concat(lines, "\n"))
  return decoded and type(snap) == "table" and snap or nil
end

local THEMES_GLOB =
  "/Applications/Rex*.app/Contents/Resources/SuperUI_SuperDesignTokens.bundle/Contents/Resources/themes.json"

local cache = { path = nil, mtime = nil, themes = nil }

local function read_settings()
  local result = vim.system({ "defaults", "read", "com.superlogical.rex" }, { text = true }):wait()

  if result.code ~= 0 then
    error("rex: cannot read the Rex settings")
  end

  local function setting(key)
    return result.stdout:match('\n%s*"Workspace%.' .. key .. '" = "?([^";]+)"?;')
  end

  return {
    appearance = setting("appearance") or "system",
    dark = setting("darkThemeID"),
    light = setting("lightThemeID"),
  }
end

local function read_themes()
  local path = vim.fn.glob(THEMES_GLOB, false, true)[1]

  if not path then
    error("rex: cannot find themes.json in the Rex app")
  end

  local mtime = vim.uv.fs_stat(path).mtime.sec

  if cache.path ~= path or cache.mtime ~= mtime then
    cache = { path = path, mtime = mtime, themes = vim.json.decode(table.concat(vim.fn.readfile(path), "\n")) }
  end

  return cache.themes
end

-- The key holds JSON as plist data, which only `defaults export` and `plutil` give back as bytes.
local function read_custom_themes()
  local plist = vim.system({ "defaults", "export", "com.superlogical.rex", "-" }, { text = true }):wait()
  local data = vim
    .system({ "plutil", "-extract", "Workspace\\.customThemes", "raw", "-o", "-", "-" }, { stdin = plist.stdout, text = true })
    :wait()

  if plist.code ~= 0 or data.code ~= 0 then
    return {}
  end

  return vim.json.decode(vim.base64.decode(vim.trim(data.stdout)))
end

-- A custom theme holds each color as sRGB channels from 0 to 1.
local function to_hex(color)
  local function channel(value)
    return math.floor(value * 255 + 0.5)
  end

  return string.format("#%02x%02x%02x", channel(color.red), channel(color.green), channel(color.blue))
end

-- Gives a custom theme the shape of a themes.json entry.
local function from_custom(theme)
  local palette = theme.palette

  return {
    id = theme.id,
    source = {
      background = to_hex(palette.background),
      foreground = to_hex(palette.foreground),
      selectionBackground = to_hex(palette.selectionBackground),
      selectionForeground = to_hex(palette.selectionForeground),
      ansi = vim.tbl_map(to_hex, palette.ansi),
    },
  }
end

-- Returns "system", "dark" or "light": the appearance setting of Rex.
function M.appearance()
  local snap = read_snapshot()
  if snap then
    return snap.appearance or "dark"
  end
  if vim.fn.has("mac") == 0 then
    return "dark"
  end
  return read_settings().appearance
end

-- Returns the Rex theme for a background. Its source holds background, foreground, ansi[1..16] and more.
function M.theme(background)
  if vim.fn.has("mac") == 0 then
    local snap = read_snapshot()
    local theme = snap and (snap[background] or snap.dark or snap.light)
    if not theme then
      error("rex: no Rex theme from the Mac yet (" .. snapshot_path() .. ")")
    end
    return theme
  end

  local id = read_settings()[background]

  for _, theme in ipairs(read_themes()) do
    if theme.id == id then
      return theme
    end
  end

  for _, theme in ipairs(read_custom_themes()) do
    if theme.id == id then
      return from_custom(theme)
    end
  end

  error("rex: cannot find the Rex theme " .. tostring(id))
end

return M
