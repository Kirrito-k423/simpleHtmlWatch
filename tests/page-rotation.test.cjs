const {test} = require('node:test');
const assert = require('node:assert/strict');
const {PageRotation} = require('../web/page-rotation.js');

test('33 machines at 16 per page rotate through all pages and wrap', () => {
  const rotation = new PageRotation(); rotation.setSeconds(10, 0);
  assert.equal(rotation.advance(0, 3, false, 9999), 0);
  assert.equal(rotation.advance(0, 3, false, 10000), 1);
  assert.equal(rotation.advance(1, 3, false, 20000), 2);
  assert.equal(rotation.advance(2, 3, false, 30000), 0);
});
test('manual navigation resets dwell time and delayed timers never skip pages', () => {
  const rotation = new PageRotation(); rotation.setSeconds(10, 0); rotation.reset(9000);
  assert.equal(rotation.advance(1, 3, false, 10000), 1);
  assert.equal(rotation.advance(1, 3, false, 19000), 2);
  assert.equal(rotation.advance(2, 3, false, 200000), 0);
  assert.equal(rotation.advance(0, 3, false, 200001), 0);
});
test('pause, dialogs and hidden pages suspend rotation without accumulating jumps', () => {
  const rotation = new PageRotation(); rotation.setSeconds(15, 0);
  assert.equal(rotation.advance(1, 3, true, 60000), 1);
  assert.equal(rotation.advance(1, 3, false, 61000), 1);
  assert.equal(rotation.advance(1, 3, false, 75000), 2);
});
test('zero/one page, disabling and invalid saved settings do not advance', () => {
  const rotation = new PageRotation(); rotation.setSeconds(10, 0);
  assert.equal(rotation.advance(0, 0, false, 20000), 0);
  assert.equal(rotation.advance(0, 1, false, 40000), 0);
  assert.equal(rotation.advance(0, 2, false, 41000), 0);
  rotation.setSeconds(0, 41000);
  assert.equal(rotation.advance(0, 2, false, 100000), 0);
  rotation.setSeconds(1, 0); assert.equal(rotation.seconds, 0);
  rotation.setSeconds(NaN, 0); assert.equal(rotation.seconds, 0);
});
test('changing interval or filtered machine list starts a full dwell period', () => {
  const rotation = new PageRotation(); rotation.setSeconds(10, 0);
  rotation.setSeconds(30, 9000);
  assert.equal(rotation.advance(0, 3, false, 10000), 0);
  assert.equal(rotation.remaining(10000), 29);
  rotation.reset(20000);
  assert.equal(rotation.advance(0, 2, false, 39000), 0);
  assert.equal(rotation.advance(0, 2, false, 50000), 1);
});
