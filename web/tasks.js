'use strict';
(() => {
  if (new URLSearchParams(location.search).get('tasks') !== '1') return;
  const panel = document.querySelector('#task-panel');
  const token = document.querySelector('meta[name="watch-token"]').content;
  const labels = {dispatching:'发送中', running:'运行中', unknown:'结果未知', succeeded:'命令成功', failed:'命令失败', abandoned:'人工解除', archive_ready:'结果已回收', archive_error:'回收失败', legacy_created:'旧记录创建', legacy_finished:'旧记录结束'};
  const state = {jobs:[], ready:[], machines:[], collapsed:new Set(), view:null, busy:false, selected:null, pending:null, logs:null, notice:'', error:'', refreshing:false, scope:'auto', group:'', machine:''};
  const esc = value => String(value ?? '').replace(/[&<>"']/g, char => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[char]));
  const full = value => value ? new Date(value).toLocaleString('zh-CN', {hour12:false}) : '—';
  const timeline = window.TaskTimeline;
  const symbols = {dispatching:'◆', running:'●', succeeded:'✓', failed:'✕', unknown:'?', abandoned:'⊘', archive_ready:'■', archive_error:'!', legacy_created:'◇', legacy_finished:'□'};
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
    const groups = [...new Set(state.machines.map(item => item.group).filter(Boolean))].sort();
    if (state.group && !groups.includes(state.group)) groups.unshift(state.group);
    const group = panel.querySelector('#task-group');
    group.innerHTML = groups.map(item => `<option value="${esc(item)}">${esc(item)}${state.ready.some(host => host.group === item) ? '' : ' · 暂无 ready 机器'}</option>`).join('');
    if (!state.group && groups.length) state.group = groups[0];
    group.value = state.group;
    const machines = [...state.machines];
    if (state.machine && !machines.some(item => item.id === state.machine)) machines.unshift({id:state.machine, name:state.machine + ' · 暂无 ready'});
    const machine = panel.querySelector('#task-machine');
    machine.innerHTML = machines.map(item => `<option value="${esc(item.id)}">${esc(item.name || item.id)} · ${esc(item.reason || '暂无 ready')} · ${esc(item.id)}</option>`).join('');
    if (!state.machine && machines.length) state.machine = machines[0].id;
    machine.value = state.machine;
    formScope();
  }
  function renderBoard() {
    const board = panel.querySelector('#task-board'), now = Date.now();
    const bounds = timeline.extent(state.jobs, now);
    const view = state.view ? timeline.clamp(state.view, bounds) : bounds;
    if (state.view) state.view = view;
    const groups = timeline.groupMachines(state.machines, state.jobs);
    panel.querySelector('#task-count').textContent = `${groups.length} 台机器 · ${state.jobs.length} 条任务`;
    panel.querySelector('#task-range').textContent = `${full(view.start)} — ${full(view.end)} · 跨度 ${elapsed(view.end - view.start)}`;
    const ticks = timeline.ticks(view, Math.max(2, (board.clientWidth - 220) / 125));
    const grid = ticks.map(tick => `<i class="task-gridline" data-pos="${tick.percent}"></i>`).join('');
    const nowLine = now >= view.start && now <= view.end ? `<i class="task-now-line" data-pos="${timeline.position(now, view)}" title="现在"></i>` : '';
    const row = job => {
      const start = Date.parse(job.createdAt), end = job.finishedAt ? Date.parse(job.finishedAt) : now;
      const left = Math.max(0, timeline.position(start, view)), right = Math.min(100, timeline.position(end, view));
      const intersects = start <= view.end && end >= view.start;
      const events = timeline.eventsFor(job).filter(event => Date.parse(event.at) >= view.start && Date.parse(event.at) <= view.end);
      let previous = -Infinity, level = 0;
      const markers = events.map(event => {
        const pos = timeline.position(Date.parse(event.at), view);
        level = (pos - previous) * Math.max(1, board.clientWidth - 220) / 100 < 22 ? (level + 1) % 3 : 0;
        previous = pos;
        const title = `${labels[event.type] || event.type} · ${full(event.at)} · ${event.text || ''}`;
        return `<button class="task-marker ${esc(event.type)} level-${level}" data-pos="${pos}" data-task-id="${esc(job.id)}" title="${esc(title)}" aria-label="${esc(title)}">${symbols[event.type] || '◇'}</button>`;
      }).join('');
      return `<div class="task-lane ${job.id === state.selected ? 'selected' : ''}" role="row"><button class="task-row-title" data-task-id="${esc(job.id)}" role="rowheader" title="${esc(job.id)}"><strong>${esc(job.id)}</strong><span><em class="task-status ${esc(job.status)}">${esc(labels[job.status] || job.status)}</em> · ${elapsed(Math.max(0, end - start))}${job.finishedAt ? '' : '（持续占用）'}</span></button><div class="task-track" role="cell">${grid}${nowLine}${intersects ? `<button class="task-duration ${esc(job.status)}" data-pos="${left}" data-width="${Math.max(0, right - left)}" data-task-id="${esc(job.id)}" aria-label="查看任务 ${esc(job.id)}" title="${esc(full(start))} → ${job.finishedAt ? esc(full(end)) : '现在（未知状态不代表持续运行）'} · ${elapsed(Math.max(0, end - start))}"></button>` : ''}${markers}</div></div>`;
    };
    board.innerHTML = `<div class="task-axis" role="row"><div class="task-axis-label">实际机器 / 任务</div><div class="task-track" role="columnheader">${ticks.map(tick => `<time data-pos="${tick.percent}" title="${esc(full(tick.at))}">${esc(clock(tick.at))}<small>${esc(new Date(tick.at).toLocaleDateString('zh-CN'))}</small></time>`).join('')}</div></div>${groups.map(group => {
      const ready = group.machines.some(machine => machine.ready);
      const active = group.jobs.filter(job => !job.finishedAt).length;
      const reason = [...new Set(group.machines.map(machine => machine.reason))].join(' / ') || '历史机器（已移出配置）';
      const reservations = [...new Set(group.machines.flatMap(machine => machine.reservationIds || []))];
      const blockers = [...new Set(group.machines.flatMap(machine => machine.taskIds || []))];
      return `<section class="task-machine-group" aria-label="${esc(group.name)}"><div class="task-machine-heading"><div><button class="task-machine-toggle" data-machine-toggle="${esc(group.key)}" aria-expanded="${!state.collapsed.has(group.key)}" aria-label="${state.collapsed.has(group.key) ? '展开' : '收起'}机器 ${esc(group.name)}">${state.collapsed.has(group.key) ? '▸' : '▾'} <strong>${esc(group.name)}</strong></button><span>${esc(group.host)}${group.machines.length > 1 ? ` · ${group.machines.length} 个配置别名` : ''}</span></div><span class="${ready ? 'green' : ''}">${esc(reason)} · ${active} 个未结束 / ${group.jobs.length} 个任务</span>${reservations.length ? `<small>预约：${esc(reservations.join(', '))}</small>` : ''}${blockers.length ? `<div class="task-blockers">占用任务：${blockers.map(id => `<button data-task-id="${esc(id)}">${esc(id)}</button>`).join('')}</div>` : ''}</div>${state.collapsed.has(group.key) ? '' : group.jobs.length ? group.jobs.map(row).join('') : '<div class="task-no-jobs">暂无任务记录 · 机器仍在配置中</div>'}</section>`;
    }).join('') || '<div class="task-empty">还没有配置机器，请先返回监控添加机器。</div>'}`;
    // CSSOM assignments are compatible with the strict style-src CSP. No inline
    // style attributes are parsed from task data or HTML templates.
    board.querySelectorAll('[data-pos]').forEach(element => { element.style.left = element.classList.contains('task-marker') ? `clamp(11px, ${element.dataset.pos}%, calc(100% - 11px))` : element.dataset.pos + '%'; });
    board.querySelectorAll('[data-width]').forEach(element => { element.style.width = element.dataset.width + '%'; });
  }
  function changeView(action, anchor = .5, factor) {
    const bounds = timeline.extent(state.jobs), view = state.view || bounds;
    const span = view.end - view.start;
    if (action === 'fit') state.view = null;
    else if (action === 'earliest') state.view = timeline.clamp({start:bounds.start, end:bounds.start + span}, bounds);
    else if (action === 'latest') state.view = timeline.clamp({start:bounds.end - span, end:bounds.end}, bounds);
    else if (action === 'pan-left' || action === 'pan-right') state.view = timeline.pan(view, bounds, action === 'pan-left' ? -.5 : .5);
    else state.view = timeline.zoom(view, bounds, factor || (action === 'zoom-in' ? .5 : 2), anchor);
    renderBoard();
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
      <div class="task-detail-actions">${!job.finishedAt && job.status !== 'dispatching' ? '<button data-task-action="probe">重新探测远端状态</button>' : ''}<button data-task-action="logs">读取日志末尾</button>${hasExit(job) && !job.archiveReady ? '<button data-task-action="collect">重试回收</button>' : ''}${job.archiveReady ? '<button data-task-action="download">下载结果包</button>' : ''}</div>
      ${job.status === 'unknown' && !job.finishedAt ? '<div class="task-problem">结果未知会保留机器占用。先探测状态、读取日志，并核实远端进程及子进程已停止，再解除本地占用。<button data-task-action="resolve">已核实远端停止，解除占用…</button></div>' : ''}
      ${job.reservationId ? `<p class="task-log-hint">所属预约：${esc(job.reservationId)}。解除任务后，独立预约仍由原调用方管理。</p>` : ''}
      <details><summary>全部里程碑（${timeline.eventsFor(job).length}）</summary><ol class="task-event-list">${timeline.eventsFor(job).map(event => `<li><b>${symbols[event.type] || '◇'} ${esc(labels[event.type] || event.type)}</b><time>${esc(full(event.at))}</time><span>${esc(event.text)}</span></li>`).join('')}</ol></details>
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
    const online = state.machines.filter(machine => ['online','partial'].includes(machine.status)).length;
    const completed = state.jobs.filter(job => job.finishedAt).length;
    panel.querySelector('#task-stats').innerHTML = `<span><b>${state.machines.length}</b> 注册机器</span><span><b>${online}</b> 监控在线</span><span><b>${state.ready.length}</b> ready</span><span><b>${running}</b> 运行或发送</span><span><b>${unknown}</b> 结果未知</span><span><b>${completed}</b> 已结束</span>`;
    readyOptions(); renderBoard(); renderDetail(); renderMessage();
  }
  async function refresh() {
    if (state.refreshing) return;
    state.refreshing = true;
    try {
      const [jobs, machines] = await Promise.all([request(''), request('/machines')]);
      state.jobs = Array.isArray(jobs) ? jobs : [];
      state.machines = Array.isArray(machines) ? machines : [];
      state.ready = state.machines.filter(machine => machine.ready);
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
      state.view = null;
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
    const job = selected(); if (!job || state.busy) return;
    if (action === 'resolve' && !window.confirm(`解除任务 ${job.id} 在 ${job.machineName || job.selectedMachineId}（${job.host}）的占用？\n\n仅在你已独立核实远端任务及子进程停止后确认。这不会终止远端进程，也不会把未知退出码记作成功。${job.reservationId ? '\n该任务的独立预约仍由原调用方管理。' : ''}`)) return;
    state.busy = true;
    try {
      if (action === 'logs') state.logs = {id:job.id, ...await request('/logs?id=' + encodeURIComponent(job.id))};
      if (action === 'probe') {
        const result = await request('/probe?id=' + encodeURIComponent(job.id), 'POST');
        state.notice = `任务 ${job.id} 探测完成：${labels[result.status] || result.status}${result.error ? ' · ' + result.error : ''}`;
        await refresh();
      }
      if (action === 'resolve') {
        await request('/resolve', 'POST', {id:job.id, confirm:'remote-stopped'});
        state.notice = `任务 ${job.id} 已解除本地占用，退出结果仍未知。${job.reservationId ? '独立预约仍保留，请联系原调用方释放。' : ''}`;
        await refresh();
      }
      if (action === 'collect') { await request('/collect?id=' + encodeURIComponent(job.id), 'POST'); await refresh(); }
      if (action === 'download') {
        downloadTaskArchive(job.id, token);
        state.notice = '已交给浏览器下载，请在浏览器下载列表确认进度与完成状态。';
      }
      state.error = ''; renderDetail(); renderMessage();
    } catch (error) { state.error = error.message; renderMessage(); }
    finally { state.busy = false; }
  }
  panel.innerHTML = `<div class="task-page-head"><div><div class="eyebrow">SSH TASK CONTROLLER / EVENT TIMELINE</div><h1>任务中台</h1><p>按实际机器查看任务时长与里程碑；所有注册机器均可见，忙碌和离线不会隐藏。</p></div><div class="task-head-actions"><div id="task-stats" class="task-stats"></div><button data-task-action="refresh">刷新</button><a href="/">返回监控</a></div></div>
    <div id="task-message" role="status" hidden></div><div class="task-page-grid"><section class="task-board-card"><div class="task-section-heading"><div><h2>任务时间泳道</h2><p>连续等比例时间轴 · 滚轮缩放鼠标附近时间 · Shift + 滚轮平移</p></div><div class="task-board-nav"><span id="task-count"></span><button data-task-action="collapse">收起机器</button><button data-task-action="fit">全局</button><button data-task-action="zoom-in" aria-label="放大时间轴">＋</button><button data-task-action="zoom-out" aria-label="缩小时间轴">−</button><button data-task-action="pan-left" aria-label="向前平移">‹</button><button data-task-action="pan-right" aria-label="向后平移">›</button><button data-task-action="earliest" aria-label="移到最早事件">← 最早</button><button data-task-action="latest" aria-label="移到最近事件">最近 →</button></div></div><div class="task-timeline-info"><span id="task-range"></span><div class="task-legend"><span class="dispatching">◆ 受理</span><span class="running">● 运行</span><span class="succeeded">✓ 成功</span><span class="failed">✕ 失败</span><span class="unknown">? 未知</span><span class="abandoned">⊘ 解除</span><span class="archive_ready">■ 回收</span></div></div><div id="task-board" class="task-board" role="table" aria-label="按实际机器分组的任务时间轴"></div><p class="task-board-note">时间是中台的受理或观测时间，远端退出可能早于轮询发现。长条表示从受理到结束（或现在）的占用时间；“未知”不证明进程持续运行。选择任务可查看全部里程碑。</p></section>
    <aside class="task-side"><form id="task-form" class="task-compose"><div class="task-section-heading"><h2>发送任务</h2><span>一次调度一台 ready 机器</span></div><label>调度范围<select id="task-scope"><option value="auto">自动调度 · 所有 ready 机器</option><option value="group">指定机器组</option><option value="machine">指定机器</option></select></label><select id="task-group" aria-label="选择机器组" hidden></select><select id="task-machine" aria-label="选择机器" hidden></select><div class="task-preview"><small>预计落点 · 实际机器以提交响应为准</small><div id="task-target"></div></div><label>远端 Bash 命令<textarea id="task-command" rows="5" spellcheck="false" maxlength="4096" placeholder="python train.py --epochs 5&#10;cp summary.json &quot;$SHW_RESULTS_DIR/&quot;"></textarea></label><p>把需要回收的文件写入 <code>$SHW_RESULTS_DIR</code>；stdout 与 stderr 单独保存。自动调度可能选到不同硬件，命令依赖型号时请限定组或机器。ready 不代表 NPU 空闲。</p><button id="task-submit" class="primary" type="submit">发送到 ready 机器</button></form><section id="task-detail" class="task-detail"></section></aside></div>`;
  document.body.classList.add('task-mode'); panel.hidden = false;
  panel.querySelector('#task-scope').onchange = event => { state.scope = event.target.value; formScope(); };
  panel.querySelector('#task-group').onchange = event => { state.group = event.target.value; formScope(); };
  panel.querySelector('#task-machine').onchange = event => { state.machine = event.target.value; formScope(); };
  panel.querySelector('#task-form').onsubmit = event => { event.preventDefault(); submit(); };
  panel.onclick = event => {
    const machine = event.target.closest('[data-machine-toggle]');
    if (machine) {
      const key = machine.dataset.machineToggle;
      if (state.collapsed.has(key)) state.collapsed.delete(key); else state.collapsed.add(key);
      renderBoard(); return;
    }
    const row = event.target.closest('[data-task-id]');
    if (row) { state.selected = row.dataset.taskId; state.logs = null; renderBoard(); renderDetail(); return; }
    const button = event.target.closest('[data-task-action]'); if (!button) return;
    const action = button.dataset.taskAction;
    if (action === 'refresh') refresh();
    else if (action === 'collapse') { state.collapsed = new Set(timeline.groupMachines(state.machines, state.jobs).map(group => group.key)); renderBoard(); }
    else if (['earliest','latest','fit','zoom-in','zoom-out','pan-left','pan-right'].includes(action)) changeView(action);
    else if (action === 'check-pending') checkPending();
    else if (action === 'retry-pending') submit(true);
    else taskAction(action);
  };
  panel.querySelector('#task-board').addEventListener('wheel', event => {
    if (!state.jobs.length) return;
    const board = panel.querySelector('#task-board'), box = board.getBoundingClientRect();
    const labelWidth = board.querySelector('.task-axis-label')?.getBoundingClientRect().width || 220;
    const x = event.clientX - box.left - labelWidth;
    if (x < 0) return; // The machine labels retain ordinary vertical scrolling.
    event.preventDefault();
    const delta = event.deltaY || event.deltaX;
    if (!delta) return;
    if (event.shiftKey) changeView(delta > 0 ? 'pan-right' : 'pan-left');
    else changeView('zoom', x / Math.max(1, box.width - labelWidth), Math.exp(Math.max(-.5, Math.min(.5, delta * .002))));
  }, {passive:false});
  new ResizeObserver(() => { if (state.machines.length || state.jobs.length) renderBoard(); }).observe(panel.querySelector('#task-board'));
  refresh();
  setInterval(() => { if (!document.hidden) refresh(); }, 5000);
})();
