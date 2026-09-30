const test = require('node:test');
const assert = require('node:assert/strict');
const timeline = require('../web/task-timeline.js');
const start = Date.parse('2026-09-25T00:00:00Z');
const hour = 3600000;
const iso = ms => new Date(ms).toISOString();

test('所有记录共同决定全局起点，整天空白仍按线性比例显示', () => {
  const jobs = Array.from({length:30}, (_, i) => ({id:String(i), createdAt:iso(start + i * hour), finishedAt:iso(start + (i + 1) * hour)}));
  const bounds = timeline.extent(jobs, start + 40 * hour);
  assert.equal(bounds.start, start);
  assert.equal(bounds.end, start + 30 * hour);
  assert.equal(timeline.position(start + 15 * hour, bounds), 50);
  assert.equal(timeline.position(start + 24 * hour, bounds), 80);
});

test('活动与 unknown 任务持续到现在，归档事件也在范围内', () => {
  const jobs = [{id:'old', createdAt:iso(start), finishedAt:iso(start + hour), events:[{at:iso(start + 3 * hour), type:'archive_ready'}]}, {id:'unknown', status:'unknown', createdAt:iso(start + hour)}];
  assert.deepEqual(timeline.extent(jobs, start + 4 * hour), {start, end:start + 4 * hour});
  assert.deepEqual(timeline.eventsFor(jobs[0]).map(e => e.type), ['legacy_created','legacy_finished','archive_ready']);
});

test('缩放保持鼠标锚点、限制到全局及 1 秒下限；平移不越界', () => {
  const bounds = {start, end:start + 24 * hour};
  const view = timeline.zoom(bounds, bounds, .5, .25);
  assert.equal(view.start + (view.end - view.start) * .25, start + 6 * hour);
  assert.deepEqual(timeline.zoom(view, bounds, 100), bounds);
  assert.equal(timeline.zoom(view, bounds, .00000001).end - timeline.zoom(view, bounds, .00000001).start, 1000);
  assert.equal(timeline.pan(view, bounds, -100).start, bounds.start);
  assert.equal(timeline.pan(view, bounds, 100).end, bounds.end);
  const ticks = timeline.ticks(view);
  assert.ok(ticks.length >= 2 && ticks.length <= 9);
  assert.ok(ticks.every(tick => tick.percent >= 0 && tick.percent <= 100));
  assert.ok(ticks.slice(2).every((tick, i) => tick.at - ticks[i + 1].at === ticks[1].at - ticks[0].at));
});

test('按物理资源合并别名，保留无任务机器以及已删除或改址机器的历史', () => {
  const machines = [
    {id:'one', name:'主机', host:'host-a', resourceKey:'physical:a'},
    {id:'alias', name:'别名', host:'host-b', resourceKey:'physical:a'},
    {id:'empty', name:'空闲机器', host:'host-c'},
  ];
  const jobs = [
    {id:'one-job', selectedMachineId:'one', host:'host-a', createdAt:iso(start)},
    {id:'alias-job', selectedMachineId:'alias', host:'host-b', createdAt:iso(start + hour)},
    {id:'moved', selectedMachineId:'one', host:'old-host', createdAt:iso(start)},
  ];
  const groups = timeline.groupMachines(machines, jobs);
  assert.equal(groups.length, 3);
  assert.equal(groups[0].machines.length, 2);
  assert.deepEqual(groups[0].jobs.map(job => job.id), ['one-job','alias-job']);
  assert.equal(groups[1].jobs.length, 0);
  assert.equal(groups[2].host, 'old-host');
});

test('无任务及坏时间不会产生无效坐标，事件不伪造运行时间', () => {
  assert.deepEqual(timeline.extent([], start), {start:start - hour, end:start});
  const events = timeline.eventsFor({id:'legacy', createdAt:iso(start), finishedAt:iso(start + hour), events:[{at:'bad', type:'running'}]});
  assert.deepEqual(events.map(event => event.type), ['legacy_created','legacy_finished']);
  assert.ok(events.every(event => event.legacy));
});
