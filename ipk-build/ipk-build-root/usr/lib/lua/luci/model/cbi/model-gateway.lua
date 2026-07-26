--[[
LuCI - Lua Configuration Interface
Model Gateway 原生应用配置（非 Docker）
]]

local uci = require "luci.model.uci".cursor()
local sys  = require "luci.sys"
local util = require "luci.util"

local m, s, o

m = Map("model-gateway", translate("Model Gateway"),
    translate("OpenAI 兼容的 AI 模型网关，聚合 NVIDIA、商汤、魔搭、Gemini 等平台的免费额度。"))

-- ========== 辅助函数 ==========

-- shell uci 读取（绕过 CBI cursor，避免内存状态不一致）
local function shell_uci_get(opt)
    return util.trim(util.exec("uci get " .. opt .. " 2>/dev/null"))
end

-- 通过单次 uci batch 完成所有写操作（set/delete/commit 在同一进程内，共享内存态）
local function shell_uci_batch(cmds)
    local script = table.concat(cmds, "\n") .. "\n"
    sys.call("uci batch <<'UCIEOF'\n" .. script .. "UCIEOF")
end

-- 根据选择器值和自定义路径解析实际存储路径
-- selector 可能是：default（内置）、custom（自定义输入）、或真实挂载点路径（如 /mnt/sda1）
local function resolve_path(selector, custom)
    if selector == "default" or selector == "" then
        return ""
    elseif selector == "custom" then
        return custom or ""
    else
        -- selector 为动态枚举到的真实挂载点，按 iStoreOS 约定放在 <挂载点>/Configs/<应用> 下
        return selector .. "/Configs/model-gateway"
    end
end

