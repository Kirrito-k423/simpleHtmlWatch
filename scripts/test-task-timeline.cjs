const assert = require('node:assert/strict');
const {buildTaskTimeline} = require('../web/task-timeline.js');

const start = '2026-09-25T00:00:00Z';
const shortlyAfter = '2026-09-25T00:00:30Z';
const nextDay = '2026-09-26T00:00:00Z';
const jobs = [
  {id:'one', createdAt:start, finishedAt:nextDay, events:[
    {at:start, type:'dispatching', text:'提交'},
    {at:shortlyAfter, type:'running', text:'启动'},
    {at:nextDay, type:'succeeded', text:'退出码 0'}
  ]}
];
const columns = buildTaskTimeline(jobs, Date.parse(nextDay));
assert.deepEqual(columns.map(item => item.kind), ['events', 'gap', 'events']);
assert.deepEqual(columns[0].events.map(item => item.at), [start, shortlyAfter]);
assert.equal(columns[1].duration, Date.parse(nextDay) - Date.parse(shortlyAfter));
assert.equal(columns[2].events[0].at, nextDay);
assert.equal(columns[1].width, 82); // A day of silence occupies one narrow column.

const old = buildTaskTimeline([{id:'old', createdAt:start, finishedAt:nextDay, status:'failed'}]);
assert.deepEqual(old.filter(item => item.kind === 'events').flatMap(item => item.events.map(event => event.type)), ['legacy_created', 'legacy_finished']);
assert.ok(old.flatMap(item => item.events || []).every(event => event.legacy));

const mixed = buildTaskTimeline([{id:'mixed', createdAt:start, finishedAt:nextDay, status:'succeeded', events:[{at:nextDay, type:'archive_ready', text:'结果已回收'}]}]);
assert.deepEqual(mixed.filter(item => item.kind === 'events').flatMap(item => item.events.map(event => event.type)), ['legacy_created', 'archive_ready', 'legacy_finished']);
