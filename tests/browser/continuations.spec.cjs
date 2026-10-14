const {test, expect} = require('@playwright/test');

test('实验等待单：原对话状态、暂停恢复与不确定写请求不重放', async ({page}) => {
  const errors = [], controls = [];
  page.on('pageerror', error => errors.push(error.message));
  let uncertain = false;
  const job = {id:'wait-fixture', experimentId:'实验 <script>fixture</script>', stepId:'step-1', state:'waiting_resources', generation:0,
    resources:{machineCount:2,model:'910B',group:'A3'}, eligibleCount:1,
    target:{threadId:'original-thread'}, policy:{wakeMode:'heartbeat'},
    checkpoint:{path:'/fixture/checkpoint.json'}, tasks:[{id:'task-1'},{id:'task-2'}],
    nextCheckAt:'2026-10-10T04:30:00Z', excluded:{'machine-2':'npu_process_busy'}};
  await page.route('**/api/continuations**', async route => {
    const request = route.request();
    if (request.method() === 'POST') {
      controls.push(request.postDataJSON());
      expect(request.headers()['x-watch-instance']).toBe('browser-fixture');
      if (uncertain) { await route.abort('failed'); return; }
      job.state = request.postDataJSON().action === 'pause' ? 'paused' : 'waiting_resources';
      await route.fulfill({json:job});
    } else { await route.fulfill({json:{schema:'simplehtmlwatch.continuations.v1',continuations:[job]}}); }
  });
  await page.goto('/?tasks=1');
  const panel = page.getByRole('region',{name:'实验等待与自动继续'});
  await expect(panel).toContainText('最近合格 1 台');
  await expect(panel).toContainText('original-thread');
  await expect(panel.locator('script')).toHaveCount(0);
  await panel.getByRole('button',{name:'暂停后续',exact:true}).click();
  await expect(panel).toContainText('已暂停');
  await panel.getByRole('button',{name:'恢复后续',exact:true}).click();
  await expect(panel).toContainText('等待资源');
  uncertain = true;
  await panel.getByRole('button',{name:'取消后续',exact:true}).click();
  await expect(panel).toContainText('操作未自动重试');
  await page.waitForTimeout(300);
  expect(controls.map(request => request.action)).toEqual(['pause','resume','cancel']);
  expect(errors).toEqual([]);
});
