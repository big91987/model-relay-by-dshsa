import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter } from 'react-router-dom'
import { AuthProvider } from './lib/auth'
import { initLanguage, type Language } from './lib/i18n'
import { setRuntimeStatus } from './lib/runtime'
import { ThemeProvider } from './lib/theme'
import { ToastProvider } from './lib/toast'
import './styles.css'

function storedLanguage(): Language | null {
  try {
    const value = window.localStorage.getItem('mr-lang')
    return value === 'zh' || value === 'en' ? value : null
  } catch {
    return null
  }
}

async function boot() {
  const root = document.getElementById('root')
  if (!root) throw new Error('root element not found')

  // The public status endpoint decides whether this gateway is an explicitly
  // enabled local test environment; those consoles open in Chinese.
  let language = storedLanguage() ?? 'en'
  try {
    const response = await fetch('/api/status', { credentials: 'same-origin' })
    if (response.ok) {
      const status = await response.json()
      setRuntimeStatus(status)
      if (!storedLanguage() && status.test_mode) language = 'zh'
    }
  } catch {
    // Opening the console without a reachable API is handled by AuthProvider;
    // keep the default language so the login page can still explain the error.
  }
  initLanguage(language)

  // Import after initLanguage so module-level constants that call t() during
  // evaluation see the selected language.
  const { default: App } = await import('./App')
  createRoot(root).render(
    <StrictMode>
      <BrowserRouter>
        <ThemeProvider>
          <AuthProvider>
            <ToastProvider>
              <App />
            </ToastProvider>
          </AuthProvider>
        </ThemeProvider>
      </BrowserRouter>
    </StrictMode>,
  )
}

void boot()
