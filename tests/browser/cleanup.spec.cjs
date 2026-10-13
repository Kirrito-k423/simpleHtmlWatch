const {test, expect} = require('@playwright/test');
const {readFile} = require('node:fs/promises');

const status = () => ({schema:'simplehtmlwatch.cleanup.v1', policy:{enabled:false,authorizationRef:'',updatedAt:'2026-10-13T07:00:00Z'}, minimumMachines:4,minimumSeconds:300,probeIntervalSeconds:60,auditCount:0,audits:[],error:''});
const panel = page => page.getByRole('region', {name:'常驻排队清理',exact:true});
const control = page => panel(page).getByRole('switch', {name:'常驻排队清理'});

test('页面直接启停：真实策略接口保存授权，刷新页面保留状态', async ({page}) => {
  const errors = [], writes = [];
  page.on('pageerror', error => errors.push(error.message));
  page.on('request', request => {
    if (request.url().endsWith('/api/tasks/cleanup') && request.method() === 'POST') writes.push(request);
  });
  await page.goto('/?tasks=1');
  await expect(control(page)).toBeEnabled();
  if (await control(page).getAttribute('aria-checked') === 'true') {
    await control(page).click();
    await expect(control(page)).not.toBeChecked();
    writes.length = 0;
  }
  // One click, without a Python client, terminal or confirmation dialog.
  await control(page).click();
  await expect(control(page)).toBeChecked();
  await expect(panel(page)).toContainText('设置已保存');
  expect(writes).toHaveLength(1);
  expect(writes[0].headers()['x-watch-instance']).toBe('browser-fixture');
  expect(writes[0].headers()['x-watch-token']).toBeTruthy();
  expect(writes[0].postDataJSON()).toMatchObject({enabled:true});
  expect(writes[0].postDataJSON().authorizationRef).toMatch(/^task-dashboard:.*用户点击启用/);
  await page.reload();
  await expect(control(page)).toBeChecked();
  await control(page).click();
  await expect(control(page)).not.toBeChecked();
  await expect(panel(page)).toContainText('已有诊断和清理日志保留');
  expect(writes).toHaveLength(2);
  expect(errors).toEqual([]);
});

test('日志分类、完整证据及下载：不把清理请求回执当作资源释放', async ({page,browserName}, testInfo) => {
  const current = status(), errors = [], policyErrors = [];
  page.on('pageerror', error => errors.push(error.message));
  page.on('console', message => { if (/Content Security Policy|violates.*directive/i.test(message.text())) policyErrors.push(message.text()); });
  const audit = {schema:'simplehtmlwatch.cleanup.v1',id:'residue-fixture',machineId:'B06-2',host:'192.0.2.6',continuationId:'wait-four',phase:'terminated',reason:'终止请求完成，等待重新核查 NPU 进程表',createdAt:'2026-10-13T07:00:00Z',updatedAt:'2026-10-13T07:06:00Z',signalIntentAt:'2026-10-13T07:05:00Z',signalResult:'SIGTERM returned',last:{pid:2468,command:'python worker.py',logs:[{fd:'2',size:400,tail:'Traceback <script>window.logInjected=true</script> & failure'}]},events:[{at:'2026-10-13T07:00:00Z',phase:'observing',reason:'已发现失败证据'},{at:'2026-10-13T07:06:00Z',phase:'terminated',reason:'终止请求返回'}]};
  current.auditCount = 3;
  current.audits = [{id:audit.id,machineId:audit.machineId,pid:2468,phase:audit.phase,reason:audit.reason,updatedAt:audit.updatedAt},
    {id:'residue-debug',machineId:'B06-3',pid:4321,phase:'debug',reason:'调试器附着，继续等待',updatedAt:audit.updatedAt},
    {id:'residue-unknown',machineId:'B06-4',pid:3210,phase:'unknown',reason:'缺少明确失败证据 <img src=x onerror=alert(1)>',updatedAt:audit.updatedAt}];
  await page.route('**/api/tasks/cleanup**', async route => {
    expect(route.request().headers()['x-watch-instance']).toBe('browser-fixture');
    await route.fulfill({json:new URL(route.request().url()).searchParams.has('id') ? {audit} : current});
  });
  await page.goto('/?tasks=1');
  await panel(page).locator('summary').first().click();
  await expect(panel(page)).toContainText('调试保护 · 继续等待');
  await expect(panel(page)).toContainText('无法判断 · 继续等待');
  await panel(page).getByRole('button',{name:/B06-2 · PID 2468/}).click();
  const detail = panel(page).getByRole('region',{name:'清理日志详情'});
  await expect(detail).toContainText('终止请求已返回 · 待核查');
  await expect(detail).toContainText('SIGTERM returned');
  await expect(detail).toContainText('Traceback <script>');
  await expect(panel(page).locator('script,img')).toHaveCount(0);
  expect(await page.evaluate(() => window.logInjected)).toBeUndefined();
  await detail.getByText('完整诊断证据',{exact:true}).click();
  await expect(detail.locator('details pre')).toContainText('"continuationId": "wait-four"');
  const downloading = page.waitForEvent('download');
  await detail.getByRole('button',{name:'下载完整记录'}).click();
  const download = await downloading;
  expect(download.suggestedFilename()).toBe('residue-fixture.json');
  expect(JSON.parse(await readFile(await download.path(),'utf8'))).toEqual(audit);
  if (browserName !== 'webkit') await panel(page).screenshot({path:testInfo.outputPath('queue-cleanup-logs.png')});
  await page.setViewportSize({width:390,height:844});
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(390);
  await expect(control(page)).toBeVisible();
  expect(errors).toEqual([]);
  expect(policyErrors).toEqual([]);
});

