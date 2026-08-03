
/* ============ 鉴权与 fetch 封装 ============ */
const TOKEN_KEY = 'gateway_admin_token';
let _adminKey = "";
function getToken() { return _adminKey; }
function setToken(t) {
  _adminKey = t || "";
  try { if (t) localStorage.setItem(TOKEN_KEY, t); else localStorage.removeItem(TOKEN_KEY); } catch (e) {}
}
function clearToken() {
  _adminKey = "";
  try { localStorage.removeItem(TOKEN_KEY); } catch (e) {}
}
function loadSavedToken() {
  try { return localStorage.getItem(TOKEN_KEY) || ""; } catch (e) { return ""; }
}

/* P0 安全修复：后端不再向匿名请求返回完整 admin_key（旧版靠可伪造的 Origin 头判同源）。
   面板首次访问弹出密钥门，验证通过后写入 localStorage，后续自动免登录。 */
function showGate() {
  let g = document.getElementById('authGate');
  if (!g) {
    g = document.createElement('div');
    g.id = 'authGate';
    g.style.cssText = 'position:fixed;inset:0;z-index:99999;background:rgba(0,0,0,.55);display:flex;align-items:center;justify-content:center;backdrop-filter:blur(3px);';
    g.innerHTML = '<div style="background:var(--bg-card,#1f2430);color:var(--text-primary,#e8eaf0);border:1px solid rgba(128,128,128,.25);border-radius:14px;padding:28px 26px;width:380px;max-width:92vw;box-shadow:0 12px 48px rgba(0,0,0,.45);">'
      + '<div style="font-size:17px;font-weight:600;margin-bottom:6px;">\uD83D\uDD10 管理密钥验证</div>'
      + '<div style="font-size:12px;opacity:.75;line-height:1.7;margin-bottom:14px;">请输入网关管理密钥（admin_key）。忘记了可在路由器终端执行：<br><code style="font-size:11px;">uci get model-gateway.settings.admin_key</code></div>'
      + '<input id="gateKeyInput" type="password" placeholder="sk-..." autocomplete="off" style="width:100%;box-sizing:border-box;padding:9px 10px;border-radius:8px;border:1px solid rgba(128,128,128,.35);background:transparent;color:inherit;font-size:13px;" onkeydown="if(event.key===\'Enter\')submitGate()">'
      + '<div id="gateError" style="color:#e5534b;font-size:12px;min-height:16px;margin-top:8px;"></div>'
      + '<button onclick="submitGate()" style="width:100%;margin-top:6px;padding:9px 0;border:none;border-radius:8px;background:var(--accent,#4f8cff);color:#fff;font-size:14px;cursor:pointer;">进入面板</button>'
      + '</div>';
    document.body.appendChild(g);
  }
  g.style.display = 'flex';
  const inp = document.getElementById('gateKeyInput');
  if (inp) { inp.value = ''; setTimeout(function () { inp.focus(); }, 50); }
}
function hideGate() {
  const g = document.getElementById('authGate');
  if (g) g.style.display = 'none';
}
async function submitGate() {
  const inp = document.getElementById('gateKeyInput');
  const err = document.getElementById('gateError');
  const key = ((inp && inp.value) || '').trim();
  if (!key) { if (err) err.textContent = '请输入密钥'; return; }
  if (err) err.textContent = '';
  try {
    const r = await fetch('/api/config', { headers: { 'Authorization': 'Bearer ' + key } });
    const d = await r.json();
    if (d && d.admin_key === key) {
      setToken(key);
      hideGate();
      const keyEl = document.getElementById('localApiKey');
      if (keyEl) keyEl.textContent = key;
      if (!window._appStarted) { window._appStarted = true; startApp(); }
    } else {
      if (err) err.textContent = '密钥不正确';
    }
  } catch (e) {
    if (err) err.textContent = '验证失败：' + (e && e.message || e);
  }
}

async function apiFetch(url, options = {}) {
  const token = getToken();
  if (token) {
    options.headers = Object.assign({}, options.headers || {}, { 'Authorization': 'Bearer ' + token });
  }
  if (options.body && typeof options.body === 'string') {
    options.headers = Object.assign({}, options.headers || {}, { 'Content-Type': 'application/json' });
  }
  // 30s 超时保护：后端若崩溃/挂起，前端不再永久卡在“加载中…”，
  // 而是抛出“请求超时”，由调用方显示明确的失败提示（如“加载预设失败：请求超时”）。
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), 30000);
  try {
    const r = await fetch(url, Object.assign({ signal: ctrl.signal }, options));
    if (r.status === 401) {
      clearToken();
      showGate();
      throw new Error('Unauthorized');
    }
    return r;
  } catch (e) {
    if (e && e.name === 'AbortError') {
      throw new Error('请求超时（30s）');
    }
    throw e;
  } finally {
    clearTimeout(timer);
  }
}

/* ============ Toast 提示 ============ */
let _toastTimer = null;
function showToast(msg, type = 'ok', duration = 3000) {
  const el = document.getElementById('toast');
  if (!el) return;
  el.textContent = msg;
  el.className = 'toast show ' + type;
  if (_toastTimer) clearTimeout(_toastTimer);
  _toastTimer = setTimeout(() => { el.className = 'toast ' + type; }, duration);
}

/* ============ 主题 ============ */
function toggleTheme() {
  const isDark = document.documentElement.getAttribute('data-theme') === 'dark';
  if (isDark) {
    document.documentElement.removeAttribute('data-theme');
    document.getElementById('themeBtn').innerHTML = '🌙';
    localStorage.setItem('theme', 'light');
  } else {
    document.documentElement.setAttribute('data-theme', 'dark');
    document.getElementById('themeBtn').innerHTML = '☀️';
    localStorage.setItem('theme', 'dark');
  }
}
// U4 修复：页面加载时按当前主题渲染正确图标（深色模式不再错显 🌙）
function syncThemeIcon() {
  const tb = document.getElementById('themeBtn');
  if (tb) tb.innerHTML = (document.documentElement.getAttribute('data-theme') === 'dark') ? '☀️' : '🌙';
}
document.addEventListener('DOMContentLoaded', function () {
  syncThemeIcon();
  // U8 修复：为装饰性关闭按钮补充 aria-label（无障碍）
  document.querySelectorAll('.mac-btn.close').forEach(function (b) { if (!b.getAttribute('aria-label')) b.setAttribute('aria-label', '关闭'); });
});
// U8 修复：Esc 关闭任意已打开的 modal
document.addEventListener('keydown', function (e) {
  if (e.key === 'Escape') {
    document.querySelectorAll('.modal-overlay.show, .modal.show').forEach(function (m) { m.classList.remove('show'); });
  }
});

/* ============ 数据 ============ */
let data = [];
let stabilityData = [];
let modelDetails = {};
let modelCatalog = {};   // id -> {tier, capabilities, price_in, price_out, family, virtual, strategy}（来自 /v1/models 参考库注入）

function copyText(text, btn) {
  function showCopied() {
    if (!btn) return;
    const oldText = btn.innerText;
    btn.innerText = '已复制';
    btn.style.background = 'rgba(50, 215, 75, 0.2)';
    btn.style.color = '#32D74B';
    setTimeout(() => { btn.innerText = oldText; btn.style.background = ''; btn.style.color = ''; }, 2000);
  }
  function fallbackCopy() {
    // 局域网 HTTP 访问下 navigator.clipboard 不可用（非安全上下文），退回 execCommand
    try {
      const ta = document.createElement('textarea');
      ta.value = text;
      ta.style.position = 'fixed';
      ta.style.top = '-9999px';
      ta.style.opacity = '0';
      document.body.appendChild(ta);
      ta.focus();
      ta.select();
      const ok = document.execCommand('copy');
      document.body.removeChild(ta);
      if (ok) { showCopied(); } else { prompt('复制失败，请手动复制：', text); }
    } catch (e) {
      prompt('复制失败，请手动复制：', text);
    }
  }
  if (navigator.clipboard && window.isSecureContext) {
    navigator.clipboard.writeText(text).then(showCopied).catch(fallbackCopy);
  } else {
    fallbackCopy();
  }
}

let _detailsLoaded = false;
let _detailsLoading = false;
async function load(force) {
  try {
    // 模型详情改为后台并行加载，不再阻塞"上游提供商"区域渲染。
    // 此前 await loadModelDetails() 串行等待，若后端 /api/model-details
    // 因模型缓存刷新被短暂阻塞，提供商区域会空白 10~30 秒（bug #29）。
    if (!_detailsLoaded && !_detailsLoading) {
      _detailsLoading = true;
      loadModelDetails()
        .then(() => { _detailsLoaded = true; })
        .catch(() => {})
        .finally(() => { _detailsLoading = false; });
    }
    const r = await apiFetch('/api/providers');
    const list = await r.json();
    data = [];
    window.modelEnabledMap = {};

    let providersHtml = '';
    let providerOptionsHtml = '<option value="all">全部提供商</option>';
    list.forEach(p => {
      providerOptionsHtml += `<option value="${p.name}">${p.name}</option>`;
      providersHtml += `
        <div class="provider-card">
          <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 12px; gap: 6px; flex-wrap: wrap;">
            <div class="p-name" style="margin-bottom: 0;">${p.name} Configuration</div>
            <div style="display: flex; gap: 6px; flex-shrink: 0;">
              <button class="btn" style="padding: 4px 8px; font-size: 11px;" onclick="openModelManager('${p.name}')">⚙️ 模型</button>
              <button class="btn" style="padding: 4px 8px; font-size: 11px;" onclick="openProviderEditor('${p.name}')">✏️ 编辑</button>
              <button class="btn" style="padding: 4px 8px; font-size: 11px; color: var(--danger);" onclick="deleteProvider('${p.name}')">🗑 删除</button>
            </div>
          </div>
          <div class="copy-row">
            <span>Base URL:</span>
            <code>${p.base_url}</code>
            <button class="btn-copy" onclick="copyText('${p.base_url}', this)">Copy</button>
          </div>
          <div class="copy-row">
            <span>API Key:</span>
            <code>${p.api_key_masked || '****'}</code>
          </div>
        </div>
      `;
      (p.models || []).forEach(m => {
        window.modelEnabledMap[`${p.name}||${m}`] = !(p.disabled_models || []).includes(m);
        const h = p.health?.[m] || { status: 'unknown' };
        const d = modelDetails[m] || {};
        const ctxLen = d.context_length || d.max_model_len || '--';
        data.push({
          provider: p.name, model: m, status: h.status || 'unknown',
          latency_ms: h.latency_ms ?? null, code: h.code ?? null,
          context_length: ctxLen
        });
      });
    });

    document.getElementById('providersSection').innerHTML = providersHtml;

    const providerSelect = document.getElementById('providerFilter');
    const currentVal = providerSelect.value;
    providerSelect.innerHTML = providerOptionsHtml;
    if ([...providerSelect.options].some(o => o.value === currentVal)) {
      providerSelect.value = currentVal;
    }

    loadStability(force);
    loadUsage();
  } catch (e) {
    if (e.message !== 'Unauthorized') console.error("Failed to load data:", e);
  }
}

async function loadStability(force) {
  const hours = document.getElementById('stabilityHours').value;
  try {
    const r = await apiFetch(`/api/stability?hours=${hours}${force ? '&refresh=1' : ''}`);
    stabilityData = await r.json();
    renderStability();
  } catch (e) {
    if (e.message !== 'Unauthorized') console.error("Failed to load stability:", e);
  }
}

let statsDays = 1;
async function loadUsage() {
  const box = document.getElementById('usageOverview');
  try {
    const r = await apiFetch(`/api/usage?days=${statsDays}`);
    const data = await r.json();
    renderUsage(data);
  } catch (e) {
    if (e.message !== 'Unauthorized') {
      console.error("Failed to load usage:", e);
      if (box) box.innerHTML = '<div class="empty" style="color:var(--danger)">消耗统计加载失败：' + escapeHtml(e.message) + ' <button class="btn" style="font-size:12px;padding:2px 8px;" onclick="loadUsage()">重试</button></div>';
    }
  }
}
function switchStatsTab(days) {
  statsDays = days;
  document.querySelectorAll('.stats-tab').forEach(b => {
    const on = parseInt(b.dataset.days, 10) === days;
    b.classList.toggle('btn-primary', on);
  });
  loadUsage();
  loadCostDashboard();
}

async function loadCallLog() {
  try {
    const r = await apiFetch('/api/call-log');
    const data = await r.json();
    const tbody = document.getElementById('callLogBody');
    if (!data || !data.length) {
      tbody.innerHTML = '<tr><td colspan="4" class="empty">暂无调用记录</td></tr>';
      return;
    }
    tbody.innerHTML = data.slice().reverse().map(d => {
      const stIcon = d.status === 'ok' ? '✅' : '❌';
      const stColor = d.status === 'ok' ? 'var(--success)' : 'var(--danger)';
      const tokens = d.tokens ? fmtTokens(d.tokens) : '-';
      const err = d.error ? ` <span style="font-size:11px;color:${stColor};">(${d.error})</span>` : '';
      return `<tr>
        <td style="font-size:12px;color:var(--text-secondary);">${d.time}</td>
        <td><span style="font-size:16px;">${stIcon}</span>${err}</td>
        <td>
          <span class="model-provider-badge">${d.provider || '-'}</span>
          <span style="font-size:13px;">${d.model || '-'}</span>
        </td>
        <td style="font-size:12px;color:var(--text-secondary);font-variant-numeric:tabular-nums;">${tokens}</td>
      </tr>`;
    }).join('');
  } catch (e) {
    if (e.message !== 'Unauthorized') console.error("loadCallLog failed:", e);
  }
}

function fmtTokens(n) {
  if (n >= 10000) return (n / 10000).toFixed(1).replace(/\.0$/, '') + ' 万';
  return n.toLocaleString();
}
function fmtNum(n) {
  n = n || 0;
  if (n < 10000) return n.toLocaleString();
  if (n < 100000000) return (n / 10000).toFixed(1).replace(/\.0$/, '') + ' 万';
  return (n / 100000000).toFixed(2).replace(/\.?0+$/, '') + ' 亿';
}
function renderUsage(data) {
  const t = data.total || {};
  const cards = [
    { label: '输入 Token', value: t.pt },
    { label: '输出 Token', value: t.ct },
    { label: '合计 Token', value: t.tt },
    { label: '请求数', value: t.requests },
  ];
  document.getElementById('usageOverview').innerHTML = cards.map(c =>
    `<div class="provider-card" style="padding: 14px 16px;">
       <div style="font-size: 12px; color: var(--text-secondary);">${c.label}</div>
       <div style="font-size: 22px; font-weight: 600; color: var(--text-primary); margin-top: 4px;">${fmtNum(c.value)}</div>
     </div>`
  ).join('');
  const rows = data.by_model || [];
  document.getElementById('usageBody').innerHTML = rows.length
    ? rows.map(r => `<tr>
        <td>
          <div class="model-name" style="display: flex; align-items: center;">
            <span class="model-provider-badge">${r.provider || 'unknown'}</span>
            <span>${r.model || 'unknown'}</span>
          </div>
        </td>
        <td>${fmtNum(r.requests)}</td>
        <td>${fmtNum(r.pt)}</td>
        <td>${fmtNum(r.ct)}</td>
        <td>${fmtNum(r.tt)}</td>
      </tr>`).join('')
    : '<tr><td colspan="5" class="empty">暂无消耗记录</td></tr>';
}

// 隐藏失败模型开关切换：记忆到 localStorage 并重渲染
function onHideFailedChange() {
  const el = document.getElementById('hideFailedToggle');
  if (el) {
    try { localStorage.setItem('hideFailedModels_v2', el.checked ? '1' : '0'); } catch (e) {}
  }
  renderStability();
}
// 启动时恢复开关状态（默认打开）。
// 键名带 _v2：旧键 hideFailedModels 可能被历史版本写成 '0'，
// 换代后所有安装（含升级用户）首次进入一律回到默认"打开"，之后再记忆用户选择。
function restoreHideFailedToggle() {
  const el = document.getElementById('hideFailedToggle');
  if (!el) return;
  el.checked = true; // 默认打开（不依赖 HTML 属性，防浏览器表单状态恢复干扰）
  try {
    const saved = localStorage.getItem('hideFailedModels_v2');
    if (saved !== null) el.checked = saved === '1';
  } catch (e) {}
}

function relTime(ts) {
  const diff = Math.floor(Date.now() / 1000) - ts;
  if (diff < 0) diff = 0;
  if (diff < 60) return '刚刚';
  if (diff < 3600) return Math.floor(diff / 60) + '分钟前';
  const d = new Date(ts * 1000);
  const now = new Date();
  const pad = n => (n < 10 ? '0' : '') + n;
  const sameDay = d.getFullYear() === now.getFullYear() && d.getMonth() === now.getMonth() && d.getDate() === now.getDate();
  if (sameDay) return '今天 ' + pad(d.getHours()) + ':' + pad(d.getMinutes());
  const y = new Date(now); y.setDate(now.getDate() - 1);
  const isYest = d.getFullYear() === y.getFullYear() && d.getMonth() === y.getMonth() && d.getDate() === y.getDate();
  if (isYest) return '昨天 ' + pad(d.getHours()) + ':' + pad(d.getMinutes());
  return (d.getMonth() + 1) + '月' + d.getDate() + '日';
}

