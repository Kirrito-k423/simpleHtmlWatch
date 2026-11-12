const {test,expect}=require('@playwright/test');
const {readFile}=require('node:fs/promises');

// Synthetic 64-node dashboard; fake addresses only, never a hardware claim.
function fixture(scope='work',nodes=1,group=''){
  const n={work:2,off:1,all:3}[scope],expected={work:3,off:10,all:13}[scope]*3600,idle=n*1800+600,busy=1800;
  const counts=Object.fromEntries([120,60,30,15,5].map(minutes=>['minutes'+minutes,Math.floor(idle/(minutes*60))]));
  const machines=Array.from({length:64},(_,i)=>({resourceKey:'physical:node-'+String(i).padStart(2,'0'),machineIds:['machine-'+String(i).padStart(2,'0')],name:i===0?'机器 <甲>':'A5 B'+String(Math.floor(i/8)+1).padStart(2,'0')+'-'+(i%8+1),group:'A5 B'+String(Math.floor(i/8)+1).padStart(2,'0'),pool:'A5 B'+String(Math.floor(i/8)+1).padStart(2,'0'),counts:{...counts},idleSeconds:idle,busySeconds:busy,unknownSeconds:expected-idle-busy,fragmentSeconds:600,longestIdleSeconds:idle,coveragePercent:(idle+busy)/expected*100})).filter(r=>!group||r.group===group);
  const total=Object.fromEntries(Object.entries(counts).map(([key,value])=>[key,value*machines.length/nodes]));
  const plans={};
  if(nodes>1)for(const minutes of [120,60,30,15,5]){
    const count=counts['minutes'+minutes],resources=Array.from({length:nodes},(_,i)=>'physical:node-'+(48+i));
    plans['minutes'+minutes]=count?[{pool:'A5 B07',after:'2026-11-11T01:00:00Z',before:new Date(Date.parse('2026-11-11T01:00:00Z')+count*minutes*60000).toISOString(),minutes,count,resources}]:[];
  }
  return {schema:'simplehtmlwatch.daily-idle-windows.v1',scope,nodes,countPolicy:nodes>1?'nonoverlapping-feasible-packing-v1':'independent-single-node',plans,omittedPlans:{},expectedSeconds:expected,timeZone:'Asia/Shanghai',after:'2026-11-10T16:00:00Z',before:'2026-11-11T05:00:00Z',generatedAt:'2026-11-11T05:00:00Z',counts:total,machineCount:machines.length,configuredEntries:machines.length,idleSeconds:idle*machines.length,busySeconds:busy*machines.length,unknownSeconds:(expected-idle-busy)*machines.length,fragmentSeconds:600*machines.length,coveragePercent:(idle+busy)/expected*100,historyBytes:4096,historyLimitBytes:262144,historyTruncated:false,machines};
}

