import { readonly, ref } from 'vue'

import { logoUrl as defaultLogoUrl } from '@/shared/utils/assets'

const DEFAULT_PRODUCT_NAME = 'CPA-Helper'

const productName = ref(DEFAULT_PRODUCT_NAME)
const productLogo = ref(defaultLogoUrl)

function normalizeProductName(raw: string): string {
  return raw.trim() || DEFAULT_PRODUCT_NAME
}

function normalizeProductLogo(raw: string): string {
  return raw.trim() || defaultLogoUrl
}

export function loadProductInfo(): void {
  const info = (window as Record<string, unknown>).__PRODUCT_INFO__ as
    | { product_name?: string; product_logo?: string }
    | undefined
  if (info) {
    productName.value = normalizeProductName(typeof info.product_name === 'string' ? info.product_name : '')
    productLogo.value = normalizeProductLogo(typeof info.product_logo === 'string' ? info.product_logo : '')
  }
  document.title = productName.value
}

export function setProductInfo(name: string, logo: string): void {
  productName.value = normalizeProductName(name)
  productLogo.value = normalizeProductLogo(logo)
  document.title = productName.value
}

export function useProductInfo() {
  return {
    productName: readonly(productName),
    productLogo: readonly(productLogo),
  }
}
