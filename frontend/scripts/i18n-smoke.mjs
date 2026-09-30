import assert from 'node:assert/strict'
import { fileURLToPath } from 'node:url'

import { createServer } from 'vite'

const root = fileURLToPath(new URL('..', import.meta.url))
const languageStorageKey = 'cpa-helper-language'

const server = await createServer({
  root,
  logLevel: 'error',
  server: { middlewareMode: true },
})

let moduleCase = 0

function installBrowserStubs({ storedLanguage = null, browserLanguages = ['en-US'] } = {}) {
  const store = new Map()
  if (storedLanguage !== null) {
    store.set(languageStorageKey, storedLanguage)
  }

  Object.defineProperty(globalThis, 'localStorage', {
    configurable: true,
    value: {
      getItem: (key) => store.get(key) ?? null,
      setItem: (key, value) => store.set(key, String(value)),
    },
  })
  Object.defineProperty(globalThis, 'navigator', {
    configurable: true,
    value: {
      language: browserLanguages[0],
      languages: browserLanguages,
    },
  })
  Object.defineProperty(globalThis, 'document', {
    configurable: true,
    value: {
      documentElement: {
        lang: '',
      },
    },
  })

  return store
}

async function loadI18nWithBrowserStubs(options) {
  const store = installBrowserStubs(options)
  server.moduleGraph.invalidateAll()
  const i18n = await server.ssrLoadModule(`/src/shared/i18n/index.ts?case=${moduleCase++}`)
  return { i18n, store }
}

