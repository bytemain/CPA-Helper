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
  const { isAntigravityAccount, isQuotaExhaustedAccount } = await server.ssrLoadModule(
    '/src/features/codex-keeper/keeperQuotaExhaustion.ts',
  )

  // Codex account at priority -1 (not disabled) IS quota-exhausted.
  assert.equal(
    isQuotaExhaustedAccount({ provider: 'codex', disabled: false, priority: -1 }),
    true,
    'codex priority -1 should be exhausted',
  )

  // Antigravity account at priority -1 must NOT be counted as quota-exhausted — Antigravity does
  // not run the Codex quota->priority policy. This is the provider-isolation guard both the status
  // page and the inspection-settings page share.
  assert.equal(
    isQuotaExhaustedAccount({ provider: 'antigravity', disabled: false, priority: -1 }),
    false,
    'antigravity priority -1 must not be exhausted',
  )

  // Disabled codex at -1 is reported by its disabled state, not exhaustion.
  assert.equal(isQuotaExhaustedAccount({ provider: 'codex', disabled: true, priority: -1 }), false)

  // Non -1 priority is never exhausted (including the ?? 0 default when priority is missing).
  assert.equal(isQuotaExhaustedAccount({ provider: 'codex', disabled: false, priority: 5 }), false)
  assert.equal(isQuotaExhaustedAccount({ provider: 'codex', disabled: false }), false)

  // Provider discriminator.
  assert.equal(isAntigravityAccount({ provider: 'antigravity' }), true)
  assert.equal(isAntigravityAccount({ provider: 'codex' }), false)
  assert.equal(isAntigravityAccount({}), false)

  console.log('keeper-quota-exhaustion-smoke: OK')
} finally {
  await server.close()
}
