import { createApp } from 'vue'

import App from './app/App.vue'
import { router } from './app/router'
import { loadProductInfo } from './shared/state/productInfo'
import './styles/tokens.css'

loadProductInfo()
createApp(App).use(router).mount('#app')

