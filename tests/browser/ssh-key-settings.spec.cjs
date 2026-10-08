const {test, expect} = require('@playwright/test');
const {readFile} = require('node:fs/promises');

test('SSH key 设置：密码可选、保存私钥路径和口令、导出不包含秘密', async ({page}) => {
  let config, saved;
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  // Keep this test entirely in the settings UI; saving must never replace the
  // local fixture's machine configuration or start real SSH monitoring.
  await page.route('**/api/config', async route => {
    if (route.request().method() === 'PUT') {
      saved = route.request().postDataJSON();
      config = {...saved, profiles:saved.profiles.map(profile => ({
        ...profile, password:'', keyPassphrase:'', clearPassword:false,
        hasPassword:!profile.clearPassword && (!!profile.password || !!profile.hasPassword),
        hasKeyPassphrase:!!profile.keyPassphrase || !!profile.hasKeyPassphrase,
      }))};
      return route.fulfill({json:config});
    }
    if (!config) config = await (await route.fetch()).json();
    return route.fulfill({json:config});
  });
  await page.goto('/');
  await page.locator('#settings-btn').click();
  await expect(page.locator('#settings-dialog')).toBeVisible();
  await page.locator('#add-profile').click();
  const profile = page.locator('.profile-row').last();
  await profile.locator('[data-field="name"]').fill('密钥测试');
  await profile.locator('[data-field="username"]').fill('test-user');
  await expect(profile.locator('[data-field="password"]')).not.toHaveAttribute('required', '');
  await profile.locator('[data-field="privateKeyPath"]').fill('~/.ssh/id_ed25519');
  await profile.locator('[data-field="keyPassphrase"]').fill('browser-only-secret-passphrase');
  await page.locator('.profile-row').first().locator('[data-field="clearPassword"]').check();
  const downloadPromise = page.waitForEvent('download');
  await page.locator('#export-btn').click();
  const download = await downloadPromise;
  const exportedText = await readFile(await download.path(), 'utf8');
  expect(exportedText).not.toContain('browser-only-secret-passphrase');
  const exported = JSON.parse(exportedText);
  for (const item of exported.profiles) {
    for (const key of ['password','keyPassphrase','hasPassword','hasKeyPassphrase','clearPassword']) expect(item).not.toHaveProperty(key);
  }
  expect(exported.profiles.at(-1).privateKeyPath).toBe('~/.ssh/id_ed25519');
  await page.locator('#save-btn').click();
  await expect(page.locator('#settings-dialog')).not.toBeVisible();
  expect(saved.profiles.at(-1).password).toBe('');
  expect(saved.profiles.at(-1).privateKeyPath).toBe('~/.ssh/id_ed25519');
  expect(saved.profiles.at(-1).keyPassphrase).toBe('browser-only-secret-passphrase');
  expect(saved.profiles[0].clearPassword).toBe(true);
  await expect(page.locator('#profiles-editor')).toBeEmpty();
  await page.locator('#settings-btn').click();
  await expect(profile.locator('[data-field="keyPassphrase"]')).toHaveValue('');
  await expect(profile).toContainText('私钥口令（已保存，留空保持）');
  await expect(profile.locator('[data-field="privateKeyPath"]')).toHaveValue('~/.ssh/id_ed25519');
  await page.setViewportSize({width:720,height:900});
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(720);
  expect(errors).toEqual([]);
});
