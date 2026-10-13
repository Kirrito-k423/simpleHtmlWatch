'use strict';
(() => {
  if (new URLSearchParams(location.search).get('tasks') !== '1') return;
  const token = document.querySelector('meta[name="watch-token"]').content;
  const esc = value => String(value ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const time = value => value && !value.startsWith('0001-') ? new Date(value).toLocaleString('zh-CN', {hour12:false}) : '—';
  const phases = {debug:'调试保护 · 继续等待', unknown:'无法判断 · 继续等待', observing:'观察无进展', confirmed:'已确认失败残留', signal_intent:'已记录清理意图', terminated:'终止请求已返回 · 待核查', uncertain:'清理结果待核实', verified:'原进程退出或身份变化已核实'};
  const section = document.createElement('section');
  section.className = 'cleanup-panel';
  section.setAttribute('aria-label', '常驻排队清理');
  section.innerHTML = `<div class="cleanup-heading"><div><h2>常驻排队清理</h2><p id="cleanup-description">至少 4 台机器的任务排队且资源不足时，诊断单卡占用超过 5 分钟、利用率为 0 的进程。仅清理确认失败且持续无进展的残留；debug 和无法判断的继续等待。</p></div><button type="button" role="switch" aria-checked="false" aria-label="常驻排队清理" aria-describedby="cleanup-description cleanup-consent" data-cleanup-toggle disabled><span class="cleanup-switch-track" aria-hidden="true"></span><span data-cleanup-switch-label>读取中…</span></button></div>
    <div class="cleanup-policy-row"><p data-cleanup-policy>正在读取本实例设置…</p><button type="button" data-cleanup-refresh>刷新清理状态</button></div>
    <p id="cleanup-consent" class="cleanup-hint">点击启用即授权本实例按上述规则自动诊断和清理，操作记录随设置保存；重启后保留。</p>
    <p class="cleanup-message" data-cleanup-message role="status" hidden></p>
    <details data-cleanup-logs><summary>诊断与清理日志 <span data-cleanup-count></span></summary><p class="cleanup-hint">按最近更新时间显示，点击一条记录查看判定、进程日志和清理回执。</p><div class="cleanup-log-layout"><div data-cleanup-list></div><section class="cleanup-detail" aria-label="清理日志详情" data-cleanup-detail><p class="cleanup-hint">选择一条诊断记录查看详情。</p></section></div></details>`;
  document.querySelector('#task-panel .task-page-head').after(section);
  const toggle = section.querySelector('[data-cleanup-toggle]');
  const refreshButton = section.querySelector('[data-cleanup-refresh]');
  const detail = section.querySelector('[data-cleanup-detail]');
  const state = {status:null, service:null, available:false, busy:false, saving:false, error:'', notice:'', selected:'', audit:null, detailSequence:0, listSignature:''};

  async function json(url, options = {}) {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 15000);
    try {
      const response = await fetch(url, {...options, cache:'no-store', redirect:'error', signal:controller.signal});
      const result = await response.json();
      if (!response.ok) throw new Error(result.error || `HTTP ${response.status}`);
      return result;
    } catch (error) {
      if (error.name === 'AbortError') throw new Error('请求超时，请查询当前状态');
      throw error;
    } finally { clearTimeout(timer); }
  }
  async function identity() {
    const info = await json('/healthz');
    if (!info.runId || !info.capabilities?.includes('queue-residue-cleanup-v1')) throw new Error('当前实例不支持常驻排队清理，请由维护者升级后刷新页面');
    if (state.service?.serviceId && info.serviceId !== state.service.serviceId) throw new Error('当前服务已换成另一实例，请刷新页面重新核对');
    state.service = info;
    return info;
  }
  async function request(path = '', method = 'GET', data, info) {
    info = info || await identity();
    return json('/api/tasks/cleanup' + path, {method,
      headers:{'X-Watch-Token':token, 'X-Watch-Instance':info.runId, ...(data ? {'Content-Type':'application/json'} : {})},
      body:data ? JSON.stringify(data) : undefined});
  }
  function acceptStatus(result) {
    if (typeof result.policy?.enabled !== 'boolean' || !Array.isArray(result.audits)) throw new Error('清理状态响应不完整，请刷新查询');
    state.status = result;
    state.available = true;
  }
  function render() {
    const status = state.status, enabled = !!status?.policy.enabled;
    toggle.setAttribute('aria-checked', String(enabled));
    toggle.disabled = state.busy || !state.available;
    refreshButton.disabled = state.busy;
    section.querySelector('[data-cleanup-switch-label]').textContent = state.saving ? '保存中…' : !state.available ? '状态待核对' : enabled ? '已启用' : '已关闭';
    section.querySelector('[data-cleanup-policy]').textContent = status
      ? `本实例 ${state.service.instance} · ${state.available ? '' : '上次读取：'}${enabled ? '已启用' : '已关闭'} · 最近变更 ${time(status.policy.updatedAt)}`
      : '尚未取得有效清理状态';
    const message = section.querySelector('[data-cleanup-message]');
    message.textContent = [state.error || state.notice, status?.error ? '后台诊断提示：' + status.error : ''].filter(Boolean).join(' ');
    message.hidden = !message.textContent;
    message.classList.toggle('error', !!(state.error || status?.error));
    section.querySelector('[data-cleanup-count]').textContent = status ? `（${status.auditCount} 条）` : '';
    const audits = status?.audits || [], signature = JSON.stringify(audits);
    if (signature !== state.listSignature) {
      state.listSignature = signature;
      section.querySelector('[data-cleanup-list]').innerHTML = audits.length ?
        `<p class="cleanup-hint">显示最近 ${audits.length} / ${esc(status.auditCount)} 条</p>` + audits.map(audit => `<button type="button" class="cleanup-log-row" data-cleanup-id="${esc(audit.id)}" aria-pressed="false"><span class="cleanup-log-heading"><strong>${esc(audit.machineId)} · PID ${esc(audit.pid)}</strong><time>${esc(time(audit.updatedAt))}</time></span><span class="cleanup-phase" data-phase="${esc(audit.phase)}">${esc(phases[audit.phase] || audit.phase)}</span><span>${esc(audit.reason)}</span></button>`).join('') :
        '<p class="cleanup-empty">暂无诊断记录。有效的至少四机资源等待单缺少空闲机器时，后台才会开始检查。</p>';
    }
    section.querySelectorAll('[data-cleanup-id]').forEach(button => button.setAttribute('aria-pressed', String(button.dataset.cleanupId === state.selected)));
  }
  function renderAudit(audit) {
    const last = audit.last || {};
    detail.innerHTML = `<div class="cleanup-detail-heading"><h3>诊断详情</h3><button type="button" data-cleanup-download>下载完整记录</button></div>
      <p class="cleanup-phase" data-phase="${esc(audit.phase)}">${esc(phases[audit.phase] || audit.phase)}</p><p>${esc(audit.reason)}</p>
      <dl><div><dt>机器</dt><dd>${esc(audit.machineId)} · ${esc(audit.host)}</dd></div><div><dt>进程 PID</dt><dd>${esc(last.pid)}</dd></div><div><dt>资源等待单</dt><dd>${esc(audit.continuationId)}</dd></div><div><dt>开始 / 最近诊断</dt><dd>${esc(time(audit.createdAt))} / ${esc(time(audit.updatedAt))}</dd></div><div><dt>清理意图</dt><dd>${esc(time(audit.signalIntentAt))}</dd></div><div><dt>清理回执</dt><dd>${esc(audit.signalResult || '暂无回执')}</dd></div></dl>
      <h3>诊断与清理经过</h3><ol class="cleanup-events">${(audit.events || []).map(event => `<li><time>${esc(time(event.at))}</time><b>${esc(phases[event.phase] || event.phase)}</b><span>${esc(event.reason)}</span></li>`).join('') || '<li>暂无事件记录</li>'}</ol>
      <h3>最近进程日志</h3>${(last.logs || []).map(log => `<p class="cleanup-hint">${esc(log.fd)} · ${esc(log.size)} 字节 · 以下为末尾片段</p><pre>${esc(log.tail || '（空）')}</pre>`).join('') || '<p class="cleanup-hint">没有可读取的日志片段，详见判定原因。</p>'}
      <details><summary>完整诊断证据</summary><pre>${esc(JSON.stringify(audit, null, 2))}</pre></details>`;
  }
  async function loadAudit(id) {
    const sequence = ++state.detailSequence;
    state.selected = id;
    state.audit = null;
    detail.textContent = '正在读取诊断证据…';
    render();
    try {
      const result = await request('?id=' + encodeURIComponent(id));
      if (sequence !== state.detailSequence) return;
      if (result.audit?.id !== id) throw new Error('日志身份不一致，请重新查询');
      state.audit = result.audit;
      renderAudit(result.audit);
    } catch (error) { if (sequence === state.detailSequence) detail.textContent = '读取日志失败：' + error.message; }
  }
  async function refresh() {
    if (state.busy) return;
    state.busy = true; render();
    try {
      acceptStatus(await request());
      state.error = '';
      const latest = state.status.audits.find(audit => audit.id === state.selected);
      if (section.querySelector('[data-cleanup-logs]').open && latest && (!state.audit || latest.updatedAt !== state.audit.updatedAt)) await loadAudit(state.selected);
    } catch (error) { state.available = false; state.error = '读取清理状态失败：' + error.message; }
    finally { state.busy = false; render(); }
  }
  async function changePolicy() {
    if (state.busy || !state.available) return;
    const enabled = !state.status.policy.enabled;
    state.busy = true; state.saving = true; state.error = ''; state.notice = ''; render();
    try {
      const info = await identity();
      const authorizationRef = `task-dashboard:${crypto.randomUUID()} · 实例 ${info.instance} · 用户点击${enabled ? '启用' : '关闭'}常驻排队清理 · 仅在至少四机排队资源不足时清理确认失败残留，debug 和无法判断的继续等待，保留诊断和清理日志`;
      acceptStatus(await request('', 'POST', {enabled, authorizationRef}, info));
      state.notice = enabled ? '已启用，设置已保存，重启后继续生效。' : '已关闭后续自动清理，已有诊断和清理日志保留。';
    } catch (error) {
      state.available = false;
      state.error = `保存结果未确认：${error.message}。操作未自动重试。`;
      // A lost response may follow a successful write. Only read the state;
      // never turn polling or a refresh into a repeated policy mutation.
      try {
        acceptStatus(await request());
        state.error += ` 已重新读取当前状态：${state.status.policy.enabled ? '已启用' : '已关闭'}。`;
      } catch { state.error += ' 请刷新状态核对。'; }
    } finally { state.busy = false; state.saving = false; render(); }
  }
  function downloadAudit() {
    if (!state.audit) return;
    const url = URL.createObjectURL(new Blob([JSON.stringify(state.audit, null, 2) + '\n'], {type:'application/json'}));
    const link = document.createElement('a');
    link.href = url; link.download = state.audit.id.replace(/[^a-zA-Z0-9_-]/g, '_') + '.json';
    document.body.append(link); link.click(); link.remove();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  }
  section.addEventListener('click', event => {
    if (event.target.closest('[data-cleanup-toggle]')) changePolicy();
    else if (event.target.closest('[data-cleanup-refresh]')) refresh();
    else if (event.target.closest('[data-cleanup-download]')) downloadAudit();
    else {
      const button = event.target.closest('[data-cleanup-id]');
      if (button) loadAudit(button.dataset.cleanupId);
    }
  });
  refresh();
  setInterval(() => { if (!document.hidden) refresh(); }, 5000);
})();
