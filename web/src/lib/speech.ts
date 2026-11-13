import type { ModelInfo } from './types'

export const SPEECH_MAX_TEXT = 10000
export const SPEECH_DEFAULT_MODEL = 'speech-2.8-hd'

// isSpeechModel reports whether a public model is a MiniMax speech model. The
// console only offers these to the speech and voice-design workflows.
export function isSpeechModel(model: ModelInfo): boolean {
  if (!model.types.includes('minimax')) return false
  return /^speech-/i.test(model.id) || /^minimax-speech/i.test(model.id)
}

export function formatSpeechDuration(durationMS: number): string {
  if (!durationMS || durationMS <= 0) return 'Unknown length'
  const seconds = Math.round(durationMS / 1000)
  const minutes = Math.floor(seconds / 60)
  return `${minutes}:${String(seconds % 60).padStart(2, '0')}`
}
