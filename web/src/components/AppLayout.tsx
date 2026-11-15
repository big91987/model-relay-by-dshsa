import { useEffect, useState } from 'react'
import { NavLink, Outlet, useLocation, useNavigate } from 'react-router-dom'
import { api } from '../lib/api'
import { useAuth } from '../lib/auth'
import { t } from '../lib/i18n'
import { databaseBackend, isTestMode } from '../lib/runtime'
import { Icon, type IconName } from './Icon'
import { ThemeSwitcher } from './ThemeSwitcher'

const navigation: { to: string; label: string; icon: IconName }[] = [
  { to: '/dashboard', label: t("Dashboard"), icon: 'dashboard' },
  { to: '/playground', label: t("Playground"), icon: 'spark' },
  { to: '/channels', label: t("Channels"), icon: 'channels' },
  { to: '/models', label: t("Models"), icon: 'models' },
  { to: '/videos', label: t("Video"), icon: 'video' },
  { to: '/speech', label: t("Speech"), icon: 'speech' },
  { to: '/voice-design', label: t("Voice design"), icon: 'spark' },
  { to: '/music', label: 'Music', icon: 'music' },
  { to: '/tokens', label: t("API keys"), icon: 'key' },
  { to: '/logs', label: t("Logs"), icon: 'logs' },
]

const titles: Record<string, string> = {
  '/dashboard': t("Dashboard"),
  '/playground': t("Playground"),
  '/channels': t("Channels"),
  '/models': t("Models"),
  '/videos': t("Video generation"),
  '/speech': t("Text to speech"),
  '/voice-design': t("Voice design"),
  '/music': 'Music generation',
  '/tokens': t("API keys"),
  '/logs': t("Logs"),
}

export function AppLayout() {
  const { user, logout } = useAuth()
  const navigate = useNavigate()
  const location = useLocation()
  const [version, setVersion] = useState('')
  const [signingOut, setSigningOut] = useState(false)

  useEffect(() => {
    let active = true
    api
      .status()
      .then((status) => {
        if (active) setVersion(status.version)
      })
      .catch(() => undefined)
    return () => {
      active = false
    }
  }, [])

  const handleSignOut = async () => {
    setSigningOut(true)
    try {
      await logout()
      navigate('/login', { replace: true })
    } finally {
      setSigningOut(false)
    }
  }

  const title = t(titles[location.pathname] ?? t("Console"))
  const initial = user?.username?.charAt(0).toUpperCase() ?? 'A'

  return (
    <div className="app-shell">
      <aside className="sidebar">
        <div className="sidebar-brand">
          <div className="brand-mark">MR</div>
          <div>
            <div className="brand-name">{t("Model Relay")}</div>
            <div className="brand-sub">{t('Gateway console')}</div>
          </div>
        </div>
        <nav className="nav" aria-label={t('Main navigation')}>
          {navigation.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              className={({ isActive }) => `nav-link${isActive ? ' active' : ''}`}
            >
              <Icon name={item.icon} size={18} />
              <span>{t(item.label)}</span>
            </NavLink>
          ))}
        </nav>
        <div className="sidebar-footer">
          <div className="sidebar-status">
            <span className="pulse-dot" />
            {t('Gateway operational')}
          </div>
          <div className="sidebar-version">{t("Model Relay")} {version ? `v${version}` : ''}</div>
        </div>
      </aside>
      <div className="main-column">
        <header className="topbar">
          <div className="topbar-title">
            <span className="topbar-root">{t('Console')}</span>
            <span className="topbar-sep">/</span>
            <span>{title}</span>
          </div>
          <div className="topbar-actions">
            <ThemeSwitcher />
            <span className="user-chip">
              <span className="avatar">{initial}</span>
              <span>{user?.username}</span>
            </span>
            <button
              className="icon-button"
              onClick={handleSignOut}
              disabled={signingOut}
              title={t('Sign out')}
              aria-label={t('Sign out')}
            >
              <Icon name="logout" size={18} />
            </button>
          </div>
        </header>
        {isTestMode() ? (
          <div className="test-mode-strip" role="note">
            <span className="pulse-dot" />
            <strong>{t('Test environment')}</strong>
            <span>
              {t('Database backend')}: {databaseBackend() === 'postgres' ? t('PostgreSQL (DATABASE_URL)') : t('SQLite (default)')}
            </span>
            <span>{t('The console opened in Chinese because TEST_MODE is enabled.')}</span>
          </div>
        ) : null}
        <main className="content">
          <Outlet />
        </main>
      </div>
    </div>
  )
}
