// Lightweight console localization. English strings are the keys; the Chinese
// dictionary is selected at boot when the gateway runs in test mode, so no
// translation library or runtime dependency is needed.
import { zh as coreZh } from './i18n/zh-core'
import { zh as channelsZh } from './i18n/zh-channels'
import { zh as modelsZh } from './i18n/zh-models'
import { zh as tokensZh } from './i18n/zh-tokens'
import { zh as logsZh } from './i18n/zh-logs'
import { zh as playgroundZh } from './i18n/zh-playground'
import { zh as speechZh } from './i18n/zh-speech'
import { zh as videosZh } from './i18n/zh-videos'
import { zh as voiceDesignZh } from './i18n/zh-voice-design'
import { zh as musicZh } from './i18n/zh-music'

export type Language = 'zh' | 'en'

const dictionaries: Record<string, string> = {
  ...coreZh,
  ...channelsZh,
  ...modelsZh,
  ...tokensZh,
  ...logsZh,
  ...playgroundZh,
  ...speechZh,
  ...videosZh,
  ...voiceDesignZh,
  ...musicZh,
}

let language: Language = 'en'

// initLanguage must run before the React tree renders. English keys used in
// module-level constants therefore have to be translated at render time.
export function initLanguage(next: Language): void {
  language = next
  if (typeof document !== 'undefined') {
    document.documentElement.lang = next === 'zh' ? 'zh-CN' : 'en'
  }
}

export function currentLanguage(): Language {
  return language
}

export function t(text: string, vars?: Record<string, string | number>): string {
  const template = language === 'zh' ? (dictionaries[text] ?? text) : text
  if (!vars) return template
  return template.replace(/\{(\w+)\}/g, (match, key: string) =>
    Object.prototype.hasOwnProperty.call(vars, key) ? String(vars[key]) : match,
  )
}

// hasTranslation lets a few hot paths skip formatting when no Chinese entry
// exists, without exposing the dictionary itself.
export function hasTranslation(text: string): boolean {
  return Object.prototype.hasOwnProperty.call(dictionaries, text)
}
