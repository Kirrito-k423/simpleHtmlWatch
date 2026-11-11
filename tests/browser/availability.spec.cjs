const {test,expect}=require('@playwright/test');
const {readFile}=require('node:fs/promises');

// Synthetic UI data only, never a hardware or historical availability claim.
function fixture(scope='work'){
  const n={work:2,off:1,all:3}[scope],expected={work:3,off:10,all:13}[scope]*3600;
  const counts={minutes120:0,minutes60:Math.floor(n/2),minutes30:n,minutes15:n*2,minutes5:n*6+2};
  const idle=n*1800+600,busy=3600,unknown=expected-idle-busy;
  return {schema:'simplehtmlwatch.daily-idle-windows.v1',scope,expectedSeconds:expected,timeZone:'Asia/Shanghai',after:'2026-11-10T16:00:00Z',before:'2026-11-11T05:00:00Z',generatedAt:'2026-11-11T05:00:00Z',counts,machineCount:2,configuredEntries:3,idleSeconds:idle,busySeconds:busy,unknownSeconds:unknown+expected,fragmentSeconds:600,coveragePercent:(idle+busy)/expected/2*100,historyBytes:4096,historyLimitBytes:262144,historyTruncated:false,machines:[
    {resourceKey:'physical:a',machineIds:['a','alias-a'],name:'机器 <甲>',group:'A5',counts,idleSeconds:idle,busySeconds:busy,unknownSeconds:unknown,fragmentSeconds:600,longestIdleSeconds:idle,coveragePercent:(idle+busy)/expected*100},
    {resourceKey:'physical:b',machineIds:['b'],name:'尚未观测',group:'A5',counts:{minutes120:0,minutes60:0,minutes30:0,minutes15:0,minutes5:0},idleSeconds:0,busySeconds:0,unknownSeconds:expected,fragmentSeconds:0,longestIdleSeconds:0,coveragePercent:0}
  ]};
}

test('首页默认 NPU 与工作时段图：五档、三个范围、图像及明细导出、失败清空',async({page},testInfo)=>{
  const errors=[];page.on('pageerror',e=>errors.push(e.message));let failed=false;const queries=[];
  await page.route('**/api/config',async route=>{
    const response=await route.fetch(),data=await response.json();
    data.config.customCommands=[{id:'custom-enabled',name:'后台自定义指令',shell:'printf fixture',enabled:true}];
    await route.fulfill({response,json:data});
  });
  await page.route('**/api/availability?*',async route=>{
    const q=new URL(route.request().url()).searchParams;queries.push(q);
    return route.fulfill(failed?{status:503,json:{error:'窗口历史暂不可读'}}:{json:fixture(q.get('scope'))});
  });
  await page.goto('/');
  await expect(page.locator('#availability-dialog')).toBeVisible();
  await expect(page.locator('#view-tabs [data-view="npu"]')).toHaveClass(/active/);
  await expect(page.locator('#custom-form')).toBeHidden();
  await expect(page.locator('#availability-scope')).toHaveValue('work');
  await expect(page.locator('#availability-chart svg')).toBeVisible();
  await expect(page.locator('#availability-summary > div')).toHaveCount(5);
  await expect(page.locator('#availability-table tbody tr').first()).toContainText('机器 <甲>');
  await expect(page.locator('#availability-table tbody tr').first().locator('td').nth(4)).toHaveText('14');
  await expect(page.locator('#availability-table tbody tr').nth(1)).toContainText('0.0%');
  await expect(page.locator('#availability-chart')).toContainText('今日机器紧张情况 · 工作时段');
  await expect(page.locator('#availability-chart')).toContainText('可用半小时窗 2 个');
  const svgDownload=page.waitForEvent('download');await page.locator('#availability-svg').click();
  const svg=await readFile(await(await svgDownload).path(),'utf8');
  expect(svg).toContain('工作时段');expect(svg).toContain('机器 &lt;甲&gt;');expect(svg).toContain('未观测');
  const pngDownload=page.waitForEvent('download');await page.locator('#availability-png').click();
  const png=await readFile(await(await pngDownload).path());expect([...png.subarray(0,8)]).toEqual([137,80,78,71,13,10,26,10]);
  for(const scope of ['off','all']){
    await page.locator('#availability-scope').selectOption(scope);
    await expect(page.locator('#availability-chart')).toContainText('可用半小时窗 '+fixture(scope).counts.minutes30+' 个');
    expect(queries.at(-1).get('scope')).toBe(scope);
  }
  const csvDownload=page.waitForEvent('download');await page.locator('#availability-csv').click();
  const csv=await readFile(await(await csvDownload).path(),'utf8');expect(csv).toContain('5分钟独立窗');expect(csv).toContain('全天');
  await page.setViewportSize({width:720,height:980});expect(await page.evaluate(()=>document.documentElement.scrollWidth)).toBeLessThanOrEqual(720);
  await page.screenshot({path:testInfo.outputPath('availability.png'),fullPage:false});
  failed=true;await page.locator('#availability-refresh').click();
  await expect(page.locator('#availability-message')).toHaveText('窗口历史暂不可读');
  await expect(page.locator('#availability-chart')).toBeEmpty();await expect(page.locator('#availability-table')).toBeEmpty();await expect(page.locator('#availability-png')).toBeDisabled();
  expect(errors).toEqual([]);
});

test('任务中台入口保持直接访问，不自动遮挡任务页面',async({page})=>{
  await page.goto('/?tasks=1');await expect(page.locator('#task-panel')).toBeVisible();
  await expect(page.locator('#availability-dialog')).not.toBeVisible();
});