test('策略写入回执丢失：只查询实际状态，不重发开关请求', async ({page}) => {
  const current = status(), writes = [];
  await page.route('**/api/tasks/cleanup**', async route => {
    if (route.request().method() === 'POST') {
      writes.push(route.request().postDataJSON());
      current.policy = {...current.policy,...writes.at(-1)};
      await route.abort('failed');
    } else await route.fulfill({json:current});
  });
  await page.goto('/?tasks=1');
  await expect(control(page)).toBeEnabled();
  await control(page).click();
  await expect(panel(page)).toContainText('操作未自动重试');
  await expect(control(page)).toBeChecked();
  await panel(page).getByRole('button',{name:'刷新清理状态'}).click();
  await expect(control(page)).toBeEnabled();
  expect(writes).toHaveLength(1);
});

test('状态读取失败不伪装成关闭，也不能继续盲写', async ({page}) => {
  const current = status(); current.policy.enabled = true;
  let broken = false, writes = 0;
  await page.route('**/api/tasks/cleanup**', async route => {
    if (route.request().method() === 'POST') writes++;
    if (broken) await route.fulfill({status:503,json:{error:'状态暂时不可读'}});
    else await route.fulfill({json:current});
  });
  await page.goto('/?tasks=1');
  await expect(control(page)).toBeChecked();
  broken = true;
  await panel(page).getByRole('button',{name:'刷新清理状态'}).click();
  await expect(panel(page)).toContainText('读取清理状态失败');
  await expect(control(page)).toBeDisabled();
  await expect(control(page)).toBeChecked();
  await expect(panel(page)).toContainText('上次读取：已启用');
  broken = false;
  await panel(page).getByRole('button',{name:'刷新清理状态'}).click();
  await expect(control(page)).toBeEnabled();
  expect(writes).toBe(0);
});

test('旧实例缺少能力时禁用开关，任务页面仍可使用', async ({page}) => {
  let cleanupRequests = 0;
  await page.route('**/healthz', async route => {
    const response = await route.fetch(); const info = await response.json();
    info.capabilities = info.capabilities.filter(value => value !== 'queue-residue-cleanup-v1');
    await route.fulfill({json:info});
  });
  await page.route('**/api/tasks/cleanup**', async route => { cleanupRequests++; await route.fulfill({json:status()}); });
  await page.goto('/?tasks=1');
  await expect(panel(page)).toContainText('当前实例不支持常驻排队清理');
  await expect(control(page)).toBeDisabled();
  await expect(page.locator('#task-stats')).toContainText('16 注册机器');
  expect(cleanupRequests).toBe(0);
});

test('切换所选日志时，较迟返回的旧日志不能覆盖新详情', async ({page}) => {
  const current = status(); let releaseFirst;
  current.auditCount = 2;
  current.audits = ['first','second'].map(id => ({id:'residue-'+id,machineId:id,pid:1,phase:'unknown',reason:id,updatedAt:'2026-10-13T07:00:00Z'}));
  const firstGate = new Promise(resolve => { releaseFirst = resolve; });
  await page.route('**/api/tasks/cleanup**', async route => {
    const id = new URL(route.request().url()).searchParams.get('id');
    if (!id) { await route.fulfill({json:current}); return; }
    if (id === 'residue-first') await firstGate;
    await route.fulfill({json:{audit:{id,machineId:id,phase:'unknown',reason:id,last:{pid:1},events:[]}}});
  });
  await page.goto('/?tasks=1');
  await panel(page).locator('summary').first().click();
  await panel(page).getByRole('button',{name:/first · PID/}).click();
  await panel(page).getByRole('button',{name:/second · PID/}).click();
  await expect(panel(page).getByRole('region',{name:'清理日志详情'})).toContainText('residue-second');
  releaseFirst();
  await page.waitForTimeout(100);
  await expect(panel(page).getByRole('region',{name:'清理日志详情'})).not.toContainText('residue-first');
});
