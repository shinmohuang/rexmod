-- nvim follows the Rex theme: colors/rex.lua reads it, and a 'background' change reloads it.
local function follow(system)
  local appearance = require("rex_theme").appearance()
  local background = appearance == "system" and system or appearance

  if vim.o.background ~= background then
    vim.o.background = background
  end
end

-- On a remote Rex host the appearance comes with the theme the Mac handed over; there is no
-- system appearance to follow.
if vim.fn.has("mac") == 0 then
  follow("dark")
end

return {
  { "nvim-mini/mini.base16", lazy = true },
  {
    -- Rex does not answer the OSC 11 background query, so nvim cannot detect the appearance itself.
    "f-person/auto-dark-mode.nvim",
    cond = vim.fn.has("mac") == 1,
    lazy = false,
    opts = {
      set_dark_mode = function()
        follow("dark")
      end,
      set_light_mode = function()
        follow("light")
      end,
    },
  },
  { "LazyVim/LazyVim", opts = { colorscheme = "rex" } },
}
