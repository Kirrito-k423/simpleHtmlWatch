/* Linear observed-time model shared by the dashboard and regression tests. */
(() => {
  'use strict';
  const stamp = value => Date.parse(value);
  function eventsFor(job) {
    const recorded = Array.isArray(job.events) ? job.events.filter(item => Number.isFinite(stamp(item.at))) : [];
    const events = recorded.map(item => ({...item, taskId:job.id, legacy:false}));
    if (!recorded.some(item => item.type === 'dispatching') && Number.isFinite(stamp(job.createdAt))) events.push({at:job.createdAt, type:'legacy_created', text:'旧记录：只知道创建时间', taskId:job.id, legacy:true});
    if (job.finishedAt && !recorded.some(item => ['succeeded','failed','abandoned'].includes(item.type)) && Number.isFinite(stamp(job.finishedAt))) events.push({at:job.finishedAt, type:'legacy_finished', text:'旧记录：只知道结束时间与当前结果', taskId:job.id, legacy:true});
    return events.sort((a, b) => stamp(a.at) - stamp(b.at));
  }
  function extent(jobs, now = Date.now()) {
    let start = Infinity, end = -Infinity;
    for (const job of jobs) {
      for (const at of [stamp(job.createdAt), stamp(job.finishedAt), ...eventsFor(job).map(event => stamp(event.at))]) {
        if (Number.isFinite(at)) { start = Math.min(start, at); end = Math.max(end, at); }
      }
      if (!job.finishedAt) end = Math.max(end, now);
    }
    if (!Number.isFinite(start)) return {start:now - 3600000, end:now};
    return {start, end:Math.max(end, start + 1000)};
  }
  function clamp(view, bounds) {
    const span = Math.min(bounds.end - bounds.start, Math.max(1000, view.end - view.start));
    const start = Math.max(bounds.start, Math.min(bounds.end - span, view.start));
    return {start, end:start + span};
  }
  function zoom(view, bounds, factor, anchor = .5) {
    anchor = Math.max(0, Math.min(1, anchor));
    const span = Math.max(1000, Math.min(bounds.end - bounds.start, (view.end - view.start) * factor));
    const at = view.start + (view.end - view.start) * anchor;
    return clamp({start:at - span * anchor, end:at + span * (1 - anchor)}, bounds);
  }
  function pan(view, bounds, fraction) {
    const shift = (view.end - view.start) * fraction;
    return clamp({start:view.start + shift, end:view.end + shift}, bounds);
  }
  const position = (at, view) => (at - view.start) / (view.end - view.start) * 100;
  function ticks(view, count = 8) {
    const intervals = [1000,5000,10000,30000,60000,300000,600000,1800000,3600000,10800000,21600000,43200000,86400000,604800000,2592000000,31536000000];
    const wanted = (view.end - view.start) / Math.max(2, count);
    const step = intervals.find(value => value >= wanted) || Math.ceil(wanted / 31536000000) * 31536000000;
    const out = [];
    for (let at = Math.ceil(view.start / step) * step; at <= view.end; at += step) out.push({at, percent:position(at, view)});
    return out;
  }
  const created = job => Number.isFinite(stamp(job?.createdAt)) ? stamp(job.createdAt) : 0;
  const newestFirst = (a, b) => created(b) - created(a) || String(a.id).localeCompare(String(b.id));
  function filterJobs(jobs, mode = 'running') {
    return jobs.filter(job => mode === 'all' || !job.finishedAt && (mode === 'unknown'
      ? job.status === 'unknown' : ['running','dispatching'].includes(job.status))).sort(newestFirst);
  }
  function groupMachines(machines, jobs) {
    const groups = new Map(), byID = new Map(), byHost = new Map();
    const hostKey = host => String(host || '').trim().toLowerCase();
    for (const machine of machines) {
      const host = hostKey(machine.host);
      const key = byHost.get(host)?.key || machine.resourceKey || 'host:' + (host || machine.id);
      if (!groups.has(key)) groups.set(key, {key, name:machine.name || machine.id, host:machine.host, machines:[], jobs:[]});
      const group = groups.get(key);
      group.machines.push(machine); byID.set(machine.id, group); byHost.set(host, group);
    }
    for (const job of jobs) {
      const host = hostKey(job.host);
      // A config ID may now point at a different host. Keep its old task on the
      // actual recorded host instead of silently moving history to the new one.
      let group = byHost.get(host);
      if (!host) group = byID.get(job.selectedMachineId);
      if (!group) {
        const key = host ? 'history:' + host : 'history:' + (job.selectedMachineId || 'unknown');
        if (!groups.has(key)) groups.set(key, {key, name:job.machineName || job.selectedMachineId || '历史机器', host:job.host || '', machines:[], jobs:[]});
        group = groups.get(key);
      }
      group.jobs.push(job);
    }
    return [...groups.values()].map(group => ({...group, jobs:group.jobs.sort(newestFirst)}))
      .sort((a, b) => (b.jobs.length ? created(b.jobs[0]) : -Infinity) - (a.jobs.length ? created(a.jobs[0]) : -Infinity) || 0);
  }
  const api = {eventsFor, extent, clamp, zoom, pan, position, ticks, groupMachines, filterJobs};
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else window.TaskTimeline = api;
})();
