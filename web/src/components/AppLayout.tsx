import { useEffect, useState } from 'react'
import { NavLink, Outlet, useLocation, useNavigate } from 'react-router-dom'
import { api } from '../lib/api'
import { useAuth } from '../lib/auth'
import { Icon, type IconName } from './Icon'
import { ThemeSwitcher } from './ThemeSwitcher'

const navigation: { to: string; label: string; icon: IconName }[] = [
  { to: '/dashboard', label: 'Dashboard', icon: 'dashboard' },
  { to: '/playground', label: 'Playground', icon: 'spark' },
  { to: '/channels', label: 'Channels', icon: 'channels' },
  { to: '/models', label: 'Models', icon: 'models' },
  { to: '/videos', label: 'Video', icon: 'video' },
  { to: '/music', label: 'Music', icon: 'music' },
  { to: '/tokens', label: 'API keys', icon: 'key' },
  { to: '/logs', label: 'Logs', icon: 'logs' },
]

const titles: Record<string, string> = {
  '/dashboard': 'Dashboard',
  '/playground': 'Playground',
  '/channels': 'Channels',
  '/models': 'Models',
  '/videos': 'Video generation',
  '/music': 'Music generation',
  '/tokens': 'API keys',
  '/logs': 'Logs',
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

  const title = titles[location.pathname] ?? 'Console'
  const initial = user?.username?.charAt(0).toUpperCase() ?? 'A'

  return (
    <div className="app-shell">
      <aside className="sidebar">
        <div className="sidebar-brand">
          <div className="brand-mark">MR</div>
          <div>
            <div className="brand-name">Model Relay</div>
            <div className="brand-sub">Gateway console</div>
          </div>
        </div>
        <nav className="nav" aria-label="Main navigation">
          {navigation.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              className={({ isActive }) => `nav-link${isActive ? ' active' : ''}`}
            >
              <Icon name={item.icon} size={18} />
              <span>{item.label}</span>
            </NavLink>
          ))}
        </nav>
        <div className="sidebar-footer">
          <div className="sidebar-status">
            <span className="pulse-dot" />
            Gateway operational
          </div>
          <div className="sidebar-version">Model Relay {version ? `v${version}` : ''}</div>
        </div>
      </aside>
      <div className="main-column">
        <header className="topbar">
          <div className="topbar-title">
            <span className="topbar-root">Console</span>
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
              title="Sign out"
              aria-label="Sign out"
            >
              <Icon name="logout" size={18} />
            </button>
          </div>
        </header>
        <main className="content">
          <Outlet />
        </main>
      </div>
    </div>
  )
}