-- 动态枚举 /mnt 下真实挂载且可写的外部存储（自动识别盘名，不再写死 nvme/sd）
-- 依据 iStoreOS storage-path 方法论：读 /proc/mounts → 过滤伪文件系统 → df 有空间 → 可写测试
local function list_external_mounts()
    local mounts = {}
    local seen = {}
    local f = io.open("/proc/mounts", "r")
    if f then
        for line in f:lines() do
            local parts = {}
            for w in line:gmatch("%S+") do parts[#parts + 1] = w end
            local mp = parts[2]
            local fstype = parts[3]
            if mp and mp:match("^/mnt/") and mp ~= "/mnt" then
                -- 过滤伪文件系统/不可写介质
                if fstype ~= "tmpfs" and fstype ~= "devtmpfs" and fstype ~= "proc"
                   and fstype ~= "sysfs" and fstype ~= "cgroup" and fstype ~= "cgroup2"
                   and fstype ~= "debugfs" and fstype ~= "overlay" and fstype ~= "mqueue"
                   and fstype ~= "squashfs" and fstype ~= "autofs" then
                    -- 仅按 /proc/mounts 枚举，不做 df/touch 写测试。
                    -- 写测试若放在此处，会在每次渲染/提交时执行；系统刚装完或正忙的瞬间可能
                    -- 偶发失败，导致本次下拉选项列表与用户提交值不一致，ListValue 严格校验
                    -- 失败后静默丢弃选择（表现为“需点两次才生效”）。
                    -- 可写性校验已下沉到 migrate_storage（仅迁移时一次性执行）。
                    if not seen[mp] then
                        mounts[#mounts + 1] = mp
                        seen[mp] = true
                    end
                end
            end
        end
        f:close()
    end
    return mounts
end

-- 获取当前 UCI 中记录的外部存储路径（通过 config_path 字段）
local function get_config_path()
    local cp = shell_uci_get("model-gateway.settings.config_path")
    return cp
end

-- 执行存储路径迁移（不使用 symlink，uci 不支持通过 symlink 写入）
local function migrate_storage(new_path)
    if new_path == "" then
        return false, "未指定有效的配置路径"
    end

    local current_cp = get_config_path()

    -- 目标路径与当前相同，跳过
    if current_cp == new_path then
        return true, "路径未变化，无需迁移"
    end

    -- 检查外部存储是否可用且可写（迁移时一次性校验，不放在枚举下拉处）
    local mount_base = new_path:match("^(/mnt/[^/]+)")
    if mount_base then
        local stat = util.trim(util.exec("df '" .. mount_base .. "' 2>/dev/null | tail -1 | awk '{print $2}'"))
        if stat == "" or stat == "0" then
            return false, "外部存储 " .. mount_base .. " 不可用或未挂载"
        end
        -- 可写性测试：创建目标目录并尝试写入一个临时文件（替代原先放在下拉枚举处的 touch 测试）
        sys.call("mkdir -p '" .. new_path .. "'")
        local wt = util.trim(util.exec("touch '" .. new_path .. "/.mg_wtest' 2>/dev/null && echo 1; rm -f '" .. new_path .. "/.mg_wtest' 2>/dev/null"))
        if wt ~= "1" then
            return false, "外部存储 " .. mount_base .. " 不可写（请检查挂载权限）"
        end
    end

    -- 1. 创建目标目录
    sys.call("mkdir -p '" .. new_path .. "/data'")

    -- 2. 复制配置文件（/etc/config/model-gateway 始终是普通文件，uci 依赖它）
    sys.call("rm -f '" .. new_path .. "/model-gateway'")
    sys.call("cp /etc/config/model-gateway '" .. new_path .. "/model-gateway'")

    -- 验证目标配置文件已创建
    local target_ok = util.exec("test -s '" .. new_path .. "/model-gateway' && echo 1 || echo 0")
    if util.trim(target_ok) ~= "1" then
        return false, "配置文件复制失败: cp /etc/config/model-gateway → " .. new_path
    end

    -- 3. 复制数据文件（如果默认数据目录存在）
    local default_data = "/var/lib/model-gateway"
    local stat_data = util.trim(util.exec("test -d '" .. default_data .. "' && echo 1 || echo 0"))
    if stat_data == "1" then
        sys.call("cp -a '" .. default_data .. "/.' '" .. new_path .. "/data/' 2>/dev/null")
    end

    return true, "迁移完成"
end

-- 同步配置文件到外部路径（uci commit 只写 /etc/config/，需手动同步到外部）
local function sync_config_to_external(config_path)
    if config_path == "" then return end
    sys.call("test -d '" .. config_path .. "' && cp /etc/config/model-gateway '" .. config_path .. "/model-gateway' 2>/dev/null")
end

-- 捕获用户在“解析阶段”实际选择的存储路径（由下方 write 钩子赋值），
-- on_after_commit 直接使用，避免依赖事后 shell uci get 读取（消除读-写竞态/读空风险）。
local selected_selector = nil
local selected_custom = nil

-- ========== 合并为单个 TypedSection ==========
s = m:section(TypedSection, "model-gateway")
s.addremove = false
s.anonymous = true

-- [服务控制]
o = s:option(DummyValue, "_sep_control", "")
o.rawhtml = true
o.cfgvalue = function() return '<h3>' .. translate("服务控制") .. '</h3>' end

-- 状态显示（由 JS 实时反映实际运行态，取代原“运行中（未启用自启）”）
o = s:option(DummyValue, "_status", translate("状态"))
o.rmempty = true
o.rawhtml = true
o.cfgvalue = function(self, section)
    return '<span id="mg-svc-status" style="font-weight:600;color:#888;">检测中…</span>'
end

-- 管理面板按钮
o = s:option(DummyValue, "_open_panel", translate("管理面板"))
o.rmempty = true
o.rawhtml = true
o.cfgvalue = function(self, section)
    local port = uci:get("model-gateway", "settings", "port") or "8080"
    local ip = util.trim(util.exec("uci get network.lan.ipaddr 2>/dev/null || echo 192.168.100.1"))
    local url = "http://" .. ip .. ":" .. port
    return '<a class="cbi-button cbi-button-apply" href="' .. url .. '" target="_blank">' .. translate("进入管理面板") .. '</a>'
end

-- 启动/停止切换按钮（取代原“启用服务”勾选框，作为唯一启停入口）
o = s:option(DummyValue, "_startstop", translate("服务启停"))
o.rmempty = true
o.rawhtml = true
o.cfgvalue = function(self, section)
    local d = require "luci.dispatcher"
    local url_start  = d.build_url("admin", "services", "model-gateway", "start")
    local url_stop   = d.build_url("admin", "services", "model-gateway", "stop")
    local url_status = d.build_url("admin", "services", "model-gateway", "svcstatus")
    local html = '<button type="button" id="mg-svc-btn" class="cbi-button" '
        .. 'style="min-width:84px;padding:4px 14px;font-weight:600;cursor:pointer;'
        .. 'border:none;border-radius:4px;color:#fff;background:#3c8c3c;">检查中…</button>'
        .. '<script type="text/javascript">'
        .. '(function(){'
        .. 'var btn=document.getElementById("mg-svc-btn");'
        .. 'var st=document.getElementById("mg-svc-status");'
        .. 'if(!btn)return;'
        .. 'var URL_START=' .. string.format('%q', url_start) .. ';'
        .. 'var URL_STOP=' .. string.format('%q', url_stop) .. ';'
        .. 'var URL_STATUS=' .. string.format('%q', url_status) .. ';'
        .. 'function setRunning(r){'
        .. 'if(r){btn.textContent="停止";btn.style.background="#c0392b";btn.dataset.action="stop";}'
        .. 'else{btn.textContent="启动";btn.style.background="#3c8c3c";btn.dataset.action="start";}'
        .. 'if(st){st.textContent=r?"运行中":"已停止";st.style.color=r?"#2e7d32":"#888";}'
        .. '}'
        .. 'function refresh(){btn.disabled=true;btn.textContent="处理中…";'
        .. 'return fetch(URL_STATUS,{cache:"no-store"}).then(function(r){return r.json();})'
        .. '.then(function(j){setRunning(!!(j&&j.running));}).catch(function(){setRunning(false);})'
        .. '.then(function(){btn.disabled=false;});}'
        .. 'function act(url){btn.disabled=true;btn.textContent="处理中…";'
        .. 'fetch(url,{cache:"no-store"}).then(function(r){return r.json();})'
        .. '.then(function(j){setRunning(!!(j&&j.running));}).catch(function(){})'
        .. '.then(function(){return refresh();});}'
        .. 'btn.addEventListener("click",function(){if(btn.disabled)return;'
        .. 'act(btn.dataset.action==="stop"?URL_STOP:URL_START);});'
        .. 'refresh();'
        .. '})();'
        .. '</script>'
    return html
end

-- [存储路径]
o = s:option(DummyValue, "_sep_storage", "")
o.rawhtml = true
o.cfgvalue = function() return '<h3>' .. translate("存储路径") .. '</h3>' end

-- 当前存储位置
o = s:option(DummyValue, "_current_path", translate("当前存储位置"))
o.rmempty = true
o.rawhtml = true
o.cfgvalue = function(self, section)
    local cp = uci:get("model-gateway", "settings", "config_path")
    if cp and cp ~= "" then
        -- 检查外部配置文件是否真实存在：盘没挂载/文件丢失时 init.d 会静默回退到默认路径，
        -- 此处如实反映"实际运行路径"，避免显示外部路径却跑在默认路径的误导
        local exists = (sys.call("test -s '" .. cp .. "/model-gateway' >/dev/null 2>&1") == 0)
        if exists then
            return util.pcdata(cp .. "/model-gateway")
        else
            return '<span style="color:#c00;">&#9888; ' ..
                util.pcdata(cp .. "/model-gateway") ..
                translate("（外部存储不可用，当前实际运行于 /etc/config/model-gateway）") ..
                '</span>'
        end
    end
    return "/etc/config/model-gateway"
end

-- 上次路径操作结果反馈（修复"不知道有没有成功修改"）
o = s:option(DummyValue, "_last_path_msg", translate("上次路径操作"))
o.rmempty = true
o.rawhtml = true
o.cfgvalue = function(self, section)
    local msg = uci:get("model-gateway", "settings", "last_path_msg")
    if msg and msg ~= "" then
        return '<span style="color:#2e7d32;">' .. util.pcdata(msg) .. '</span>'
    end
    return '<span style="color:#aaa;">—</span>'
end

-- 动态枚举外部挂载点（自动识别真实盘名）
local current_cp = get_config_path()
local mounts = list_external_mounts()

-- 配置路径选择
o = s:option(ListValue, "path_selector", translate("配置文件路径"),
    translate("配置和数据的存储位置。外部存储可避免占用路由器内置闪存空间，也便于备份迁移。当前检测到 " .. tostring(#mounts) .. " 个外部存储。"))
o:value("default", translate("默认路径（/etc/config/model-gateway）"))
for _, mp in ipairs(mounts) do
    o:value(mp, translate("外部存储（" .. mp .. "/Configs/model-gateway）"))
end
o:value("custom", translate("自定义路径"))
local function pick_selector_default()
    if current_cp == "" then return "default" end
    for _, mp in ipairs(mounts) do
        if current_cp == mp .. "/Configs/model-gateway" then return mp end
    end
    return "custom"
end
o.default = pick_selector_default()
o.rmempty = false

-- 解析阶段捕获用户实际选择（ListValue 校验通过才会调用 write），
-- on_after_commit 优先使用，不再依赖事后 uci get 读取，彻底避免“读空/回退默认”。
local orig_ps_write = o.write
function o:write(section, value)
    selected_selector = value
    return orig_ps_write(self, section, value)
end

-- 自定义路径
o = s:option(Value, "custom_path", translate("自定义路径"),
    translate("输入完整的目录路径，配置文件将保存为该目录下的 model-gateway 文件。"))
o.rmempty = true
o:depends("path_selector", "custom")
o.placeholder = "/mnt/sda1/Configs/model-gateway"
local is_known = (current_cp == "")
for _, mp in ipairs(mounts) do
    if current_cp == mp .. "/Configs/model-gateway" then is_known = true end
end
if current_cp ~= "" and not is_known then
    o.default = current_cp
end

-- 解析阶段捕获用户填写的自定义路径（自由输入，始终合法）
local orig_cp_write = o.write
function o:write(section, value)
    selected_custom = value
    return orig_cp_write(self, section, value)
end

-- [基本设置]
o = s:option(DummyValue, "_sep_settings", "")
o.rawhtml = true
o.cfgvalue = function() return '<h3>' .. translate("基本设置") .. '</h3>' end

-- 模型网关URL
o = s:option(DummyValue, "_api_url", translate("模型网关URL"))
o.rmempty = true
o.cfgvalue = function(self, section)
    local port = uci:get("model-gateway", "settings", "port") or "8080"
    local ip = util.trim(util.exec("uci get network.lan.ipaddr 2>/dev/null || echo 192.168.100.1"))
    return "http://" .. ip .. ":" .. port .. "/v1"
end

-- 模型网关KEY
o = s:option(Value, "admin_key", translate("模型网关KEY"))
o.rmempty = true
o.password = true

-- 模型网关端口
o = s:option(Value, "port", translate("模型网关端口"))
o.rmempty = false
o.datatype = "port"
o.default = "12211"

-- ========== 保存后处理 ==========

-- 应用服务启停（通过 shell 读取 UCI，确保读到 commit 后的最新值）
-- 采用 iStoreOS 标准闭环：enable/disable 控制开机自启（S 软链），restart/stop 控制即时运行态。
-- 用 restart（procd 原生的“停+起”原子操作）替代 stop+sleep+start，避免与 init.d 的 file 监视在 uci apply 时抢跑。
local function apply_service()
    local enable_val = shell_uci_get("model-gateway.settings.enable")
    sys.call("logger -t model-gateway 'apply_service: enable=" .. enable_val .. "'")
    if enable_val == "1" then
        sys.call("/etc/init.d/model-gateway enable >/dev/null 2>&1")
        sys.call("/etc/init.d/model-gateway restart >/dev/null 2>&1")
        -- 兜底：restart 后仍未来起，强制 start 一次
        if sys.call("pidof model-gatewayd model-gatewayd-arm64 >/dev/null 2>&1") ~= 0 then
            sys.call("/etc/init.d/model-gateway start >/dev/null 2>&1")
        end
    else
        sys.call("/etc/init.d/model-gateway disable >/dev/null 2>&1")
        sys.call("/etc/init.d/model-gateway stop >/dev/null 2>&1")
    end
end

m.on_after_commit = function(self)
    -- 优先使用“解析阶段已捕获”的用户选择（write 钩子赋值），避免 ListValue 严格校验或
    -- 读-写竞态导致读空；捕获失败再回退到 shell uci get（已 commit 后的值）。
    local selector = selected_selector or shell_uci_get("model-gateway.settings.path_selector")
    if selector == "" or selector == nil then selector = "default" end
    local custom = selected_custom or shell_uci_get("model-gateway.settings.custom_path")

    -- 本次未提交任何路径选择（下拉/自定义均为空）：说明只改了其它字段（如 KEY/端口），
    -- 保持现有 config_path 不变，既不迁移也不删除，避免误清外部存储配置。
    if (selector == nil or selector == "") and (custom == nil or custom == "") then
        sys.call("logger -t model-gateway 'on_after_commit: 未改动存储路径，保持现状'")
        self.message = "配置已保存（存储路径未变动）"
        apply_service()
        return
    end

    sys.call("logger -t model-gateway 'on_after_commit: selector=" .. selector .. " custom=" .. custom .. "'")

    -- 校验：选择了自定义路径但未填写
    if selector == "custom" and custom == "" then
        sys.call("logger -t model-gateway '自定义路径为空，跳过路径迁移'")
        shell_uci_batch({
            "delete model-gateway.settings.path_selector",
            "delete model-gateway.settings.custom_path",
            "delete model-gateway.settings.last_path_msg",
            "commit model-gateway"
        })
        -- 即使跳过路径迁移，也要应用“启用服务”开关，否则勾选框不生效
        apply_service()
        return
    end

    local new_path = resolve_path(selector, custom)
    local ok, msg = true, ""
    local migrated = false

    if new_path ~= "" then
        ok, msg = migrate_storage(new_path)
        if ok then
            migrated = true
            msg = "配置路径已修改为：" .. new_path .. "/model-gateway"
            sys.call("logger -t model-gateway '路径迁移成功: " .. new_path .. "'")
        else
            sys.call("logger -t model-gateway '路径迁移失败: " .. tostring(msg) .. "'")
        end
    else
        -- 切回默认路径
        local current_cp_val = get_config_path()
        if current_cp_val ~= "" then
            -- 仅解除外部路径引用（下方 delete config_path 执行），不再把外部数据复制回内置闪存，
            -- 外部盘上的 model-gateway 与 data 目录原样保留，用户切回外部后仍可继续使用。
            sys.call("logger -t model-gateway '切回默认路径，解除外部引用（外部数据保留）'")
        else
            sys.call("logger -t model-gateway '已在默认路径，无需迁移'")
        end
        ok = true
        msg = "已切换回默认路径 /etc/config/model-gateway"
    end

    -- ===== 所有 UCI 写操作合并为单次 uci batch =====
    -- 关键：set/delete/commit 必须在同一 uci 进程内，否则 set 的内存态在进程退出时丢失，
    -- commit 只会写回旧磁盘内容，导致 config_path 永远残留。
    -- 仅当“迁移成功”或“切回默认”才改 config_path；迁移失败则保留原值（既不 set 也不 delete），
    -- 避免“界面显示外部、服务却跑默认路径”的误导。
    local batch_cmds = {}
    if new_path ~= "" and ok then
        table.insert(batch_cmds, "set model-gateway.settings.config_path='" .. new_path .. "'")
    elseif new_path == "" then
        table.insert(batch_cmds, "delete model-gateway.settings.config_path")
    else
        sys.call("logger -t model-gateway '迁移失败，保留原配置路径不变'")
    end
    table.insert(batch_cmds, "delete model-gateway.settings.path_selector")
    table.insert(batch_cmds, "delete model-gateway.settings.custom_path")
    table.insert(batch_cmds, "commit model-gateway")
    shell_uci_batch(batch_cmds)

    sys.call("logger -t model-gateway 'uci batch commit 完成'")

    -- 同步配置到外部路径（仅当迁移成功且 config_path 已更新）
    local final_cp = shell_uci_get("model-gateway.settings.config_path")
    if final_cp ~= "" and ok and new_path ~= "" then
        sync_config_to_external(final_cp)
        sys.call("logger -t model-gateway '配置已同步到外部: " .. final_cp .. "'")
    end

    -- 界面反馈：写入 last_path_msg（页面顶部消息框 self.message + 持久化字段“上次路径操作”均可见）
    self.message = msg
    sys.call("uci set model-gateway.settings.last_path_msg='" .. msg .. "' 2>/dev/null")
    sys.call("uci commit model-gateway 2>/dev/null")

    -- 统一重启服务（在 commit 之后，确保读到最新值）
    apply_service()
end

return m