function inspectStatusHtml(d) {
  const map = {
    ok:      { dot: 'var(--success)',        text: '正常',   pulse: false },
    fail:    { dot: 'var(--danger)',         text: '异常',   pulse: true },
    error:   { dot: 'var(--danger)',         text: '异常',   pulse: true },
    pending: { dot: 'var(--text-secondary)', text: '未检测', pulse: false },
    unknown: { dot: 'var(--text-secondary)', text: '未知',   pulse: false },
  };
  const s = map[d.last_status] || map.unknown;
  let rel;
  if (d.last_check_at && d.last_check_at > 0) {
    rel = relTime(d.last_check_at);
  } else if (d.last_status === 'pending') {
    rel = '等待巡检';
  } else {
    rel = '—';
  }
  return `<span style="display:inline-flex;align-items:center;gap:6px;">
    <span style="display:inline-block;width:8px;height:8px;border-radius:50%;background:${s.dot};${s.pulse ? 'animation:pulse 2s infinite;' : ''}"></span>
    <span style="color:${s.dot};font-weight:600;">${s.text}</span>
    <span style="color:var(--text-secondary);font-size:12px;">${rel}</span>
  </span>`;
}

function renderStability() {
  if (!window.hiddenCols) loadColVisibility();
  initTableFeatures();
  const filter = document.getElementById('stableFilter').value;
  const search = document.getElementById('stableSearch').value.toLowerCase();
  const provider = document.getElementById('providerFilter').value;
  const tierFilter = document.getElementById('tierFilter').value;
  const capFilter = document.getElementById('capFilter').value;
  const hideFailedEl = document.getElementById('hideFailedToggle');
  const hideFailed = hideFailedEl ? hideFailedEl.checked : true;

  let filtered = stabilityData.filter(d => {
    if (provider !== 'all' && d.provider !== provider) return false;
    if (search && !d.model.toLowerCase().includes(search)) return false;
    // 隐藏失败模型：最近一次巡检失败（红点）的模型不显示
    if (hideFailed && (d.last_status === 'fail' || d.last_status === 'error')) return false;
    if (filter === 'high') return d.availability >= 90;
    if (filter === 'mid') return d.availability >= 50 && d.availability < 90;
    if (filter === 'low') return d.availability < 50;
    // 档位 / 能力分类筛选（基于参考库注入的 tier / capabilities）
    const ci = modelCatalog[d.model] || {};
    const tier = ci.tier || 'none';
    const caps = ci.capabilities || [];
    if (tierFilter !== 'all' && tierFilter !== tier) return false;
    if (capFilter !== 'all') {
      const capsLower = (Array.isArray(caps) ? caps : []).map(c => String(c).toLowerCase());
      if (!capsLower.includes(capFilter)) return false;
    }
    return true;
  });

  const tbody = document.getElementById('stableBody');
  if (!filtered.length) {
    const hint = hideFailed
      ? '该时间范围内暂无巡检记录。<br><span style="font-size:12px;color:var(--text-secondary)">（已开启「隐藏失败模型」，如需查看失败模型请关闭右侧开关；新添加的平台请稍候片刻，巡检完成后自动显示）</span>'
      : '该时间范围内暂无巡检记录。<br><span style="font-size:12px;color:var(--text-secondary)">（新添加的平台请稍候片刻，巡检完成后自动显示）</span>';
    tbody.innerHTML = `<tr><td colspan="10" class="empty">${hint}</td></tr>`;
    applyColVisibility();
    return;
  }

  tbody.innerHTML = filtered.map(d => {
    const isPending = d.last_status === 'pending';
    const pctNum = typeof d.availability === 'number' ? d.availability : parseFloat(d.availability || 0);
    const pct = isPending ? '—' : pctNum.toFixed(1);
    const barColor = isPending ? 'var(--text-secondary)' : (pctNum >= 90 ? 'var(--success)' : pctNum >= 50 ? 'var(--warning)' : 'var(--danger)');
    const avgL = d.avg_latency_ms !== null ? Math.round(d.avg_latency_ms) + ' ms' : '--';
    const minL = d.min_latency_ms !== null ? Math.round(d.min_latency_ms) + ' ms' : '--';
    const maxL = d.max_latency_ms !== null ? Math.round(d.max_latency_ms) + ' ms' : '--';

    const detail = modelDetails[d.model] || {};
    const ctxLen = detail.context_length || detail.max_model_len || '--';
    const descHtml = detail.desc ? `<div class="model-desc">${escapeHtml(detail.desc)}</div>` : '';

    // 参考库分类信息（tier / capabilities / 价格）
    const ci = modelCatalog[d.model] || {};
    const tier = ci.tier || 'mid';
    const tierLabel = { lite: '轻量', mid: '均衡', top: '旗舰' }[tier] || tier;
    const tierBadge = `<span class="model-tier-badge ${tier || 'none'}">${tierLabel}</span>`;
    const capLabelMap = { text: '文本', vision: '视觉', reasoning: '推理', image: '图像', audio: '音频', embedding: '嵌入' };
    const caps = Array.isArray(ci.capabilities) ? ci.capabilities : [];
    const capBadges = caps.map(c => `<span class="model-cap-badge">${capLabelMap[String(c).toLowerCase()] || escapeHtml(c)}</span>`).join('');
    const priceIn = (ci.price_in !== undefined && ci.price_in !== null) ? ci.price_in : null;
    const priceOut = (ci.price_out !== undefined && ci.price_out !== null) ? ci.price_out : null;
    const priceHtml = (priceIn !== null || priceOut !== null)
      ? `<span class="model-price">in <b>${priceIn !== null ? '¥' + priceIn : '-'}</b> · out <b>${priceOut !== null ? '¥' + priceOut : '-'}</b></span>`
      : '<span class="model-price">--</span>';

    let lastStatusHtml = '';
    if (d.last_status === 'ok') {
      lastStatusHtml = `<span title="上次巡检：正常" style="display:inline-block;width:8px;height:8px;border-radius:50%;background:var(--success);margin-left:6px;"></span>`;
    } else if (d.last_status === 'fail' || d.last_status === 'error') {
      lastStatusHtml = `<span title="上次巡检：异常 (通常为提供商限流或报错)" style="display:inline-block;width:8px;height:8px;border-radius:50%;background:var(--danger);margin-left:6px;animation: pulse 2s infinite;"></span>`;
    } else if (d.last_status === 'pending') {
      lastStatusHtml = `<span title="还没检测到（等待巡检）" style="display:inline-block;width:8px;height:8px;border-radius:50%;background:var(--text-secondary);margin-left:6px;"></span>`;
    } else {
      lastStatusHtml = `<span title="上次巡检：等待中..." style="display:inline-block;width:8px;height:8px;border-radius:50%;background:var(--text-secondary);margin-left:6px;"></span>`;
    }

    const enKey = `${d.provider}||${d.model}`;
    const isEnabled = window.modelEnabledMap[enKey] !== false;
    const toggleBtn = `<button class="model-toggle-btn ${isEnabled ? 'enabled' : 'disabled'}" data-provider="${escAttr(d.provider)}" data-model="${escAttr(d.model)}" onclick="toggleModelFromBtn(this)">${isEnabled ? '● 启用' : '○ 停用'}</button>`;

    return `<tr class="${isEnabled ? '' : 'model-disabled-row'}">
      <td class="model-name-cell">
        <div class="model-name" style="display: flex; align-items: center;">
          <span class="model-provider-badge">${escapeHtml(d.provider)}</span>
          <span>${escapeHtml(d.model)}</span>
          ${lastStatusHtml}
        </div>
        ${descHtml}
      </td>
      <td>
        ${tierBadge}${capBadges}
      </td>
      <td>
        <span class="avail-text" style="color:${barColor}">${isPending ? '⚪ 未检测' : pct + '%'}</span>
      </td>
      <td class="inspect-cell">
        ${inspectStatusHtml(d)}
      </td>
      <td><span class="ctx-cell" data-model="${d.model}" title="点击修改上下文长度" style="cursor:pointer;color:var(--text-secondary);font-variant-numeric:tabular-nums;font-family:ui-monospace,monospace;font-size:12px;">${ctxLen === '--' ? '--' : ctxLen}</span></td>
      <td>${priceHtml}</td>
      <td style="font-variant-numeric:tabular-nums; font-size:12px;">
        ${isPending ? '<span style="color:var(--text-secondary)">— / —</span>' : `<span style="color:var(--success);font-weight:600">${d.ok}</span><span style="color:var(--text-secondary);margin:0 4px">/</span><span style="color:var(--danger);font-weight:600">${d.fail}</span>`}
      </td>
      <td class="latency ${d.avg_latency_ms && d.avg_latency_ms < 3000 ? 'fast' : d.avg_latency_ms && d.avg_latency_ms < 8000 ? 'medium' : ''}">${avgL}</td>
      <td class="latency" style="font-size:12px;">
        <span style="color:var(--success)">${minL}</span>
        <span style="color:var(--text-secondary);margin:0 4px">~</span>
        <span style="color:var(--text-secondary)">${maxL}</span>
      </td>
      <td>${toggleBtn}</td>
    </tr>`;
  }).join('');
  applyColVisibility();
}

/* ---------- 列显隐设置（齿轮按钮） ---------- */
function loadColVisibility() {
  if (window.hiddenCols) return;
  try {
    const arr = JSON.parse(localStorage.getItem('mg_hidden_cols') || '[]');
    window.hiddenCols = new Set(Array.isArray(arr) ? arr : []);
  } catch (e) {
    window.hiddenCols = new Set();
  }
}

function saveColVisibility() {
  try { localStorage.setItem('mg_hidden_cols', JSON.stringify(Array.from(window.hiddenCols || []))); } catch (e) {}
}

function applyColVisibility() {
  const table = document.getElementById('stabilityTable');
  if (!table) return;
  const headRow = table.tHead && table.tHead.rows[0];
  if (headRow) {
    for (let i = 0; i < headRow.cells.length; i++) {
      headRow.cells[i].style.display = (window.hiddenCols && window.hiddenCols.has(i)) ? 'none' : '';
    }
  }
  const body = table.tBodies[0];
  if (!body) return;
  for (const row of body.rows) {
    if (row.cells.length < headRow.cells.length) continue; // 空状态行跳过
    for (let i = 0; i < row.cells.length; i++) {
      if (row.cells[i]) row.cells[i].style.display = (window.hiddenCols && window.hiddenCols.has(i)) ? 'none' : '';
    }
  }
}

/* 列宽拖拽调整 + 默认宽度应用（与列显隐按索引隐藏互不冲突，这里按列名存储） */
const COL_DEFAULT_WIDTHS = {
  '分类 / 能力': 170, 'SLA 可用率': 95, '巡检状态': 150, '上下文窗口': 120,
  '价格 / 百万 tokens': 165, '探测累计': 120, '平均延迟': 110, '极速 / 最慢': 130, '操作': 95
};
function initTableFeatures() {
  if (window._tableFeaturesInited) return;
  window._tableFeaturesInited = true;
  applyColWidths();
  initColResize();
}
function applyColWidths() {
  let store;
  try { store = JSON.parse(localStorage.getItem('mg_col_widths') || '{}'); } catch (e) { store = {}; }
  const table = document.getElementById('stabilityTable');
  if (!table || !table.tHead) return;
  const headRow = table.tHead.rows[0];
  for (let i = 0; i < headRow.cells.length; i++) {
    const th = headRow.cells[i];
    const col = th.getAttribute('data-col');
    if (!col || col === '模型名称') continue; // 模型名称列保持弹性，吸收剩余宽度
    const w = store[col] || COL_DEFAULT_WIDTHS[col];
    if (w) th.style.width = w + 'px';
  }
}
function initColResize() {
  const table = document.getElementById('stabilityTable');
  if (!table) return;
  const resizers = table.querySelectorAll('th .col-resizer');
  resizers.forEach(function (r) {
    r.addEventListener('mousedown', function (e) {
      e.preventDefault();
      e.stopPropagation();
      const th = r.closest('th');
      const colName = th.getAttribute('data-col');
      const isFlex = (colName === '模型名称'); // 弹性列：吸收剩余宽度，不能存固定宽
      const startX = e.clientX;
      let targetTh, startTargetW;
      if (isFlex) {
        // 拖动弹性列右边界 = 反向调整下一列（弹性列始终吸收差额，总宽保持 100%）
        targetTh = nextVisibleTh(table, th);
        if (!targetTh) return;
        startTargetW = targetTh.getBoundingClientRect().width;
      } else {
        targetTh = th;
        startTargetW = th.getBoundingClientRect().width;
      }
      r.classList.add('active');
      function onMove(ev) {
        const delta = ev.clientX - startX;
        // flex 列：反向调整下一列；普通列：直接调整自身（弹性列吸收差额）
        let w = isFlex ? (startTargetW - delta) : (startTargetW + delta);
        if (w < 60) w = 60;
        targetTh.style.width = w + 'px';
      }
      function onUp() {
        r.classList.remove('active');
        document.removeEventListener('mousemove', onMove);
        document.removeEventListener('mouseup', onUp);
        saveColWidths();
      }
      document.addEventListener('mousemove', onMove);
      document.addEventListener('mouseup', onUp);
    });
  });
}
function nextVisibleTh(table, th) {
  const row = table.tHead.rows[0];
  const cells = row.cells;
  for (let i = 0; i < cells.length; i++) {
    if (cells[i] === th) {
      for (let j = i + 1; j < cells.length; j++) {
        if (cells[j].style.display !== 'none') return cells[j];
      }
      return null;
    }
  }
  return null;
}
function saveColWidths() {
  const table = document.getElementById('stabilityTable');
  if (!table || !table.tHead) return;
  const headRow = table.tHead.rows[0];
  const map = {};
  for (let i = 0; i < headRow.cells.length; i++) {
    const th = headRow.cells[i];
    const col = th.getAttribute('data-col');
    if (!col || col === '模型名称') continue;
    if (th.style.display === 'none') continue;
    map[col] = Math.round(th.getBoundingClientRect().width);
  }
  try { localStorage.setItem('mg_col_widths', JSON.stringify(map)); } catch (e) {}
}

function buildColPanel() {
  loadColVisibility();
  const table = document.getElementById('stabilityTable');
  const panel = document.getElementById('colPanel');
  if (!table || !panel) return;
  const headRow = table.tHead.rows[0];
  let html = '<h4>显示列</h4>';
  for (let i = 0; i < headRow.cells.length; i++) {
    const label = (headRow.cells[i].textContent || '').trim();
    const checked = window.hiddenCols.has(i) ? '' : 'checked';
    html += '<label class="col-item"><input type="checkbox" data-col="' + i + '" ' + checked + '> ' + escAttr(label) + '</label>';
  }
  html += '<div class="col-panel-actions"><button type="button" class="btn" onclick="resetColVisibility()">显示全部</button></div>';
  panel.innerHTML = html;
  panel.querySelectorAll('input[type=checkbox]').forEach(cb => {
    cb.addEventListener('change', function () {
      const idx = parseInt(cb.dataset.col, 10);
      if (cb.checked) window.hiddenCols.delete(idx); else window.hiddenCols.add(idx);
      saveColVisibility();
      applyColVisibility();
    });
  });
}

function toggleColPanel() {
  const panel = document.getElementById('colPanel');
  const btn = document.getElementById('colSettingsBtn');
  if (!panel || !btn) return;
  if (panel.style.display === 'block') {
    panel.style.display = 'none';
    return;
  }
  if (!window._colPanelBuilt) { buildColPanel(); window._colPanelBuilt = true; }
  const r = btn.getBoundingClientRect();
  panel.style.top = (r.bottom + 6) + 'px';
  panel.style.left = r.left + 'px';
  panel.style.display = 'block';
}

function resetColVisibility() {
  window.hiddenCols = new Set();
  saveColVisibility();
  buildColPanel();
  applyColVisibility();
  const panel = document.getElementById('colPanel');
  if (panel) panel.style.display = 'none';
}

// 点击面板外部自动关闭
document.addEventListener('click', function (e) {
  const panel = document.getElementById('colPanel');
  if (!panel || panel.style.display !== 'block') return;
  if (panel.contains(e.target)) return;
  if (e.target.closest && e.target.closest('#colSettingsBtn')) return;
  if (e.target.id === 'colSettingsBtn') return;
  panel.style.display = 'none';
});

function escAttr(s) {
  return String(s).replace(/&/g, '&amp;').replace(/"/g, '&quot;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

async function toggleModelFromBtn(btn) {
  const provider = btn.dataset.provider;
  const model = btn.dataset.model;
  const targetEnabled = btn.classList.contains('disabled'); // 当前显示“停用”→ 目标为启用
  const original = btn.innerText;
  btn.disabled = true;
  try {
    const res = await apiFetch(`/api/providers/${encodeURIComponent(provider)}/toggle-model`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ model: model, enabled: targetEnabled })
    });
    if (res.ok) {
      window.modelEnabledMap[`${provider}||${model}`] = targetEnabled;
      renderStability();
    } else {
      alert('切换启用状态失败');
    }
  } catch (e) {
    if (e.message !== 'Unauthorized') { console.error(e); alert('请求出错，请重试'); }
  } finally {
    btn.disabled = false;
  }
}

