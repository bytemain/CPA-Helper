// Locale-correct punctuation for the Antigravity quota summary. The status table cell and the
// account drawer both stitch together bucket lines, so the separators/parentheses must follow the
// active UI language — Chinese uses full-width `：，（）`, English uses ASCII `: , ()`. Hardcoding
// full-width punctuation made the English account page render e.g. `Gemini Models：Weekly …（Refresh
// in …）`. These helpers are pure so they can be unit-smoke-tested for both locales.
export type QuotaLang = 'zh' | 'en'

// antigravityResetWithCountdown appends the countdown to the absolute reset time in locale-correct
// parentheses. An empty countdown returns the reset time unchanged.
export function antigravityResetWithCountdown(resetTime: string, countdown: string, lang: QuotaLang): string {
  if (!countdown) {
    return resetTime
  }
  return lang === 'zh' ? `${resetTime}（${countdown}）` : `${resetTime} (${countdown})`
}

// joinAntigravityBuckets joins bucket summaries within a group.
export function joinAntigravityBuckets(parts: string[], lang: QuotaLang): string {
  return parts.join(lang === 'zh' ? '，' : ', ')
}

// antigravityGroupLine prefixes a group's label to its joined buckets.
export function antigravityGroupLine(label: string, buckets: string, lang: QuotaLang): string {
  return lang === 'zh' ? `${label}：${buckets}` : `${label}: ${buckets}`
}
