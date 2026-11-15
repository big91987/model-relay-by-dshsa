import { useState } from 'react'
import type { FormEvent } from 'react'
import { Navigate } from 'react-router-dom'
import { useAuth } from '../lib/auth'
import { t } from '../lib/i18n'
import { isTestMode } from '../lib/runtime'
import { ThemeSwitcher } from '../components/ThemeSwitcher'
import { ErrorBanner, FormField, Spinner } from '../components/ui'

export default function Login() {
  const { user, loading, login } = useAuth()
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const testMode = isTestMode()

  if (loading) {
    return (
      <div className="boot-screen">
        <div className="brand-mark large">MR</div>
        <Spinner size={20} />
      </div>
    )
  }
  if (user) return <Navigate to="/dashboard" replace />

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    setBusy(true)
    setError('')
    try {
      await login(username.trim(), password)
    } catch (err) {
      setError(err instanceof Error ? err.message : t('Sign in failed'))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="login-page">
      <div className="login-orb login-orb-a" />
      <div className="login-orb login-orb-b" />
      <div className="login-theme">
        <ThemeSwitcher />
      </div>
      <div className="login-panel">
        <div className="login-brand">
          <div className="brand-mark large">MR</div>
          <div>
            <div className="brand-name">{t("Model Relay")}</div>
            <div className="brand-sub">{t('OpenAI-compatible gateway')}</div>
          </div>
        </div>
        <h1>{t('Welcome back')}</h1>
        <p className="login-lead">{t('Sign in to manage providers, API keys and request traffic.')}</p>
        {testMode ? (
          <div className="login-test-hint" role="note">
            <strong>{t('Local test environment')}</strong>
            <span>{t('Default test account: admin / admin. Enabled by TEST_MODE; never use it in production.')}</span>
          </div>
        ) : null}
        <form onSubmit={submit} className="login-form">
          <FormField label={t('Username')} htmlFor="username">
            <input
              id="username"
              className="input"
              value={username}
              onChange={(event) => setUsername(event.target.value)}
              autoComplete="username"
              autoFocus
              required
              placeholder="admin"
            />
          </FormField>
          <FormField label={t('Password')} htmlFor="password">
            <input
              id="password"
              className="input"
              type="password"
              value={password}
              onChange={(event) => setPassword(event.target.value)}
              autoComplete="current-password"
              required
              placeholder={t('Your password')}
            />
          </FormField>
          {error ? <ErrorBanner message={error} /> : null}
          <button className="btn btn-primary btn-block" type="submit" disabled={busy}>
            {busy ? <Spinner size={16} /> : null}
            {busy ? t('Signing in…') : t('Sign in')}
          </button>
        </form>
        <div className="login-foot">
          <span className="pulse-dot" />
          {t('Self-hosted · Keys stay on your infrastructure')}
        </div>
      </div>
    </div>
  )
}
