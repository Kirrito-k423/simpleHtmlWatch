const {test, expect} = require('@playwright/test');

test('快捷 SSH：选机、交互、缩放、关闭及 CSP；输出不保存到浏览器存储', async ({page}, testInfo) => {
  const errors = [], violations = [], messages = [], urls = [];
  let closes = 0;
  page.on('pageerror', error => errors.push(error.message));
  page.on('console', message => { if (/Content Security Policy|violates.*directive/i.test(message.text())) violations.push(message.text()); });
  await page.routeWebSocket('**/ssh', ws => {
    urls.push(ws.url());
    ws.onMessage(raw => {
      const message = JSON.parse(String(raw)); messages.push(message);
      if (message.type === 'auth') {
        ws.send(JSON.stringify({type:'status',state:'connected',message:'已连接测试机器'}));
        ws.send(Buffer.from('\x1b[32mfixture$ \x1b[0m'));
      } else if (message.type === 'input') {
        ws.send(Buffer.from(message.data));
      }
    });
    ws.onClose(() => { closes++; });
  });
  await page.goto('/?tasks=1');
  const savedBefore = await page.evaluate(() => ({local:{...localStorage},session:{...sessionStorage}}));
  await page.locator('#ssh-btn').click();
  await expect(page.locator('#ssh-dialog')).toBeVisible();
  await expect(page.locator('#ssh-machine option')).toHaveCount(17);
  await page.locator('#ssh-machine').selectOption('machine-15');
  await expect(page.locator('#ssh-message')).toContainText('已连接测试机器');
  await expect(page.locator('#ssh-terminal .xterm-screen')).toBeVisible();
  await page.locator('#ssh-terminal .xterm-helper-textarea').pressSequentially('pwd');
  await page.locator('#ssh-terminal .xterm-helper-textarea').press('Enter');
  await page.locator('#ssh-terminal .xterm-helper-textarea').press('Control+c');
  await expect.poll(() => messages.filter(message => message.type === 'input').map(message => message.data).join('')).toContain('pwd\r\x03');
  await expect.poll(() => messages.filter(message => message.type === 'ack').length).toBeGreaterThan(0);
  await page.setViewportSize({width:720,height:900});
  await expect.poll(() => messages.filter(message => message.type === 'resize').length).toBeGreaterThan(0);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(720);
  await page.screenshot({path:testInfo.outputPath('ssh-terminal.png'),fullPage:true});
  await page.locator('#ssh-close').click();
  await expect(page.locator('#ssh-dialog')).not.toBeVisible();
  await expect.poll(() => closes).toBe(1);
  expect(urls[0]).toMatch(/\/ssh$/);
  expect(urls[0]).not.toContain('token');
  expect(messages.find(message => message.type === 'auth').machineId).toBe('machine-15');
  expect(await page.evaluate(() => ({local:{...localStorage},session:{...sessionStorage}}))).toEqual(savedBefore);
  expect(errors).toEqual([]);
  expect(violations).toEqual([]);
});
