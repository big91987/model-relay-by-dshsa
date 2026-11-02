import type { IconName } from './Icon'
import { Icon } from './Icon'
import { useTheme } from '../lib/theme'
import type { ThemePreference } from '../lib/theme'

const options: { value: ThemePreference; label: string; icon: IconName }[] = [
  { value: 'light', label: 'Light theme', icon: 'sun' },
  { value: 'dark', label: 'Dark theme', icon: 'moon' },
  { value: 'system', label: 'Follow system theme', icon: 'monitor' },
]

export function ThemeSwitcher() {
  const { preference, setPreference } = useTheme()

  return (
    <div className="theme-switch" role="group" aria-label="Color theme">
      {options.map((option) => {
        const active = preference === option.value
        return (
          <button
            key={option.value}
            type="button"
            className={`theme-option${active ? ' active' : ''}`}
            onClick={() => setPreference(option.value)}
            title={option.label}
            aria-label={option.label}
            aria-pressed={active}
          >
            <Icon name={option.icon} size={15} />
          </button>
        )
      })}
    </div>
  )
}
