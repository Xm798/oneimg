import { test, expect, type Locator, type Page } from '@playwright/test'

test.beforeEach(async ({ page }) => {
  await page.addInitScript(() => {
    Object.assign(window, { previewInjected: 0, svgInjected: 0 })
  })
})

const injected = (page: Page) => page.evaluate(() => (window as unknown as { previewInjected: number }).previewInjected)

// 历史文件名、标签名里的 HTML 只能按文本显示，下载文件名不能带路径或引号
const expectInert = async (page: Page, scope: Locator) => {
  expect(await scope.locator('script, iframe, [onclick], [onerror], [onload]').count()).toBe(0)
  expect(await injected(page)).toBe(0)
}
const expectSafeDownload = async (page: Page, link: Locator) => {
  expect(await link.getAttribute('download')).not.toMatch(/["'<>?#/\\]/)
  const [downloaded] = await Promise.all([page.waitForEvent('download'), link.click()])
  expect(await downloaded.failure()).toBeNull()
}

test('historical metadata stays data in the gallery inspector and viewer', async ({ page }) => {
  await page.goto('/gallery')
  const target = page.locator('img[src*="legacy%22"]').first()
  await expect(target).toBeVisible()
  await target.click()
  const inspector = page.getByRole('dialog', { name: 'photo"><img src=x onerror="window.previewInjected=1">.png' })
  await expect(inspector).toBeVisible()
  await expect(inspector).toContainText('photo"><img src=x onerror="window.previewInjected=1">.png')
  await expect(inspector).toContainText('t"><img src=x onerror="window.previewInjected=1">')
  await expectInert(page, inspector)
  // 链接里的引号被编码，HTML 格式里的文件名被转义
  expect(await inspector.getByRole('textbox', { name: 'URL 链接' }).inputValue()).toContain('legacy%22')
  expect(await inspector.getByRole('textbox', { name: 'HTML 链接' }).inputValue()).toContain('alt="photo&quot;&gt;&lt;img')
  await expectSafeDownload(page, inspector.getByRole('link', { name: '下载' }))

  await inspector.getByRole('button', { name: '放大查看', exact: true }).click()
  const viewer = page.getByRole('dialog', { name: 'photo"><img src=x onerror="window.previewInjected=1">.png' }).last()
  await expect(viewer.locator('img[draggable="false"][src*="legacy%22"]')).toBeVisible()
  await expectInert(page, viewer)
  await page.keyboard.press('Escape')
  await expect(page.locator('img[src*="legacy%22"][draggable="false"]')).toHaveCount(0)
  await expect(inspector).toBeVisible()
  await inspector.getByRole('button', { name: '关闭' }).click()
  await expect(inspector).not.toBeVisible()
})

test('historical metadata stays data in the upload page viewer', async ({ page }) => {
  await page.goto('/upload')
  const target = page.locator('img[src*="legacy%22"]').first()
  await expect(target).toBeVisible()
  await target.click()
  const viewer = page.getByRole('dialog', { name: 'photo"><img src=x onerror="window.previewInjected=1">.png' })
  await expect(viewer).toBeVisible()
  await expectInert(page, viewer)
  await expectSafeDownload(page, viewer.getByRole('link', { name: '下载' }))
  await viewer.getByRole('button', { name: /^复制 .* 链接$/ }).click()
  await page.keyboard.press('Escape')
  await expect(viewer).not.toBeVisible()
})

test('mobile inspector preserves actions without injected nodes', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await page.goto('/gallery')
  await page.locator('img[src*="legacy%22"]').first().click()
  const inspector = page.getByRole('dialog', { name: 'photo"><img src=x onerror="window.previewInjected=1">.png' })
  await expect(inspector).toBeVisible()
  const box = await inspector.boundingBox()
  expect(box!.width).toBeLessThanOrEqual(390)
  let removed = false
  await page.route('**/api/v1/images/1/tags/*', async route => {
    if (route.request().method() === 'DELETE') { removed = true; await route.fulfill({ status: 204 }) } else await route.continue()
  })
  await inspector.getByRole('button', { name: /^移除标签/ }).click()
  await expect.poll(() => removed).toBeTruthy()
  let deleted = false
  await page.route('**/api/v1/images/1', async route => {
    if (route.request().method() === 'DELETE') { deleted = true; await route.fulfill({ status: 204 }) } else await route.continue()
  })
  await inspector.getByRole('button', { name: '删除文件', exact: true }).click()
  await page.getByRole('dialog', { name: /^删除「/ }).getByRole('button', { name: '删除', exact: true }).click()
  await expect.poll(() => deleted).toBeTruthy()
  expect(await injected(page)).toBe(0)
})

test('legacy SVG blocks scripts, external resources, embedded pages and forms', async ({ page }) => {
  // Request events also include CSP-blocked attempts. Count only requests received by the server.
  const before = await (await page.request.get('/__security/csp-requests')).json()
  await page.request.get('/svg-control')
  const attempts = await (await page.request.get('/__security/csp-requests')).json()
  expect(attempts).toBe(before + 1)
  const response = await page.goto('/uploads/legacy.svg')
  expect(response?.headers()['content-security-policy']).toContain("sandbox; default-src 'none'")
  expect(response?.headers()['x-content-type-options']).toBe('nosniff')
  await expect(page.locator('svg > rect')).toBeVisible()
  expect(await page.locator('svg > rect').evaluate(node => getComputedStyle(node).fill)).toBe('rgb(255, 0, 0)')
  expect(await page.evaluate(() => (window as unknown as { svgInjected?: number }).svgInjected || 0)).toBe(0)
  await page.locator('button').click({ force: true })
  await page.waitForTimeout(300)
  expect(await (await page.request.get('/__security/csp-requests')).json()).toBe(attempts)
  expect(page.url()).toContain('/uploads/legacy.svg')
})
