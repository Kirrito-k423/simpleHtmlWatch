'use strict';
const $ = (s, root = document) => root.querySelector(s);
const $$ = (s, root = document) => [...root.querySelectorAll(s)];
const esc = v => String(v ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const uid = () => crypto.randomUUID();
const token = $('meta[name="watch-token"]').content;
let config = {machines:[], profiles:[], interval:4}, commands = [], states = {}, view = 'npu', page = 0;
let paused = false, demo = false, draft, detailID, detailView, trustPending, busy = false, toastTimer;
let backendOK = true;
const labels = {online:'正常',partial:'命令异常',offline:'连接失败',untrusted:'待确认指纹',connecting:'连接中',disabled:'已停用'};
async function api(path, method = 'GET', data) {
  const r = await fetch('/api/' + path, {method, headers:{'X-Watch-Token':token, ...(data ? {'Content-Type':'application/json'} : {})}, body:data ? JSON.stringify(data) : undefined});
  const body = await r.json(); if (!r.ok) throw new Error(body.error || `请求失败 ${r.status}`); return body;
}
function toast(message) { $('#toast').textContent = message; $('#toast').hidden = false; clearTimeout(toastTimer); toastTimer = setTimeout(() => $('#toast').hidden = true, 3200); }
function setGroups() {
  const prev = $('#group-filter').value;
  $('#group-filter').innerHTML = '<option value="">所有分组</option>' + [...new Set(config.machines.map(m => m.group).filter(Boolean))].sort().map(g => `<option value="${esc(g)}">${esc(g)}</option>`).join('');
  $('#group-filter').value = prev; if (!$('#group-filter').value) $('#group-filter').value = '';
}
function time(t) { return !t || t.startsWith('0001') ? '尚未采集' : new Date(t).toLocaleTimeString('zh-CN', {hour12:false}); }
function textFor(m, s, selected) {
  if (!m.enabled) return '此机器已停用。可在「管理机器」中重新启用。';
  if (s.status === 'untrusted') return `${s.keyChanged ? '主机密钥已变化，请重新核对。' : '首次连接，需要确认主机身份。'}\n\n${s.fingerprint}\n\n确认后才会使用 SSH 密码登录。`;
  if (s.status === 'offline') return `连接失败\n${s.error}\n\n将自动重试。最近连接成功：${time(s.lastSuccess)}`;
  if (!m.commands.includes(selected)) return '此机器尚未启用该监控项。\n可在「管理机器」中勾选。';
  const r = s.results?.find(r => r.commandId === selected);
  if (!r) return '正在等待采集…';
  return (r.output || (r.error ? '' : '未发现匹配的进程。')) + (r.error ? `\n[命令失败] ${r.error}` : '') + (r.truncated ? '\n[输出超过 256 KiB，已截断]' : '');
}
function render() {
  const query = $('#search').value.toLowerCase().trim(), group = $('#group-filter').value, size = Number($('#layout').value);
  const filtered = config.machines.filter(m => (!group || m.group === group) && `${m.name} ${m.host}`.toLowerCase().includes(query));
  page = Math.max(0, Math.min(page, Math.ceil(filtered.length / size) - 1));
  $('#total').textContent = String(config.machines.length).padStart(2, '0');
  $('#online').textContent = String(Object.values(states).filter(s => s.status === 'online').length).padStart(2, '0');
  $('#attention').textContent = String(Object.values(states).filter(s => ['offline','partial','untrusted'].includes(s.status)).length).padStart(2, '0');
  $('#empty').hidden = config.machines.length > 0;
  $('#grid').hidden = !config.machines.length;
  $('#grid').dataset.layout = size;
  const visible = filtered.slice(page * size, (page + 1) * size);
  const signature = visible.map(m => m.id).join('|');
  if ($('#grid').dataset.signature !== signature) {
    $('#grid').dataset.signature = signature;
    $('#grid').replaceChildren();
    for (const m of visible) {
      const card = document.createElement('article'); card.className = 'machine-card'; card.dataset.id = m.id;
      card.innerHTML = '<div class="card-head"><span class="card-name"></span><span class="host"></span><span class="group"></span><span class="badge" tabindex="0"></span><button class="trust-action" title="核对 SSH 主机指纹" hidden>指纹</button><button class="expand" title="放大机器输出" aria-label="放大机器输出">↗</button></div><pre class="card-output" tabindex="0"></pre>';
      $('.expand', card).onclick = () => openDetail(m.id);
      $('.trust-action', card).onclick = () => openTrust(m.id);
      $('#grid').append(card);
    }
  }
  if (!visible.length && config.machines.length) $('#grid').innerHTML = '<div class="no-results">没有匹配的机器，试试其他名称或分组。</div>';
  for (const card of $$('.machine-card')) {
    const m = config.machines.find(m => m.id === card.dataset.id), s = states[m.id] || {status:'connecting'};
    const name = $('.card-name', card); name.textContent = m.name; name.title = m.name;
    const badge = $('.badge', card); badge.className = `badge ${s.status}`; badge.textContent = labels[s.status] || s.status;
    $('.host', card).textContent = `${m.host}:${m.port}`;
    $('.group', card).textContent = m.group || '未分组';
    const out = $('.card-output', card), value = textFor(m, s, view);
    // Keep each terminal's scroll position while replacing its text.
    if (out.textContent !== value) { const top = out.scrollTop, left = out.scrollLeft; out.textContent = value; out.scrollTop = top; out.scrollLeft = left; }
    out.classList.toggle('message', !['online','partial'].includes(s.status));
    const updated = `${time(s.updatedAt)}${s.durationMs != null ? ` · ${s.durationMs} ms` : ''}`;
    badge.title = `${labels[s.status] || s.status} · ${updated}`;
    badge.setAttribute('aria-label', badge.title);
    $('.card-head', card).title = `${m.name} · ${m.host}:${m.port} · ${m.group || '未分组'} · ${updated}`;
    $('.host', card).title = `${m.host}:${m.port}`;
    $('.group', card).title = m.group || '未分组';
    $('.trust-action', card).hidden = s.status !== 'untrusted' || demo;
  }
  $('#page-info').textContent = filtered.length ? `${page * size + 1}–${Math.min((page + 1) * size, filtered.length)} / ${filtered.length} 台 · 第 ${page + 1} / ${Math.ceil(filtered.length / size)} 页` : '0 台机器';
  $('#prev-btn').disabled = page === 0; $('#next-btn').disabled = (page + 1) * size >= filtered.length;
  $('#live-status').innerHTML = `<i></i>${demo ? '演示数据 · 不连接真实机器' : !backendOK ? '本地服务失联 · 显示上次快照' : paused ? '显示已暂停 · 后台继续采集' : `每 ${config.interval} 秒采集 · SSH 连接复用`}`;
  $('#demo-tag').hidden = !demo; $('#exit-demo').hidden = !demo; $('#settings-btn').disabled = demo;
  if ($('#detail-dialog').open) updateDetail();
}
async function loadConfig() { const data = await api('config'); config = data.config; commands = data.commands; setGroups(); }
async function poll() {
  if (!demo && !paused && !busy) {
    try { states = await api('status'); backendOK = true; $('#connection-error').hidden = true; render(); }
    catch (e) { backendOK = false; $('#connection-error').textContent = `无法读取监控状态：${e.message}。请确认本地程序仍在运行。`; $('#connection-error').hidden = false; render(); }
  }
  setTimeout(poll, 1000);
}
$('#view-tabs').onclick = e => { const b = e.target.closest('[data-view]'); if (!b) return; view = b.dataset.view; $$('#view-tabs button').forEach(el => el.classList.toggle('active', el === b)); render(); };
for (const id of ['search','group-filter','layout']) $('#' + id).addEventListener(id === 'search' ? 'input' : 'change', () => { page = 0; render(); });
$('#prev-btn').onclick = () => { page--; render(); }; $('#next-btn').onclick = () => { page++; render(); };
$('#pause-btn').onclick = () => { paused = !paused; $('#pause-btn').textContent = paused ? '▶ 恢复' : 'Ⅱ 暂停'; render(); };
$('#refresh-btn').onclick = async () => { if (demo) return toast('当前是演示数据'); try { await api('refresh','POST'); toast('已请求刷新'); } catch(e) { toast(e.message); } };
$('#fullscreen-btn').onclick = async () => { try { if (document.fullscreenElement) await document.exitFullscreen(); else await document.documentElement.requestFullscreen(); } catch(e) { toast('浏览器未允许全屏，请尝试按 F11'); } };
document.addEventListener('fullscreenchange', () => { $('#fullscreen-btn').textContent = document.fullscreenElement ? '⛶ 退出全屏' : '⛶ 全屏'; $('#fullscreen-btn').setAttribute('aria-pressed', !!document.fullscreenElement); });
$$('[data-close]').forEach(b => b.onclick = () => $('#' + b.dataset.close).close());
function openDetail(id) { detailID = id; detailView = view; updateDetail(); $('#detail-dialog').showModal(); }
function updateDetail() {
  const m = config.machines.find(m => m.id === detailID); if (!m) return $('#detail-dialog').close();
  const s = states[m.id] || {status:'connecting'};
  $('#detail-title').textContent = m.name; $('#detail-subtitle').textContent = `${m.host}:${m.port} · ${labels[s.status]} · ${time(s.updatedAt)}`;
  $('#detail-tabs').innerHTML = commands.map(c => `<button data-cmd="${c.id}" class="${detailView === c.id ? 'active' : ''}">${esc(c.name)}</button>`).join('');
  $('#detail-output').textContent = textFor(m,s,detailView);
}
$('#detail-tabs').onclick = e => { const b = e.target.closest('[data-cmd]'); if (b) { detailView = b.dataset.cmd; updateDetail(); } };
function openTrust(id) { trustPending = states[id]; $('#trust-title').textContent = trustPending.keyChanged ? '主机密钥已变化，请核对' : '确认 SSH 主机指纹'; $('#trust-address').textContent = trustPending.address; $('#trust-fingerprint').textContent = trustPending.fingerprint; $('#trust-error').textContent = ''; $('#trust-dialog').showModal(); }
$('#trust-confirm').onclick = async () => { try { await api('trust','POST',{address:trustPending.address,fingerprint:trustPending.fingerprint}); $('#trust-dialog').close(); toast('已保存主机指纹，正在连接'); } catch(e) { $('#trust-error').textContent = e.message; } };
function profileOptions(selected) { return '<option value="">选择凭据</option>' + draft.profiles.map(p => `<option value="${esc(p.id)}" ${selected === p.id ? 'selected' : ''}>${esc(p.name || '未命名凭据')} · ${esc(p.username || 'root')}</option>`).join(''); }
function renderEditors() {
  $('#profiles-editor').innerHTML = draft.profiles.map(p => `<div class="profile-row" data-id="${esc(p.id)}"><label>凭据名称<input data-field="name" value="${esc(p.name)}" placeholder="实验室 root" required></label><label>用户名<input data-field="username" value="${esc(p.username)}" required autocomplete="off"></label><label>SSH 密码${p.hasPassword ? '（已保存，留空保持）' : ''}<input data-field="password" value="${esc(p.password || '')}" type="password" autocomplete="new-password" placeholder="${p.hasPassword ? '留空保持原密码' : '输入密码'}" ${p.hasPassword ? '' : 'required'}></label><button type="button" class="delete" data-remove-profile="${esc(p.id)}">删除</button></div>`).join('') || '<div class="editor-empty">先新增一组凭据，多台机器可以共用。</div>';
  $('#machines-editor').innerHTML = draft.machines.map(m => `<div class="machine-edit" data-id="${esc(m.id)}"><div class="machine-fields"><label>机器名称<input data-field="name" value="${esc(m.name)}" required></label><label>IP / 主机名<input data-field="host" value="${esc(m.host)}" required placeholder="192.0.2.11"></label><label>SSH 端口<input data-field="port" type="number" value="${m.port}" min="1" max="65535" required></label><label>分组<input data-field="group" value="${esc(m.group)}" placeholder="训练集群"></label><label>共享凭据<select data-field="profileId" required>${profileOptions(m.profileId)}</select></label><button type="button" class="delete" data-remove-machine="${esc(m.id)}">删除</button></div><div class="machine-options">${commands.map(c => `<label><input type="checkbox" data-command="${c.id}" ${m.commands.includes(c.id) ? 'checked' : ''}>${esc(c.name)}</label>`).join('')}<label class="enabled"><input type="checkbox" data-field="enabled" ${m.enabled ? 'checked' : ''}>启用监控</label></div></div>`).join('') || '<div class="editor-empty">添加机器，或粘贴 IP 列表批量添加。</div>';
}
function readEditors() {
  for (const row of $$('.profile-row')) { const p = draft.profiles.find(p => p.id === row.dataset.id); $$('[data-field]',row).forEach(el => p[el.dataset.field] = el.value); }
  for (const row of $$('.machine-edit')) { const m = draft.machines.find(m => m.id === row.dataset.id); $$('[data-field]',row).forEach(el => m[el.dataset.field] = el.type === 'checkbox' ? el.checked : el.type === 'number' ? Number(el.value) : el.value.trim()); m.commands = $$('[data-command]:checked',row).map(el => el.dataset.command); }
  draft.interval = Number($('#interval').value);
}
function openSettings() { draft = structuredClone(config); $('#interval').value = config.interval; $('#settings-error').textContent = ''; renderEditors(); $('#settings-dialog').showModal(); }
$('#settings-btn').onclick = openSettings; $('#empty-add').onclick = openSettings;
$('#settings-dialog').addEventListener('close', () => { draft = undefined; $('#profiles-editor').replaceChildren(); $('#machines-editor').replaceChildren(); $('#import-file').value = ''; });
$('#add-profile').onclick = () => { readEditors(); draft.profiles.push({id:uid(),name:'',username:'root',password:''}); renderEditors(); $$('.profile-row').at(-1).querySelector('input').focus(); };
$('#profiles-editor').oninput = e => { if (!['name','username'].includes(e.target.dataset.field)) return; readEditors(); $$('.machine-edit').forEach(row => { const m = draft.machines.find(m => m.id === row.dataset.id); $('[data-field="profileId"]',row).innerHTML = profileOptions(m.profileId); }); };
$('#profiles-editor').onclick = e => { const b = e.target.closest('[data-remove-profile]'); if (!b) return; readEditors(); if (draft.machines.some(m => m.profileId === b.dataset.removeProfile)) { $('#settings-error').textContent = '这组凭据仍被机器引用，请先修改这些机器的凭据或删除机器。'; return; } draft.profiles = draft.profiles.filter(p => p.id !== b.dataset.removeProfile); renderEditors(); };
$('#add-machine').onclick = () => { readEditors(); draft.machines.push({id:uid(),name:'',host:'',port:22,group:'',profileId:draft.profiles[0]?.id || '',commands:commands.map(c => c.id),enabled:true}); renderEditors(); $$('.machine-edit').at(-1).querySelector('input').focus(); };
$('#machines-editor').onclick = e => { const b = e.target.closest('[data-remove-machine]'); if (!b) return; readEditors(); draft.machines = draft.machines.filter(m => m.id !== b.dataset.removeMachine); renderEditors(); };
$('#settings-form').onsubmit = async e => {
  e.preventDefault(); readEditors(); $('#settings-error').textContent = ''; $('#save-btn').disabled = true; busy = true;
  try { config = await api('config','PUT',draft); states = {}; setGroups(); render(); $('#settings-dialog').close(); toast('配置已保存，正在连接机器'); }
  catch(e) { $('#settings-error').textContent = e.message; }
  finally { $('#save-btn').disabled = false; busy = false; }
};
$('#batch-btn').onclick = () => { readEditors(); if (!draft.profiles.length) { $('#settings-error').textContent = '请先新增一组共享凭据。'; return; } $('#batch-profile').innerHTML = profileOptions(draft.profiles[0].id); $('#batch-input').value = ''; $('#batch-error').textContent = ''; $('#batch-dialog').showModal(); };
$('#batch-confirm').onclick = () => {
  if (!$('#batch-profile').value) { $('#batch-error').textContent = '请选择共享凭据'; return; }
  const lines = $('#batch-input').value.split('\n').map(s => s.trim()).filter(Boolean), additions = [];
  for (const [i,line] of lines.entries()) {
    const parts = line.split(/[,，\t]/).map(s => s.trim());
    if (parts.length > 3) { $('#batch-error').textContent = `第 ${i+1} 行：格式应为 名称,IP,分组`; return; }
    const address = parts.length === 1 ? parts[0] : parts[1];
    let host = address, port = 22;
    const match = address.match(/^\[([^\]]+)\](?::(\d+))?$|^([^:]+):(\d+)$/);
    if (match) { host = match[1] || match[3]; port = Number(match[2] || match[4] || 22); }
    if (!host || port < 1 || port > 65535 || /\s/.test(host)) { $('#batch-error').textContent = `第 ${i+1} 行：地址或端口无效`; return; }
    if ([...draft.machines,...additions].some(m => m.host === host && m.port === port)) continue;
    additions.push({id:uid(),name:parts.length === 1 ? host : (parts[0] || host),host,port,group:parts[2] || '',profileId:$('#batch-profile').value,commands:commands.map(c => c.id),enabled:true});
  }
  if (!additions.length) { $('#batch-error').textContent = '没有可添加的机器；空行和重复地址会被跳过。'; return; }
  if (draft.machines.length + additions.length > 200) { $('#batch-error').textContent = '最多配置 200 台机器。'; return; }
  draft.machines.push(...additions); renderEditors(); $('#batch-dialog').close(); toast(`已添加 ${additions.length} 台，保存后生效`);
};
$('#export-btn').onclick = () => {
  readEditors(); const exported = structuredClone(draft); exported.profiles.forEach(p => { delete p.password; delete p.hasPassword; });
  const url = URL.createObjectURL(new Blob([JSON.stringify(exported,null,2)],{type:'application/json'})); const a = document.createElement('a'); a.href = url; a.download = 'simpleHtmlWatch.config.json'; a.click(); setTimeout(() => URL.revokeObjectURL(url),1000); toast('已导出不含密码的配置');
};
$('#import-btn').onclick = () => $('#import-file').click();
$('#import-file').onchange = async e => {
  try {
    const f = e.target.files[0]; if (!f) return; if (f.size > 1024*1024) throw new Error('配置文件不能超过 1 MiB');
    const c = JSON.parse(await f.text());
    if (!Array.isArray(c.machines) || !Array.isArray(c.profiles) || !Number.isInteger(c.interval) || c.interval < 3 || c.interval > 3600) throw new Error('配置结构或刷新间隔无效');
    if (c.machines.length > 200 || c.profiles.length > 100) throw new Error('配置数量超出限制');
    const validID = s => typeof s === 'string' && /^[a-zA-Z0-9_-]{1,80}$/.test(s);
    const seenProfiles = new Set(), seenMachines = new Set();
    for (const p of c.profiles) {
      if (!p || !validID(p.id) || seenProfiles.has(p.id) || typeof p.name !== 'string' || typeof p.username !== 'string') throw new Error('凭据格式无效或 ID 重复');
      seenProfiles.add(p.id); p.password = typeof p.password === 'string' ? p.password : ''; p.hasPassword = !!config.profiles.find(old => old.id === p.id && old.hasPassword);
    }
    for (const m of c.machines) {
      if (!m || !validID(m.id) || seenMachines.has(m.id) || !['name','host','group','profileId'].every(k => typeof m[k] === 'string') || !seenProfiles.has(m.profileId) || !Number.isInteger(m.port) || m.port < 1 || m.port > 65535 || typeof m.enabled !== 'boolean' || !Array.isArray(m.commands) || !m.commands.length || !m.commands.every(id => commands.some(cmd => cmd.id === id))) throw new Error('机器格式无效、ID 重复或凭据缺失');
      seenMachines.add(m.id);
    }
    if (!confirm('导入将替换当前编辑中的列表；保存后生效。继续？')) return;
    draft = c; $('#interval').value = c.interval; renderEditors(); $('#settings-error').textContent = '已导入。新凭据需填写密码，再保存。';
  } catch(e) { $('#settings-error').textContent = `导入失败：${e.message}`; } finally { e.target.value = ''; }
};
function enterDemo() {
  demo = true; paused = false; $('#pause-btn').textContent = 'Ⅱ 暂停'; states = {}; config = {interval:4,profiles:[],machines:[]};
  for (let i=1;i<=16;i++) {
    const id = `demo-${i}`, n = String(i).padStart(2,'0'), status = i === 7 ? 'offline' : i === 12 ? 'partial' : 'online';
    config.machines.push({id,name:`ascend-${n}`,host:`192.0.2.${10+i}`,port:22,group:i<9?'训练集群':'开发集群',profileId:'demo',commands:['npu','python','usage'],enabled:true});
    const npu = ['+--------------------------------------------------+','| NPU   Name           Health   Power(W)  Temp(C)   |','| Chip  AICore(%)       Memory-Usage(MB)             |','+--------------------------------------------------+', ...Array.from({length:4},(_,j)=>`| ${j}     Ascend 910B     OK       ${260+i+j}        ${48+j}      |\n|       ${String((i*7+j*13)%98).padStart(2)}%              ${28000+i*127} / 65536          |`),'+--------------------------------------------------+'].join('\n');
    states[id] = {machineId:id,status,error:status==='offline'?'演示：SSH 连接超时':'',updatedAt:new Date().toISOString(),lastSuccess:new Date().toISOString(),durationMs:130+i*13,results:[{commandId:'npu',output:status==='partial'?'bash: npu-smi: command not found':npu,error:status==='partial'?'演示：exit status 127':''},{commandId:'python',output:`UID        PID  PPID  C STIME TTY      TIME CMD\nroot      ${2100+i}     1 97 09:30 ?    02:31:42 python train.py --rank 0\nroot      ${2200+i}     1 95 09:30 ?    02:28:15 python train.py --rank 1`},{commandId:'usage',output:`USER   PID  PPID %CPU %MEM    RSS   ELAPSED COMMAND\nroot  ${2100+i}    1 97.3  3.2 831220  02:31:42 python train.py\nroot  ${2200+i}    1 95.1  3.0 801210  02:28:15 python train.py`}]};
  }
  page=0; $('#search').value=''; setGroups(); render();
}
$('#demo-btn').onclick = enterDemo;
$('#exit-demo').onclick = async () => { try { await loadConfig(); demo=false; states={}; page=0; render(); } catch(e) { toast(e.message); } };
(async () => { try { await loadConfig(); render(); if (new URLSearchParams(location.search).get('demo') === '1') enterDemo(); } catch(e) { $('#connection-error').hidden=false; $('#connection-error').textContent=e.message; } poll(); })();
