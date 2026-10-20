const {test,expect}=require('@playwright/test');
const {readFile}=require('node:fs/promises');

// UI-only synthetic hourly buckets; no hardware claims.
function utilizationFixture(period){
  const now=Math.floor(Date.now()/1000),today=Math.floor((now+28800)/86400)*86400-28800;
  const days=period==='week'?7:1,after=period==='today'?today:today-days*86400;
  const hours=Array.from({length:days*24},(_,i)=>({at:after+i*3600,mean:i===0?0:2,peak:9,lowPercent:100,coveragePercent:i===0?50:100,observedSeconds:3600,samples:360,occupiedPercent:i===1?null:i===0?100:0,occupancyCoveragePercent:i===1?0:i===0?50:100,occupancyObservedSeconds:i===1?0:3600}));
  const pattern=hours.slice(0,24).map((h,i)=>({...h,at:i}));
  return {source:'npu-smi',timeZone:'Asia/Shanghai',after,before:after+days*86400,generatedAt:new Date().toISOString(),retentionDays:30,bytes:4096,limitBytes:2000000,machines:[
    {id:'machine-00',name:'机器 <测试>',group:'A5',model:'Ascend950DT',metric:'NPU Util(%)',mean:2,coveragePercent:95,occupiedPercent:10,occupancyCoveragePercent:90,hours,hourOfDay:pattern,lowWindows:[{after:after+3600,before:after+3*3600}]},
    {id:'machine-01',name:'尚无数据',mean:null,coveragePercent:0,occupiedPercent:null,occupancyCoveragePercent:0,hours:hours.map(h=>({...h,mean:null,peak:null,lowPercent:null,coveragePercent:0,samples:0,occupiedPercent:null,occupancyCoveragePercent:0,occupancyObservedSeconds:0})),hourOfDay:pattern.map(h=>({...h,mean:null,peak:null,lowPercent:null,coveragePercent:0,occupiedPercent:null,occupancyCoveragePercent:0,occupancyObservedSeconds:0})),lowWindows:[]}
  ]};
}
test('NPU 小时汇总：零与缺失、周规律、明细、CSV、失败与窄屏',async({page},testInfo)=>{
  const errors=[];page.on('pageerror',e=>errors.push(e.message));
  let failed=false;const queries=[];
  await page.route('**/api/utilization?*',async route=>{
    const q=new URL(route.request().url()).searchParams;queries.push(q);
    if(failed)return route.fulfill({status:503,json:{error:'汇总存储未初始化'}});
    return route.fulfill({json:utilizationFixture(q.get('period'))});
  });
  await page.goto('/');
  await page.locator('#utilization-btn').click();
  await expect(page.locator('#utilization-dialog')).toBeVisible();
  await expect(page.locator('.utilization-table tbody tr')).toHaveCount(2);
  const first=page.locator('.utilization-table tbody tr').first().locator('td');
  await expect(first.first().locator('.util-value').first()).toHaveText('算力 0.0%');
  await expect(first.first().locator('.util-value').nth(1)).toHaveText('占用 100.0%');
  await expect(first.nth(1).locator('.util-value').nth(1)).toHaveText('占用 —');
  await expect(first.nth(2).locator('.util-value').nth(1)).toHaveText('占用 0.0%');
  await expect(page.locator('.util-partial')).toHaveCount(2);
  await expect(page.locator('.utilization-table tbody tr').nth(1).locator('td').first()).toHaveText('算力 —占用 —');
  await expect(page.locator('#utilization-storage')).toContainText('/ 1.91 MiB');
  await page.locator('[data-util-machine="0"]').click();
  await expect(page.locator('#utilization-detail tbody tr')).toHaveCount(24);
  await expect(page.locator('#utilization-detail h3')).toContainText('机器 <测试>');
  await expect(page.locator('#utilization-detail thead')).toContainText('占用时长比例');
  await expect(page.locator('#utilization-detail tbody tr').first().locator('td').nth(2)).toHaveText('100.0%');
  await page.locator('#utilization-period').selectOption('week');
  await expect(page.locator('#utilization-day')).toHaveValue('pattern');
  expect(queries.at(-1).get('period')).toBe('week');
  await page.locator('[data-util-machine="0"]').click();
  await expect(page.locator('#utilization-detail tbody tr')).toHaveCount(168);
  const download=page.waitForEvent('download');
  await page.locator('#utilization-export').click();
  const csv=await readFile(await(await download).path(),'utf8');
  expect(csv).toContain('覆盖率');
  expect(csv).toContain('整机占用时长比例(%)');
  expect(csv).toContain('占用覆盖率(%)');
  expect(csv).toContain('NPU Util(%)');
  expect(csv).not.toContain('cardId');
  await page.setViewportSize({width:720,height:900});
  expect(await page.evaluate(()=>document.documentElement.scrollWidth)).toBeLessThanOrEqual(720);
  await page.screenshot({path:testInfo.outputPath('npu-utilization.png'),fullPage:true});
  failed=true;await page.locator('#utilization-refresh').click();
  await expect(page.locator('#utilization-message')).toContainText('汇总存储未初始化');
  await expect(page.locator('#utilization-table')).toBeEmpty();
  await expect(page.locator('#utilization-detail')).toBeEmpty();
  await expect(page.locator('#utilization-export')).toBeDisabled();
  expect(errors).toEqual([]);
});
