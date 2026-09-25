'use strict';
(() => {
  if (new URLSearchParams(location.search).get('tasks') !== '1') return;
  const panel = document.querySelector('#task-panel');
  const token = document.querySelector('meta[name="watch-token"]').content;
  const labels = {dispatching:'发送中', running:'运行中', unknown:'结果未知', succeeded:'命令成功', failed:'命令失败', abandoned:'人工解除', archive_ready:'结果已回收', archive_error:'回收失败', legacy_created:'旧记录创建', legacy_finished:'旧记录结束'};
  const state = {jobs:[], ready:[], selected:null, pending:null, logs:null, notice:'', error:'', refreshing:false, scope:'auto', group:'', machine:''};
  const esc = value => String(value ?? '').replace(/[&<>"']/g, char => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[char]));
  const full = value => value ? new Date(value).toLocaleString('zh-CN', {hour12:false}) : '—';
  const clock = value => new Date(value).toLocaleTimeString('zh-CN', {hour12:false});
  const elapsed = ms => {
    const seconds = Math.round(ms / 1000);
    if (seconds >= 86400) return `${Math.floor(seconds / 86400)}天 ${Math.floor(seconds % 86400 / 3600)}小时`;
    if (seconds >= 3600) return `${Math.floor(seconds / 3600)}小时 ${Math.floor(seconds % 3600 / 60)}分`;
    if (seconds >= 60) return `${Math.floor(seconds / 60)}分 ${seconds % 60}秒`;
    return `${seconds}秒`;
  };
  const selected = () => state.jobs.find(job => job.id === state.selected);
  const hasExit = job => job && job.exitCode !== undefined && job.exitCode !== null;
  async function request(path, method = 'GET', data) {
    const response = await fetch('/api/tasks' + path, {method, headers:{'X-Watch-Token':token, ...(data ? {'Content-Type':'application/json'} : {})}, body:data ? JSON.stringify(data) : undefined});
    if (!response.ok) {
      let detail;
      try { detail = (await response.json()).error; } catch { detail = response.statusText; }
      const error = new Error(detail || `HTTP ${response.status}`);
      error.status = response.status;
      throw error;
    }
    return response.json();
  }
  function formScope() {
    const group = panel.querySelector('#task-group');
    const machine = panel.querySelector('#task-machine');
    group.hidden = state.scope !== 'group';
    machine.hidden = state.scope !== 'machine';
    panel.querySelector('#task-scope').value = state.scope;
    const candidates = state.ready.filter(item => state.scope === 'auto' || state.scope === 'group' && item.group === state.group || state.scope === 'machine' && item.id === state.machine);
    const target = candidates[0];
    panel.querySelector('#task-target').innerHTML = target
      ? `<strong>${esc(target.name || target.id)}</strong><span>${esc(target.group || '未分组')} · ${esc(target.id)} · 监控更新 ${esc(full(target.updatedAt))}</span>`
      : '<strong>暂无 ready 机器</strong><span>请检查机器监控状态，或改选调度范围。</span>';
    panel.querySelector('#task-submit').disabled = !target || !!state.pending;
  }
  function readyOptions() {
    const groups = [...new Set(state.ready.map(item => item.group).filter(Boolean))].sort();
    if (state.group && !groups.includes(state.group)) groups.unshift(state.group);
    const group = panel.querySelector('#task-group');
    group.innerHTML = groups.map(item => `<option value="${esc(item)}">${esc(item)}${state.ready.some(host => host.group === item) ? '' : ' · 暂无 ready 机器'}</option>`).join('');
    if (!state.group && groups.length) state.group = groups[0];
    group.value = state.group;
    const machines = [...state.ready];
    if (state.machine && !machines.some(item => item.id === state.machine)) machines.unshift({id:state.machine, name:state.machine + ' · 暂无 ready'});
    const machine = panel.querySelector('#task-machine');
    machine.innerHTML = machines.map(item => `<option value="${esc(item.id)}">${esc(item.name || item.id)} · ${esc(item.id)}</option>`).join('');
    if (!state.machine && machines.length) state.machine = machines[0].id;
    machine.value = state.machine;
    formScope();
  }
  function eventCell(job, column, now) {
    if (column.kind === 'events') {
      const hits = column.events.filter(event => event.taskId === job.id);
      if (hits.length) return `<div class="task-event-stack">${hits.map(event => `<button class="task-event ${esc(event.type)}" data-task-id="${esc(job.id)}" title="${esc(full(event.at))} · ${esc(event.text)}"><time>${esc(clock(event.at))}</time><b>${esc(labels[event.type] || event.type)}</b></button>`).join('')}</div>`;
    }
    const start = Date.parse(job.createdAt), end = job.finishedAt ? Date.parse(job.finishedAt) : now;
    if (column.kind === 'now') return !job.finishedAt ? `<span class="task-current ${esc(job.status)}">${esc(labels[job.status] || job.status)}</span>` : '';
    return start <= column.end && end >= column.start ? '<span class="task-continuation" title="这段时间没有新的状态事件">···</span>' : '';
  }
  function axisCell(column) {
    if (column.kind === 'gap') return `<div class="task-axis-gap" title="${esc(full(column.start))} 至 ${esc(full(column.end))}，无状态事件，时间跨度已压缩"><span>∿</span><small>无事件<br>${esc(elapsed(column.duration))}<br>已压缩</small></div>`;
    if (column.kind === 'now') return `<div class="task-axis-now"><b>现在</b><small>${esc(clock(column.start))}</small></div>`;
    return `<div class="task-axis-time"><b>${esc(clock(column.start))}</b><small>${esc(new Date(column.start).toLocaleDateString('zh-CN'))}${column.end > column.start ? ' · 至 ' + esc(clock(column.end)) : ''}</small></div>`;
  }
  function renderBoard() {
    const jobs = state.jobs.slice(0, 24);
    const board = panel.querySelector('#task-board');
    const previousScroll = board.scrollLeft;
    panel.querySelector('#task-count').textContent = `最近 ${jobs.length} / ${state.jobs.length} 条任务`;
    if (!jobs.length) {
      board.innerHTML = '<div class="task-empty">还没有任务。左侧时间线将在首次提交后出现；可以在右侧选择自动调度并发送命令。</div>';
      return;
    }
    const now = Date.now(), columns = buildTaskTimeline(jobs, now);
    board.innerHTML = `<table class="task-time-table"><colgroup><col style="width:212px">${columns.map(column => `<col style="width:${column.width}px">`).join('')}</colgroup><thead><tr><th scope="col" class="task-sticky">任务 / 实际机器</th>${columns.map(column => `<th scope="col" class="${column.kind === 'gap' ? 'task-gap-column' : ''}">${axisCell(column)}</th>`).join('')}</tr></thead><tbody>${jobs.map(job => `<tr class="${job.id === state.selected ? 'task-selected-row' : ''}"><th scope="row" class="task-sticky"><button data-task-id="${esc(job.id)}" class="task-row-title"><strong>${esc(job.id)}</strong><span>${esc(job.machineName || job.selectedMachineId || '机器未知')}</span><em class="task-status ${esc(job.status)}">${esc(labels[job.status] || job.status)}</em></button></th>${columns.map(column => `<td class="${column.kind === 'gap' ? 'task-gap-column' : ''}">${eventCell(job, column, now)}</td>`).join('')}</tr>`).join('')}</tbody></table>`;
    if (!board.dataset.initialized) { board.scrollLeft = board.scrollWidth; board.dataset.initialized = '1'; }
    else board.scrollLeft = previousScroll;
  }
  function renderDetail() {
    const job = selected(), detail = panel.querySelector('#task-detail');
    if (!job) { detail.innerHTML = '<div class="task-detail-empty">选择一条任务查看退出码、日志与结果文件。</div>'; return; }
    const status = labels[job.status] || job.status;
    const archive = job.archiveReady ? '已回收' : job.archiveError ? '回收失败' : hasExit(job) ? '等待回收' : '等待退出结果';
    detail.innerHTML = `<div class="task-detail-head"><div><span>选中任务</span><h3>${esc(job.id)}</h3></div><span class="task-status ${esc(job.status)}">${esc(status)}</span></div>
      <dl class="task-facts"><div><dt>实际机器</dt><dd>${esc(job.machineName || job.selectedMachineId)} · ${esc(job.host || '')}</dd></div><div><dt>远端退出码</dt><dd>${hasExit(job) ? esc(job.exitCode) : '未知'}</dd></div><div><dt>结果包</dt><dd>${esc(archive)}</dd></div><div><dt>创建时间</dt><dd>${esc(full(job.createdAt))}</dd></div><div><dt>结束时间</dt><dd>${esc(full(job.finishedAt))}</dd></div></dl>
      ${job.error ? `<p class="task-problem">${esc(job.error)}</p>` : ''}${job.archiveError ? `<p class="task-problem">结果回收：${esc(job.archiveError)}</p>` : ''}
      ${job.eventsDropped ? `<p class="task-problem">较早的 ${job.eventsDropped} 条状态事件已从任务记录中裁剪。</p>` : ''}
      <div class="task-detail-actions"><button data-task-action="logs">读取日志末尾</button>${hasExit(job) && !job.archiveReady ? '<button data-task-action="collect">重试回收</button>' : ''}${job.archiveReady ? '<button data-task-action="download">下载结果包</button>' : ''}</div>
      <details><summary>提交的命令</summary><pre>${esc(job.shell)}</pre></details>
      ${state.logs?.id === job.id ? `<div class="task-log-pair"><div><b>stdout · 最后 16 KiB</b><pre>${esc(state.logs.stdout || '（空）')}</pre></div><div><b>stderr · 最后 16 KiB</b><pre>${esc(state.logs.stderr || '（空）')}</pre></div></div>` : '<p class="task-log-hint">点击“读取日志末尾”查询远端；这里不会把日志内容误当作退出证据。</p>'}`;
  }
  function renderMessage() {
    const box = panel.querySelector('#task-message');
    box.hidden = !(state.notice || state.error || state.pending);
    box.className = 'task-message' + (state.error ? ' error' : '');
    box.innerHTML = `${state.error ? esc(state.error) : esc(state.notice)}${state.pending ? `<div>待确认任务 ID：<code>${esc(state.pending.id)}</code>。网络结果不明时只用同一 ID 核对或重试原请求。 <button data-task-action="check-pending">查询任务 ID</button><button data-task-action="retry-pending">同 ID 重试</button></div>` : ''}`;
  }
  function render() {
    const running = state.jobs.filter(job => job.status === 'running' || job.status === 'dispatching').length;
    const unknown = state.jobs.filter(job => job.status === 'unknown').length;
    const completed = state.jobs.filter(job => job.finishedAt).length;
    panel.querySelector('#task-stats').innerHTML = `<span><b>${state.ready.length}</b> ready</span><span><b>${running}</b> 运行或发送</span><span><b>${unknown}</b> 结果未知</span><span><b>${completed}</b> 已结束</span>`;
    readyOptions(); renderBoard(); renderDetail(); renderMessage();
  }
  async function refresh() {
    if (state.refreshing) return;
    state.refreshing = true;
    try {
      const [jobs, ready] = await Promise.all([request(''), request('/ready')]);
      state.jobs = Array.isArray(jobs) ? jobs : [];
      state.ready = Array.isArray(ready) ? ready : [];
      if (!state.selected || !state.jobs.some(job => job.id === state.selected)) state.selected = state.jobs[0]?.id || null;
      state.error = '';
      render();
    } catch (error) { state.error = '刷新任务失败：' + error.message; renderMessage(); }
    finally { state.refreshing = false; }
  }
  function newTaskId() { return 'ui-' + crypto.randomUUID().replaceAll('-', ''); }
  function payload() {
    const shell = panel.querySelector('#task-command').value.trim();
    if (!shell || new TextEncoder().encode(shell).length > 4096) throw new Error('请输入不超过 4096 字节的 Bash 命令。');
    const out = {id:newTaskId(), shell};
    if (state.scope === 'group') out.group = state.group;
    if (state.scope === 'machine') out.machineId = state.machine;
    return out;
  }
  async function submit(retry = false) {
    let task;
    try { task = retry ? state.pending : payload(); } catch (error) { state.error = error.message; renderMessage(); return; }
    if (!task) return;
    state.pending = task;
    state.error = ''; state.notice = '正在提交任务 ' + task.id + '…'; renderMessage(); formScope();
    try {
      const job = await request('', 'POST', task);
      state.pending = null;
      state.selected = job.id;
      state.logs = null;
      state.notice = `任务 ${job.id} 已受理，实际分配到 ${job.machineName || job.selectedMachineId}。请看时间线确认启动、退出与回收。`;
      panel.querySelector('#task-board').dataset.initialized = '';
      await refresh();
    } catch (error) {
      if (error.status === 400 || error.status === 409) state.pending = null;
      state.error = `任务 ${task.id}：${error.message}`;
      renderMessage(); formScope();
    }
  }
  async function checkPending() {
    if (!state.pending) return;
    try {
      const job = await request('?id=' + encodeURIComponent(state.pending.id));
      state.pending = null; state.selected = job.id;
      state.notice = `任务 ${job.id} 已存在，实际分配到 ${job.machineName || job.selectedMachineId}。`;
      await refresh();
    } catch (error) { state.error = `查询 ${state.pending.id}：${error.message}`; renderMessage(); }
  }
  async function taskAction(action) {
    const job = selected(); if (!job) return;
    try {
      if (action === 'logs') state.logs = {id:job.id, ...await request('/logs?id=' + encodeURIComponent(job.id))};
      if (action === 'collect') { await request('/collect?id=' + encodeURIComponent(job.id), 'POST'); await refresh(); }
      if (action === 'download') {
        const response = await fetch('/api/tasks/archive?id=' + encodeURIComponent(job.id), {headers:{'X-Watch-Token':token}});
        if (!response.ok) throw new Error('下载失败：HTTP ' + response.status);
        const url = URL.createObjectURL(await response.blob());
        const link = document.createElement('a'); link.href = url; link.download = job.id + '.tar.gz'; link.click();
        setTimeout(() => URL.revokeObjectURL(url), 60000);
      }
      state.error = ''; renderDetail(); renderMessage();
    } catch (error) { state.error = error.message; renderMessage(); }
  }
  panel.innerHTML = `<div class="task-page-head"><div><div class="eyebrow">SSH TASK CONTROLLER / EVENT TIMELINE</div><h1>任务中台</h1><p>本机观测到的状态事件按实际时间排列；长时间无事件的区间压缩成窄列，时长仍如实标注。</p></div><div class="task-head-actions"><div id="task-stats" class="task-stats"></div><button data-task-action="refresh">刷新</button><a href="/">返回监控</a></div></div>
    <div id="task-message" role="status" hidden></div><div class="task-page-grid"><section class="task-board-card"><div class="task-section-heading"><div><h2>任务时间泳道</h2><p>非等比例时间轴 · 相邻 1 分钟内的事件堆叠 · 无事件区间压缩</p></div><div class="task-board-nav"><span id="task-count"></span><button data-task-action="earliest" aria-label="移到最早事件">← 最早</button><button data-task-action="latest" aria-label="移到最近事件">最近 →</button></div></div><div id="task-board" class="task-board"></div><p class="task-board-note">时间是中台的受理或观测时间，远端退出可能早于轮询发现。“结果未知”保留机器占用；结果回收单列显示。</p></section>
    <aside class="task-side"><form id="task-form" class="task-compose"><div class="task-section-heading"><h2>发送任务</h2><span>一次调度一台 ready 机器</span></div><label>调度范围<select id="task-scope"><option value="auto">自动调度 · 所有 ready 机器</option><option value="group">指定机器组</option><option value="machine">指定机器</option></select></label><select id="task-group" aria-label="选择机器组" hidden></select><select id="task-machine" aria-label="选择机器" hidden></select><div class="task-preview"><small>预计落点 · 实际机器以提交响应为准</small><div id="task-target"></div></div><label>远端 Bash 命令<textarea id="task-command" rows="5" spellcheck="false" maxlength="4096" placeholder="python train.py --epochs 5&#10;cp summary.json &quot;$SHW_RESULTS_DIR/&quot;"></textarea></label><p>把需要回收的文件写入 <code>$SHW_RESULTS_DIR</code>；stdout 与 stderr 单独保存。自动调度可能选到不同硬件，命令依赖型号时请限定组或机器。ready 不代表 NPU 空闲。</p><button id="task-submit" class="primary" type="submit">发送到 ready 机器</button></form><section id="task-detail" class="task-detail"></section></aside></div>`;
  document.body.classList.add('task-mode'); panel.hidden = false;
  panel.querySelector('#task-scope').onchange = event => { state.scope = event.target.value; formScope(); };
  panel.querySelector('#task-group').onchange = event => { state.group = event.target.value; formScope(); };
  panel.querySelector('#task-machine').onchange = event => { state.machine = event.target.value; formScope(); };
  panel.querySelector('#task-form').onsubmit = event => { event.preventDefault(); submit(); };
  panel.onclick = event => {
    const row = event.target.closest('[data-task-id]');
    if (row) { state.selected = row.dataset.taskId; state.logs = null; renderBoard(); renderDetail(); return; }
    const button = event.target.closest('[data-task-action]'); if (!button) return;
    const action = button.dataset.taskAction;
    if (action === 'refresh') refresh();
    else if (action === 'earliest') panel.querySelector('#task-board').scrollLeft = 0;
    else if (action === 'latest') { const board = panel.querySelector('#task-board'); board.scrollLeft = board.scrollWidth; }
    else if (action === 'check-pending') checkPending();
    else if (action === 'retry-pending') submit(true);
    else taskAction(action);
  };
  refresh();
  setInterval(() => { if (!document.hidden) refresh(); }, 5000);
})();
