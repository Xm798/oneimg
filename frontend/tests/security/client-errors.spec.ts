import { test, expect, type Page, type Route } from '@playwright/test'

const storedUser = (page: Page) => page.evaluate(() => localStorage.getItem('userInfo'))

const PNG = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==', 'base64')

const interceptUpload = (page: Page, fulfil: (route: Route) => Promise<void>) =>
  page.route(url => url.pathname === '/api/v1/images', route => route.request().method() === 'POST' ? fulfil(route) : route.fallback())

const uploadOne = async (page: Page) => {
  // 上传选项决定允许的类型，加载完之前加入的文件会被前端拒绝
  await Promise.all([page.waitForResponse(response => new URL(response.url()).pathname === '/api/v1/upload-options'), page.goto('/upload')])
  await page.locator('input[type="file"]').setInputFiles({ name: 'probe.png', mimeType: 'image/png', buffer: PNG })
  const row = page.locator('li.upload-row', { hasText: 'probe.png' })
  await expect(row).toBeVisible()
  await page.getByRole('button', { name: '上传全部' }).click()
  return row
}

test('upload with a non-JSON error body fails the item without sticking the queue', async ({ page }) => {
  await interceptUpload(page, route => route.fulfill({ status: 413, contentType: 'text/html', body: '<html><body>413 Request Entity Too Large</body></html>' }))
  const row = await uploadOne(page)
  await expect(row.getByRole('alert')).toContainText('上传失败')
  await expect(row.getByRole('button', { name: '重试' })).toBeEnabled()
})

test('rate-limited upload reports the Retry-After delay', async ({ page }) => {
  await interceptUpload(page, route => route.fulfill({
    status: 429,
    headers: { 'Retry-After': '7' },
    contentType: 'application/problem+json',
    body: JSON.stringify({ type: 'about:blank', title: 'Too Many Requests', status: 429, detail: 'rate limited', code: 'rate_limited' }),
  }))
  const row = await uploadOne(page)
  await expect(row.getByRole('alert')).toContainText('7 秒')
})

test('truncated success body reports an unknown upload result', async ({ page }) => {
  await interceptUpload(page, route => route.fulfill({ status: 201, contentType: 'application/json', body: '{"data":{"files":[{"succ' }))
  const row = await uploadOne(page)
  await expect(row.getByRole('alert')).toContainText('上传结果未知，请到图库确认')
})

test('router guard logs out when /me returns 401', async ({ page }) => {
  await page.route('**/api/v1/me', route => route.fulfill({
    status: 401,
    contentType: 'application/problem+json',
    body: JSON.stringify({ type: 'about:blank', title: 'Unauthorized', status: 401, code: 'unauthorized' }),
  }))
  await page.goto('/gallery')
  await expect(page).toHaveURL(/\/login\?redirect=\/gallery$/)
  expect(await storedUser(page)).toBeNull()
})

for (const [name, handle] of [
  ['503', (route: Route) => route.fulfill({ status: 503, contentType: 'text/plain', body: 'unavailable' })],
  ['429', (route: Route) => route.fulfill({ status: 429, headers: { 'Retry-After': '5' }, contentType: 'application/problem+json', body: '{"status":429}' })],
  ['network failure', (route: Route) => route.abort('failed')],
] as const) {
  test(`router guard keeps the session when /me fails with ${name}`, async ({ page }) => {
    await page.route('**/api/v1/me', handle)
    await page.goto('/gallery')
    await expect(page.getByText('暂时无法确认登录状态，请稍后重试', { exact: true })).toBeVisible()
    await expect(page).toHaveURL(/\/gallery$/)
    expect(await storedUser(page)).not.toBeNull()
  })
}
