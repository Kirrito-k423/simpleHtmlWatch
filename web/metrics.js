'use strict';
(() => {
  const dialog = document.querySelector('#metrics-dialog');
  const token = document.querySelector('meta[name="watch-token"]').content;
  const $ = selector => dialog.querySelector(selector);
  const escape = v => String(v ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const full = value => new Date(value).toLocaleString('zh-CN', {hour12:false});
  const clock = seconds => new Date(seconds * 1000).toLocaleTimeString('zh-CN', {hour12:false});
  const names = {user:'用户态',system:'内核态',nice:'低优先级',iowait:'I/O 等待',irq:'硬中断',softirq:'软中断',steal:'虚拟机等待',guest:'虚拟机',used:'已使用',cached:'缓存',buffers:'缓冲',free:'空闲',in:'读取',out:'写入',received:'接收',sent:'发送'};
  const eventNames = {dispatching:'受理',running:'启动确认',succeeded:'成功',failed:'失败',unknown:'状态未知',archive_ready:'结果回收',abandoned:'人工解除'};
  let machines = [], machineId = '', taskId = '', snapshot = null, request = null, generation = 0, follow = true, manualRange = null, isDemo = false;
  const number = n => Number.isFinite(n) ? n.toLocaleString('zh-CN', {maximumFractionDigits:2}) : '缺失';
  const localInput = seconds => {
    const d = new Date(seconds * 1000);
    const part = n => String(n).padStart(2,'0');
    return `${d.getFullYear()}-${part(d.getMonth()+1)}-${part(d.getDate())}T${part(d.getHours())}:${part(d.getMinutes())}:${part(d.getSeconds())}`;
  };
  function message(text, error = false) { $('#metrics-message').textContent = text; $('#metrics-message').classList.toggle('error', error); }
  function clear() { snapshot = null; $('#metrics-charts').replaceChildren(); $('#metrics-meta').textContent = ''; $('#metrics-hover').textContent = ''; }
  function cancel() { generation++; request?.abort(); request = null; }
  function taskMarkers(data, geometry) {
    if (!data.task) return '';
    const start = Date.parse(data.task.start) / 1000, end = data.task.end ? Date.parse(data.task.end) / 1000 : data.before;
    const lo = Math.max(start, data.after), hi = Math.min(end, data.before);
    let markup = hi > lo ? `<rect class="metric-task-window" x="${geometry.x(lo)}" y="12" width="${geometry.x(hi)-geometry.x(lo)}" height="138"/>` : '';
    for (const event of data.task.events || []) {
      const at = Date.parse(event.at) / 1000;
      if (at < data.after || at > data.before) continue;
      const x = geometry.x(at);
      markup += `<line class="metric-task-marker" x1="${x}" x2="${x}" y1="12" y2="150"><title>${escape(eventNames[event.type] || event.type)} · ${escape(full(event.at))} · ${escape(event.text)}</title></line>`;
    }
    return markup;
  }
  function values(chart, at = null) {
    return chart.series.map((series, i) => {
      const point = at === null ? series.points.at(-1) : MetricPlots.nearest(series.points, at, chart.step);
      return `<span class="metric-${MetricPlots.colors[i % MetricPlots.colors.length]}"><b>${escape(names[series.name] || series.name)}</b> ${number(point?.value)} ${escape(series.unit)}</span>`;
    }).join('');
  }
  function draw(data) {
    clear(); snapshot = data;
    if (data.task) { taskId = data.task.id; $('#metrics-task-label').textContent = `任务 ${data.task.id} · 本机观测时间`; }
    else $('#metrics-task-label').textContent = '整机系统指标';
    $('#metrics-after').value = localInput(data.after); $('#metrics-before').value = localInput(data.before);
    $('#metrics-meta').textContent = `${isDemo ? '模拟数据' : `Netdata ${data.agentVersion || ''}`} · 查询 ${full(data.fetchedAt)}${data.nodeId ? ' · 节点 ' + data.nodeId : ''}`;
    const cards = data.charts.map((chart, index) => {
      if (chart.error) return `<article class="metric-card"><h3>${escape(chart.name)}</h3><p class="metric-problem">${escape(chart.error)}</p></article>`;
      const g = MetricPlots.geometry(chart.series, data.after, data.before, chart.step, chart.context === 'system.cpu' ? 100 : 0);
      const stale = follow && chart.lastEntry > 0 && Math.floor(Date.now()/1000) - chart.lastEntry > Math.max(30, chart.updateEvery * 3);
      return `<article class="metric-card" data-chart="${index}"><div class="metric-card-head"><h3>${escape(chart.name)} <small>${escape(chart.unit)}</small></h3><span class="${stale ? 'metric-stale' : ''}">${chart.lastEntry ? 'Agent 最新数据 ' + escape(clock(chart.lastEntry)) : '无最新采样'} · 采样 ${chart.updateEvery}s / 显示 ${chart.step}s</span></div>
        ${g.hasData ? `<svg class="metric-plot" viewBox="0 0 760 180" role="img" aria-label="${escape(chart.name)}时间曲线"><line class="metric-gridline" x1="56" x2="748" y1="12" y2="12"/><line class="metric-gridline" x1="56" x2="748" y1="81" y2="81"/><line class="metric-gridline" x1="56" x2="748" y1="150" y2="150"/><text x="50" y="18" text-anchor="end">${number(g.max)}</text><text x="50" y="153" text-anchor="end">${number(g.min)}</text>${taskMarkers(data,g)}${g.paths.map(path => `<path class="metric-line metric-${path.color}" d="${path.path}"/>`).join('')}<line class="metric-cursor" x1="56" x2="56" y1="12" y2="150" hidden/><text x="56" y="174">${escape(clock(data.after))}</text><text x="402" y="174" text-anchor="middle">${escape(clock((data.after+data.before)/2))}</text><text x="748" y="174" text-anchor="end">${escape(clock(data.before))}</text></svg>` : '<div class="metric-no-data">这个时间段没有有效采样。可能超出 Agent 保留范围，或采集尚未开始。</div>'}
        <div class="metric-values">${values(chart)}</div></article>`;
    });
    $('#metrics-charts').innerHTML = cards.join('');
    const notes = [];
    if (isDemo) notes.push('演示数据均为虚构，未连接服务器。');
    if (data.windowClipped) notes.push('任务跨度超过 7 天，当前显示最后 7 天；可指定更早的时间段。');
    if (Math.abs(data.clockSkewSeconds || 0) > 5) notes.push('Agent 响应时钟与本机时间相差超过 5 秒，请核对时钟与网络延迟后判断任务与曲线的对应关系。');
    const failed = data.charts.filter(chart => chart.error).length;
    if (failed) notes.push(`${failed} 项指标读取失败，其他图表仍可查看。`);
    message(notes.join(' ') || (data.task ? '阴影与竖线对应任务观测区间和状态事件。' : '移动鼠标查看同一时刻，滚轮缩放时间范围。'));
    $('#metrics-live').checked = follow;
  }
  function demoSnapshot(after, before) {
    const labels = [['user','system'],['used','cached','free'],['in','out'],['received','sent']];
    const units = ['%','MiB','KiB/s','kilobits/s'];
    const contexts = ['system.cpu','system.ram','system.io','system.net'];
    const titles = ['CPU','内存','磁盘吞吐','网络流量'];
    const step = Math.max(1, Math.ceil((before-after)/180));
    return {machineId,source:'demo',machineName:machines.find(m => m.id === machineId)?.name,agentVersion:'',fetchedAt:new Date().toISOString(),after,before,charts:contexts.map((context,i) => ({context,name:titles[i],unit:units[i],step,updateEvery:1,lastEntry:before,series:labels[i].map((name,j) => ({name,unit:units[i],points:Array.from({length:181},(_,k) => ({at:after+k*(before-after)/180,value:k>=78 && k<=86 ? null : Math.max(0,(i ? 1500*(j+1) : 22/(j+1)) * (1+.45*Math.sin(k/14+j+i)))}))}))}))};
  }
  async function load() {
    cancel(); const current = generation;
    const machine = machines.find(m => m.id === machineId);
    clear();
    if (!machine) { message('还没有可查询的机器。请先在“管理机器”中添加服务器。'); return; }
    if (!isDemo && (!machine.enabled || !machine.netdata?.enabled)) {
      message(`${machine.name} 未启用系统指标。在服务器安装 Netdata 后，在“管理机器”中勾选“Netdata 系统指标”。`); return;
    }
    message('正在通过 SSH 读取系统指标…');
    const q = new URLSearchParams(taskId ? {taskId} : {machineId});
    let range = manualRange;
    if (follow) { const before = Math.floor(Date.now()/1000); range = {before,after:before-Number($('#metrics-span').value || 900)}; }
    if (range) { q.set('after', String(range.after)); q.set('before', String(range.before)); }
    if (isDemo) { const before = Math.floor(Date.now()/1000); draw(demoSnapshot(range?.after || before-900,range?.before || before)); return; }
    const controller = new AbortController(); request = controller;
    try {
      const response = await fetch('/api/metrics?' + q, {signal:controller.signal,headers:{'X-Watch-Token':token}});
      const data = await response.json();
      if (!response.ok) throw new Error(data.error || `读取失败 HTTP ${response.status}`);
      if (current !== generation || !dialog.open) return;
      draw(data);
    } catch (error) {
      if (current === generation && error.name !== 'AbortError') { clear(); message(error.message,true); }
    } finally { if (current === generation) request = null; }
  }
  async function open(options = {}) {
    cancel(); clear(); manualRange = null; taskId = options.taskId || ''; machineId = options.machineId || '';
    isDemo = typeof demo !== 'undefined' && demo;
    follow = !taskId; $('#metrics-live').checked = follow; $('#metrics-span').value = taskId ? 'task' : '900';
    $('#metrics-task-range').disabled = !taskId;
    $('#metrics-task-label').textContent = taskId ? `任务 ${taskId} · 本机观测时间` : '整机系统指标';
    if (!dialog.open) dialog.showModal();
    message('正在读取机器配置…');
    const current = generation;
    try {
      if (isDemo) machines = config.machines;
      else {
        const response = await fetch('/api/config', {headers:{'X-Watch-Token':token}});
        const body = await response.json(); if (!response.ok) throw new Error(body.error || '读取配置失败');
        if (current !== generation || !dialog.open) return;
        machines = body.config.machines;
      }
      if (!machineId) machineId = machines.find(m => m.enabled && m.netdata?.enabled)?.id || machines[0]?.id || '';
      $('#metrics-machine').innerHTML = machines.map(m => `<option value="${escape(m.id)}">${escape(m.name)}${m.netdata?.enabled || isDemo ? '' : ' · 未启用'}</option>`).join('');
      $('#metrics-machine').value = machineId;
      await load();
    } catch (error) { if (current === generation) message(error.message,true); }
  }
  function setRange(range, preservePreset = false) {
    const now = Math.floor(Date.now()/1000);
    if (range.before > now) { range.after -= range.before-now; range.before = now; }
    manualRange = range; follow = false; $('#metrics-live').checked = false;
    if (!preservePreset) $('#metrics-span').value = 'custom';
    load();
  }
  function transform(factor, pan = 0, anchor = .5) {
    if (!snapshot) return;
    const {after,before} = snapshot;
    const range = MetricPlots.zoom(after,before,factor,anchor);
    const shift = Math.round((before-after)*pan); range.after += shift; range.before += shift;
    if (range.after < 1) return;
    setRange(range);
  }
  $('#metrics-machine').onchange = () => { machineId = $('#metrics-machine').value; taskId = ''; follow = true; manualRange = null; $('#metrics-task-range').disabled = true; $('#metrics-span').value = '900'; $('#metrics-task-label').textContent = '整机系统指标'; load(); };
  $('#metrics-span').onchange = () => {
    if ($('#metrics-span').value === 'task') { manualRange = null; follow = false; load(); return; }
    const before = Math.floor(Date.now()/1000); setRange({before,after:before-Number($('#metrics-span').value)},true);
  };
  $('#metrics-live').onchange = () => {
    follow = $('#metrics-live').checked;
    if (follow && !Number($('#metrics-span').value)) $('#metrics-span').value = '900';
    if (!follow && snapshot) manualRange = {after:snapshot.after,before:snapshot.before}; load();
  };
  $('#metrics-refresh').onclick = load;
  $('#metrics-apply').onclick = () => {
    const after = Math.floor(Date.parse($('#metrics-after').value)/1000), before = Math.floor(Date.parse($('#metrics-before').value)/1000);
    if (!Number.isFinite(after) || !Number.isFinite(before) || after < 1 || before <= after || before-after > 7*86400 || before > Math.floor(Date.now()/1000)+1) { message('请选择有效的开始与结束时间，跨度最多 7 天，结束时间不晚于现在。',true); return; }
    setRange({after,before});
  };
  $('#metrics-close').onclick = () => dialog.close();
  dialog.addEventListener('close', () => { cancel(); clear(); machines = []; $('#metrics-machine').replaceChildren(); });
  dialog.addEventListener('click', event => {
    const action = event.target.closest('[data-metric-action]')?.dataset.metricAction;
    if (action === 'zoom-in') transform(.5); if (action === 'zoom-out') transform(2);
    if (action === 'prev') transform(1,-.5); if (action === 'next') transform(1,.5);
  });
  $('#metrics-charts').addEventListener('mousemove', event => {
    const svg = event.target.closest('.metric-plot'); if (!svg || !snapshot) return;
    const box = svg.getBoundingClientRect();
    const ratio = Math.max(0,Math.min(1,((event.clientX-box.left)/box.width*760-56)/692));
    const at = snapshot.after+(snapshot.before-snapshot.after)*ratio;
    $('#metrics-hover').textContent = full(at*1000);
    dialog.querySelectorAll('.metric-card[data-chart]').forEach(card => {
      const chart = snapshot.charts[Number(card.dataset.chart)];
      card.querySelector('.metric-values').innerHTML = values(chart,at);
      const cursor = card.querySelector('.metric-cursor');
      if (cursor) { cursor.removeAttribute('hidden'); cursor.setAttribute('x1',String(56+ratio*692)); cursor.setAttribute('x2',String(56+ratio*692)); }
    });
  });
  $('#metrics-charts').addEventListener('mouseleave', () => {
    if (!snapshot) return;
    $('#metrics-hover').textContent = '';
    dialog.querySelectorAll('.metric-card[data-chart]').forEach(card => { card.querySelector('.metric-values').innerHTML = values(snapshot.charts[Number(card.dataset.chart)]); card.querySelector('.metric-cursor')?.setAttribute('hidden',''); });
  });
  $('#metrics-charts').addEventListener('wheel', event => {
    const svg = event.target.closest('.metric-plot'); if (!svg || !snapshot) return;
    event.preventDefault(); const box = svg.getBoundingClientRect();
    transform(event.deltaY < 0 ? .7 : 1.4,0,(event.clientX-box.left)/box.width);
  }, {passive:false});
  document.querySelector('#metrics-btn').onclick = () => open();
  setInterval(() => { if (dialog.open && follow && !document.hidden && !request) load(); },10000);
  window.WatchMetrics = {open};
})();