async function checkAll() {
  const btn = document.getElementById('checkAllBtn');
  const originalHtml = btn.innerHTML;
  btn.innerHTML = '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="animation: spin 1s linear infinite"><line x1="12" y1="2" x2="12" y2="6"></line><line x1="12" y1="18" x2="12" y2="22"></line><line x1="4.93" y1="4.93" x2="7.76" y2="7.76"></line><line x1="16.24" y1="16.24" x2="19.07" y2="19.07"></line><line x1="2" y1="12" x2="6" y2="12"></line><line x1="18" y1="12" x2="22" y2="12"></line><line x1="4.93" y1="19.07" x2="7.76" y2="16.24"></line><line x1="16.24" y1="7.76" x2="19.07" y2="4.93"></line></svg> 检测中...';
  btn.disabled = true;
  try {
    const r = await apiFetch('/api/check/all', { method: 'POST' });
    const results = await r.json();
    let ok = 0, fail = 0, locked = 0;
    const lockedNames = [];
    Object.entries(results).forEach(([k, v]) => {
      if (!v) return;
      if (v.status === 'ok') ok++; else fail++;
      if (v.locked) { locked++; if (lockedNames.length < 5) lockedNames.push(k); }
    });
    let msg = `检测完成：${ok} 正常 / ${fail} 异常`;
    if (locked > 0) msg += ` ｜ 🔒 ${locked} 个模型锁定（连续失败，巡检恢复后自动解锁）`;
    showToast(msg, fail > 0 || locked > 0 ? 'warn' : 'ok');
  } catch (e) {
    if (e.message !== 'Unauthorized') { console.error("Check failed", e); showToast("检测请求失败", "error"); }
  } finally {
    btn.innerHTML = originalHtml;
    btn.disabled = false;
    load(true);
  }
}

async function updatePollStatus() {
  try {
    const r = await apiFetch('/api/poll-status');
    const d = await r.json();
    // 同步策略选择器（后台可能被 LuCI 侧改动）
    if (d.poll_strategy) {
      const sel = document.getElementById('pollStrategySelect');
      if (sel && sel.value !== d.poll_strategy) sel.value = d.poll_strategy;
    }
    // 智能巡检模式徽标
    let badge = '';
    if (d.poll_strategy === 'limited') {
      if (d.poll_stage === 'idle') {
        badge = ` <span title="已完成密集巡检，进入休眠，每天中午自动复查一次" style="margin-left:8px;font-size:11px;font-weight:600;color:var(--success);background:rgba(52,199,89,0.12);padding:1px 8px;border-radius:10px;">🔋 智能巡检·休眠</span>`;
      } else if (typeof d.poll_count === 'number' && d.poll_max) {
        badge = ` <span title="智能巡检：巡检 ${d.poll_max} 次后休眠" style="margin-left:8px;font-size:11px;font-weight:600;color:var(--accent-primary);background:rgba(0,122,255,0.1);padding:1px 8px;border-radius:10px;">🔋 智能巡检 ${Math.min(d.poll_count, d.poll_max)}/${d.poll_max}</span>`;
      }
    } else if (d.poll_strategy === 'continuous') {
      badge = ` <span title="每 5 分钟持续巡检" style="margin-left:8px;font-size:11px;font-weight:600;color:var(--accent-primary);background:rgba(0,122,255,0.1);padding:1px 8px;border-radius:10px;">📡 持续监控</span>`;
    }
    if (d.last_poll_time > 0) {
      const date = new Date(d.last_poll_time * 1000);
      const pad = n => String(n).padStart(2, '0');
      const timeStr = `${date.getFullYear()}-${pad(date.getMonth()+1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
      document.getElementById('pollStatus').innerHTML = `上次同步：<span style="color:var(--text-primary);font-weight:500">${timeStr}</span> <span style="margin:0 8px;color:rgba(255,255,255,0.2)">|</span> 已扫描 <span style="color:var(--text-primary);font-weight:500">${d.total_models}</span> 个模型${badge}`;
    } else {
      document.getElementById('pollStatus').innerHTML = `初始化中… ${d.total_models} 个模型待扫描${badge}`;
    }
  } catch (e) {
    if (e.message !== 'Unauthorized') {}
  }
}

// 加载当前巡检策略到选择器
async function loadPollStrategy() {
  try {
    const r = await apiFetch('/api/poll-strategy');
    const d = await r.json();
    const sel = document.getElementById('pollStrategySelect');
    if (sel && d.strategy) sel.value = d.strategy;
  } catch (e) {
    if (e.message !== 'Unauthorized') {}
  }
}

// 切换巡检策略
async function setPollStrategy(strategy) {
  const sel = document.getElementById('pollStrategySelect');
  try {
    const r = await apiFetch('/api/poll-strategy', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ strategy })
    });
    if (!r.ok) throw new Error('save failed');
    showToast(strategy === 'limited' ? '已切换为「智能巡检」策略' : '已切换为「持续监控」策略', 'ok');
    updatePollStatus();
  } catch (e) {
    if (e.message !== 'Unauthorized') showToast('切换策略失败', 'error');
    // 失败时回读服务端真实值
    loadPollStrategy();
  }
}

// 展开/收起策略优缺点说明
function togglePollStrategyInfo(e) {
  if (e) e.stopPropagation();
  const box = document.getElementById('pollStrategyInfo');
  if (!box) return;
  const show = box.style.display === 'none';
  box.style.display = show ? 'block' : 'none';
  if (show) {
    setTimeout(() => {
      document.addEventListener('click', closePollStrategyInfoOnce);
    }, 0);
  }
}
function closePollStrategyInfoOnce(ev) {
  const box = document.getElementById('pollStrategyInfo');
  if (box && !box.contains(ev.target)) {
    box.style.display = 'none';
    document.removeEventListener('click', closePollStrategyInfoOnce);
  }
}

let currentManagingProvider = null;

async function openModelManager(provider) {
  currentManagingProvider = provider;
  document.getElementById('modalTitle').innerText = `${provider} - 模型管理`;
  document.getElementById('modalSearchInput').value = '';
  document.getElementById('modalModelList').innerHTML = '<div class="empty">正在获取远端可用模型列表...<br><br><svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="animation: spin 1s linear infinite"><line x1="12" y1="2" x2="12" y2="6"></line><line x1="12" y1="18" x2="12" y2="22"></line><line x1="4.93" y1="4.93" x2="7.76" y2="7.76"></line><line x1="16.24" y1="16.24" x2="19.07" y2="19.07"></line><line x1="2" y1="12" x2="6" y2="12"></line><line x1="18" y1="12" x2="22" y2="12"></line><line x1="4.93" y1="19.07" x2="7.76" y2="16.24"></line><line x1="16.24" y1="7.76" x2="19.07" y2="4.93"></line></svg></div>';
  document.getElementById('modalStatusText').innerText = '';
  document.getElementById('modelModal').classList.add('show');

  try {
    const rProviders = await apiFetch('/api/providers');
    const pList = await rProviders.json();
    const pData = pList.find(p => p.name === provider);
    const currentModels = new Set(pData ? (pData.models || []) : []);
    // 免费模型巡检按钮：仅在 free_only=true 时可用
    const btnCheckFree = document.getElementById('btnCheckFreeChanges');
    if (btnCheckFree) {
      btnCheckFree.style.display = (pData && pData.free_only) ? '' : 'none';
    }

    const rAvailable = await apiFetch(`/api/providers/${provider}/available-models`);
    const availableData = await rAvailable.json();
    if (!availableData.ok) throw new Error("API返回错误");

    const rawModels = availableData.models || [];
    // 兼容新旧格式：后端可能返回字符串数组（旧）或对象数组（新，含 is_free/pricing）
    const allModels = rawModels.map(m => typeof m === 'string' ? m : (m.id || m.model || '')).filter(Boolean);
    const freeModelSet = new Set();
    if (pData && pData.free_only) {
      rawModels.forEach(m => {
        if (typeof m === 'object' && m.is_free) {
          freeModelSet.add(m.id || m.model);
        }
      });
    }
    if (allModels.length === 0) {
      document.getElementById('modalModelList').innerHTML = '<div class="empty">该供应商未返回任何可用模型。</div>';
      return;
    }
    currentModels.forEach(m => { if (!allModels.includes(m)) allModels.push(m); });
    allModels.sort();

    let html = '';
    allModels.forEach(m => {
      const isChecked = currentModels.has(m) || freeModelSet.has(m) ? 'checked' : '';
      html += `
        <label class="model-checkbox-item">
          <input type="checkbox" class="modal-model-checkbox" value="${m}" ${isChecked}>
          <div class="model-checkbox-label">${m}</div>
        </label>`;
    });
    document.getElementById('modalModelList').innerHTML = html;
    document.getElementById('modalStatusText').innerText = `共加载了 ${allModels.length} 个模型`;
  } catch (e) {
    if (e.message === 'Unauthorized') return;
    console.error(e);
    document.getElementById('modalModelList').innerHTML = '<div class="empty" style="color:var(--danger)">获取可用模型列表失败，请检查网络或后端日志。</div>';
  }
}

function closeModelManager() {
  document.getElementById('modelModal').classList.remove('show');
  currentManagingProvider = null;
}

function filterModalModels() {
  const query = document.getElementById('modalSearchInput').value.toLowerCase();
  document.querySelectorAll('#modalModelList .model-checkbox-item').forEach(item => {
    const label = item.querySelector('.model-checkbox-label').innerText.toLowerCase();
    item.style.display = label.includes(query) ? 'flex' : 'none';
  });
}

function selectAllModels(check) {
  document.querySelectorAll('#modalModelList .model-checkbox-item').forEach(item => {
    if (item.style.display !== 'none') {
      const cb = item.querySelector('.modal-model-checkbox');
      if (cb) cb.checked = check;
    }
  });
}

async function saveModelSelection() {
  if (!currentManagingProvider) return;
  const selectedModels = [];
  document.querySelectorAll('.modal-model-checkbox').forEach(cb => { if (cb.checked) selectedModels.push(cb.value); });

  const btn = document.querySelector('#modelModal .btn-primary');
  const originalText = btn.innerText;
  btn.innerText = '保存中...';
  btn.disabled = true;
  try {
    const res = await apiFetch(`/api/providers/${currentManagingProvider}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ models: selectedModels })
    });
    if (res.ok) {
      closeModelManager();
      load();
      checkAll();
    } else {
      alert("保存失败");
    }
  } catch (e) {
    if (e.message !== 'Unauthorized') { console.error(e); alert("请求发生错误"); }
  } finally {
    btn.innerText = originalText;
    btn.disabled = false;
  }
}

