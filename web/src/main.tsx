import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import '@fontsource-variable/manrope'
import '@fontsource/dm-mono/400.css'
import '@fontsource/dm-mono/500.css'
import App from './App'
import './lib/i18n'
import './styles/theme.css'

document.documentElement.dataset.runtime='__TAURI_INTERNALS__' in window?'desktop':'web'

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
