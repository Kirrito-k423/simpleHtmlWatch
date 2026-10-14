const {test} = require('node:test');
const assert = require('node:assert/strict');
const plots = require('../web/metric-plots.js');

test('缺失采样与时间间断拆开曲线，真实零值保留', () => {
  const series = [{points:[{at:100,value:2},{at:101,value:0},{at:102,value:null},{at:103,value:1},{at:110,value:3}]}];
  const g = plots.geometry(series,100,110,1);
  assert.equal((g.paths[0].path.match(/M/g)||[]).length,3);
  assert.equal((g.paths[0].path.match(/L/g)||[]).length,1);
  assert.equal(g.hasData,true);
  assert.equal(plots.nearest(series[0].points,102,1).value,null);
  assert.equal(plots.nearest(series[0].points,106,1),null);
  assert.equal(plots.geometry([{points:[{at:100,value:null}]}],100,110).hasData,false);
});

test('时间缩放保留鼠标锚点，并限制最小和最大跨度', () => {
  assert.deepEqual(plots.zoom(1000,2000,.5,.25),{after:1125,before:1625});
  const small = plots.zoom(1000,1001,.01);
  assert.equal(small.before-small.after,60);
  const large = plots.zoom(1000000,2000000,100);
  assert.equal(large.before-large.after,7*86400);
});
