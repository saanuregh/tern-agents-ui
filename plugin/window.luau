-- Palette commands "New Claude chat" and "New Codex chat": run tclaude or
-- tcodex, built next to this folder in ../bin, in a pane beside the focused
-- one and in its directory.

local BIN = tern.plugin.dir .. "/../bin/"

local function quote(s: string): string
	return "'" .. string.gsub(s, "'", "'\\''") .. "'"
end

local function open(cx, command: string, extra: string)
	local id = cx.session:focused()
	local pane = if id then cx.session:pane(id) else nil
	local cwd = if pane and pane.cwd ~= "" then pane.cwd else nil
	local launch = { command = quote(BIN .. command) .. extra, cwd = cwd }
	if id then
		cx.layout:split(id, "right", launch)
	else
		cx.layout:new_tab(launch)
	end
end

local agents = {
	{ id = "claude", name = "Claude", command = "tclaude" },
	{ id = "codex", name = "Codex", command = "tcodex" },
}

for _, agent in ipairs(agents) do
	tern.command({
		id = agent.id .. "-chat",
		title = "New " .. agent.name .. " chat",
		icon = "app-" .. agent.id,
		run = function(cx)
			open(cx, agent.command, "")
		end,
	})
	tern.command({
		id = agent.id .. "-continue",
		title = "Continue last " .. agent.name .. " chat",
		icon = "app-" .. agent.id,
		run = function(cx)
			open(cx, agent.command, " -continue")
		end,
	})
end
