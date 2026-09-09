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
  const { antigravityResetWithCountdown, joinAntigravityBuckets, antigravityGroupLine } =
    await server.ssrLoadModule('/src/features/codex-keeper/antigravityQuotaFormat.ts')

  // Reset + countdown: Chinese uses full-width parens with no leading space; English uses ASCII
  // parens with a leading space. This is the exact spot that rendered `…（Refresh in …）` in English.
  assert.equal(antigravityResetWithCountdown('2026-09-13 14:43', '4 天 6 小时后刷新', 'zh'), '2026-09-13 14:43（4 天 6 小时后刷新）')
  assert.equal(antigravityResetWithCountdown('2026-09-13 14:43', 'Refresh in 4d 6h', 'en'), '2026-09-13 14:43 (Refresh in 4d 6h)')
  // Empty countdown returns the reset time unchanged (no dangling parens) in both locales.
  assert.equal(antigravityResetWithCountdown('2026-09-13 14:43', '', 'zh'), '2026-09-13 14:43')
  assert.equal(antigravityResetWithCountdown('2026-09-13 14:43', '', 'en'), '2026-09-13 14:43')

  // Bucket separator: full-width comma (zh) vs ", " (en).
  assert.equal(joinAntigravityBuckets(['a', 'b', 'c'], 'zh'), 'a，b，c')
  assert.equal(joinAntigravityBuckets(['a', 'b', 'c'], 'en'), 'a, b, c')

  // Group line: label + full-width colon (zh) vs ": " (en). No Chinese punctuation may leak into en.
  assert.equal(antigravityGroupLine('Gemini 模型', 'a，b', 'zh'), 'Gemini 模型：a，b')
  const en = antigravityGroupLine('Gemini Models', 'Weekly 83% remaining, refreshes X (Refresh in 4d 6h)', 'en')
  assert.equal(en, 'Gemini Models: Weekly 83% remaining, refreshes X (Refresh in 4d 6h)')
  assert.ok(!/[：，（）]/.test(en), 'English group line must contain no full-width punctuation')

  console.log('antigravity-quota-format-smoke: OK')
} finally {
  await server.close()
}
