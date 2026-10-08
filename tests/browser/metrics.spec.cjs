const {test, expect} = require('@playwright/test');
const {readFile} = require('node:fs/promises');

function snapshot(query, task = null) {
  const now = Math.floor(Date.now()/1000);
  const before = Number(query.get('before')) || now;
  const after = Number(query.get('after')) || before-900;
  const step = Math.ceil((before-after)/180);
  const units = ['%','MiB','KiB/s','kilobits/s'];
  return {machineId:'machine-00',machineName:'测试机器 00',source:'netdata',agentVersion:'browser-fixture',nodeId:'fixture-node',fetchedAt:new Date().toISOString(),after,before,task,charts:['system.cpu','system.ram','system.io','system.net'].map((context,i) => ({
    context,name:['CPU','内存','磁盘吞吐','网络流量'][i],unit:units[i],step,updateEvery:1,lastEntry:before,series:[{name:i ? 'used' : 'user',unit:units[i],points:Array.from({length:181},(_,k)=>({at:after+(before-after)*k/180,value:k>=85 && k<=95 ? null : (i+1)*10+(i+1)*5*Math.sin(k/10)}))}],
  }))};
}

test('系统指标：联动悬停、缩放、任务关联、失败清空与窄屏', async ({page,browserName},testInfo) => {
  const pageErrors = [], policyErrors = [], queries = [];
  let mode = 'normal';
  page.on('pageerror',error=>pageErrors.push(error.message));
  page.on('console',message=>{if (/Content Security Policy|violates.*directive/i.test(message.text())) policyErrors.push(message.text());});
  await page.route('**/api/config',async route=>{
    const body = await (await route.fetch()).json();
    body.config.machines[0].netdata = {enabled:true,port:19999};
    await route.fulfill({json:body});
  });
  await page.route('**/api/metrics?*',async route=>{
    const query = new URL(route.request().url()).searchParams; queries.push(query);
    const data = snapshot(query,query.get('taskId') ? {id:query.get('taskId'),start:new Date((Date.now()/1000-600)*1000).toISOString(),end:new Date((Date.now()/1000-300)*1000).toISOString(),events:[{at:new Date((Date.now()/1000-550)*1000).toISOString(),type:'running',text:'本地浏览器模拟事件'}]} : null);
    if (mode === 'failure') return route.fulfill({status:502,json:{error:'SSH 转发失败 · 浏览器模拟'}});
    if (mode === 'partial') { data.charts[2].error = '磁盘采集器未启用'; data.charts[2].series = []; }
    await route.fulfill({json:data});
  });
  await page.goto('/');
  await page.locator('#metrics-btn').click();
  await expect(page.locator('#metrics-dialog')).toBeVisible();
  await expect(page.locator('.metric-plot')).toHaveCount(4);
  await expect(page.locator('#metrics-meta')).toContainText('browser-fixture');
  const svg = page.locator('.metric-plot').first();
  const box = await svg.boundingBox();
  await page.mouse.move(box.x+box.width*(56+692*.5)/760,box.y+box.height*.5);
  await expect(page.locator('#metrics-hover')).not.toBeEmpty();
  await expect(page.locator('.metric-values').first()).toContainText('缺失');
  await expect(page.locator('.metric-cursor:not([hidden])')).toHaveCount(4);
  await page.getByRole('button',{name:'放大指标时间范围'}).click();
  await expect(page.locator('#metrics-live')).not.toBeChecked();
  await expect.poll(()=>queries.at(-1).get('before')-queries.at(-1).get('after')).toBe(450);
  await page.locator('#metrics-span').selectOption('3600');
  await expect.poll(()=>queries.at(-1).get('before')-queries.at(-1).get('after')).toBe(3600);
  mode = 'partial'; await page.locator('#metrics-refresh').click();
  await expect(page.locator('.metric-plot')).toHaveCount(3);
  await expect(page.locator('#metrics-message')).toContainText('1 项指标读取失败');
  await expect(page.locator('.metric-problem')).toContainText('磁盘采集器未启用');
  mode = 'failure'; await page.locator('#metrics-refresh').click();
  await expect(page.locator('#metrics-message')).toContainText('SSH 转发失败');
  await expect(page.locator('#metrics-charts')).toBeEmpty();
  await page.locator('#metrics-close').click(); mode = 'normal';
  await page.goto('/?tasks=1');
  await page.locator('#task-filter').selectOption('all');
  await page.locator('.task-row-title[data-task-id="history-00"]').click();
  await page.getByRole('button',{name:'运行期间系统指标'}).click();
  await expect(page.locator('#metrics-task-label')).toContainText('任务 history-00');
  await expect(page.locator('.metric-plot')).toHaveCount(4);
  await expect(page.locator('.metric-task-marker')).toHaveCount(4);
  await expect(page.locator('#metrics-live')).not.toBeChecked();
  expect(queries.at(-1).get('taskId')).toBe('history-00');
  expect(queries.at(-1).has('after')).toBe(false); // Backend derives the task interval.
  if (browserName !== 'webkit') await page.screenshot({path:testInfo.outputPath('metrics-dashboard.png'),fullPage:true});
  await page.setViewportSize({width:720,height:900});
  await expect(page.locator('#metrics-dialog')).toBeVisible();
  expect(await page.evaluate(()=>document.documentElement.scrollWidth)).toBeLessThanOrEqual(720);
  expect(pageErrors).toEqual([]); expect(policyErrors).toEqual([]);
});

