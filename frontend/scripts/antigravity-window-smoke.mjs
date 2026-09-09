import assert from 'node:assert/strict'
import { fileURLToPath } from 'node:url'

import { createServer } from 'vite'

const root = fileURLToPath(new URL('..', import.meta.url))

const server = await createServer({
  root,
  logLevel: 'error',
  server: { middlewareMode: true },
})

try {
  const { normalizeAntigravityWindow } = await server.ssrLoadModule(
    '/src/features/codex-keeper/antigravityWindow.ts',
  )

  // 5-hour window: every spelling/alias collapses to the same key so the UI localizes it.
  for (const alias of ['5h', '5H', '5 hour', '5-hour', '5_hours', ' 5H ']) {
    assert.equal(normalizeAntigravityWindow(alias), '5h', `5h alias: ${alias}`)
  }

  // Weekly.
  for (const alias of ['weekly', 'Weekly', 'week', 'WEEK']) {
    assert.equal(normalizeAntigravityWindow(alias), 'weekly', `weekly alias: ${alias}`)
  }

  // Daily — a window the reference implementation covers but the old label() dropped to English.
  for (const alias of ['daily', 'Daily', 'day', ' DAY ']) {
    assert.equal(normalizeAntigravityWindow(alias), 'daily', `daily alias: ${alias}`)
  }

  // Monthly.
  for (const alias of ['monthly', 'Monthly', 'month', 'MONTH']) {
    assert.equal(normalizeAntigravityWindow(alias), 'monthly', `monthly alias: ${alias}`)
  }

  // Genuinely unknown windows / empty / null return null so the caller keeps display_name.
  for (const unknown of ['yearly', 'hourly', '', '  ', null, undefined]) {
    assert.equal(normalizeAntigravityWindow(unknown), null, `unknown window: ${unknown}`)
  }

  console.log('antigravity-window-smoke: OK')
} finally {
  await server.close()
}
