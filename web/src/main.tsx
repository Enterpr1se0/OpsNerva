import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import App from './App'
import './i18n'
import './theme.css'

document.documentElement.dataset.runtime='__TAURI_INTERNALS__' in window?'desktop':'web'

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
