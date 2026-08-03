module("luci.controller.model-gateway", package.seeall)

function index()
    entry({"admin", "services", "model-gateway"}, alias("admin", "services", "model-gateway", "config"), _("Model Gateway"), 30).dependent = true
    entry({"admin", "services", "model-gateway", "config"}, cbi("model-gateway"))
    entry({"admin", "services", "model-gateway", "status"}, template("model-gateway/status"))
    -- 即时启停（供前端按钮调用，服务端直接操作 init.d 并同步 UCI enable）
    entry({"admin", "services", "model-gateway", "svcstatus"}, call("action_svcstatus"))
    entry({"admin", "services", "model-gateway", "start"}, call("action_start"))
    entry({"admin", "services", "model-gateway", "stop"}, call("action_stop"))
end

local function svc_running()
    local sys = require "luci.sys"
    return (sys.call("pidof model-gatewayd model-gatewayd-arm64 >/dev/null 2>&1") == 0)
end

function action_svcstatus()
    local running = svc_running()
    luci.http.prepare_content("application/json")
    luci.http.write('{"running":' .. (running and "true" or "false") .. '}')
end

function action_start()
    local sys = require "luci.sys"
    local uci = require "luci.model.uci".cursor()
    uci:set("model-gateway", "settings", "enable", "1")
    uci:commit("model-gateway")
    sys.call("/etc/init.d/model-gateway enable >/dev/null 2>&1")
    sys.call("/etc/init.d/model-gateway restart >/dev/null 2>&1")
    -- 兜底：restart 后仍未来起，强制 start 一次
    if not svc_running() then
        sys.call("/etc/init.d/model-gateway start >/dev/null 2>&1")
    end
    luci.http.prepare_content("application/json")
    luci.http.write('{"running":' .. (svc_running() and "true" or "false") .. '}')
end

function action_stop()
    local sys = require "luci.sys"
    local uci = require "luci.model.uci".cursor()
    uci:set("model-gateway", "settings", "enable", "0")
    uci:commit("model-gateway")
    sys.call("/etc/init.d/model-gateway disable >/dev/null 2>&1")
    sys.call("/etc/init.d/model-gateway stop >/dev/null 2>&1")
    luci.http.prepare_content("application/json")
    luci.http.write('{"running":false}')
end
