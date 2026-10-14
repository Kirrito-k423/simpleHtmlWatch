'use strict';
(() => {
  if (new URLSearchParams(location.search).get('tasks') !== '1') return;
  const panel = document.querySelector('#task-panel');
  const token = document.querySelector('meta[name="watch-token"]').content;
  const escape = value => String(value ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const labels = {waiting_resources:'等待资源', offered:'等待 AI 领取', claimed:'AI 已接手', executing:'实验执行中', awaiting_review:'等待 AI 核对结果', recovery_needed:'需要查询原任务', completed:'已完成', paused:'已暂停', cancelled:'已取消', expired:'授权已到期'};
  const delivery = {pending:'尚未发送', sending:'正在发送', delivered:'送达已确认', delivery_uncertain:'送达待核实', failed:'发送被拒绝'};
  const time = value => value ? new Date(value).toLocaleString('zh-CN', {timeZone:'Asia/Shanghai', hour12:false}) : '—';
  const section = document.createElement('section');
  section.className = 'continuation-panel';
  section.setAttribute('aria-label', '实验等待与自动继续');
  section.innerHTML = '<div class="continuation-heading"><div><h2>实验等待与自动继续</h2><p>资源满足时继续原 AI 对话；取消只停止后续调度。</p></div><button type="button" data-continuation-refresh>刷新等待单</button></div><div data-continuation-message role="status"></div><div data-continuation-list></div>';
  panel.append(section);
  let busy = false, runId = null, capable = false, operationMessage = '';
  async function identity() {
    const response = await fetch('/healthz');
    if (!response.ok) throw new Error('无法核对实例身份，请刷新页面或联系实例维护者');
    const info = await response.json();
    runId = info.runId;
    capable = info.capabilities?.includes('experiment-continuations-v1');
    if (!runId || !capable) throw new Error('当前实例没有实验等待能力');
  }
  async function request(path = '', method = 'GET', data) {
    await identity();
    const response = await fetch('/api/continuations' + path, {method,
      headers:{'X-Watch-Token':token,'X-Watch-Instance':runId,...(data ? {'Content-Type':'application/json'} : {})},
      body:data ? JSON.stringify(data) : undefined});
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || `HTTP ${response.status}`);
    return result;
  }
  function render(jobs) {
    section.querySelector('[data-continuation-list]').innerHTML = jobs.length ? jobs.map(job => {
      const terminal = ['completed','cancelled','expired'].includes(job.state);
      const machines = (job.allocations || []).map(item => item.machineId).join('、');
      const details = [...Object.entries(job.excluded || {})].map(([id, reason]) => `${id}：${reason}`).join('\n');
      return `<article class="continuation-row" data-continuation-id="${escape(job.id)}"><div class="continuation-row-heading"><strong>${escape(job.experimentId)} / ${escape(job.stepId)}</strong><span>${escape(labels[job.state] || job.state)}${job.stopRequested ? ' · 已请求停止后续' : ''}</span></div>
        <p>需要 ${escape(job.resources.machineCount)} 台 ${escape(job.resources.model)} · ${escape(job.resources.group || job.resources.machineIds?.join('、'))}${machines ? ` · 分配：${escape(machines)}` : ` · 最近合格 ${escape(job.eligibleCount)} 台`}</p>
        <p>原对话 ${escape(job.target.threadId)} · ${escape(job.policy.wakeMode === 'heartbeat' ? '原对话心跳' : '中台主动唤醒')} · ${escape(delivery[job.wake?.status] || '等待资源检查')}</p>
        <p>最近检查 ${escape(time(job.lastCheckAt))} · 下次检查 ${escape(time(job.nextCheckAt))}${job.offerUntil ? ` · 暂留至 ${escape(time(job.offerUntil))}` : ''}</p>
        ${job.reason || job.wake?.reason ? `<p class="continuation-reason">${escape(job.reason || job.wake.reason)}</p>` : ''}
        <div class="continuation-actions">${terminal ? '' : `<button type="button" data-continuation-control="${job.state === 'paused' || job.stopRequested ? 'resume' : 'pause'}">${job.state === 'paused' || job.stopRequested ? '恢复后续' : '暂停后续'}</button><button type="button" data-continuation-control="cancel">取消后续</button>`}
        <details><summary>查看恢复与排除原因</summary><pre>${escape(details || '无排除记录')}\n等待单：${escape(job.id)}\n代数：${escape(job.generation)}\n事件：${escape(job.wake?.id || '—')}\n轮次：${escape(job.wake?.turnId || '—')}\n任务：${escape(job.tasks.map(task => task.id).join('、'))}\n检查点：${escape(job.checkpoint.path)}</pre></details></div></article>`;
    }).join('') : '<p class="continuation-empty">暂无等待单。AI 遇到资源不足时可登记实验，随后自动继续。</p>';
  }
  async function refresh() {
    if (busy) return;
    busy = true;
    try {
      const result = await request();
      render(result.continuations || []);
      section.querySelector('[data-continuation-message]').textContent = operationMessage || result.schedulerError || '';
    } catch (error) { section.querySelector('[data-continuation-message]').textContent = error.message; }
    finally { busy = false; }
  }
  section.onclick = async event => {
    if (event.target.closest('[data-continuation-refresh]')) { refresh(); return; }
    const button = event.target.closest('[data-continuation-control]');
    if (!button || busy || !capable) return;
    busy = true; button.disabled = true;
    try {
      await request('/control', 'POST', {id:button.closest('[data-continuation-id]').dataset.continuationId, action:button.dataset.continuationControl});
      operationMessage = '';
    } catch (error) { operationMessage = `${error.message}；操作未自动重试，请先查询状态。`; section.querySelector('[data-continuation-message]').textContent = operationMessage; }
    finally { busy = false; button.disabled = false; }
    await refresh();
  };
  refresh();
  setInterval(() => { if (!document.hidden) refresh(); }, 5000);
})();
