import { test, expect, type Page } from '@playwright/test'

const USER_CODES = ['user:list', 'user:create', 'user:delete', 'user:permission:update', 'user:password:reset', 'user:passkey:reset']
const now = '2026-01-01T00:00:00Z'

// A limited admin without 修改角色; the list holds a peer, a lower admin and a
// plain user with stale codes stored on its row.
const actor = { id: 2, username: 'limited', role: 1, password_change_required: false, permission: { codes: [...USER_CODES, 'tag:create'], bucket_ids: [] } }
const rows = [
  { id: 1, username: 'browser-test', role: 1, permission: { codes: [], bucket_ids: [] } },
  actor,
  { id: 3, username: 'peer-admin', role: 1, permission: { codes: [...USER_CODES, 'tag:create'], bucket_ids: [] } },
  { id: 4, username: 'lower-admin', role: 1, permission: { codes: ['user:list'], bucket_ids: [] } },
  { id: 5, username: 'stale-user', role: 3, permission: { codes: ['storage:delete'], bucket_ids: [] } },
].map(row => ({ ...row, avatar_url: null, passkey_count: 0, created_at: now, updated_at: now }))

const asLimitedAdmin = async (page: Page, me = actor) => {
  // The router guard requires /me to match the stored session user.
  await page.addInitScript(value => localStorage.setItem('userInfo', value), JSON.stringify(me))
  await page.route(url => url.pathname === '/api/v1/me', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ data: me }) }))
  await page.route(url => url.pathname === '/api/v1/users', route => route.request().method() === 'GET'
    ? route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ data: rows, meta: { pagination: { page: 1, page_size: 20, total: rows.length, total_pages: 1 } } }) })
    : route.fallback())
  await page.goto('/users')
  await expect(page.getByText('lower-admin')).toBeVisible()
}

const row = (page: Page, name: string) => page.locator('tr', { hasText: name })

test('limited admin gets no actions on a peer and none on the super admin', async ({ page }) => {
  await asLimitedAdmin(page)
  await expect(row(page, 'peer-admin')).toContainText('无权管理')
  await expect(row(page, 'peer-admin').getByRole('button')).toHaveCount(0)
  await expect(row(page, 'browser-test')).toContainText('受保护')
  await expect(row(page, 'lower-admin').getByRole('button', { name: '权限' })).toBeVisible()
})

test('limited admin cannot pick the admin role when creating a user', async ({ page }) => {
  await asLimitedAdmin(page)
  await page.getByRole('button', { name: '新建用户' }).click()
  const dialog = page.getByRole('dialog')
  await expect(dialog.getByLabel('用户名')).toBeVisible()
  await expect(dialog.getByRole('radiogroup', { name: '角色' })).toHaveCount(0)
})

test('switches for codes the actor lacks are disabled', async ({ page }) => {
  await asLimitedAdmin(page)
  await row(page, 'lower-admin').getByRole('button', { name: '权限' }).click()
  const drawer = page.getByRole('dialog')
  await expect(drawer.getByRole('switch', { name: '查看用户' })).toBeEnabled()
  await expect(drawer.getByRole('switch', { name: '新增标签' })).toBeEnabled()
  await expect(drawer.getByRole('switch', { name: '删除存储' })).toBeDisabled()
})

test('promoting a plain user with stale codes starts with every switch off', async ({ page }) => {
  await asLimitedAdmin(page, { ...actor, permission: { codes: [...USER_CODES, 'user:role:update', 'tag:create', 'storage:delete'], bucket_ids: [] } })
  await row(page, 'stale-user').getByRole('button', { name: '权限' }).click()
  const drawer = page.getByRole('dialog')
  await drawer.getByRole('radio', { name: '管理员' }).click()
  await expect(drawer.getByRole('switch', { name: '删除存储' })).toBeEnabled()
  await expect(drawer.getByRole('switch', { checked: true })).toHaveCount(0)
})
