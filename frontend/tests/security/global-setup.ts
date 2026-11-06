import { mkdir, writeFile } from 'node:fs/promises'
import { dirname, resolve } from 'node:path'
import { request, type FullConfig } from '@playwright/test'

export const AUTH_STATE = resolve(__dirname, '../../playwright/.auth/browser-test.json')

// Login allows 10 attempts per IP per 5 minutes; one login per run keeps worker restarts from exhausting it.
export default async function globalSetup(config: FullConfig) {
  const baseURL = config.projects[0].use.baseURL!
  const context = await request.newContext({ baseURL })
  const response = await context.post('/api/v1/auth/login', { data: { username: 'browser-test', password: 'browser-test-password' } })
  if (!response.ok()) throw new Error(`fixture login failed: ${response.status()} ${await response.text()}`)
  const user = (await response.json()).data
  const state = await context.storageState()
  await context.dispose()
  state.origins = [{ origin: new URL(baseURL).origin, localStorage: [{ name: 'userInfo', value: JSON.stringify(user) }] }]
  await mkdir(dirname(AUTH_STATE), { recursive: true })
  await writeFile(AUTH_STATE, JSON.stringify(state))
}
