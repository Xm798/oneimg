import { defineConfig, devices } from '@playwright/test'
import { AUTH_STATE } from './tests/security/global-setup'

export default defineConfig({
  testDir: './tests/security',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 30_000,
  globalTimeout: 180_000,
  globalSetup: './tests/security/global-setup.ts',
  use: { baseURL: 'http://127.0.0.1:18089', storageState: AUTH_STATE, trace: 'retain-on-failure', launchOptions: { timeout: 30_000 } },
  projects: [
    { name: 'chromium', use: { ...devices['Desktop Chrome'] } },
    { name: 'firefox', use: { ...devices['Desktop Firefox'] } },
    { name: 'webkit', use: { ...devices['Desktop Safari'] } },
  ],
  webServer: process.env.ONEIMG_SECURITY_EXTERNAL_FIXTURE === '1' ? undefined : {
    command: 'go run ./internal/securityfixture',
    cwd: '..',
    url: 'http://127.0.0.1:18089/api/v1/public/config',
    reuseExistingServer: false,
    timeout: 120_000,
  },
})
