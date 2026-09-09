// formatAntigravityResetCountdown renders a FINE-GRAINED reset countdown for
// Antigravity quota buckets, mirroring upstream CPAMC's duration buckets:
// day+hour / hour+minute / minute. This deliberately differs from Codex's coarse
// formatQuotaResetCountdown ("1小时后" for 59m, etc.). Pure and dependency-free so
// it can be unit-tested in isolation.
//
// Uses CEIL-to-minute (CPAMC's algorithm) so we NEVER under-report the remaining
// time: e.g. 4h56m30s rounds up to 4h57m rather than flooring to 4h56m.
//
// - resetAtIso null/undefined/unparseable → '' (caller renders a dash).
// - deltaMs <= 0        → zh '可刷新' / en 'Refresh available'.
// - days >= 1           → zh '${d} 天 ${h} 小时后刷新' / en 'Refresh in ${d}d ${h}h'.
// - hours >= 1          → zh '${h} 小时 ${m} 分钟后刷新' / en 'Refresh in ${h}h ${m}m'.
// - otherwise           → zh '${m} 分钟后刷新' / en 'Refresh in ${m}m'.
//   (minutes is >= 1 here because Math.ceil of any positive delta is >= 1)
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
  const deltaMs = resetMs - nowMs
  if (deltaMs <= 0) {
    return lang === 'zh' ? '可刷新' : 'Refresh available'
  }
  const totalMinutes = Math.ceil(deltaMs / 60000)
  const days = Math.floor(totalMinutes / 1440)
  const remMin = totalMinutes % 1440
  const hours = Math.floor(remMin / 60)
  const minutes = remMin % 60
  if (days >= 1) {
    return lang === 'zh'
      ? `${days} 天 ${hours} 小时后刷新`
      : `Refresh in ${days}d ${hours}h`
  }
  if (hours >= 1) {
    return lang === 'zh'
      ? `${hours} 小时 ${minutes} 分钟后刷新`
      : `Refresh in ${hours}h ${minutes}m`
  }
  return lang === 'zh' ? `${minutes} 分钟后刷新` : `Refresh in ${minutes}m`
}
