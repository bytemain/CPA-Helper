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
  const { formatAntigravityResetCountdown } = await server.ssrLoadModule(
    '/src/features/codex-keeper/antigravityCountdown.ts',
  )

  const now = Date.UTC(2026, 0, 1, 0, 0, 0)
  const MINUTE = 60_000
  const HOUR = 3_600_000
  const DAY = 86_400_000

  const at = (delta) => new Date(now + delta).toISOString()
  const zh = (delta) => formatAntigravityResetCountdown(at(delta), now, 'zh')
  const en = (delta) => formatAntigravityResetCountdown(at(delta), now, 'en')

  // 4 days 6 hours → day + hour bucket
  assert.equal(zh(4 * DAY + 6 * HOUR), '4 天 6 小时后刷新')
  assert.equal(en(4 * DAY + 6 * HOUR), 'Refresh in 4d 6h')

  // 4 hours 56 min → hour + minute bucket
  assert.equal(zh(4 * HOUR + 56 * MINUTE), '4 小时 56 分钟后刷新')
  assert.equal(en(4 * HOUR + 56 * MINUTE), 'Refresh in 4h 56m')

  // 59 min → minute bucket
  assert.equal(zh(59 * MINUTE), '59 分钟后刷新')
  assert.equal(en(59 * MINUTE), 'Refresh in 59m')

  // 30 sec → under a minute
  assert.equal(zh(30_000), '小于 1 分钟后刷新')
  assert.equal(en(30_000), 'Refresh in <1m')

  // -5 min → expired / refreshable now
  assert.equal(zh(-5 * MINUTE), '可刷新')
  assert.equal(en(-5 * MINUTE), 'Refresh available')

  // null resetAtIso → empty string (caller renders a dash)
  assert.equal(formatAntigravityResetCountdown(null, now, 'zh'), '')
  assert.equal(formatAntigravityResetCountdown(null, now, 'en'), '')

  // unparseable string → empty string as well
  assert.equal(formatAntigravityResetCountdown('not-a-date', now, 'zh'), '')

  console.log('antigravity-countdown-smoke: OK')
} finally {
  await server.close()
}
