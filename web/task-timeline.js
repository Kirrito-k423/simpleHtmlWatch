/* Event-focused task timeline. Columns are ordered by real timestamps; gaps are
   deliberately non-linear so a quiet day cannot swallow the useful events. */
(() => {
  'use strict';
  const minute = 60 * 1000;
  function eventsFor(job) {
    const recorded = Array.isArray(job.events) ? job.events.filter(item => Number.isFinite(Date.parse(item.at))) : [];
    const events = recorded.map(item => ({...item, taskId:job.id, legacy:false}));
    // Records made before event capture have no historical transition times.
    if (!recorded.some(item => item.type === 'dispatching') && Number.isFinite(Date.parse(job.createdAt))) events.push({at:job.createdAt, type:'legacy_created', text:'旧记录：只知道创建时间', taskId:job.id, legacy:true});
    if (job.finishedAt && !recorded.some(item => ['succeeded','failed','abandoned'].includes(item.type)) && Number.isFinite(Date.parse(job.finishedAt))) events.push({at:job.finishedAt, type:'legacy_finished', text:'旧记录：只知道结束时间与当前结果', taskId:job.id, legacy:true});
    return events;
  }
  function buildTaskTimeline(jobs, clock = Date.now()) {
    const points = jobs.flatMap(eventsFor).sort((a, b) => Date.parse(a.at) - Date.parse(b.at));
    const buckets = [];
    for (const point of points) {
      const at = Date.parse(point.at);
      const previous = buckets[buckets.length - 1];
      if (previous && at - previous.end <= minute) {
        previous.end = at;
        previous.events.push(point);
      } else buckets.push({kind:'events', start:at, end:at, events:[point], width:154});
    }
    const columns = [];
    let previous;
    for (const bucket of buckets) {
      if (previous && bucket.start - previous.end > 2 * minute) {
        columns.push({kind:'gap', start:previous.end, end:bucket.start, duration:bucket.start - previous.end, width:82});
      }
      columns.push(bucket);
      previous = bucket;
    }
    const last = buckets.at(-1);
    if (jobs.some(job => !job.finishedAt) && (!last || clock > last.end)) {
      if (last && clock - last.end > 2 * minute) columns.push({kind:'gap', start:last.end, end:clock, duration:clock - last.end, width:82});
      columns.push({kind:'now', start:clock, end:clock, width:88});
    }
    return columns;
  }
  if (typeof module !== 'undefined' && module.exports) module.exports = {buildTaskTimeline, eventsFor};
  else window.buildTaskTimeline = buildTaskTimeline;
})();
