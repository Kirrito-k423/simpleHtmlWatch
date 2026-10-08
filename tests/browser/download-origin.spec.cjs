const {test, expect} = require('@playwright/test');

test('复现旧 no-referrer 策略导致的原生表单 Origin null 拒绝', async ({page}) => {
  await page.route('**/?tasks=1', async route => {
    const response = await route.fetch();
    await route.fulfill({response, headers:{...response.headers(), 'referrer-policy':'no-referrer'}});
  });
  await page.goto('/?tasks=1');
  await expect(page.locator('#task-filter')).toHaveValue('running');
  await page.locator('#task-filter').selectOption('all');
  await page.locator('.task-row-title[data-task-id="history-00"]').click();
  const requestPromise = page.context().waitForEvent('request', request => request.url().includes('/api/tasks/archive'));
  const popupPromise = page.waitForEvent('popup');
  await page.getByRole('button',{name:'下载结果包',exact:true}).click();
  const request = await requestPromise;
  expect((await request.allHeaders()).origin).toBe('null');
  const popup = await popupPromise;
  await expect(popup.locator('body')).toContainText('不允许跨站请求');
  await popup.close();
});
