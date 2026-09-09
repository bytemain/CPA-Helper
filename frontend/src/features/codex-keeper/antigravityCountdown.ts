// formatAntigravityResetCountdown renders a FINE-GRAINED reset countdown for
// Antigravity quota buckets, mirroring upstream CPAMC's duration buckets:
// day+hour / hour+minute / minute. This deliberately differs from Codex's coarse
// formatQuotaResetCountdown ("1小时后" for 59m, etc.). Pure and dependency-free so
// it can be unit-tested in isolation.
//
// - resetAtIso null/undefined/unparseable → '' (caller renders a dash).
// - remaining <= 0                        → zh '可刷新' / en 'Refresh available'.
// - remaining < 1 min                     → zh '小于 1 分钟后刷新' / en 'Refresh in <1m'.
// - remaining >= 1 day  → zh '${d} 天 ${h} 小时后刷新' / en 'Refresh in ${d}d ${h}h'.
// - remaining >= 1 hour → zh '${h} 小时 ${m} 分钟后刷新' / en 'Refresh in ${h}h ${m}m'.
// - remaining >= 1 min  → zh '${m} 分钟后刷新' / en 'Refresh in ${m}m'.
const MINUTE_MS = 60_000
const HOUR_MS = 3_600_000
const DAY_MS = 86_400_000

export function formatAntigravityResetCountdown(
  resetAtIso: string | null | undefined,
  nowMs: number,
  lang: 'zh' | 'en',
): string {
  if (!resetAtIso) {
    return ''
  }
  const resetMs = new Date(resetAtIso).getTime()
  if (Number.isNaN(resetMs)) {
    return ''
  }
  const remaining = resetMs - nowMs
  if (remaining <= 0) {
    return lang === 'zh' ? '可刷新' : 'Refresh available'
  }
  if (remaining < MINUTE_MS) {
    return lang === 'zh' ? '小于 1 分钟后刷新' : 'Refresh in <1m'
  }
  if (remaining >= DAY_MS) {
    const days = Math.floor(remaining / DAY_MS)
    const hours = Math.floor((remaining % DAY_MS) / HOUR_MS)
    return lang === 'zh'
      ? `${days} 天 ${hours} 小时后刷新`
      : `Refresh in ${days}d ${hours}h`
  }
  if (remaining >= HOUR_MS) {
    const hours = Math.floor(remaining / HOUR_MS)
    const minutes = Math.floor((remaining % HOUR_MS) / MINUTE_MS)
    return lang === 'zh'
      ? `${hours} 小时 ${minutes} 分钟后刷新`
      : `Refresh in ${hours}h ${minutes}m`
  }
  const minutes = Math.floor(remaining / MINUTE_MS)
  return lang === 'zh' ? `${minutes} 分钟后刷新` : `Refresh in ${minutes}m`
}
