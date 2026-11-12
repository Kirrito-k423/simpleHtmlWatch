const {test}=require('node:test');
const assert=require('node:assert/strict');
const {sortMachines}=require('../web/availability-charts.js');
const row=(name,idle,coverage,counts)=>({name,resourceKey:name,group:'A5 B07',idleSeconds:idle*60,coveragePercent:coverage,counts});
const machines=[
  row('B07-1',20,100,{}),
  // Two separate 70-minute runs versus one continuous 130-minute run.
  row('B07-2',140,100,{minutes30:4,minutes120:0}),
  row('B07-3',130,100,{minutes30:4,minutes120:1}),
  row('B07-4',0,0,{}),
];
test('each duration prioritizes green, then remaining yellow; input order is preserved',()=>{
  assert.deepEqual(sortMachines(machines,30).map(r=>r.name),['B07-2','B07-3','B07-1','B07-4']);
  assert.deepEqual(sortMachines(machines,120).map(r=>r.name),['B07-3','B07-2','B07-1','B07-4']);
  assert.deepEqual(machines.map(r=>r.name),['B07-1','B07-2','B07-3','B07-4']);
});
test('equal idle capacity prioritizes observed nodes and uses deterministic name ties',()=>{
  const busy=row('B07-9',0,100,{}),unknown=row('B07-0',0,0,{}),other=row('B07-8',0,100,{});
  assert.deepEqual(sortMachines([busy,unknown,other],30).map(r=>r.name),['B07-8','B07-9','B07-0']);
});
test('disabling idle sorting restores the original stable node order',()=>{
  assert.deepEqual(sortMachines([...machines].reverse(),120,false).map(r=>r.name),machines.map(r=>r.name));
});
