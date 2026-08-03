
/* ============ G3 Playground ============ */
let pgCtrl = null;

async function openPlayground() {
  document.getElementById('playgroundModal').classList.add('show');
  await pgLoadModels();
  await pgLoadTemplates();
}
function closePlayground() {
  document.getElementById('playgroundModal').classList.remove('show');
  if (pgCtrl) pgCtrl.abort();
}
async function pgLoadModels() {
  const sel = document.getElementById('pg_model');
  sel.innerHTML = '<option value="">— 选择模型 —</option>';
  try {
    const list = await (await apiFetch('/api/providers')).json();
    const seen = {};
    (list || []).forEach(p => (p.models || []).forEach(m => {
      if (seen[m]) return; seen[m] = 1;
      const o = document.createElement('option'); o.value = m; o.textContent = m; sel.appendChild(o);
    }));
  } catch (e) { if (e.message !== 'Unauthorized') console.error(e); }
}
async function pgLoadTemplates() {
  const sel = document.getElementById('pg_template');
  sel.innerHTML = '<option value="">— 不使用 —</option>';
  try {
    const list = await (await apiFetch('/api/templates')).json();
    (list || []).forEach(t => {
      const o = document.createElement('option'); o.value = t.id; o.textContent = t.name; sel.appendChild(o);
    });
  } catch (e) { if (e.message !== 'Unauthorized') console.error(e); }
}
async function pgApplyTemplate() {
  const id = document.getElementById('pg_template').value;
  const box = document.getElementById('pg_vars');
  box.innerHTML = '';
  if (!id) return;
  try {
    const list = await (await apiFetch('/api/templates')).json();
    const t = (list || []).find(x => x.id === id);
    if (!t) return;
    document.getElementById('pg_system').value = t.content || '';
    const vars = (t.content.match(/\{\{\s*([\w\u4e00-\u9fa5]+)\s*\}\}/g) || []).map(s => s.replace(/[{}]/g, '').trim());
    const uniq = [...new Set(vars)];
    uniq.forEach(v => {
      const wrap = document.createElement('div'); wrap.style.marginBottom = '6px';
      wrap.innerHTML = '<label style="font-size:12px;color:var(--text-secondary);">' + v + '</label>' +
        '<input type="text" class="pg_var" data-var="' + v + '" style="width:100%;" placeholder="填入 ' + v + '">';
      box.appendChild(wrap);
    });
    if (uniq.length) showToast('已载入模板，请在下方填入变量', 'ok');
  } catch (e) { if (e.message !== 'Unauthorized') console.error(e); }
}
function pgSubstitute(text) {
  const inputs = document.querySelectorAll('#pg_vars .pg_var');
  inputs.forEach(inp => {
    const v = inp.getAttribute('data-var');
    const val = inp.value || '';
    text = text.split('{{' + v + '}}').join(val).split('{{ ' + v + ' }}').join(val);
  });
  return text;
}
async function pgSend() {
  const model = document.getElementById('pg_model').value;
  let system = document.getElementById('pg_system').value;
  const user = document.getElementById('pg_user').value;
  if (!model || !user.trim()) { showToast('请选择模型并输入内容', 'error'); return; }
  system = pgSubstitute(system);
  const messages = [];
  if (system.trim()) messages.push({ role: 'system', content: system });
  messages.push({ role: 'user', content: user });
  const out = document.getElementById('pg_output');
  out.textContent = '';
  document.getElementById('pg_status').textContent = '请求中…';
  pgCtrl = new AbortController();
  document.getElementById('pg_send').disabled = true;
  document.getElementById('pg_stop').disabled = false;
  const t0 = Date.now();
  // U3 修复：测试台超时自动取消（5 分钟），避免流式请求无限挂起。
  const pgTimeout = setTimeout(() => { if (pgCtrl) pgCtrl.abort(); }, 5 * 60 * 1000);
  try {
    const resp = await fetch('/v1/chat/completions', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'Authorization': 'Bearer ' + getToken() },
      body: JSON.stringify({ model, messages, stream: true }),
      signal: pgCtrl.signal
    });
    clearTimeout(pgTimeout);
    if (!resp.ok) {
      const errTxt = await resp.text();
      if (resp.status === 401) {
        clearToken();
        showGate();
        out.textContent = '密钥已失效，请重新验证管理密钥。';
        document.getElementById('pg_status').textContent = '未授权';
        return;
      }
      out.textContent = '[HTTP ' + resp.status + '] ' + errTxt;
      document.getElementById('pg_status').textContent = '失败';
      return;
    }
    const reader = resp.body.getReader();
    const decoder = new TextDecoder();
    let buf = '', usage = null, chars = 0;
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      let idx;
      while ((idx = buf.indexOf('\n\n')) >= 0) {
        const chunk = buf.slice(0, idx); buf = buf.slice(idx + 2);
        if (chunk.startsWith('data:')) {
          const data = chunk.slice(5).trim();
          if (data === '[DONE]') continue;
          try {
            const j = JSON.parse(data);
            const d = j.choices && j.choices[0] && j.choices[0].delta;
            if (d && d.content) { out.textContent += d.content; chars += d.content.length; out.scrollTop = out.scrollHeight; }
            if (j.usage) usage = j.usage;
          } catch (e) {}
        }
      }
    }
    let st = '完成 · ' + ((Date.now() - t0) / 1000).toFixed(1) + 's';
    if (usage) st += ' · ~' + ((usage.prompt_tokens || 0)) + '+' + (usage.completion_tokens || 0) + ' tok';
    else if (chars) st += ' · ~' + chars + ' 字符';
    document.getElementById('pg_status').textContent = st;
  } catch (e) {
    if (e.name !== 'AbortError') { out.textContent += '\n[error] ' + e.message; out.scrollTop = out.scrollHeight; document.getElementById('pg_status').textContent = '错误'; }
  } finally {
    document.getElementById('pg_send').disabled = false;
    document.getElementById('pg_stop').disabled = true;
  }
}
function pgStop() { if (pgCtrl) pgCtrl.abort(); }
function pgClear() { document.getElementById('pg_output').textContent = ''; document.getElementById('pg_user').value = ''; document.getElementById('pg_status').textContent = ''; }