try {
  installBrowserStubs({ browserLanguages: ['en-US'] })

  let {
    localizedApiErrorMessage,
    localizedKeeperStatusDetail,
    localizedServerMessage,
    setLanguage,
  } = await server.ssrLoadModule('/src/shared/i18n/index.ts')

  setLanguage('en')
  assert.equal(localizedApiErrorMessage('validation_error', null), 'Invalid request parameters')
  assert.equal(localizedApiErrorMessage(null, null), 'Request failed')
  assert.equal(localizedServerMessage('巡检完成'), 'Inspection complete')
  assert.equal(
    localizedServerMessage('巡检完成：健康 1，坏凭证禁用 2，恢复启用 3，优先级降级 4，网络错误 5，缓存跳过 6'),
    'Inspection complete: 1 healthy, 2 bad credentials disabled, 3 restored, 4 priorities lowered, 5 network errors, 6 skipped by cache',
  )
  assert.equal(
    localizedServerMessage('Codex Keeper 已开始按计划自动巡检'),
    'Codex Keeper scheduled automatic inspection started',
  )
  assert.equal(
    localizedServerMessage('Codex Keeper 已停止自动巡检'),
    'Codex Keeper automatic inspection stopped',
  )
  assert.equal(
    localizedServerMessage('codex@example.com-plus.json: 降为低优先级：额度使用率达到阈值 100%'),
    'codex@example.com-plus.json: Lowered priority: quota usage reached the 100% threshold',
  )
  assert.equal(
    localizedServerMessage('codex@example.com-plus.json: 已启用 WebSocket 传输'),
    'codex@example.com-plus.json: enabled WebSocket transport',
  )
  assert.equal(localizedKeeperStatusDetail('守护运行中'), 'Automatic inspection running')

  setLanguage('zh')
  assert.equal(localizedApiErrorMessage('validation_error', null), '请求参数无效')
  assert.equal(localizedApiErrorMessage(null, null), '请求失败')
  assert.equal(localizedKeeperStatusDetail('守护运行中'), '自动巡检运行中')
  const { formatCompact } = await server.ssrLoadModule('/src/shared/utils/format.ts')
  assert.equal(formatCompact(12_300), '12.3K')
  assert.equal(formatCompact(52_646_000), '52.6M')
  assert.equal(formatCompact(3_560_000_000), '3.6B')

  let browserCase = await loadI18nWithBrowserStubs({
    browserLanguages: ['fr-FR', 'zh-CN', 'en-US'],
  })
  assert.equal(browserCase.i18n.currentLanguage.value, 'zh')
  assert.equal(globalThis.document.documentElement.lang, 'zh-CN')
  assert.equal(browserCase.store.get(languageStorageKey), 'zh')

  browserCase = await loadI18nWithBrowserStubs({
    storedLanguage: 'en',
    browserLanguages: ['zh-CN', 'en-US'],
  })
  assert.equal(browserCase.i18n.currentLanguage.value, 'en')
  assert.equal(globalThis.document.documentElement.lang, 'en')
  assert.equal(browserCase.store.get(languageStorageKey), 'en')
  browserCase.i18n.toggleLanguage()
  await Promise.resolve()
  assert.equal(browserCase.i18n.currentLanguage.value, 'zh')
  assert.equal(globalThis.document.documentElement.lang, 'zh-CN')
  assert.equal(browserCase.store.get(languageStorageKey), 'zh')

  browserCase = await loadI18nWithBrowserStubs({
    storedLanguage: 'de',
    browserLanguages: ['es-ES', 'en-US', 'zh-CN'],
  })
  assert.equal(browserCase.i18n.currentLanguage.value, 'en')

  ;({ localizedApiErrorMessage, localizedKeeperStatusDetail, localizedServerMessage, setLanguage } =
    browserCase.i18n)
  setLanguage('en')
  assert.equal(
    localizedApiErrorMessage('validation_error', 'API KEY 描述不能为空'),
    'API key description is required',
  )
  assert.equal(
    localizedServerMessage('请求体不是有效 JSON'),
    'Request body is not valid JSON',
  )
  assert.equal(
    localizedServerMessage('CLIProxyAPI 未确认重置成功（响应缺少 status=ok 或 auth_index 不匹配）'),
    'CLIProxyAPI did not confirm the reset (response missing status=ok or auth_index mismatch)',
  )
  assert.equal(
    localizedServerMessage('该账号缺少 auth_index，请先刷新账号列表'),
    'This account has no auth_index yet; refresh the account list first',
  )
  // Reset-quota (real credit consume) errors must localize to specific recovery
  // guidance, not degrade to the generic validation message.
  assert.equal(
    localizedServerMessage('账号正在巡检或重置中，请稍后重试'),
    'The account is being inspected or reset. Try again shortly.',
  )
  assert.equal(
    localizedServerMessage('账号 auth_index 已变化，请刷新账号列表后重试'),
    'The account auth_index has changed. Refresh the account list and try again.',
  )
  assert.equal(
    localizedServerMessage('同一 OpenAI 账号的另一路由正在重置，请稍后重试'),
    'Another route for the same OpenAI account is being reset. Try again shortly.',
  )
  assert.equal(
    localizedServerMessage('账号身份冲突：列表与详情的 account_id/auth_index 不一致，已保留原快照'),
    'Account identity conflict: the list and detail disagree on account_id/auth_index; the previous snapshot was preserved.',
  )
  assert.equal(
    localizedServerMessage('账号身份冲突：Antigravity 列表与详情的 name/type/auth_index/project_id/email 不一致，已保留原快照'),
    'Account identity conflict: the Antigravity list and detail disagree on name/type/auth_index/project_id/email; the previous snapshot was preserved.',
  )
  // Antigravity quota inspection log/last_error strings must localize (English account page
  // must not show Chinese): a bare failure exact, plus name-prefixed success/failure log lines.
  assert.equal(
    localizedServerMessage('Antigravity 配额读取失败'),
    'Failed to read Antigravity quota',
  )
  assert.equal(
    localizedServerMessage('api_key_prefix 超出最大长度 32'),
    'api_key_prefix exceeds the maximum length of 32',
  )
  assert.equal(
    localizedServerMessage('api_key_prefix 只能包含字母、数字、- 和 _，且不能以 - 开头或结尾'),
    'api_key_prefix may only contain letters, digits, - and _, and must not start or end with -',
  )
  // Model price mapping rule rejections: every field/index must survive translation, and must not
  // fall through to the generic `…不能为空` / `…超出最大长度` families.
  assert.equal(
    localizedServerMessage('模型价格映射规则 #1 的 source_model 不能为空'),
    'Model price mapping rule #1: source_model is required',
  )
  assert.equal(
    localizedServerMessage('模型价格映射规则 #2 的 target_provider 不能为空'),
    'Model price mapping rule #2: target_provider is required',
  )
  assert.equal(
    localizedServerMessage('模型价格映射规则 #3 的 target_model 不能为空'),
    'Model price mapping rule #3: target_model is required',
  )
  assert.equal(
    localizedServerMessage('模型价格映射规则 #1 的 source_model 最多只能包含一个 *'),
    'Model price mapping rule #1: source_model may contain at most one *',
  )
  assert.equal(
    localizedServerMessage('模型价格映射规则 #4 的 target_model 最多只能包含一个 *'),
    'Model price mapping rule #4: target_model may contain at most one *',
  )
  assert.equal(
    localizedServerMessage('模型价格映射规则 #5 的 target_model 含有 *，但 source_model 没有 *'),
    'Model price mapping rule #5: target_model contains * but source_model does not',
  )
  assert.equal(
    localizedServerMessage('模型价格映射规则 #6 的 source_provider 不能包含 *'),
    'Model price mapping rule #6: source_provider must not contain *',
  )
  assert.equal(
    localizedServerMessage('模型价格映射规则 #7 的 target_provider 不能包含 *'),
    'Model price mapping rule #7: target_provider must not contain *',
  )
  assert.equal(
    localizedServerMessage('模型价格映射规则 #8 与前面的规则重复（source_provider + source_model 相同）'),
    'Model price mapping rule #8 duplicates an earlier rule (same source_provider + source_model)',
  )
  assert.equal(
    localizedServerMessage('模型价格映射规则 #9 的 source_model 超出最大长度 180'),
    'Model price mapping rule #9: source_model exceeds the maximum length of 180',
  )
  assert.equal(
    localizedServerMessage('模型价格映射规则最多 50 条'),
    'At most 50 model price mapping rules are allowed',
  )
  assert.equal(
    localizedServerMessage('antigravity@example.com.json：Antigravity 配额刷新成功（2 组）'),
    'antigravity@example.com.json: Antigravity quota refreshed (2 groups)',
  )
  assert.equal(
    localizedServerMessage('antigravity@example.com.json：Antigravity 配额读取失败'),
    'antigravity@example.com.json: Failed to read Antigravity quota',
  )
  assert.equal(
    localizedServerMessage('账号 account_id 身份冲突（列表与详情不一致），请刷新后重试'),
    'Account account_id identity conflict (list and detail disagree). Refresh and try again.',
  )
  assert.equal(
    localizedServerMessage('账号不是 Codex 类型，无法主动重置'),
    'This account is not a Codex account and cannot be reset.',
  )
  assert.equal(
    localizedServerMessage('当前为 dry-run 模式，已阻止真实核销/重置；请关闭 dry-run 后重试'),
    'Dry-run mode blocked the real redemption/reset. Turn dry-run off and try again.',
  )
  assert.equal(
    localizedServerMessage('重置额度核销状态异常，请稍后重试'),
    'The reset-credit redemption state is inconsistent. Try again shortly.',
  )
  assert.equal(
    localizedServerMessage('账号缺少 account_id，无法安全核销，请刷新后重试'),
    'The account has no account_id; a safe redemption is not possible. Refresh and try again.',
  )
  assert.equal(
    localizedServerMessage('账号尚未确认身份（缺少 account_id），请先刷新账号列表后再重置'),
    'The account identity is not confirmed yet (no account_id). Refresh the account list before resetting.',
  )
  assert.equal(
    localizedServerMessage('无法确认可用重置额度（快照未知），请刷新后重试'),
    'Cannot confirm available reset credits (snapshot unknown). Refresh and try again.',
  )
  assert.equal(
    localizedServerMessage('核销主动重置额度失败：网络异常，未确认是否已核销'),
    'Failed to redeem the reset credit: network error; redemption is unconfirmed.',
  )
  assert.equal(
    localizedServerMessage('核销主动重置额度失败：OpenAI 拒绝核销'),
    'Failed to redeem the reset credit: OpenAI rejected the redemption.',
  )
  assert.equal(localizedServerMessage('auth_name 不能为空'), 'auth_name must not be empty')
  assert.equal(localizedServerMessage('账号不存在'), 'Account not found')
  assert.equal(localizedKeeperStatusDetail(null), 'Not running')

  const { apiClient } = await server.ssrLoadModule(`/src/shared/api/apiClient.ts?case=${moduleCase++}`)
  globalThis.fetch = async () => ({
    ok: false,
    status: 418,
    statusText: 'I am a teapot',
    json: async () => {
      throw new Error('not json')
    },
  })
  await assert.rejects(
    () => apiClient.get('/broken'),
    (error) => error instanceof Error && error.message === 'Request failed',
  )
} finally {
  await server.close()
  delete globalThis.fetch
  delete globalThis.localStorage
  delete globalThis.navigator
  delete globalThis.document
}
