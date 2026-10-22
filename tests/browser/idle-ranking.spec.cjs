const {test,expect}=require('@playwright/test');
const {readFile}=require('node:fs/promises');

function fixture(){
  const now=new Date().toISOString();
  const windows=Object.fromEntries(['5m','1h','2h','3h','4h'].map(key=>[key,{occupiedPercent:0,coveragePercent:100,minimumHourlyCoveragePercent:100,complete:true}]));
  const row={machineId:'idle-one',name:'空闲机器 <测试>',resourceKey:'physical:one',group:'gpu',model:'Ascend950DT',tier:1,tierLabel:'≥3小时观测零占用',historyReason:'observed_zero_3h',windows,eligible:true,recommended:true,currentReason:'ready',rank:1,lastNpuSampleAt:now,devices:16,minFreeHbmMB:93000,requiresLiveCheck:true};
  return {schema:'simplehtmlwatch.idle-ranking.v1',generatedAt:now,historyThrough:now,nextRefreshAt:now,exportedAt:now,exportPath:'/synthetic/exports/idle-ranking.json',historyBytes:4096,historyLimitBytes:65536,requiresLiveCheck:true,machines:[
    row,
    {...row,machineId:'unknown-history',name:'历史不足机器',rank:0,tier:0,recommended:false,historyReason:'history_insufficient',windows:Object.fromEntries(Object.keys(windows).map(key=>[key,{occupiedPercent:null,coveragePercent:0,complete:false}]))},
    {...row,machineId:'busy-one',name:'进程占用机器',rank:0,eligible:false,recommended:false,currentReason:'npu_process_busy',devices:0,minFreeHbmMB:null},
  ]};
}

test('五档空闲排序：历史与实时状态、过滤、JSON、自动检查和失败清空',async({page},testInfo)=>{
  const errors=[];page.on('pageerror',error=>errors.push(error.message));
  await page.clock.install();
  let failed=false;const queries=[];
  await page.route('**/api/idle-ranking?*',async route=>{
    queries.push(new URL(route.request().url()).searchParams);
    return failed?route.fulfill({status:503,json:{error:'空闲排序未初始化'}}):route.fulfill({json:fixture()});
  });
  await page.goto('/');await page.locator('#idle-ranking-btn').click();
  await expect(page.locator('#idle-ranking-dialog')).toBeVisible();
  await expect(page.locator('#idle-ranking-table tbody tr')).toHaveCount(3);
  await expect(page.locator('#idle-ranking-message')).toContainText('可推荐 1 台');
  await expect(page.locator('#idle-ranking-table tbody tr').first()).toContainText('空闲机器 <测试>');
  await expect(page.locator('#idle-ranking-table')).toContainText('5分钟历史不足');
  await expect(page.locator('#idle-ranking-table')).toContainText('NPU有进程占用');
  await expect(page.locator('#idle-ranking-table tbody tr').nth(1).locator('td').nth(2)).toContainText('—');
  await expect(page.locator('#idle-ranking-path')).toContainText('/synthetic/exports/idle-ranking.json');
  await page.locator('#idle-ranking-available').check();
  await expect(page.locator('#idle-ranking-table tbody tr')).toHaveCount(2);
  await page.locator('#idle-ranking-hbm').fill('0');await page.locator('#idle-ranking-hbm').press('Tab');
  await expect.poll(()=>queries.at(-1)?.get('minFreeHbmMB')).toBe('0');
  await page.locator('#idle-ranking-devices').fill('16');await page.locator('#idle-ranking-devices').press('Tab');
  await expect.poll(()=>queries.at(-1)?.get('devicesPerMachine')).toBe('16');
  const download=page.waitForEvent('download');await page.locator('#idle-ranking-export').click();
  const output=JSON.parse(await readFile(await(await download).path(),'utf8'));
  expect(output.requiresLiveCheck).toBe(true);expect(output.machines).toHaveLength(3);
  const previous=queries.length;await page.clock.fastForward(10000);
  await expect.poll(()=>queries.length).toBeGreaterThan(previous);
  await page.screenshot({path:testInfo.outputPath('idle-ranking-wide.png'),fullPage:true});
  await page.setViewportSize({width:720,height:900});
  expect(await page.evaluate(()=>document.documentElement.scrollWidth)).toBeLessThanOrEqual(720);
  await page.screenshot({path:testInfo.outputPath('idle-ranking-narrow.png'),fullPage:true});
  failed=true;await page.locator('#idle-ranking-refresh').click();
  await expect(page.locator('#idle-ranking-message')).toContainText('空闲排序未初始化');
  await expect(page.locator('#idle-ranking-table')).toBeEmpty();
  await expect(page.locator('#idle-ranking-export')).toBeDisabled();
  await page.locator('#idle-ranking-close').click();
  const closed=queries.length;await page.clock.fastForward(20000);expect(queries.length).toBe(closed);
  expect(errors).toEqual([]);
});