test('全屏五图方格：64节点同屏、五档、同组多机、明细和导出',async({page},testInfo)=>{
  await page.setViewportSize({width:1800,height:1226});
  const errors=[];page.on('pageerror',e=>errors.push(e.message));let failed=false;const queries=[];
  await page.route('**/api/config',async route=>{
    const response=await route.fetch(),data=await response.json();
    data.config.customCommands=[{id:'custom-enabled',name:'后台自定义指令',shell:'printf fixture',enabled:true}];
    data.config.machines=fixture().machines.map((r,i)=>({id:r.machineIds[0],name:r.name,host:'192.0.2.'+(i+1),port:22,group:r.group,commands:['npu'],enabled:true}));
    await route.fulfill({response,json:data});
  });
  await page.route('**/api/availability?*',async route=>{
    const q=new URL(route.request().url()).searchParams;queries.push(q);
    return route.fulfill(failed?{status:503,json:{error:'窗口历史暂不可读'}}:{json:fixture(q.get('scope'),Number(q.get('nodes')||1),q.get('group'))});
  });
  await page.goto('/');
  const dialog=page.locator('#availability-dialog');await expect(dialog).toBeVisible();
  await expect(page.locator('#view-tabs [data-view="npu"]')).toHaveClass(/active/);await expect(page.locator('#custom-form')).toBeHidden();
  await expect(page.locator('#availability-scope')).toHaveValue('work');await expect(page.locator('#availability-nodes')).toHaveValue('1');
  await expect(page.locator('.availability-duration')).toHaveCount(5);await expect(page.locator('.availability-node')).toHaveCount(320);
  const box=await dialog.boundingBox();expect(box).toMatchObject({x:0,y:0,width:1800,height:1226});
  const layout=await page.locator('#availability-chart').evaluate(el=>{
    const rects=[...el.querySelectorAll('.availability-node > rect')].map(r=>r.getBoundingClientRect());
    return {allVisible:rects.every(r=>r.width>1&&r.x>=0&&r.y>=0&&r.right<=innerWidth+.5&&r.bottom<=innerHeight+.5),allSquare:rects.every(r=>Math.abs(r.width-r.height)<.1),scrolling:el.scrollHeight>el.clientHeight+1||el.scrollWidth>el.clientWidth+1};
  });
  expect(layout).toEqual({allVisible:true,allSquare:true,scrolling:false});
  await page.screenshot({path:testInfo.outputPath('fleet-five-rings-desktop.png'),fullPage:false});
  const svgDownload=page.waitForEvent('download');await page.locator('#availability-svg').click();
  const svg=await readFile(await(await svgDownload).path(),'utf8');expect(svg.match(/class="availability-duration"/g)).toHaveLength(5);expect(svg.match(/class="availability-node"/g)).toHaveLength(320);expect(svg).toContain('机器 &lt;甲&gt;');
  const pngDownload=page.waitForEvent('download');await page.locator('#availability-png').click();const png=await readFile(await(await pngDownload).path());
  expect([...png.subarray(0,8)]).toEqual([137,80,78,71,13,10,26,10]);expect(png.readUInt32BE(16)).toBe(2000);expect(png.readUInt32BE(20)).toBe(1200);
  for(const nodes of [2,4,8]){
    await page.locator('#availability-nodes').selectOption(String(nodes));
    await expect(page.locator('[data-duration="30"]')).toContainText('可排布 '+fixture('work',nodes).counts.minutes30+' 次');
    expect(queries.at(-1).get('nodes')).toBe(String(nodes));await expect(page.locator('.availability-node')).toHaveCount(320);
  }
  await page.locator('#availability-nodes').selectOption('2');
  await expect(page.locator('#availability-message')).toContainText('非最大值');
  await page.locator('[data-node-resource="physical:node-48"][data-node-duration="30"]').click();
  await expect(page.locator('#availability-detail-dialog')).toBeVisible();
  await expect(page.locator('#availability-detail-body')).toContainText('A5 B07');await expect(page.locator('#availability-detail-body')).toContainText('B07-1、B07-2');await expect(page.locator('#availability-detail-body')).not.toContainText('B06');
  await page.locator('#availability-detail-close').click();
  await page.locator('#availability-group').selectOption('A5 B07');await expect(page.locator('.availability-node')).toHaveCount(40);
  await page.locator('#availability-group').selectOption('');await expect(page.locator('.availability-node')).toHaveCount(320);
  for(const scope of ['off','all']){await page.locator('#availability-scope').selectOption(scope);await expect(page.locator('[data-duration="30"]')).toContainText('可排布 '+fixture(scope,2).counts.minutes30+' 次');expect(queries.at(-1).get('scope')).toBe(scope);}
  const csvDownload=page.waitForEvent('download');await page.locator('#availability-csv').click();const csv=await readFile(await(await csvDownload).path(),'utf8');expect(csv).toContain('节点参与次数');expect(csv).toContain('独立组网实验窗');expect(csv).toContain('5分钟');
  await page.setViewportSize({width:720,height:980});expect(await page.evaluate(()=>document.documentElement.scrollWidth)).toBeLessThanOrEqual(720);await expect(page.locator('.availability-node')).toHaveCount(320);
  failed=true;await page.locator('#availability-refresh').click();await expect(page.locator('#availability-message')).toHaveText('窗口历史暂不可读');await expect(page.locator('#availability-chart')).toBeEmpty();await expect(page.locator('#availability-png')).toBeDisabled();
  expect(errors).toEqual([]);
});

test('任务中台入口保持直接访问，不自动遮挡任务页面',async({page})=>{
  await page.goto('/?tasks=1');await expect(page.locator('#task-panel')).toBeVisible();await expect(page.locator('#availability-dialog')).not.toBeVisible();
});
