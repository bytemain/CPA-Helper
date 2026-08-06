import { readonly, ref } from 'vue'

import { apiClient } from '@/shared/api/apiClient'
import type { ProductInfoResponse } from '@/shared/types/api'
import { logoUrl as defaultLogoUrl } from '@/shared/utils/assets'

const DEFAULT_PRODUCT_NAME = 'CPA-Helper'

const productName = ref(DEFAULT_PRODUCT_NAME)
const productLogo = ref(defaultLogoUrl)

export async function loadProductInfo(): Promise<void> {
  try {
    const info = await apiClient.get<ProductInfoResponse>('/product-info')
    productName.value = info.product_name.trim() || DEFAULT_PRODUCT_NAME
    productLogo.value = info.product_logo.trim() || defaultLogoUrl
  } catch {
    // fall back to defaults on error
  }
}

export function setProductInfo(name: string, logo: string): void {
  productName.value = name.trim() || DEFAULT_PRODUCT_NAME
  productLogo.value = logo.trim() || defaultLogoUrl
}

export function useProductInfo() {
  return {
    productName: readonly(productName),
    productLogo: readonly(productLogo),
  }
}
