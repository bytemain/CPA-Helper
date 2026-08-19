import { readonly, ref } from 'vue'

import { logoUrl as defaultLogoUrl } from '@/shared/utils/assets'

const DEFAULT_PRODUCT_NAME = 'CPA-Helper'

const productName = ref(DEFAULT_PRODUCT_NAME)
const productLogo = ref(defaultLogoUrl)

function syncDocumentTitle(name: string): void {
  document.title = name
}

export function loadProductInfo(): void {
  const info = (window as Record<string, unknown>).__PRODUCT_INFO__ as
    | { product_name?: string; product_logo?: string }
    | undefined
  if (info) {
    productName.value = (typeof info.product_name === 'string' ? info.product_name.trim() : '') || DEFAULT_PRODUCT_NAME
    productLogo.value = (typeof info.product_logo === 'string' ? info.product_logo.trim() : '') || defaultLogoUrl
  }
}

export function setProductInfo(name: string, logo: string): void {
  productName.value = name.trim() || DEFAULT_PRODUCT_NAME
  productLogo.value = logo.trim() || defaultLogoUrl
  syncDocumentTitle(productName.value)
}

export function useProductInfo() {
  return {
    productName: readonly(productName),
    productLogo: readonly(productLogo),
  }
}