test('Netdata 设置与导出：令牌遮蔽、导入停用、旧配置默认关闭', async ({page}) => {
  let config, saved;
  await page.route('**/api/config',async route=>{
    if (route.request().method() === 'PUT') {
      saved = route.request().postDataJSON();
      config = {...saved,machines:saved.machines.map(machine=>({...machine,netdata:machine.netdata && {...machine.netdata,token:'',hasToken:!!machine.netdata.token || machine.netdata.hasToken}}))};
      return route.fulfill({json:config});
    }
    if (!config) config = (await (await route.fetch()).json()).config;
    await route.fulfill({json:{config,commands:[{id:'npu',name:'NPU 状态'},{id:'python',name:'Python 进程'},{id:'usage',name:'Python CPU / 内存'}]}});
  });
  await page.goto('/');
  await page.locator('#settings-btn').click();
  const row = page.locator('.machine-edit').first();
  await expect(row.locator('[data-netdata="enabled"]')).not.toBeChecked();
  await row.locator('[data-netdata="enabled"]').check();
  await row.locator('[data-netdata="port"]').fill('20000');
  await row.locator('[data-netdata="token"]').fill('browser-only-bearer-secret');
  const downloadPromise = page.waitForEvent('download');
  await page.locator('#export-btn').click();
  const exportedText = await readFile(await (await downloadPromise).path(),'utf8');
  expect(exportedText).not.toContain('browser-only-bearer-secret');
  const exported = JSON.parse(exportedText);
  expect(exported.machines[0].netdata).toEqual({enabled:true,port:20000});
  await page.locator('#save-btn').click();
  await expect(page.locator('#settings-dialog')).not.toBeVisible();
  expect(saved.machines[0].netdata.token).toBe('browser-only-bearer-secret');
  await page.locator('#settings-btn').click();
  await expect(row.locator('[data-netdata="token"]')).toHaveValue('');
  await expect(row.locator('[data-netdata="token"]')).toHaveAttribute('placeholder','已保存，留空保持');
  page.once('dialog',dialog=>dialog.accept());
  await page.locator('#import-file').setInputFiles({name:'config.json',mimeType:'application/json',buffer:Buffer.from(exportedText)});
  await expect(row.locator('[data-netdata="enabled"]')).not.toBeChecked();
  await expect(page.locator('#settings-error')).toContainText('Netdata 接入已停止');
});
