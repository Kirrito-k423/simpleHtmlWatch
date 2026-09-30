const {test, expect} = require('@playwright/test');

test('默认筛选运行任务，最新任务靠上，切换历史及刷新保留筛选', async ({page}) => {
  let jobs;
  await page.route('**/api/tasks', async route => {
    const response = await route.fetch();
    if (!jobs) {
      jobs = await response.json();
      const now = Date.now();
      const active = (id, sourceID, status, minutes) => {
        const source = jobs.find(job => job.id === sourceID);
        const at = new Date(now - minutes * 60000).toISOString();
        return {...source, id, status, createdAt:at, updatedAt:at, finishedAt:undefined,
          exitCode:undefined, archiveReady:false,
          events:[{at,type:'dispatching',text:'本地测试受理'}, ...(status === 'running' ? [{at,type:'running',text:'本地测试启动'}] : [])]};
      };
      jobs.push(active('active-old','history-00','running',30),
        active('active-new','history-03','dispatching',20),
        active('active-other','history-02','running',10));
    }
    await route.fulfill({response, json:jobs});
  });
  await page.goto('/?tasks=1');
  const filter = page.locator('#task-filter');
  await expect(filter).toHaveValue('running');
  await expect(page.locator('.task-machine-group')).toHaveCount(16);
  await expect(page.locator('.task-lane')).toHaveCount(3);
  await expect(page.locator('.task-duration.succeeded')).toHaveCount(0);
  await expect(page.locator('#task-count')).toContainText('显示 3 / 36 条任务');
  expect(await page.locator('.task-row-title').evaluateAll(rows => rows.map(row => row.dataset.taskId)))
    .toEqual(['active-other','active-new','active-old']);
  await page.getByRole('button',{name:'刷新',exact:true}).click();
  await expect(filter).toHaveValue('running');
  await expect(page.locator('.task-lane')).toHaveCount(3);

  await filter.selectOption('unknown');
  await expect(page.locator('.task-lane')).not.toHaveCount(0);
  expect(await page.locator('.task-lane .task-status').allTextContents())
    .toEqual(Array(await page.locator('.task-lane').count()).fill('结果未知'));
  await filter.selectOption('all');
  await expect(page.locator('.task-lane')).toHaveCount(36);
  const machine = page.locator('.task-machine-group').filter({has:page.getByRole('button',{name:'收起机器 测试机器 00',exact:true})});
  const ids = await machine.locator('.task-row-title').evaluateAll(rows => rows.map(row => row.dataset.taskId));
  expect(ids.indexOf('active-new')).toBeLessThan(ids.indexOf('active-old'));
  expect(ids.indexOf('history-27')).toBeLessThan(ids.indexOf('history-00'));

  await filter.selectOption('running');
  jobs = jobs.map(job => job.id.startsWith('active-') ? {...job,status:'succeeded',finishedAt:new Date().toISOString(),exitCode:0} : job);
  await page.getByRole('button',{name:'刷新',exact:true}).click();
  await expect(filter).toHaveValue('running');
  await expect(page.locator('.task-lane')).toHaveCount(0);
  await expect(page.locator('.task-filter-empty')).toContainText('当前没有运行中或发送中的任务');
  await expect(page.locator('.task-machine-group')).toHaveCount(16);
  await filter.selectOption('all');
  await expect(page.locator('.task-lane')).toHaveCount(36);
  await page.reload();
  await expect(filter).toHaveValue('running');
  await expect(page.locator('.task-lane')).toHaveCount(0);
});
