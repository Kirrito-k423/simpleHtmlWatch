const test = require('node:test');
const assert = require('node:assert/strict');
const {downloadTaskArchive} = require('../web/task-download.js');

test('原生下载通过 POST 请求体鉴权，不把令牌放进 URL 或创建 Blob', () => {
  let submitted, removed = false;
  global.document = {
    createElement(tag) {
      return {tag, children: [], appendChild(child) { this.children.push(child); },
        submit() { submitted = this; }, remove() { removed = true; }};
    },
    body: {appendChild(form) { assert.equal(form.tag, 'form'); }},
  };
  try {
    downloadTaskArchive('job & 中文', 'secret-session');
    assert.equal(submitted.method, 'POST');
    assert.equal(submitted.action, '/api/tasks/archive?id=job%20%26%20%E4%B8%AD%E6%96%87');
    assert.equal(submitted.target, '_blank');
    assert.equal(submitted.rel, 'noopener');
    assert.equal(submitted.children[0].name, 'token');
    assert.equal(submitted.children[0].value, 'secret-session');
    assert.ok(removed);
  } finally { delete global.document; }
});
