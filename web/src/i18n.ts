import i18n from 'i18next'
import { initReactI18next } from 'react-i18next'
import en from './locales/en'
import zh from './locales/zh'

export const supportedLanguages = ['zh', 'en'] as const
export type SupportedLanguage = typeof supportedLanguages[number]

const storageKey = 'opsnerva.language'

function initialLanguage(): SupportedLanguage {
  try {
    const stored = localStorage.getItem(storageKey)
    if (stored === 'zh' || stored === 'en') return stored
  } catch { /* storage may be disabled */ }
  return navigator.language.toLowerCase().startsWith('zh') ? 'zh' : 'en'
}

void i18n.use(initReactI18next).init({
  resources: { en: { translation: en }, zh: { translation: zh } },
  lng: initialLanguage(),
  fallbackLng: 'en',
  supportedLngs: [...supportedLanguages],
  interpolation: { escapeValue: false },
})

function applyLanguage(language: string) {
  const normalized = language.startsWith('zh') ? 'zh' : 'en'
  document.documentElement.lang = normalized === 'zh' ? 'zh-CN' : 'en'
  document.title = 'OpsNerva'
  try { localStorage.setItem(storageKey, normalized) } catch { /* storage may be disabled */ }
}

applyLanguage(i18n.resolvedLanguage || i18n.language)
i18n.on('languageChanged', applyLanguage)

export function localeFor(language: string) {
  return language.startsWith('zh') ? 'zh-CN' : 'en-US'
}

export default i18n