/* ============ G4 提示词模板 CRUD ============ */
async function openTemplates() {
  document.getElementById('templatesModal').classList.add('show');
  await tmplRender();
}
function closeTemplates() { document.getElementById('templatesModal').classList.remove('show'); }
async function tmplRender() {
  const box = document.getElementById('tmpl_list');
  box.innerHTML = '<div class="empty">加载中…</div>';
  try {
    const list = await (await apiFetch('/api/templates')).json();
    if (!list || !list.length) { box.innerHTML = '<div class="empty">还没有模板，写一条保存吧。</div>'; return; }
    box.innerHTML = '';
    list.forEach(t => {
      const row = document.createElement('div');
      row.style.cssText = 'display:flex; gap:10px; align-items:center; padding:8px 0; border-bottom:1px solid var(--border);';
      const name = document.createElement('div');
      name.style.cssText = 'flex:1; font-weight:600;';
      name.textContent = t.name;
      const edit = document.createElement('button'); edit.className = 'btn'; edit.style.fontSize = '12px'; edit.textContent = '编辑';
      edit.onclick = () => { document.getElementById('tmpl_name').value = t.name; document.getElementById('tmpl_content').value = t.content || ''; };
      const del = document.createElement('button'); del.className = 'btn'; del.style.fontSize = '12px'; del.textContent = '删除';
      del.onclick = () => tmplDelete(t.id);
      row.appendChild(name); row.appendChild(edit); row.appendChild(del);
      box.appendChild(row);
    });
  } catch (e) { if (e.message !== 'Unauthorized') { console.error(e); box.innerHTML = '<div class="empty">加载失败</div>'; } }
}
async function tmplSave() {
  const name = document.getElementById('tmpl_name').value.trim();
  const content = document.getElementById('tmpl_content').value;
  if (!name) { showToast('请填写模板名称', 'error'); return; }
  try {
    const res = await apiFetch('/api/templates', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name, content })
    });
    if (res.ok) { showToast('模板已保存', 'ok'); document.getElementById('tmpl_name').value = ''; document.getElementById('tmpl_content').value = ''; await tmplRender(); }
    else showToast('保存失败', 'error');
  } catch (e) { if (e.message !== 'Unauthorized') console.error(e); }
}
function tmplNew() { document.getElementById('tmpl_name').value = ''; document.getElementById('tmpl_content').value = ''; }
async function tmplDelete(id) {
  if (!confirm('确定删除该模板？')) return;
  try {
    const res = await apiFetch('/api/templates/' + encodeURIComponent(id), { method: 'DELETE' });
    if (res.ok) { showToast('已删除', 'ok'); await tmplRender(); }
    else showToast('删除失败', 'error');
  } catch (e) { if (e.message !== 'Unauthorized') console.error(e); }
}
