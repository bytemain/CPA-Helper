import type { CodexKeeperAccount } from '@/shared/types/api'

// isAntigravityAccount identifies an Antigravity-provider Keeper account. The Keeper now inspects
// both Codex and Antigravity accounts, so provider is the discriminator, not account type.
export function isAntigravityAccount(account: Pick<CodexKeeperAccount, 'provider'>): boolean {
  return account.provider === 'antigravity'
}

// isQuotaExhaustedAccount reports whether an account is in the Codex "quota exhausted" state.
// priority === -1 is the Codex quota-usage→priority policy's exhaustion marker; Antigravity does
// NOT run that policy, so a -1 on an Antigravity account must never be read as quota exhausted.
// Shared by every Keeper view (status + inspection settings) so the provider guard can't drift
// between them.
export function isQuotaExhaustedAccount(
  account: Pick<CodexKeeperAccount, 'disabled' | 'priority' | 'provider'>,
): boolean {
  return !account.disabled && !isAntigravityAccount(account) && (account.priority ?? 0) === -1
}