async function checkFreeModelChanges() {
  if (!currentManagingProvider) return;
  const btn = document.getElementById('btnCheckFreeChanges');
  const originalText = btn.innerText;
  btn.innerText = '检查中...';
  btn.disabled = true;

  try {
    // 1. 获取当前 provider 配置
    const rProviders = await apiFetch('/api/providers');
    const pList = await rProviders.json();
    const pData = pList.find(p => p.name === currentManagingProvider);
    if (!pData) { alert('未找到提供商配置'); return; }

    const currentModels = new Set(pData.models || []);
    const freeOnly = pData.free_only || false;

    // 2. 重新拉取上游可用模型列表
    const rAvailable = await apiFetch(`/api/providers/${currentManagingProvider}/available-models`);
    const availableData = await rAvailable.json();
    if (!availableData.ok) throw new Error("API返回错误");

    const rawModels = availableData.models || [];
    const allModels = rawModels.map(m => typeof m === 'string' ? m : (m.id || m.model || '')).filter(Boolean);

    // 3. 构建当前免费模型集合（上游最新数据）
    const currentFreeSet = new Set();
    if (freeOnly) {
      rawModels.forEach(m => {
        if (typeof m === 'object' && m.is_free) {
          currentFreeSet.add(m.id || m.model);
        }
      });
    }

    // 4. 找出需要踢掉的模型：已选中但不再是免费的
    let removed = [];
    if (freeOnly) {
      removed = allModels.filter(m => currentModels.has(m) && !currentFreeSet.has(m));
    }

    // 5. 更新前端 checkbox 状态
    const checkboxes = document.querySelectorAll('#modalModelList .modal-model-checkbox');
    checkboxes.forEach(cb => {
      if (removed.includes(cb.value)) {
        cb.checked = false;
      }
    });

    // 6. 自动保存到后端
    if (removed.length > 0) {
      const newSelected = [];
      document.querySelectorAll('.modal-model-checkbox').forEach(cb => { if (cb.checked) newSelected.push(cb.value); });
      const res = await apiFetch(`/api/providers/${currentManagingProvider}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ models: newSelected })
      });
      if (!res.ok) throw new Error("保存失败");
      alert(`已移除 ${removed.length} 个不再免费的模型：\n${removed.join('\n')}`);
    } else {
      alert('免费模型���表未发生变化');
    }

    // 7. 刷新主界面
    load();
    checkAll();
  } catch (e) {
    if (e.message !== 'Unauthorized') { console.error(e); alert("检查失败：" + e.message); }
  } finally {
    btn.innerText = originalText;
    btn.disabled = false;
  }
}

/* ============ 提供商增删改 ============ */
let providerEditMode = 'add';
let providerEditName = null;

async function openProviderEditor(name) {
  providerEditMode = name ? 'edit' : 'add';
  providerEditName = name || null;
  marketSelected = null; // 手动打开时清空市场预填状态（marketAdd 会在本函数返回后再赋值）
  const titleEl = document.getElementById('providerModalTitle');
  const saveBtn = document.getElementById('providerSaveBtn');
  const nameInput = document.getElementById('pf_name');
  const nameHint = document.getElementById('pf_name_hint');
  const hintEl = document.getElementById('providerModalHint');
  const msgEl = document.getElementById('providerModalMsg');
  msgEl.innerHTML = '';
  msgEl.style.display = 'none';
  const vsEl = document.getElementById('pf_verify_status');
  if (vsEl) vsEl.innerHTML = '';

  if (providerEditMode === 'edit') {
    titleEl.innerText = '编辑提供商';
    saveBtn.innerText = '保存修改';
    hintEl.innerText = '保存后不会改变已选模型列表，仅更新连接信息';
    nameInput.value = name;
    nameInput.disabled = true;
    nameHint.style.display = 'block';
    try {
      const r = await apiFetch('/api/providers');
      const list = await r.json();
      const p = list.find(x => x.name === name) || {};
      document.getElementById('pf_base_url').value = p.base_url || '';
      document.getElementById('pf_auth_header').value = p.auth_header || '';
      document.getElementById('pf_auth_scheme').value = p.auth_scheme || '';
      document.getElementById('pf_format').value = p.format || 'openai';
      document.getElementById('pf_thinking_budget').value = p.thinking_budget || 0;
      document.getElementById('pf_free_only').checked = p.free_only === true || p.free_only === '1' || p.free_only === 1;
      document.getElementById('pf_anonymous_api_key').value = p.anonymous_api_key || '';
      // 编辑模式：始终显示密钥框（免Key 提供者允许留空，但保留填入入口——
      // 部分标称免Key的提供者如 Hackclub 实际仍需密钥，用户可在此补填）。
      //   - auth: "none" / no_auth=true → 免 Key，显示密钥框但允许留空
      //   - auth: "optional" → 可选 Key，显示密钥框（用户可填以提升限额）
      //   - auth: "apikey" → 必须 Key，显示密钥框
      const keyField = document.getElementById('pf_api_key_field');
      if (keyField) keyField.style.display = '';
    } catch (e) { if (e.message !== 'Unauthorized') console.error(e); }
    document.getElementById('pf_api_key').value = '';
    document.getElementById('pf_api_key').placeholder = '留空表示不修改密钥';
  } else {
    titleEl.innerText = '添加提供商';
    saveBtn.innerText = '保存并拉取模型';
    hintEl.innerText = '保存后将自动从上游拉取可用模型列表';
    nameInput.value = '';
    nameInput.disabled = false;
    nameHint.style.display = 'none';
    document.getElementById('pf_base_url').value = '';
    document.getElementById('pf_api_key').value = '';
    document.getElementById('pf_api_key').placeholder = 'sk-...';
    document.getElementById('pf_auth_header').value = '';
    document.getElementById('pf_auth_scheme').value = '';
    document.getElementById('pf_format').value = 'openai';
    document.getElementById('pf_thinking_budget').value = 0;
    document.getElementById('pf_free_only').checked = true;
    document.getElementById('pf_anonymous_api_key').value = '';
    // 手动添加模式：默认显示密钥框（用户可能添加任意提供者）
    const keyField = document.getElementById('pf_api_key_field');
    if (keyField) keyField.style.display = '';
  }
  document.getElementById('pf_show_key').checked = false;
  document.getElementById('pf_api_key').type = 'password';
  document.getElementById('providerModal').classList.add('show');
}

function closeProviderEditor() {
  document.getElementById('providerModal').classList.remove('show');
  providerEditName = null;
}

async function openOutputModelsModal() {
  document.getElementById('outputModelsModal').classList.add('show');
  document.getElementById('outputModelList').innerHTML = '<div class="empty">获取中...</div>';
  try {
    if (!_adminKey) { const ok = await fetchLocalInfo(); if (!ok) { showGate(); return; } }
    const res = await apiFetch('/v1/models');
    if (!res.ok) throw new Error('Failed to fetch models');
    const data = await res.json();
    let models = (data.data || []).map(m => m.id);
    models.sort();
    
    if (models.length === 0) {
      document.getElementById('outputModelList').innerHTML = '<div class="empty">暂无可输出模型</div>';
      return;
    }
    
    let html = '';
    models.forEach(m => {
      html += `
      <div style="display: flex; justify-content: space-between; align-items: center; padding: 8px 12px; background: var(--mac-window-bg); border: 1px solid var(--mac-border); border-radius: 6px;">
        <span style="font-family: ui-monospace, monospace; font-size: 13px; color: var(--text-primary); font-weight: 500;">${m}</span>
        <button class="btn" style="padding: 4px 8px; font-size: 12px;" onclick="copyText('${m}', this)">Copy</button>
      </div>`;
    });
    document.getElementById('outputModelList').innerHTML = html;
  } catch (e) {
    document.getElementById('outputModelList').innerHTML = '<div class="empty" style="color:var(--danger)">获取失败</div>';
  }
}

function showProviderMsg(msg, type) {
  const el = document.getElementById('providerModalMsg');
  const color = type === 'error' ? 'var(--danger)' : 'var(--success)';
  el.innerHTML = '<span style="color:' + color + '">' + escapeHtml(msg) + '</span>';
  el.style.display = 'block';
}

function markInvalid(id) {
  const el = document.getElementById(id);
  if (!el) return;
  el.style.borderColor = 'var(--danger)';
  el.addEventListener('input', function () { el.style.borderColor = ''; }, { once: true });
}
async function submitProviderForm() {
  const name = document.getElementById('pf_name').value.trim();
  const baseUrl = document.getElementById('pf_base_url').value.trim();
  const apiKey = document.getElementById('pf_api_key').value;
  const authHeader = document.getElementById('pf_auth_header').value.trim();
  const authScheme = document.getElementById('pf_auth_scheme').value.trim();
  const pfFormat = (document.getElementById('pf_format').value || 'openai').trim();
  const thinkingBudget = parseInt(document.getElementById('pf_thinking_budget').value || '0', 10) || 0;
  const anonymousApiKey = document.getElementById('pf_anonymous_api_key').value.trim();
  // F2 修复：新增提供商默认全选（free_only=false），付费模型不再被漏选；
  // 预设流程在后端显式传 true 时才只选免费模型。
  // 提供者面板新增"自动选择免费模型"勾选框，默认勾选，从市场添加时保留勾选状态。
  const freeOnly = document.getElementById('pf_free_only').checked;
  // 免 Key 提供者标记：从市场添加时由 marketSelected.auth 决定；手动添加时由匿名 key 或 auth_scheme 推断
  const noAuth = marketSelected ? marketSelected.auth !== 'apikey' : (!apiKey && (authScheme === 'none' || anonymousApiKey));

  if (!name || !baseUrl) {
    if (!name) markInvalid('pf_name');
    if (!baseUrl) markInvalid('pf_base_url');
    showProviderMsg('名称和 Base URL 不能为空', 'error'); return;
  }
  // 免 Key 提供者（市场目录 auth=none/optional）允许留空密钥
  const keylessOk = marketSelected && marketSelected.auth !== 'apikey';
  if (providerEditMode === 'add' && !apiKey && !keylessOk) { showProviderMsg('添加时必须填写 API Key', 'error'); return; }
  if (!/^[\u4e00-\u9fa5a-zA-Z0-9_.\-]+$/.test(name)) {
    showProviderMsg('名称只能包含中文、字母、数字、横杠(-)、下划线(_)、点(.)，不能含斜杠/空格等特殊字符', 'error');
    return;
  }

  const btn = document.getElementById('providerSaveBtn');
  const originalText = btn.innerText;
  btn.innerText = '处理中...';
  btn.disabled = true;

  try {
    let res;
    if (providerEditMode === 'add') {
      const payload = { name, base_url: baseUrl, api_key: apiKey, free_only: freeOnly, no_auth: noAuth };
      if (authHeader) payload.auth_header = authHeader;
      if (authScheme) payload.auth_scheme = authScheme;
      if (anonymousApiKey) payload.anonymous_api_key = anonymousApiKey;
      if (pfFormat && pfFormat !== 'openai') payload.format = pfFormat;
      if (thinkingBudget > 0) payload.thinking_budget = thinkingBudget;
      // 密钥留空时后端无法自动拉取上游模型列表，改用市场目录内置的模型清单兜底
      if (!apiKey && marketSelected && (marketSelected.models || []).length > 0) {
        payload.models = marketSelected.models.map(m => m.id);
      }
      res = await apiFetch('/api/providers', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload)
      });
    } else {
      const body = { base_url: baseUrl, free_only: freeOnly, no_auth: noAuth };
      if (apiKey) body.api_key = apiKey;
      body.auth_header = authHeader;   // 空字符串表示恢复默认（Authorization: Bearer）
      body.auth_scheme = authScheme;
      if (anonymousApiKey) body.anonymous_api_key = anonymousApiKey;
      body.format = pfFormat;
      if (thinkingBudget > 0) body.thinking_budget = thinkingBudget;
      res = await apiFetch('/api/providers/' + encodeURIComponent(providerEditName), {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body)
      });
    }
    if (res.ok) {
      const wasAdd = providerEditMode === 'add';
      closeProviderEditor();
      showToast(wasAdd ? '提供商已添加，模型列表已自动拉取' : '提供商信息已更新', 'ok');
      await load();
      if (wasAdd) checkAll();
    } else {
      let detail = '';
      try { detail = (await res.json()).detail || ''; } catch (_) {}
      showProviderMsg('操作失败：' + (detail || ('HTTP ' + res.status)), 'error');
    }
  } catch (e) {
    if (e.message !== 'Unauthorized') { console.error(e); showProviderMsg('请求错误：' + e.message, 'error'); }
  } finally {
    btn.innerText = originalText;
    btn.disabled = false;
  }
}

async function deleteProvider(name) {
  if (!confirm('确定删除提供商「' + name + '」吗？\n该操作会从 providers.json 移除该提供商及其模型配置，不可恢复。')) return;
  try {
    const res = await apiFetch('/api/providers/' + encodeURIComponent(name), { method: 'DELETE' });
    if (res.ok) { showToast('已删除提供商「' + name + '」', 'ok'); load(); }
    else { showToast('删除失败', 'error'); }
  } catch (e) {
    if (e.message !== 'Unauthorized') { console.error(e); showToast('删除请求错误', 'error'); }
  }
}

/* ============ 轮询与可见性 ============ */
let loadTimer = null, pollTimer = null, detailTimer = null, callLogTimer = null, catalogTimer = null, budgetTimer = null;
async function loadModelDetails() {
  try {
    const r = await apiFetch('/api/model-details');
    modelDetails = await r.json();
    renderStability();
  } catch (e) { if (e.message !== 'Unauthorized') console.error("Failed to load model details:", e); }
}
async function loadModelCatalog() {
  try {
    const r = await apiFetch('/v1/models');
    if (!r.ok) return;
    const data = await r.json();
    const arr = data.data || [];
    const map = {};
    arr.forEach(m => {
      if (m && m.id) map[m.id] = m;
      // 同时用原始模型名（含 provider/ 前缀）索引，与稳定性列表的 d.model 对齐，
      // 否则 key 格式不一致（id 用 provider-model、d.model 用 provider/model）会全部落到「未分类」
      if (m && m.model) map[m.model] = m;
    });
    modelCatalog = map;
    renderStability();
  } catch (e) { if (e.message !== 'Unauthorized') console.error("Failed to load model catalog:", e); }
}
function startPolling() {
  if (loadTimer) clearInterval(loadTimer);
  if (pollTimer) clearInterval(pollTimer);
  if (detailTimer) clearInterval(detailTimer);
  if (catalogTimer) clearInterval(catalogTimer);
  if (budgetTimer) clearInterval(budgetTimer);
  loadTimer = setInterval(load, 5000);
  pollTimer = setInterval(updatePollStatus, 3000);
  detailTimer = setInterval(loadModelDetails, 30000);
  catalogTimer = setInterval(loadModelCatalog, 30000);
  budgetTimer = setInterval(refreshBudgetBanner, 30000);
  loadModelCatalog();
  refreshBudgetBanner();
  callLogTimer = setInterval(() => {
    const sv = document.getElementById('stats-view');
    if (sv && sv.style.display !== 'none') loadCallLog();
  }, 5000);
}
function stopPolling() {
  if (loadTimer) { clearInterval(loadTimer); loadTimer = null; }
  if (pollTimer) { clearInterval(pollTimer); pollTimer = null; }
  if (detailTimer) { clearInterval(detailTimer); detailTimer = null; }
  if (catalogTimer) { clearInterval(catalogTimer); catalogTimer = null; }
  if (budgetTimer) { clearInterval(budgetTimer); budgetTimer = null; }
  if (callLogTimer) { clearInterval(callLogTimer); callLogTimer = null; }
}
document.addEventListener('visibilitychange', () => {
  if (document.hidden) stopPolling();
  else if (getToken()) { load(); updatePollStatus(); startPolling(); }
});

function switchMainTab(tab) {
  // 两标签统一处理：激活态高亮 + 对应视图显隐
  // 统计页（tab-stats）将「消耗统计」与「成本统计」合并在同一页展示
  const tabs = [
    { id: 'monitor', btn: 'tab-monitor', view: 'monitor-view' },
    { id: 'stats',   btn: 'tab-stats',   view: 'stats-view' }
  ];
  tabs.forEach(t => {
    const btn = document.getElementById(t.btn);
    const view = document.getElementById(t.view);
    const active = (t.id === tab);
    btn.style.background = active ? 'var(--mac-window-bg)' : 'transparent';
    btn.style.boxShadow = active ? '0 1px 3px rgba(0,0,0,0.1)' : 'none';
    btn.style.color = active ? 'var(--text-primary)' : 'var(--text-secondary)';
    btn.style.fontWeight = active ? '600' : '500';
    view.style.display = active ? 'block' : 'none';
  });

  if (tab === 'stats') {
    // 消耗统计 + 调用日志
    if (document.getElementById('usageOverview').innerHTML === '') {
      loadUsage();
    }
    loadCallLog();
    // 统计页：消耗统计 + 成本统计共用时间范围选择器，默认高亮 statsDays
    document.querySelectorAll('.stats-tab').forEach(b => {
      const on = parseInt(b.dataset.days, 10) === statsDays;
      b.classList.toggle('btn-primary', on);
    });
    if (document.getElementById('costOverview').innerHTML === '') {
      loadCostDashboard();
    }
  }
}

/* ============ 成本/用量仪表盘 ============ */
function fmtCost(n) {
  if (n == null) return '0.00';
  return Number(n).toFixed(4);
}
function fmtInt(n) {
  return (n || 0).toLocaleString('en-US');
}
async function loadCostDashboard() {
  try {
    const d = await (await apiFetch('/api/cost-dashboard?days=' + statsDays)).json();
    if (!d.ok) { showToast('成本统计加载失败', 'error'); return; }
    const total = d.total || {};
    // 汇总卡
    document.getElementById('costOverview').innerHTML = `
      <div class="stat-card"><div class="sc-val">${fmtInt(total.requests)}</div><div class="sc-label">请求总数</div></div>
      <div class="stat-card"><div class="sc-val">${fmtInt(total.prompt_tokens)}</div><div class="sc-label">输入 Token</div></div>
      <div class="stat-card"><div class="sc-val">${fmtInt(total.completion_tokens)}</div><div class="sc-label">输出 Token</div></div>
      <div class="stat-card"><div class="sc-val">$${fmtCost(total.cost_usd)}</div><div class="sc-label">累计成本(USD)</div></div>`;
    // 预算卡
    const b = d.budget || {};
    const budgetEl = document.getElementById('costBudgetCard');
    const budgetText = document.getElementById('costBudgetText');
    if (!b.daily_limit_usd || b.daily_limit_usd <= 0) {
      budgetEl.style.background = 'var(--mac-window-bg)';
      budgetEl.style.border = '1px solid var(--mac-border)';
      budgetEl.style.color = 'var(--text-secondary)';
      budgetText.innerHTML = `💡 未设置每日预算上限。可在「⚙️ 网关设置 → 预算护栏」中设定上限，并结合上方「按模型/渠道」成本评估用量。`;
    } else {
      const pct = b.daily_limit_usd > 0 ? (b.daily_cost_usd / b.daily_limit_usd * 100) : 0;
      const exceeded = b.daily_cost_usd >= b.daily_limit_usd;
      const warn = pct >= 80;
      const col = exceeded ? 'var(--danger)' : (warn ? 'var(--warning)' : 'var(--success)');
      budgetEl.style.background = exceeded ? 'rgba(255,59,48,0.1)' : (warn ? 'rgba(255,149,0,0.1)' : 'rgba(40,205,65,0.1)');
      budgetEl.style.border = '1px solid ' + (exceeded ? 'rgba(255,59,48,0.35)' : (warn ? 'rgba(255,149,0,0.35)' : 'rgba(40,205,65,0.3)'));
      budgetEl.style.color = col;
      budgetText.innerHTML = `💰 今日成本 <b>$${fmtCost(b.daily_cost_usd)}</b> / 上限 $${fmtCost(b.daily_limit_usd)}（${pct.toFixed(1)}%）· 剩余 $${fmtCost(b.remaining_usd)} · 动作：${b.action === 'block' ? '超限拦截' : '仅预警'}`;
    }
    // 渲染三个表
    renderCostTable('costByProvider', d.by_provider, 'provider');
    renderCostTable('costByModel', d.by_model, 'model');
    renderCostTable('costByDay', d.by_day, 'date');
  } catch (e) {
    if (e.message !== 'Unauthorized') {
      const box = document.getElementById('costOverview');
      if (box) box.innerHTML = '<div class="empty" style="color:var(--danger)">成本统计加载失败：' + escapeHtml(e.message) + ' <button class="btn" style="font-size:12px;padding:2px 8px;" onclick="loadCostDashboard()">重试</button></div>';
    }
    showToast('成本统计加载失败：' + e.message, 'error');
  }
}

function renderCostTable(elId, data, keyLabel) {
  const el = document.getElementById(elId);
  const keys = Object.keys(data || {});
  if (!keys.length) {
    el.innerHTML = '<tr><td colspan="5" class="empty">该时间段暂无用量记录</td></tr>';
    return;
  }
  // 按成本降序
  keys.sort((a, b) => (data[b].cost_usd || 0) - (data[a].cost_usd || 0));
  el.innerHTML = keys.map(k => {
    const r = data[k] || {};
    return `<tr>
      <td>${escapeHtml(k)}</td>
      <td>${fmtInt(r.requests)}</td>
      <td>${fmtInt(r.prompt_tokens)}</td>
      <td>${fmtInt(r.completion_tokens)}</td>
      <td>$${fmtCost(r.cost_usd)}</td>
    </tr>`;
  }).join('');
}

/* ============ 路由配置 ============ */
function fmtCtx(n) {
  if (!n) return '';
  if (n >= 1048576) return (n / 1048576) + 'M';
  if (n >= 1024) return Math.round(n / 1024) + 'K';
  return String(n);
}
let routersData = {};
let strategiesData = {};    // routerName -> strategy (quality/priority/least-latency/cost/loadbalance)
let allModelOptions = [];   // [{provider, model}]

async function loadVisionAssist() {
  try {
    const r = await apiFetch('/api/vision-assist');
    const d = await r.json();
    document.getElementById('visionAssistSwitch').checked = !!d.enabled;
  } catch (e) {
    document.getElementById('visionAssistSwitch').checked = false;
  }
}

async function toggleVisionAssist(el) {
  const enabled = el.checked;
  try {
    await apiFetch('/api/vision-assist', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ enabled })
    });
    showToast(enabled ? '识图辅助已开启' : '识图辅助已关闭', 'ok');
  } catch (e) {
    el.checked = !enabled;
    showToast('操作失败：' + e.message, 'error');
  }
}

async function openRouterManager() {
  document.getElementById('routerModal').classList.add('show');
  document.getElementById('newRouterName').value = '';
  // 1. 拉取所有可用模型 + 模型详情（用于显示上下文长度）
  try {
    const r = await apiFetch('/api/providers');
    const list = await r.json();
    allModelOptions = [];
    list.forEach(p => {
      (p.models || []).forEach(m => {
        allModelOptions.push({ provider: p.name, model: m });
      });
    });
  } catch (e) { if (e.message !== 'Unauthorized') console.error(e); }
  // 2. 拉取已保存的路由
  try {
    const r = await apiFetch('/api/routers');
    const d = await r.json();
    routersData = d.data || {};
    strategiesData = d.strategies || {};
  } catch (e) { if (e.message !== 'Unauthorized') console.error(e); routersData = {}; strategiesData = {}; }
  // 3. 拉取参考库分类信息（用于路由成员能力徽标），完成后重渲染
  loadModelCatalog().finally(() => renderRouterList());
  // 4. 拉取别名映射
  loadAliases();
}

function toggleRouterGroup(name) {
  const el = document.querySelector('.router-group[data-name="' + name + '"]');
  if (!el) return;
  el.classList.toggle('collapsed');
}

/* ============ 别名映射（G8：友好名 → 模型/路由组/auto） ============ */
let aliasesData = []; // [{name, target}]

async function loadAliases() {
  try {
    const r = await apiFetch('/api/aliases');
    const d = await r.json();
    aliasesData = d.data || [];
  } catch (e) { aliasesData = []; }
  // 同步预设按钮状态
  const presetOn = aliasesData.some(a => a.name === 'fast' && a.target === 'auto');
  const btn = document.getElementById('aliasPresetFastBtn');
  if (btn) {
    btn.textContent = presetOn ? '✅ 已启用 fast → auto 预设（点击关闭）' : '⚡ 启用 fast → auto 预设';
    btn.style.background = presetOn ? 'var(--success)' : '';
    btn.style.color = presetOn ? '#fff' : '';
  }
  renderAliasList();
}

async function toggleAliasPreset() {
  const hasPreset = aliasesData.some(a => a.name === 'fast' && a.target === 'auto');
  if (hasPreset) {
    aliasesData = aliasesData.filter(a => !(a.name === 'fast' && a.target === 'auto'));
  } else {
    aliasesData.push({ name: 'fast', target: 'auto' });
  }
  // 自动保存
  const cleaned = aliasesData.filter(a => a.name.trim() && a.target.trim() && a.name.trim() !== a.target.trim());
  try {
    await apiFetch('/api/aliases', { method: 'POST', body: JSON.stringify(cleaned) });
    aliasesData = cleaned;
    renderAliasList();
    showToast(hasPreset ? '已关闭 fast → auto 预设' : '已启用 fast → auto 预设', 'ok');
  } catch (e) {
    showToast('保存预设失败：' + e.message, 'error');
  }
  // 更新按钮状态
  const presetOn = aliasesData.some(a => a.name === 'fast' && a.target === 'auto');
  const btn = document.getElementById('aliasPresetFastBtn');
  if (btn) {
    btn.textContent = presetOn ? '✅ 已启用 fast → auto 预设（点击关闭）' : '⚡ 启用 fast → auto 预设';
    btn.style.background = presetOn ? 'var(--success)' : '';
    btn.style.color = presetOn ? '#fff' : '';
  }
}

function renderAliasList() {
  const container = document.getElementById('aliasListContainer');
  if (!aliasesData.length) {
    container.innerHTML = '<div class="empty" style="padding:8px;">暂无别名，点击「+ 添加别名」创建</div>';
    return;
  }
  // 目标下拉：路由组 + auto + 所有模型
  const targets = ['auto'].concat(Object.keys(routersData)).concat(allModelOptions.map(o => o.model));
  const uniq = [...new Set(targets)];
  container.innerHTML = aliasesData.map((a, i) => `
    <div style="display:flex; gap:8px; align-items:center;">
      <input type="text" value="${escapeHtml(a.name)}" placeholder="别名（如 fast）" oninput="aliasesData[${i}].name=this.value"
        style="flex:1; padding:5px 8px; border:1px solid var(--mac-border); border-radius:6px; background:transparent; color:var(--text-primary); font-size:13px;">
      <span style="color:var(--text-secondary);">→</span>
      <input type="text" list="aliasTargetList" value="${escapeHtml(a.target)}" placeholder="目标（模型/路由组/auto）" oninput="aliasesData[${i}].target=this.value"
        style="flex:1.4; padding:5px 8px; border:1px solid var(--mac-border); border-radius:6px; background:transparent; color:var(--text-primary); font-size:13px;">
      <button class="btn" style="font-size:12px; padding:3px 8px; color:var(--danger);" onclick="aliasesData.splice(${i},1);renderAliasList()">✕</button>
    </div>`).join('') +
    `<datalist id="aliasTargetList">${uniq.map(t => `<option value="${escapeHtml(t)}">`).join('')}</datalist>`;
}

function addAliasRow() {
  aliasesData.push({ name: '', target: '' });
  renderAliasList();
}

async function saveAliases() {
  const cleaned = aliasesData.filter(a => a.name.trim() && a.target.trim() && a.name.trim() !== a.target.trim());
  try {
    await apiFetch('/api/aliases', { method: 'POST', body: JSON.stringify(cleaned) });
    aliasesData = cleaned;
    renderAliasList();
    showToast('别名已保存（' + cleaned.length + ' 条）', 'ok');
  } catch (e) {
    showToast('保存别名失败：' + e.message, 'error');
  }
}

function escapeHtml(s) {
  return String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
}

// escJsStr：用于把动态值安全地嵌入单引号 JS 字符串（如 onclick="fn('...')"）。
// 在 escapeHtml 基础上额外转义单引号与反斜杠，防止单引号注入导致的属性/脚本逃逸。
function escJsStr(s) {
  return String(s == null ? '' : s)
    .replace(/\\/g, '\\\\')
    .replace(/'/g, "\\'")
    .replace(/"/g, '&quot;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/&/g, '&amp;');
}

// safeHref：仅允许 http/https/mailto/相对路径，阻断 javascript:/data: 等危险协议，防止 Markdown 链接 XSS。
function safeHref(url) {
  const u = String(url == null ? '' : url).trim();
  if (/^(https?:|mailto:|\/|#)/i.test(u)) return u;
  return '#';
}

/* ============ 网关设置（缓存 / 预算 / 并发 / 钩子） ============ */
let hooksData = []; // [{url, events, enabled, secret}]

async function openGatewaySettings() {
  document.getElementById('gatewayModal').classList.add('show');
  try {
    const [gs, hk] = await Promise.all([
      (await apiFetch('/api/gateway-settings')).json(),
      (await apiFetch('/api/hooks')).json()
    ]);
    const c = gs.cache || {}, b = gs.budget || {};
    document.getElementById('gwCacheEnabled').checked = !!c.enabled;
    document.getElementById('gwCacheTTL').value = c.ttl || 300;
    document.getElementById('gwCacheMax').value = c.max_entries || 1000;
    document.getElementById('gwCacheSemantic').checked = !!c.semantic;
    document.getElementById('gwBudgetLimit').value = b.daily_limit_usd || 0;
    document.getElementById('gwBudgetAction').value = b.action || 'warn';
    document.getElementById('gwBudgetWarnPct').value = b.warning_pct || 80;
    document.getElementById('gwMaxConcurrency').value = gs.max_concurrency || 0;
    document.getElementById('gwStrictCap').checked = !!gs.strict_capability;
    document.getElementById('gwSsrfStrict').checked = !!gs.ssrf_strict;
    document.getElementById('gwAllowClients').value = (gs.allow_clients || []).join('\n');
    document.getElementById('gwBannedProviders').value = (gs.banned_providers || []).join('\n');
    hooksData = hk.data || [];
  } catch (e) {
    if (e.message !== 'Unauthorized') showToast('加载网关设置失败：' + e.message, 'error');
    hooksData = [];
  }
  renderHookList();
  loadCacheStats();
  refreshBudgetBanner(true);
}

async function loadCacheStats() {
  const el = document.getElementById('gwCacheStats');
  try {
    const d = await (await apiFetch('/api/cache')).json();
    const s = d.stats || {};
    const rate = ((d.hit_rate || 0) * 100).toFixed(1);
    el.innerHTML = `命中 <b>${s.hits || 0}</b>（其中近似命中 ${s.semantic_hits || 0}）· 未命中 ${s.misses || 0} · 命中率 <b>${rate}%</b> · 当前条目 ${s.entries || 0}`;
  } catch (e) { el.textContent = '统计加载失败'; }
}

async function clearGatewayCache() {
  try {
    await apiFetch('/api/cache', { method: 'POST', body: JSON.stringify({ action: 'clear' }) });
    showToast('缓存已清空', 'ok');
    loadCacheStats();
  } catch (e) { showToast('清空失败：' + e.message, 'error'); }
}

function renderHookList() {
  const container = document.getElementById('hookListContainer');
  if (!hooksData.length) {
    container.innerHTML = '<div class="empty" style="padding:8px;">暂无钩子</div>';
    return;
  }
  container.innerHTML = hooksData.map((h, i) => {
    const evs = h.events || [];
    return `<div style="display:flex; flex-direction:column; gap:6px; padding:10px; border:1px solid var(--mac-border); border-radius:8px;">
      <div style="display:flex; gap:8px; align-items:center;">
        <input type="text" value="${escapeHtml(h.url)}" placeholder="https://example.com/webhook" oninput="hooksData[${i}].url=this.value"
          style="flex:1; padding:5px 8px; border:1px solid var(--mac-border); border-radius:6px; background:transparent; color:var(--text-primary); font-size:13px;">
        <label class="hf-toggle"><input type="checkbox" ${h.enabled !== false ? 'checked' : ''} onchange="hooksData[${i}].enabled=this.checked"><span class="hf-switch"></span><span>启用</span></label>
        <button class="btn" style="font-size:12px; padding:3px 8px; color:var(--danger);" onclick="hooksData.splice(${i},1);renderHookList()">✕</button>
      </div>
      <div style="display:flex; gap:14px; align-items:center; font-size:12.5px; flex-wrap:wrap;">
        <label><input type="checkbox" ${evs.includes('request_done') ? 'checked' : ''} onchange="toggleHookEvent(${i},'request_done',this.checked)"> 成功事件</label>
        <label><input type="checkbox" ${evs.includes('request_failed') ? 'checked' : ''} onchange="toggleHookEvent(${i},'request_failed',this.checked)"> 失败事件</label>
        <label><input type="checkbox" ${evs.includes('provider_down') ? 'checked' : ''} onchange="toggleHookEvent(${i},'provider_down',this.checked)"> 提供商下线</label>
        <label><input type="checkbox" ${evs.includes('provider_up') ? 'checked' : ''} onchange="toggleHookEvent(${i},'provider_up',this.checked)"> 提供商恢复</label>
        <label><input type="checkbox" ${evs.includes('quota_exceeded') ? 'checked' : ''} onchange="toggleHookEvent(${i},'quota_exceeded',this.checked)"> 配额耗尽</label>
        <label><input type="checkbox" ${evs.includes('circuit_open') ? 'checked' : ''} onchange="toggleHookEvent(${i},'circuit_open',this.checked)"> 熔断开启</label>
        <input type="text" value="${escapeHtml(h.secret || '')}" placeholder="签名密钥（可选）" oninput="hooksData[${i}].secret=this.value"
          style="flex:1; min-width:140px; padding:4px 8px; border:1px solid var(--mac-border); border-radius:6px; background:transparent; color:var(--text-primary); font-size:12px;">
      </div>
    </div>`;
  }).join('');
}

function toggleHookEvent(i, ev, on) {
  const evs = new Set(hooksData[i].events || []);
  on ? evs.add(ev) : evs.delete(ev);
  hooksData[i].events = [...evs];
}

function addHookRow() {
  hooksData.push({ url: '', events: ['request_done', 'request_failed'], enabled: true, secret: '' });
  renderHookList();
}

async function saveGatewaySettings() {
  const body = {
    cache_enabled: document.getElementById('gwCacheEnabled').checked,
    cache_ttl: parseInt(document.getElementById('gwCacheTTL').value) || 300,
    cache_max_entries: parseInt(document.getElementById('gwCacheMax').value) || 1000,
    cache_semantic: document.getElementById('gwCacheSemantic').checked,
    budget_daily_limit: parseFloat(document.getElementById('gwBudgetLimit').value) || 0,
    budget_action: document.getElementById('gwBudgetAction').value,
    budget_warning_pct: parseInt(document.getElementById('gwBudgetWarnPct').value) || 80,
    max_concurrency: parseInt(document.getElementById('gwMaxConcurrency').value) || 0,
    strict_capability: document.getElementById('gwStrictCap').checked,
    ssrf_strict: document.getElementById('gwSsrfStrict').checked,
    allow_clients: document.getElementById('gwAllowClients').value,
    banned_providers: document.getElementById('gwBannedProviders').value
  };
  const cleanedHooks = hooksData.filter(h => h.url && h.url.trim());
  try {
    await apiFetch('/api/gateway-settings', { method: 'POST', body: JSON.stringify(body) });
    await apiFetch('/api/hooks', { method: 'POST', body: JSON.stringify(cleanedHooks) });
    showToast('网关设置已保存', 'ok');
    document.getElementById('gatewayModal').classList.remove('show');
    refreshBudgetBanner(true);
  } catch (e) {
    showToast('保存失败：' + e.message, 'error');
  }
}

/* ============ 虚拟密钥（子密钥）管理 ============ */
let vkeysData = []; // [{id,name,key,enabled,quota_requests,quota_tokens,allowed_models,notes,created_at,usage:{requests,tokens}}]

function openVKeyModal() {
  document.getElementById('vkeyModal').classList.add('show');
  // 重置新建表单与结果
  document.getElementById('vkName').value = '';
  document.getElementById('vkQuotaReq').value = '';
  document.getElementById('vkQuotaTok').value = '';
  document.getElementById('vkAllowed').value = '';
  const res = document.getElementById('vkCreateResult');
  res.style.display = 'none';
  res.innerHTML = '';
  loadVKeys();
}

async function copyVKey(id, btn) {
  try {
    const r = await apiFetch('/api/vkeys/' + encodeURIComponent(id) + '/reveal');
    const d = await r.json();
    if (!d.ok || !d.key) { showToast('获取密钥失败', 'error'); return; }
    copyText(d.key, btn);
    showToast('完整密钥已复制（请妥善保管）', 'ok');
  } catch (e) {
    showToast('复制失败：' + e.message, 'error');
  }
}

async function loadVKeys() {
  const container = document.getElementById('vkeyList');
  container.innerHTML = '<div class="empty">加载中…</div>';
  try {
    const d = await (await apiFetch('/api/vkeys')).json();
    vkeysData = d.data || [];
    if (!vkeysData.length) {
      container.innerHTML = '<div class="empty">还没有虚拟密钥，使用上方表单创建一个</div>';
      return;
    }
    container.innerHTML = vkeysData.map((vk, i) => {
      const quotaReq = vk.quota_requests > 0 ? `${fmtInt(vk.usage.requests)}/${fmtInt(vk.quota_requests)}` : `${fmtInt(vk.usage.requests)}/∞`;
      const quotaTok = vk.quota_tokens > 0 ? `${fmtInt(vk.usage.tokens)}/${fmtInt(vk.quota_tokens)}` : `${fmtInt(vk.usage.tokens)}/∞`;
      const allowed = (vk.allowed_models && vk.allowed_models.length) ? vk.allowed_models.map(escapeHtml).join('、') : '全部模型';
      const disabledCls = vk.enabled ? '' : ' style="opacity:0.5;"';
      return `<div class="rg-member"${disabledCls}>
        <div style="flex:1; min-width:0;">
          <div style="display:flex; align-items:center; gap:8px;">
            <b style="font-size:13px;">${escapeHtml(vk.name || '(未命名)')}</b>
            <code style="font-size:11px; color:var(--text-secondary);">${escapeHtml(vk.key)}</code>
            <button class="btn" style="padding:2px 8px;font-size:11px;" onclick="copyVKey('${escJsStr(vk.id)}', this)" title="复制完整密钥">📋</button>
            ${vk.enabled ? '' : '<span style="font-size:11px; color:var(--danger);">已禁用</span>'}
          </div>
          <div style="font-size:11.5px; color:var(--text-secondary); margin-top:3px; line-height:1.5;">
            请求 ${quotaReq} · Token ${quotaTok} · 模型：${allowed}
            ${vk.notes ? ' · 备注：' + escapeHtml(vk.notes) : ''}
          </div>
        </div>
          <label class="hf-toggle" title="启用/禁用该密钥" style="margin:0 8px;">
          <input type="checkbox" ${vk.enabled ? 'checked' : ''} onchange="toggleVKey('${escJsStr(vk.id)}', this.checked)"><span class="hf-switch"></span>
        </label>
        <button class="rg-rm" title="删除" onclick="deleteVKey('${escJsStr(vk.id)}')">×</button>
      </div>`;
    }).join('');
  } catch (e) {
    container.innerHTML = '<div class="empty" style="color:var(--danger)">加载失败：' + escapeHtml(e.message) + '</div>';
  }
}

async function createVKey() {
  const name = document.getElementById('vkName').value.trim();
  const qr = parseInt(document.getElementById('vkQuotaReq').value) || 0;
  const qt = parseInt(document.getElementById('vkQuotaTok').value) || 0;
  const allowedRaw = document.getElementById('vkAllowed').value.trim();
  const allowed = allowedRaw ? allowedRaw.split(',').map(s => s.trim()).filter(Boolean) : [];
  const res = document.getElementById('vkCreateResult');
  try {
    const r = await apiFetch('/api/vkeys', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name, quota_requests: qr, quota_tokens: qt, allowed_models: allowed })
    });
    const d = await r.json();
    if (!r.ok || !d.ok || !d.key) {
      res.style.display = 'block';
      res.style.background = 'rgba(255,59,48,0.1)';
      res.style.border = '1px solid rgba(255,59,48,0.3)';
      res.innerHTML = '创建失败：' + escapeHtml(d.error || '未知错误');
      return;
    }
    res.style.display = 'block';
    res.style.background = 'var(--success-bg)';
    res.style.border = '1px solid rgba(40,205,65,0.3)';
    res.innerHTML = `<div style="font-weight:600; margin-bottom:6px;">✅ 密钥已生成（仅此一次显示，请立即复制保存）</div>
      <div style="display:flex; gap:8px; align-items:center;">
        <code id="vkNewKey" style="flex:1; word-break:break-all; font-size:12px; background:rgba(0,0,0,0.04); padding:6px 8px; border-radius:6px;">${escapeHtml(d.key)}</code>
        <button class="btn" style="font-size:12px; padding:4px 10px;" onclick="copyText(document.getElementById('vkNewKey').innerText, this)">复制</button>
      </div>`;
    // 清空表单
    document.getElementById('vkName').value = '';
    document.getElementById('vkQuotaReq').value = '';
    document.getElementById('vkQuotaTok').value = '';
    document.getElementById('vkAllowed').value = '';
    loadVKeys();
  } catch (e) {
    res.style.display = 'block';
    res.style.background = 'rgba(255,59,48,0.1)';
    res.style.border = '1px solid rgba(255,59,48,0.3)';
    res.innerHTML = '创建失败：' + escapeHtml(e.message);
  }
}

async function toggleVKey(id, enabled) {
  try {
    const r = await apiFetch('/api/vkeys', {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id, enabled })
    });
    const d = await r.json();
    if (!r.ok || d.status !== 'ok') { showToast('更新失败', 'error'); loadVKeys(); return; }
    showToast(enabled ? '密钥已启用' : '密钥已禁用', 'ok');
    loadVKeys();
  } catch (e) {
    showToast('更新失败：' + e.message, 'error');
    loadVKeys();
  }
}

async function deleteVKey(id) {
  if (!confirm('确定删除该虚拟密钥？删除后使用该密钥的应用将立即无法访问。')) return;
  try {
    const r = await apiFetch('/api/vkeys?id=' + encodeURIComponent(id), { method: 'DELETE' });
    const d = await r.json();
    if (!r.ok || d.status !== 'ok') { showToast('删除失败', 'error'); return; }
    showToast('已删除虚拟密钥', 'ok');
    loadVKeys();
  } catch (e) {
    showToast('删除失败：' + e.message, 'error');
  }
}

/* ============ 预算/余额预警 banner ============ */
async function refreshBudgetBanner(force) {
  const el = document.getElementById('budgetBanner');
  try {
    const d = await (await apiFetch('/api/budget-status')).json();
    const st = d.data || {};
    if (!st.limit || st.status === 'ok') { el.style.display = 'none'; return; }
    const spent = (st.daily_total || 0).toFixed(4);
    const limit = (st.limit || 0).toFixed(2);
    if (st.status === 'exceeded') {
      el.style.display = 'flex';
      el.style.background = 'rgba(255,59,48,0.1)';
      el.style.border = '1px solid rgba(255,59,48,0.35)';
      el.style.color = 'var(--danger)';
      el.innerHTML = `⛔ <b>今日成本已超预算：</b>$${spent} / $${limit}` + (st.blocked ? '，已启用拦截 —— 新请求将被拒绝（明日自动恢复，或到「网关设置」调高上限）' : '（仅预警模式，仍在放行）');
    } else {
      el.style.display = 'flex';
      el.style.background = 'rgba(255,149,0,0.1)';
      el.style.border = '1px solid rgba(255,149,0,0.35)';
      el.style.color = 'var(--warning)';
      el.innerHTML = `⚠️ <b>成本预警：</b>今日已消耗 $${spent}，达到预算 $${limit} 的 ${st.warning_pct}% 预警线`;
    }
  } catch (e) { if (force) el.style.display = 'none'; }
}

let _visionModelSet = new Set();
async function openVisionConfig() {
  document.getElementById('visionModal').classList.add('show');
  loadVisionAssist();
  const list = document.getElementById('visionList');
  list.innerHTML = '<div class="empty">加载中…</div>';
  document.getElementById('visionCount').textContent = '';
  try {
    const [pr, rr, vr] = await Promise.all([
      (await apiFetch('/api/providers')).json(),
      (await apiFetch('/api/routers')).json(),
      (await apiFetch('/api/vision-models')).json()
    ]);
    const providers = Array.isArray(pr) ? pr : (pr.data || []);
    let opts = [];
    providers.forEach(p => (p.models || []).forEach(m => opts.push({ provider: p.name, model: m })));
    routersData = rr.data || {};
    _visionModelSet = new Set(vr.data || []);
    const selected = new Set(routersData['识图'] || []);
    if (!opts.length) {
      list.innerHTML = '<div class="empty" style="color:var(--danger)">未获取到模型，请先在一键配置或上游管理对接 API Key</div>';
      return;
    }
    opts.sort((a, b) => (_visionModelSet.has(b.model) ? 1 : 0) - (_visionModelSet.has(a.model) ? 1 : 0));
    list.innerHTML = opts.map(opt => {
      const checked = selected.has(opt.model) ? 'checked' : '';
      return `      <label class="model-checkbox-item">
        <input type="checkbox" class="vision-cb" value="${escapeHtml(opt.model)}" ${checked}>
        <div class="model-checkbox-label"><span class="model-provider-badge">${escapeHtml(opt.provider)}</span>${escapeHtml(opt.model)}</div>
      </label>`;
    }).join('');
    document.getElementById('visionCount').textContent = '已选 ' + selected.size + ' · 共 ' + opts.length + ' 个模型';
  } catch (e) {
    list.innerHTML = '<div class="empty" style="color:var(--danger)">加载失败：' + escapeHtml(e.message) + '</div>';
  }
}

async function saveVisionConfig() {
  const selected = [];
  document.querySelectorAll('.vision-cb:checked').forEach(cb => selected.push(cb.value));
  try {
    const rr = await (await apiFetch('/api/routers')).json();
    const all = rr.data || {};
    all['识图'] = selected;
    await apiFetch('/api/routers', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(all) });
    routersData = all;
    showToast('识图模型已保存', 'ok');
    closeVisionConfig();
  } catch (e) {
    showToast('保存失败：' + e.message, 'error');
  }
}

function closeVisionConfig() {
  document.getElementById('visionModal').classList.remove('show');
}

// 弹窗打开时锁定底层滚动（监听所有 .modal-overlay 的 class 变化，统一处理所有弹窗）
(function() {
  const mo = new MutationObserver(() => {
    document.body.style.overflow = document.querySelector('.modal-overlay.show') ? 'hidden' : '';
  });
  document.querySelectorAll('.modal-overlay').forEach(m => mo.observe(m, { attributes: true, attributeFilter: ['class'] }));
})();

function renderRouterList() {
  const container = document.getElementById('routerListContainer');
  const names = Object.keys(routersData);
  if (names.length === 0) {
    container.innerHTML = '<div class="empty">暂无路由组，请在上方输入名称后点击「+ 添加路由」</div>';
    return;
  }
  if (allModelOptions.length === 0) {
    container.innerHTML = '<div class="empty" style="color:var(--danger)">未获取到任何模型，请先配置上游提供商</div>';
    return;
  }
  const capLabel = c => ({ text: '文本', vision: '视觉', reasoning: '推理', image: '图像', audio: '音频', embedding: '嵌入' }[String(c).toLowerCase()] || c);
  container.innerHTML = names.map(name => {
    const members = routersData[name] || [];
    const strategy = strategiesData[name] || 'quality';
    const isClassify = strategy === 'classify';
    let memberRows, addBlock, hintBlock;

    if (isClassify) {
      // 内容分类路由：成员写成 cat=group，cat∈{vision,code,long,reasoning,general}，group 为路由组名
      memberRows = members.length
        ? members.map(m => `
          <div class="rg-member">
            <span class="rg-name">${escapeHtml(m)}</span>
            <button class="rg-rm" title="移除" onclick="removeRouterMember('${name}','${encodeURIComponent(m)}')">×</button>
          </div>`).join('')
        : '<div class="rg-empty">尚未添加分类规则，下方输入 <code>cat=group</code> 后点击「+ 添加」</div>';
      addBlock = `
        <input type="text" id="rg-add-${name}" placeholder="cat=group（如 code=coders / vision=视觉 / default=general）" style="flex:1; padding:4px 8px; border:1px solid var(--mac-border); border-radius:6px; background:transparent; color:var(--text-primary); font-size:12px;">
        <button class="btn btn-primary" style="padding:4px 10px;font-size:12px;" onclick="addClassifyMember('${name}')">+ 添加</button>`;
      hintBlock = '每条规则格式 <b>分类=路由组</b>。可选分类：<code>vision</code>(带图) / <code>code</code>(代码) / <code>long</code>(长文&gt;4000字) / <code>reasoning</code>(推理) / <code>general</code>(兜底)。<b>group</b> 必须是一个已存在的路由组名称。请求先按内容分类，再转发到对应路由组；未命中任何分类时走 <code>general</code> 或 <code>default</code>。';
    } else {
      memberRows = members.length
        ? members.map(m => {
            const opt = allModelOptions.find(o => o.model === m);
            const prov = opt ? opt.provider : '';
            const ci = modelCatalog[m] || {};
            const caps = Array.isArray(ci.capabilities) ? ci.capabilities : [];
            const capStr = caps.length ? ' · ' + caps.map(capLabel).join('/') : '';
            return `<div class="rg-member" draggable="true" data-model="${m}" ondragstart="onRouterMemberDragStart(event,'${name}','${encodeURIComponent(m)}')" ondragover="onRouterMemberDragOver(event)" ondrop="onRouterMemberDrop(event,'${name}','${encodeURIComponent(m)}')" ondragend="onRouterMemberDragEnd(event)">
              <span class="rg-handle" title="拖动调整优先级">⠿</span>
              <span class="rg-name"><span class="model-provider-badge">${prov}</span>${m}${capStr}</span>
              <button class="rg-rm" title="移除" onclick="removeRouterMember('${name}','${encodeURIComponent(m)}')">×</button>
            </div>`;
          }).join('')
        : '<div class="rg-empty">尚未添加模型，下方选择后点击「+ 添加模型」</div>';

      // 可添加模型下拉（排除已选）
      const selectedSet = new Set(members);
      const availableOpts = allModelOptions
        .filter(o => !selectedSet.has(o.model))
        .map(o => `<option value="${o.model}">${o.provider} / ${o.model}</option>`).join('');

      addBlock = `
        <select id="rg-add-${name}">${availableOpts || '<option value="">（无更多可用模型）</option>'}</select>
        <button class="btn btn-primary" style="padding:4px 10px;font-size:12px;" onclick="addRouterMember('${name}')">+ 添加模型</button>`;
      hintBlock = '拖动 ⠿ 调整顺序：<b>严格优先级</b>策略按此顺序依次尝试；其余策略作为加权参考。客户端 model 字段填「' + name + '」即可命中本路由组。';
    }

    return `<div class="router-group" data-name="${name}">
      <div class="rg-head">
        <div class="rg-title" onclick="toggleRouterGroup('${name}')" style="cursor:pointer;" title="点击折叠/展开">🔀 ${name}</div>
        <div style="display:flex;gap:6px;align-items:center;">
          <select class="rg-strategy" onchange="onStrategyChange('${name}', this.value)" title="路由策略" style="padding:4px 8px;border-radius:6px;border:1px solid var(--mac-border);background:var(--mac-window-bg);color:var(--text-primary);font-size:12px;cursor:pointer;">
            <option value="quality"${strategy === 'quality' ? ' selected' : ''}>质量优先</option>
            <option value="priority"${strategy === 'priority' ? ' selected' : ''}>严格优先级</option>
            <option value="least-latency"${strategy === 'least-latency' ? ' selected' : ''}>最低延迟</option>
            <option value="cost"${strategy === 'cost' ? ' selected' : ''}>最低成本</option>
            <option value="loadbalance"${strategy === 'loadbalance' ? ' selected' : ''}>负载均衡</option>
            <option value="classify"${strategy === 'classify' ? ' selected' : ''}>内容分类</option>
          </select>
          <button class="btn" style="padding:3px 8px;font-size:11px;color:var(--danger);" onclick="removeRouterGroup('${name}')">删除</button>
        </div>
      </div>
      <div class="rg-members" id="rg-members-${name}">${memberRows}</div>
      <div class="rg-add" style="display:flex; gap:8px; align-items:center;">${addBlock}</div>
      <div class="rg-hint">${hintBlock}</div>
    </div>`;
  }).join('');
}

function onStrategyChange(name, val) {
  strategiesData[name] = val;
  renderRouterList(); // 重新渲染以切换「模型下拉」/「cat=group 输入」两种成员录入方式
}

function addRouterMember(name) {
  const sel = document.getElementById('rg-add-' + name);
  if (!sel) return;
  const m = sel.value;
  if (!m) return;
  if (!(routersData[name] || []).includes(m)) {
    if (!routersData[name]) routersData[name] = [];
    routersData[name].push(m);
  }
  renderRouterList();
}

// 内容分类路由：成员格式 cat=group（如 code=coders / vision=视觉 / default=general）
function addClassifyMember(name) {
  const inp = document.getElementById('rg-add-' + name);
  if (!inp) return;
  const raw = inp.value.trim();
  if (!raw) return;
  if (!/^[^=\s]+=.*\S$/.test(raw)) {
    showToast('格式应为 cat=group（如 code=coders）', 'warn');
    return;
  }
  if (!(routersData[name] || []).includes(raw)) {
    if (!routersData[name]) routersData[name] = [];
    routersData[name].push(raw);
  }
  renderRouterList();
}

function removeRouterMember(name, model) {
  const m = decodeURIComponent(model);
  routersData[name] = (routersData[name] || []).filter(x => x !== m);
  renderRouterList();
}

// 拖拽排序（调整路由组成员优先级）
let _dragRouter = null, _dragModel = null;
function onRouterMemberDragStart(e, router, model) {
  _dragRouter = router; _dragModel = decodeURIComponent(model);
  e.currentTarget.classList.add('dragging');
  try { e.dataTransfer.setData('text/plain', _dragModel); e.dataTransfer.effectAllowed = 'move'; } catch (err) {}
}
function onRouterMemberDragOver(e) { e.preventDefault(); }
function onRouterMemberDragEnd(e) { e.currentTarget.classList.remove('dragging'); }
function onRouterMemberDrop(e, router, targetModel) {
  e.preventDefault();
  const target = decodeURIComponent(targetModel);
  if (_dragRouter !== router || !_dragModel || _dragModel === target) return;
  const arr = routersData[router] || [];
  const from = arr.indexOf(_dragModel);
  if (from < 0) return;
  arr.splice(from, 1);
  let to = arr.indexOf(target);
  if (to < 0) to = arr.length;
  arr.splice(to, 0, _dragModel);
  routersData[router] = arr;
  _dragRouter = null; _dragModel = null;
  renderRouterList();
}

function addRouterGroup() {
  const name = document.getElementById('newRouterName').value.trim();
  if (!name) { showToast('请输入路由名称', 'warn'); return; }
  if (routersData.hasOwnProperty(name)) { showToast('该路由名称已存在', 'warn'); return; }
  routersData[name] = [];
  strategiesData[name] = 'quality';
  document.getElementById('newRouterName').value = '';
  renderRouterList();
}

function removeRouterGroup(name) {
  if (!confirm('确定删除路由组「' + name + '」？')) return;
  delete routersData[name];
  delete strategiesData[name];
  renderRouterList();
}

async function saveRouters() {
  // 组装为后端可识别的数组格式 [{name, members, strategy}]
  const payload = Object.keys(routersData)
    .filter(name => name !== 'auto')
    .map(name => ({
      name: name,
      members: routersData[name] || [],
      strategy: strategiesData[name] || 'quality'
    }));
  try {
    const r = await apiFetch('/api/routers', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload)
    });
    if (r.ok) showToast('路由配置已保存', 'ok');
    else { showToast('保存失败', 'error'); }
  } catch (e) { if (e.message !== 'Unauthorized') { console.error(e); showToast('保存请求错误', 'error'); } }
}

/* ============ 外链拦截：target=_blank 统一走系统浏览器（pywebview 内会拦截，改调 /api/open-url） ============ */
document.addEventListener('click', function(e) {
  const a = e.target.closest && e.target.closest('a[target="_blank"]');
  if (a && a.href) {
    e.preventDefault();
    apiFetch('/api/open-url', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ url: a.href }) });
  }
});

/* ============ 提供者市场 ============ */
let marketData = null;      // /api/provider-market 返回的目录（随包只读）
let marketSelected = null;  // 从市场点「添加」时选中的提供��（用于预填与免 Key 放行）
let addedProviderIds = null; // 已添加的提供者名称集合（用于「隐藏已添加」过滤）

async function openMarketModal() {
  document.getElementById('marketModal').classList.add('show');
  if (!marketData) {
    try {
      const res = await apiFetch('/api/provider-market');
      if (!res.ok) throw new Error('http ' + res.status);
      marketData = await res.json();
    } catch (e) {
      if (e.message === 'Unauthorized') return;
      document.getElementById('marketList').innerHTML = '<div class="empty" style="color:var(--danger)">加载市场目录失败，请稍后重试</div>';
      return;
    }
  }
  // 预加载已添加提供者列表（用于「隐藏已添加」过滤）
  // 每次都重新加载，确保添加/删除后缓存最新
  try {
    const r = await apiFetch('/api/providers');
    if (r.ok) {
      const list = await r.json();
      addedProviderIds = new Set((list || []).map(p => (p.name || p.id || '').toLowerCase()).filter(Boolean));
    } else {
      addedProviderIds = new Set();
    }
  } catch (e) {
    addedProviderIds = new Set();
  }
  renderMarket();
}

function closeMarketModal() {
  document.getElementById('marketModal').classList.remove('show');
}

function fmtTokens(n) {
  if (!n || n <= 0) return '0';
  if (n >= 1e9) return (n / 1e9).toFixed(n % 1e9 === 0 ? 0 : 1) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(n % 1e6 === 0 ? 0 : 1) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(n % 1e3 === 0 ? 0 : 1) + 'K';
  return '' + n;
}

// 免Key提供者实测状态样式（数据来自 providers_catalog.json 的 keyfree_status，2026-07-31 逐一探测）
const KEYFREE_STATUS_META = {
  verified:     { label: '✅ 实测可用',   bg: 'rgba(52,199,89,.18)',  fg: 'var(--success)' },
  adapter:      { label: '🔌 已内置适配', bg: 'rgba(0,122,255,.15)',  fg: 'var(--accent)'  },
  rate_limited: { label: '⚠ 上游限流',    bg: 'rgba(255,149,0,.18)',  fg: 'var(--warning)' },
  needs_key:    { label: '🔑 实为需密钥', bg: 'rgba(255,149,0,.18)',  fg: 'var(--warning)' },
  incompatible: { label: '⛔ 架构不兼容',  bg: 'rgba(255,59,48,.15)',  fg: 'var(--danger)'  }
};

function renderMarket() {
  if (!marketData) return;
  const kw = (document.getElementById('marketSearch').value || '').trim().toLowerCase();
  const freeOnly = document.getElementById('marketFreeOnly').checked;
  const keylessOnly = document.getElementById('marketKeylessOnly').checked;
  const hideAdded = document.getElementById('marketHideAdded').checked;
  const all = marketData.providers || [];
  const list = all.filter(p => {
    if (freeOnly && !(p.free_models > 0)) return false;
    if (keylessOnly && p.auth === 'apikey') return false;
    if (hideAdded && addedProviderIds && addedProviderIds.has(p.id)) return false;
    if (!kw) return true;
    if (p.id.toLowerCase().includes(kw) || (p.name || '').toLowerCase().includes(kw) || (p.base_url || '').toLowerCase().includes(kw)) return true;
    return (p.models || []).some(m => m.id.toLowerCase().includes(kw) || (m.name || '').toLowerCase().includes(kw));
  });
  // 勾选「只看免Key」时，按实测状态排序：实测可用 > 已内置适配 > 限流 > 未探测 > 架构不兼容
  if (keylessOnly) {
    const rank = { verified: 0, adapter: 1, rate_limited: 2, needs_key: 4, incompatible: 5 };
    list.sort((a, b) => (rank[a.keyfree_status] ?? 2) - (rank[b.keyfree_status] ?? 2));
  }
  const totalFree = all.reduce((s, p) => s + (p.free_models || 0), 0);
  const totalModels = all.reduce((s, p) => s + ((p.models || []).length), 0);
  const totalQuota = all.reduce((s, p) => s + (p.monthly_tokens || 0), 0);
  const hiddenCount = hideAdded && addedProviderIds ? all.filter(p => addedProviderIds.has(p.id)).length : 0;
  document.getElementById('marketStats').innerText =
    '共 ' + all.length + ' 个提供者 / ' + totalModels + ' 个已收录模型 / ' + totalFree + ' 个免费模型' +
    (totalQuota > 0 ? (' / 🎁 免费额度 ' + fmtTokens(totalQuota) + '/月') : '') +
    (hiddenCount > 0 ? (' / 已隐藏 ' + hiddenCount + ' 个已添加') : '') +
    (kw || freeOnly || keylessOnly || hideAdded ? ('，当前筛选出 ' + list.length + ' 个') : '');
  if (list.length === 0) {
    document.getElementById('marketList').innerHTML = '<div class="empty">没有匹配的提供者</div>';
    return;
  }
  let html = '';
  list.forEach(p => {
    const badges = [];
    if (p.free_models > 0) badges.push('<span style="font-size:11px; padding:1px 6px; border-radius:8px; background:rgba(52,199,89,.15); color:var(--success);">免费 ' + p.free_models + '</span>');
    if (p.auth !== 'apikey') badges.push('<span style="font-size:11px; padding:1px 6px; border-radius:8px; background:rgba(0,122,255,.12); color:var(--accent);">免 Key</span>');
    if (p.monthly_tokens > 0) badges.push('<span style="font-size:11px; padding:1px 6px; border-radius:8px; background:rgba(255,149,0,.15); color:var(--warning);">🎁 ' + fmtTokens(p.monthly_tokens) + '/月</span>');
    if (p.tos === 'avoid') badges.push('<span style="font-size:11px; padding:1px 6px; border-radius:8px; background:rgba(255,59,48,.15); color:var(--danger);">⚠ ToS 限制</span>');
    // 2026-07-31 逐一实测标注：让用户在添加前就知道该「免Key」提供者是否真能用
    const st = KEYFREE_STATUS_META[p.keyfree_status];
    if (st) badges.push('<span style="font-size:11px; padding:1px 6px; border-radius:8px; background:' + st.bg + '; color:' + st.fg + ';">' + st.label + '</span>');
    const mc = (p.models || []).length;
    const sub = escapeHtml(p.base_url) + (mc > 0 ? (' · 收录 ' + mc + ' 个模型') : ' · 添加后自动拉取模型');
    const note = p.keyfree_note ? escapeHtml(p.keyfree_note) : '';
    const unusable = p.keyfree_status === 'incompatible';
    html += `
    <div title="${note}" style="display:flex; align-items:center; gap:10px; padding:10px 12px; background:var(--mac-window-bg); border:1px solid var(--mac-border); border-radius:8px;${unusable ? ' opacity:.62;' : ''}">
      <div style="flex:1; min-width:0;">
        <div style="font-weight:600; font-size:13px; display:flex; align-items:center; gap:6px; flex-wrap:wrap;">${escapeHtml(p.name || p.id)} ${badges.join(' ')}</div>
        <div style="font-size:11px; color:var(--text-secondary); overflow:hidden; text-overflow:ellipsis; white-space:nowrap;">${sub}</div>
        ${note ? `<div style="font-size:11px; color:var(--text-secondary); margin-top:3px; overflow:hidden; text-overflow:ellipsis; white-space:nowrap;">💡 ${note}</div>` : ''}
      </div>
      <button class="btn ${unusable ? 'btn-secondary' : 'btn-primary'}" style="padding:5px 12px; font-size:12px; flex-shrink:0;" onclick="marketAdd('${escJsStr(p.id)}')">${unusable ? '仍要添加' : '添加'}</button>
    </div>`;
  });
  document.getElementById('marketList').innerHTML = html;
}

async function marketAdd(pid) {
  const p = ((marketData && marketData.providers) || []).find(x => x.id === pid);
  if (!p) return;
  closeMarketModal();
  await openProviderEditor();  // add 模式（内部会清空 marketSelected，故在其后再赋值）
  marketSelected = p;
  document.getElementById('pf_name').value = p.id;
  document.getElementById('pf_base_url').value = p.base_url;
  document.getElementById('pf_auth_header').value = p.auth_header || '';
  document.getElementById('pf_auth_header').value = p.auth_header || '';
  document.getElementById('pf_auth_scheme').value = p.auth_scheme || (p.auth !== 'apikey' ? 'none' : '');
  document.getElementById('pf_format').value = p.format || 'openai';
  const keyInput = document.getElementById('pf_api_key');
  const keyField = document.getElementById('pf_api_key_field');
  if (p.auth === 'none' || p.auth === 'optional') {
    // 免 Key / 可选 Key 提供者：显示密钥框但允许留空。
    // 注意：部分标称免Key的提供者（如 Hackclub）实际仍需密钥，
    // 因此保留填入入口；若连接失败提示需鉴权，再补填即可。
    if (keyField) keyField.style.display = '';
    keyInput.placeholder = '可留空（免Key）；若连接失败提示需鉴权，请在此填入密钥';
  } else {
    if (keyField) keyField.style.display = '';
    keyInput.placeholder = 'sk-...';
  }
  // 从市场添加时，若该提供者有免费模型则默认勾选"自动选择免费模型"；纯付费提供者则取消勾选
  document.getElementById('pf_free_only').checked = (p.free_models || 0) > 0;
  const hints = [];
  if (p.free_models > 0) hints.push('含 ' + p.free_models + ' 个免费模型');
  if ((p.models || []).length > 0) hints.push('目录已收录 ' + p.models.length + ' 个模型');
  if (p.auth_header) hints.push('鉴权头: ' + p.auth_header + (p.auth_scheme ? (' (' + p.auth_scheme + ')') : ''));
  const hintEl = document.getElementById('providerModalHint');
  hintEl.innerText = hints.join('，');
  // 附上 2026-07-31 实测结论，说明该提供者到底能不能免Key、不能的原因是什么
  if (p.keyfree_note) {
    const meta = KEYFREE_STATUS_META[p.keyfree_status];
    hintEl.innerText += (hints.length ? '\n' : '') + (meta ? meta.label + ' ' : '') + p.keyfree_note;
  }
}

/* ============ 预设一键配置 ============ */
let _presetData = null;
let _presetKeyStatus = {};
let _existingProviders = [];

async function openPresetModal() {
  document.getElementById('presetModal').classList.add('show');
  const body = document.getElementById('presetBody');
  body.innerHTML = '<div style="text-align:center; padding:24px; color:var(--text-secondary);">加载预设中…</div>';
  document.getElementById('presetApplyBtn').disabled = true;
  document.getElementById('presetVersionHint').textContent = '';
  try {
    const [r, pr] = await Promise.all([
      (await apiFetch('/api/preset-info')).json(),
      (await apiFetch('/api/providers')).json().catch(() => [])
    ]);
    if (!r || !r.platforms) {
      body.innerHTML = '<div style="text-align:center; padding:32px; color:var(--text-secondary);"><div style="font-size:18px; margin-bottom:8px;">当前未开启预设配置</div><div style="font-size:13px;">预设由开发者维护，请稍后再试，或在上方使用「+ 添加提供商」手动配置。</div></div>';
      document.getElementById('presetApplyBtn').disabled = true;
      return;
    }
    _presetData = r;
    _existingProviders = Array.isArray(pr) ? pr : [];
    _presetKeyStatus = {};
    document.getElementById('presetVersionHint').textContent = '预设版本 ' + (r.version || '');
    renderPresetCards(r);
  } catch (e) {
    body.innerHTML = '<div style="text-align:center; padding:32px; color:var(--danger);">加载预设失败：' + (e.message||'') + '</div>';
  }
}

function renderPresetCards(preset) {
  const body = document.getElementById('presetBody');
  const existingNames = new Set(_existingProviders.map(p => p.name));
  let html = '<div class="preset-intro preset-notice">已配置的平台可留空 Key 保留原配置；填入新 Key 会覆盖旧配置。</div>';
  html += '<div class="preset-warn" id="presetWarn"></div>';
  html += '<div class="preset-cards">';
  for (const [name, cfg] of Object.entries(preset.platforms)) {
    const hasExisting = existingNames.has(name);
    const badge = hasExisting ? '<span class="preset-card-badge" style="color:var(--success);">已有配置</span>' : '<span class="preset-card-badge">' + escapeHtml(cfg.auth_hint || '无需认证') + '</span>';
    const placeholder = hasExisting ? '留空保留原配置，或填入新 Key 覆盖' : '填入 ' + escAttr(name) + ' 的 API Key';
    html += '<div class="preset-card">';
    html += '<div class="preset-card-head">';
    html += '<span class="preset-card-name">' + escapeHtml(name) + '</span>';
    html += badge;
    html += '</div>';
    html += '<div class="preset-card-row">';
    html += '<input type="password" class="preset-key-input" id="preset_key_' + escAttr(name) + '" placeholder="' + placeholder + '" oninput="resetPresetKeyStatus(\'' + escJsStr(name) + '\')">';
    html += '<button class="btn" style="padding:4px 10px;font-size:12px;" onclick="verifyPresetKey(\'' + escJsStr(name) + '\')">测试</button>';
    html += '<button class="btn" style="padding:4px 10px;font-size:12px;" onclick="openPresetKeyPage(\'' + escJsStr(name) + '\')">官网</button>';
    html += '</div>';
    html += '<div class="preset-card-status" id="preset_status_' + escAttr(name) + '"></div>';
    html += '</div>';
  }
  html += '</div>';
  body.innerHTML = html;
  updatePresetApplyBtn();
}

function resetPresetKeyStatus(name) {
  _presetKeyStatus[name] = null;
  const el = document.getElementById('preset_status_' + name);
  if (el) el.innerHTML = '';
  updatePresetApplyBtn();
}

async function verifyPresetKey(name) {
  const cfg = (_presetData && _presetData.platforms && _presetData.platforms[name]) || null;
  if (!cfg) return;
  const keyEl = document.getElementById('preset_key_' + name);
  const key = (keyEl && keyEl.value) || '';
  const stEl = document.getElementById('preset_status_' + name);
  if (!key.trim()) { if (stEl) stEl.innerHTML = '<span style="color:var(--danger);">请先填写 Key</span>'; return; }
  if (stEl) stEl.innerHTML = '<span style="color:var(--text-secondary);">校验中…</span>';
  try {
    const r = await (await apiFetch('/api/providers/verify-key', { method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({ base_url: cfg.base_url, api_key: key }) })).json();
    _presetKeyStatus[name] = r;
    if (r && r.ok) stEl.innerHTML = '<span style="color:var(--success);">✓ ' + (r.detail||'连接成功') + '</span>';
    else stEl.innerHTML = '<span style="color:var(--danger);">✗ ' + (r && r.detail || '校验失败') + '</span>';
  } catch (e) {
    _presetKeyStatus[name] = { ok:false, detail:e.message };
    if (stEl) stEl.innerHTML = '<span style="color:var(--danger);">✗ ' + e.message + '</span>';
  }
  updatePresetApplyBtn();
}

function openPresetKeyPage(name) {
  const cfg = (_presetData && _presetData.platforms && _presetData.platforms[name]) || null;
  if (cfg && cfg.key_page_url) apiFetch('/api/open-url', { method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({ url: cfg.key_page_url }) });
}

function updatePresetApplyBtn() {
  const btn = document.getElementById('presetApplyBtn');
  const warn = document.getElementById('presetWarn');
  if (!btn || !_presetData) return;
  const names = Object.keys(_presetData.platforms);
  const existingNames = new Set(_existingProviders.map(p => p.name));
  // 已填 Key 的平台
  const filled = names.filter(name => {
    const el = document.getElementById('preset_key_' + name);
    return el && el.value.trim();
  });
  // 只要填了至少一个 Key（或已有配置可被保留），就允许一键应用；未填的平台自动跳过，不再强制全填
  const canApply = filled.length > 0 || existingNames.size > 0;
  btn.disabled = !canApply;
  if (warn) {
    if (filled.length === 0 && existingNames.size === 0) {
      warn.textContent = '请至少填写一个平台的 Key 后再一键应用';
    } else {
      const skipped = names.filter(name => {
        const el = document.getElementById('preset_key_' + name);
        return (!el || !el.value.trim()) && !existingNames.has(name);
      });
      warn.textContent = skipped.length ? '未填写 Key 且无已有配置的平台将自动跳过：' + skipped.join('、') : '';
    }
  }
}

async function applyPreset() {
  const btn = document.getElementById('presetApplyBtn');
  if (!_presetData) return;
  const keys = {};
  const existingNames = new Set(_existingProviders.map(p => p.name));
  const willOverwrite = [];
  for (const name of Object.keys(_presetData.platforms)) {
    const el = document.getElementById('preset_key_' + name);
    const v = (el && el.value || '').trim();
    if (v) {
      keys[name] = v;
      if (existingNames.has(name)) willOverwrite.push(name);
    }
    // 未填的 Key：不报错、不阻断，直接跳过（后端对“未填且无已存在配置”的平台自动忽略）
  }
  if (Object.keys(keys).length === 0) {
    showToast('请至少填写一个平台的 Key 后再一键应用', 'warn');
    return;
  }
  // 有覆盖操作时弹确认框
  if (willOverwrite.length) {
    const confirmMsg = '以下平台已存在配置，填入新 Key 将覆盖旧配置：\n' + willOverwrite.join('、') + '\n\n确定要覆盖吗？';
    if (!confirm(confirmMsg)) return;
  }
  btn.disabled = true; btn.textContent = '配置中请耐心等待…';
  closePresetModal();  // 关闭一键配置窗口
  document.getElementById('presetWaitModal').classList.add('show');  // 弹出等待提示
  try {
    const r = await (await apiFetch('/api/providers/preset', { method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({ keys }) })).json();
    if (r && r.ok) {
      let msg = '';
      if (r.results) {
        const okList = Object.entries(r.results).filter(([n, x]) => x.ok).map(([n, x]) => n + ': ' + x.detail);
        const failList = Object.entries(r.results).filter(([n, x]) => !x.ok).map(([n, x]) => n + ': ' + x.detail);
        if (okList.length) msg += okList.join('\n');
        if (failList.length) msg += (msg ? '\n' : '') + '未成功:\n' + failList.join('\n');
      }
      showToast('一键配置完成\n' + msg, 'ok');
      if (typeof load === 'function') load();
      if (typeof checkAll === 'function') checkAll();
    } else {
      showToast('配置未完全成功', 'error');
    }
  } catch (e) {
    showToast('应用失败：' + e.message, 'error');
  } finally {
    btn.disabled = false; btn.textContent = '一键应用';
  }
}

function closePresetModal() {
  document.getElementById('presetModal').classList.remove('show');
}
function closePresetWaitModal() {
  document.getElementById('presetWaitModal').classList.remove('show');
}

async function testProviderKey() {
  const base_url = (document.getElementById('pf_base_url') || {}).value || '';
  const api_key = (document.getElementById('pf_api_key') || {}).value || '';
  const st = document.getElementById('pf_verify_status');
  if (!base_url.trim() || !api_key.trim()) { if (st) st.innerHTML = '<span style="color:var(--danger);">请先填写 Base URL 和 API Key</span>'; return; }
  if (st) st.innerHTML = '<span style="color:var(--text-secondary);">校验中…</span>';
  try {
    const r = await (await apiFetch('/api/providers/verify-key', { method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({ base_url, api_key }) })).json();
    if (r && r.ok) st.innerHTML = '<span style="color:var(--success);">✓ ' + (r.detail||'连接成功') + '</span>';
    else st.innerHTML = '<span style="color:var(--danger);">✗ ' + (r && r.detail || '校验失败') + '</span>';
  } catch (e) {
    if (st) st.innerHTML = '<span style="color:var(--danger);">✗ ' + e.message + '</span>';
  }
}

/* ============ 系统公告 ============ */
function renderMarkdown(md) {
  if (!md || !md.trim()) return '<div class="empty">暂无公告</div>';

  let html = md.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');

  const codeBlocks = [];
  html = html.replace(/```([\s\S]*?)```/g, (m, c) => {
    const idx = codeBlocks.length;
    codeBlocks.push('<pre><code>' + c.replace(/^\n/, '').replace(/\n$/, '') + '</code></pre>');
    return '\x00CODEBLOCK' + idx + '\x00';
  });

  const inlineCodes = [];
  html = html.replace(/`([^`]+)`/g, (m, c) => {
    const idx = inlineCodes.length;
    inlineCodes.push('<code>' + c + '</code>');
    return '\x00INLINECODE' + idx + '\x00';
  });

  html = html.replace(/^#### (.*)$/gm, '<h4>$1</h4>');
  html = html.replace(/^### (.*)$/gm, '<h3>$1</h3>');
  html = html.replace(/^## (.*)$/gm, '<h2>$1</h2>');
  html = html.replace(/^# (.*)$/gm, '<h1>$1</h1>');
  html = html.replace(/\*\*(.+?)\*\*/g, '<strong>$1</strong>');
  html = html.replace(/\[([^\]]+)\]\(([^)]+)\)/g, (m, text, url) => {
    return '<a href="' + safeHref(url) + '" target="_blank" rel="noopener noreferrer">' + text + '</a>';
  });
  // 裸 URL 自动转链接（排除已被 href=" 包裹的，避免重复匹配）
  html = html.replace(/(^|[\s>])(https?:\/\/[^\s<"'）。，；！？、]+)(?=\s|$|<)/gm, (m, pre, url) => {
    return pre + '<a href="' + safeHref(url) + '" target="_blank" rel="noopener noreferrer">' + url + '</a>';
  });
  html = html.replace(/^---$/gm, '<hr>');
  html = html.replace(/^&gt; (.*)$/gm, '<blockquote>$1</blockquote>');

  // Tables
  const lines = html.split('\n');
  let out = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i].trim();
    if (/^\|.+\|$/.test(line)) {
      let rows = [];
      rows.push(line);
      i++;
      while (i < lines.length && /^\|.*\|$/.test(lines[i].trim())) {
        rows.push(lines[i].trim());
        i++;
      }
      if (rows.length >= 2 && /^\|(\s*[-:]+[\s-|:]*\|)+$/.test(rows[1])) {
        const headerCells = rows[0].split('|').slice(1, -1).map(c => c.trim());
        const bodyRows = rows.slice(2).map(row => {
          return '<tr>' + row.split('|').slice(1, -1).map(c => '<td>' + c.trim() + '</td>').join('') + '</tr>';
        });
        const headerHtml = '<thead><tr>' + headerCells.map(c => '<th>' + c + '</th>').join('') + '</tr></thead>';
        const bodyHtml = bodyRows.length ? '<tbody>' + bodyRows.join('') + '</tbody>' : '';
        out.push('<table class="announcement-table">' + headerHtml + bodyHtml + '</table>');
        continue;
      } else {
        for (const r of rows) out.push(r);
      }
    } else {
      out.push(lines[i]);
      i++;
    }
  }
  html = out.join('\n');

  // Lists
  const listLines = html.split('\n');
  out = [];
  let inList = false;
  for (let i = 0; i < listLines.length; i++) {
    const line = listLines[i];
    if (/^- /.test(line)) {
      if (!inList) { out.push('<ul>'); inList = true; }
      out.push('<li>' + line.slice(2) + '</li>');
    } else if (line.trim() === '' && inList) {
      // 空行：往后看一行，如果还是列表项就保留 ul（避免空行断开列表导致巨间距）
      let j = i + 1;
      while (j < listLines.length && listLines[j].trim() === '') j++;
      if (j < listLines.length && /^- /.test(listLines[j])) {
        continue;  // 跳过空行，ul 保持打开
      }
      if (inList) { out.push('</ul>'); inList = false; }
      out.push(line);
    } else {
      if (inList) { out.push('</ul>'); inList = false; }
      out.push(line);
    }
  }
  if (inList) out.push('</ul>');
  html = out.join('\n');

  // Paragraphs
  html = html.split(/\n\n+/).map(b => {
    const t = b.trim();
    if (!t) return '';
    if (/^<(h\d|ul|pre|hr|blockquote|table)/.test(t)) return t;
    return '<p>' + t.replace(/\n/g, '<br>') + '</p>';
  }).join('\n');

  html = html.replace(/\x00CODEBLOCK(\d+)\x00/g, (m, idx) => codeBlocks[idx]);
  html = html.replace(/\x00INLINECODE(\d+)\x00/g, (m, idx) => inlineCodes[idx]);
  return html;
}

function getSeenAnnouncementHash() {
  try { return localStorage.getItem('announcement_seen_hash') || ''; } catch (e) { return ''; }
}
function setSeenAnnouncementHash(h) {
  try { if (h) localStorage.setItem('announcement_seen_hash', h); } catch (e) {}
}

/* 把已获取的公告数据渲染进弹窗（不负责显示/隐藏） */
function renderAnnouncement(d) {
  const body = document.getElementById('announcementBody');
  const meta = document.getElementById('announcementMeta');
  if (meta) meta.style.display = 'none';
  if (!d || !d.ok) {
    body.innerHTML = '<div class="empty">' + escapeHtml((d && d.content) || '暂无公告') + '</div>';
    return;
  }
  body.innerHTML = renderMarkdown(d.content || '');
  // 渲染即视为已读，刷新后不再自动弹出（除非公告内容变化导致 hash 改变）
  if (d.hash) setSeenAnnouncementHash(d.hash);
}

/* 手动点击“系统公告”按钮：立即打开，再加载内容（用户主动触发，允许短暂加载态） */
async function openFeishuModal() {
  const body = document.getElementById('announcementBody');
  const meta = document.getElementById('announcementMeta');
  if (meta) meta.style.display = 'none';
  body.innerHTML = '<div class="empty">加载中...</div>';
  document.getElementById('feishuModal').classList.add('show');
  try {
    const r = await apiFetch('/api/announcement');
    const d = await r.json();
    renderAnnouncement(d);
  } catch (e) {
    if (e.message !== 'Unauthorized') { console.error(e); body.innerHTML = '<div class="empty" style="color:var(--danger)">加载失败</div>'; }
  }
}
function closeFeishuModal() {
  document.getElementById('feishuModal').classList.remove('show');
}

/* 有未读公告时才自动弹：单次请求→先渲染内容→再显示，避免遮罩闪烁；已读/空内容不显示 */
async function autoShowAnnouncement(d) {
  if (!d || !d.ok || !d.hash) return;                       // 无公告 → 不显示遮罩
  if (getSeenAnnouncementHash() === d.hash) return;         // 已读 → 不显示遮罩
  if (!d.content || !d.content.trim()) {                    // 空内容 → 记为已读，不显示
    setSeenAnnouncementHash(d.hash);
    return;
  }
  renderAnnouncement(d);                                    // 先把内容渲染进弹窗
  document.getElementById('feishuModal').classList.add('show'); // 内容就绪后再显示，无“加载中”闪烁
}

/* 仅在有未读公告时自动弹出：首次打开网页弹一次，刷新不再弹，公告更新才会再弹 */
async function maybeAutoOpenAnnouncement() {
  try {
    const r = await apiFetch('/api/announcement');
    const d = await r.json();
    autoShowAnnouncement(d);
  } catch (e) {}
}

/* 定时检测公告变动：内容变化（hash 不同于已读）时自动弹出 */
function startAnnouncementWatcher() {
  setInterval(async function() {
    try {
      const r = await apiFetch('/api/announcement');
      const d = await r.json();
      autoShowAnnouncement(d);
    } catch(e) {}
  }, 60000);
}

/* ============ 上下文长度 inline 编辑 ============ */
document.addEventListener('click', e => {
  const cell = e.target.closest('.ctx-cell');
  if (!cell || cell.querySelector('input')) return;
  const model = cell.dataset.model;
  const raw = cell.textContent.replace(/,/g, '').trim();
  const input = document.createElement('input');
  input.type = 'number';
  input.value = raw === '--' ? '' : raw;
  input.style.cssText = 'width:100px;padding:2px 4px;font-size:12px;font-family:ui-monospace,monospace;border:1px solid var(--accent-primary);border-radius:4px;background:#fff;color:var(--text-primary);outline:none;box-sizing:border-box;';
  cell.textContent = '';
  cell.appendChild(input);
  input.focus(); input.select();
  let done = false;
  const finish = async (save) => {
    if (done) return; done = true;
    if (!save) { cell.textContent = raw; return; }
    const v = parseInt(input.value, 10);
    if (!v || v <= 0) { cell.textContent = raw; return; }
    try {
      const r = await apiFetch('/api/context-limits', {
        method: 'PUT', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ model: model, context_length: v })
      });
      if (r.ok) {
        showToast(`已更新 ${model} = ${v}`, 'ok');
        if (modelDetails[model]) modelDetails[model].context_length = v;
        else modelDetails[model] = { context_length: v };
        cell.textContent = String(v);
        loadStability();
      }
      else { showToast('更新失败', 'error'); cell.textContent = raw; }
    } catch (e) {
      if (e.message !== 'Unauthorized') { console.error(e); showToast('请求错误', 'error'); }
      cell.textContent = raw;
    }
  };
  input.addEventListener('blur', () => finish(true));
  input.addEventListener('keydown', ev => {
    if (ev.key === 'Enter') { ev.preventDefault(); input.blur(); }
    if (ev.key === 'Escape') { ev.preventDefault(); finish(false); }
  });
});

/* ============ 在线更新 ============ */
let _updateInfo = null;
let _updatePollTimer = null;

async function checkUpdate(silent) {
  try {
    const r = await apiFetch('/api/check-update');
    const d = await r.json();
    if (!d.ok || !d.has_update) {
      if (!silent) { showToast('已是最新版本 v' + (d.current || '1.9.0'), 'ok'); }
      return;
    }
    _updateInfo = d;
    document.getElementById('updateLatestVer').textContent = d.latest;
    const notesEl = document.getElementById('updateNotes');
    if (d.release_notes) {
      notesEl.textContent = d.release_notes;
      notesEl.style.display = 'block';
    } else {
      notesEl.style.display = 'none';
    }
    document.getElementById('updateTitle').textContent = d.force_update ? '⚠️ 必须更新' : '🔔 发现新版本';
    document.getElementById('updateDoBtn').textContent = '立即更新';
    document.getElementById('updateProgress').style.display = 'none';
    document.getElementById('updateError').style.display = 'none';
    if (d.self_update === false) {
      // F3 修复：本设备（OpenWrt/iStoreOS）不支持一键在线更新，
      // 隐藏无效的“立即更新”按钮，改为给出经 iStore/opkg 手动升级的明确指引。
      document.getElementById('updateActions').style.display = 'none';
      const errEl = document.getElementById('updateError');
      errEl.textContent = '本设备（OpenWrt/iStoreOS 软路由）不支持一键在线更新，请通过 iStore 应用商店或 opkg 升级到最新版本。';
      errEl.style.display = 'block';
    } else {
      document.getElementById('updateActions').style.display = 'flex';
    }
    document.getElementById('updateSkipBtn').style.display = d.force_update ? 'none' : '';
    document.getElementById('updateModal').classList.add('show');
  } catch (e) {
    if (!silent) { showToast('检查更新失败: ' + e.message, 'danger'); }
  }
}

async function doUpdate() {
  if (!_updateInfo || !_updateInfo.download_url) {
    showToast('缺少下载地址', 'danger'); return;
  }
  document.getElementById('updateActions').style.display = 'none';
  document.getElementById('updateProgress').style.display = 'block';
  document.getElementById('updateProgressFill').style.width = '0%';
  document.getElementById('updateProgressText').textContent = '正在连接...';
  document.getElementById('updateSkipBtn').style.display = 'none';
  try {
    const r = await apiFetch('/api/start-download', { method:'POST', body: JSON.stringify({url: _updateInfo.download_url}) });
    const d = await r.json();
    if (!d.ok) { throw new Error(d.error || '启动下载失败'); }
    _updatePollTimer = setInterval(pollUpdateProgress, 500);
  } catch (e) {
    document.getElementById('updateError').textContent = '下载失败: ' + e.message;
    document.getElementById('updateError').style.display = 'block';
    document.getElementById('updateActions').style.display = 'flex';
    document.getElementById('updateDoBtn').textContent = '重试';
    document.getElementById('updateProgress').style.display = 'none';
  }
}

async function pollUpdateProgress() {
  try {
    const r = await apiFetch('/api/download-progress');
    const d = await r.json();
    if (!d.ok) return;
    if (d.error) {
      clearInterval(_updatePollTimer);
      document.getElementById('updateError').textContent = d.error;
      document.getElementById('updateError').style.display = 'block';
      document.getElementById('updateActions').style.display = 'flex';
      document.getElementById('updateDoBtn').textContent = '重试';
      document.getElementById('updateProgress').style.display = 'none';
      return;
    }
    const pct = d.progress || 0;
    document.getElementById('updateProgressFill').style.width = pct + '%';
    if (d.total > 0) {
      const mbDone = ((d.progress / 100) * d.total / 1048576).toFixed(1);
      const mbTotal = (d.total / 1048576).toFixed(1);
      document.getElementById('updateProgressText').textContent = pct + '%（' + mbDone + ' MB / ' + mbTotal + ' MB）';
    } else {
      document.getElementById('updateProgressText').textContent = pct + '%';
    }
    if (d.done) {
      clearInterval(_updatePollTimer);
      document.getElementById('updateProgressText').textContent = '下载完成，正在安装...';
      setTimeout(applyUpdateNow, 300);
    }
  } catch (e) {}
}

async function applyUpdateNow() {
  try {
    const r = await apiFetch('/api/apply-update', { method:'POST' });
    const d = await r.json();
    if (!d.ok) { throw new Error(d.error); }
  } catch (e) {
    document.getElementById('updateError').textContent = '安装失败: ' + e.message;
    document.getElementById('updateError').style.display = 'block';
  }
}

function closeUpdateModal() {
  document.getElementById('updateModal').classList.remove('show');
  if (_updatePollTimer) { clearInterval(_updatePollTimer); _updatePollTimer = null; }
}

/* ============ 启动 ============ */
/* P0 安全修复：/api/config 匿名请求只返回掩码，完整密钥必须带 Bearer 才下发。
   启动时先用 localStorage 里记住的密钥自检；无密钥或密钥失效则弹密钥门。 */
async function fetchLocalInfo() {
  const saved = loadSavedToken();
  if (!saved) return false;
  try {
    const r = await fetch("/api/config", { headers: { 'Authorization': 'Bearer ' + saved } });
    const d = await r.json();
    if (d && d.admin_key === saved) {
      _adminKey = saved;
      const keyEl = document.getElementById("localApiKey");
      if (keyEl) keyEl.textContent = saved;
      return true;
    }
  } catch (e) {}
  clearToken();
  return false;
}

function startApp() {
  restoreHideFailedToggle();
  loadModelDetails(); load(); updatePollStatus(); loadPollStrategy(); startPolling();
  maybeAutoOpenAnnouncement();
  setTimeout(function(){ checkUpdate(true); }, 2000);
  setTimeout(startAnnouncementWatcher, 5000);
}
switchMainTab('monitor');
fetchLocalInfo().then(function (ok) {
  if (ok) { window._appStarted = true; startApp(); }
  else { showGate(); }
});
