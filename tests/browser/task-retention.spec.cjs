const {test, expect} = require('@playwright/test');

test('历史详情已清理时仍显示退出码和下载；软目标满额不显示任务拒绝', async ({page}) => {
  await page.route('**/api/tasks', async route => {
    const response = await route.fetch();
    const jobs = await response.json();
    for (const job of jobs) {
      if (job.id === 'history-00') Object.assign(job, {historyCompacted:true, shell:'', events:[], eventsDropped:4});
    }
    await route.fulfill({response,json:jobs});
  });
  await page.route('**/api/tasks/storage', route => route.fulfill({json:{baselineBytes:0,bytes:3000000,growthLimitBytes:2000000,limitBytes:2000000,reservedBytes:0,policy:'prune-completed',admissionBlocked:false,compactedTasks:1}}));
  await page.goto('/?tasks=1');
  await expect(page.locator('#task-storage')).toContainText('不因预算拒绝任务');
  await page.locator('#task-filter').selectOption('all');
  await page.locator('.task-row-title[data-task-id="history-00"]').click();
  await expect(page.locator('#task-detail')).toContainText('较老的命令与事件详情已清理');
  await expect(page.locator('#task-detail')).toContainText('远端退出码');
  await expect(page.locator('#task-detail button[data-task-action="download"]')).toBeVisible();
});
