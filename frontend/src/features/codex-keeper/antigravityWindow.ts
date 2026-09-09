// normalizeAntigravityWindow collapses the many aliases the Antigravity quota schema uses for the
// same reset window into a stable key, so the UI can localize it instead of leaking the raw English
// bucket display_name. CPAMC's reference implementation treats e.g. "5h"/"5 hour"/"5-hour" as one
// window and also surfaces "daily"/"monthly"; the schema may return any of these spellings. We
// lower-case and strip spaces, hyphens, and underscores before matching. A genuinely unknown window
// returns null so the caller can fall back to the bucket's own display_name.
export type AntigravityWindowKey = 'weekly' | '5h' | 'daily' | 'monthly'

export function normalizeAntigravityWindow(window: string | null | undefined): AntigravityWindowKey | null {
  const normalized = (window ?? '').toLowerCase().replace(/[\s_-]+/g, '')
  if (normalized === '5h' || normalized === '5hour' || normalized === '5hours') {
    return '5h'
  }
  if (normalized === 'weekly' || normalized === 'week') {
    return 'weekly'
  }
  if (normalized === 'daily' || normalized === 'day') {
    return 'daily'
  }
  if (normalized === 'monthly' || normalized === 'month') {
    return 'monthly'
  }
  return null
}
