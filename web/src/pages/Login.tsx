import { useState } from 'react'
import type { FormEvent } from 'react'
import { Navigate } from 'react-router-dom'
import { useAuth } from '../lib/auth'
import { ErrorBanner, FormField, Spinner } from '../components/ui'

export default function Login() {
  const { user, loading, login } = useAuth()
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

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
      setError(err instanceof Error ? err.message : 'Sign in failed')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="login-page">
      <div className="login-orb login-orb-a" />
      <div className="login-orb login-orb-b" />
      <div className="login-panel">
        <div className="login-brand">
          <div className="brand-mark large">MR</div>
          <div>
            <div className="brand-name">Model Relay</div>
            <div className="brand-sub">OpenAI-compatible gateway</div>
          </div>
        </div>
        <h1>Welcome back</h1>
        <p className="login-lead">Sign in to manage providers, API keys and request traffic.</p>
        <form onSubmit={submit} className="login-form">
          <FormField label="Username" htmlFor="username">
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
          <FormField label="Password" htmlFor="password">
            <input
              id="password"
              className="input"
              type="password"
              value={password}
              onChange={(event) => setPassword(event.target.value)}
              autoComplete="current-password"
              required
              placeholder="Your password"
            />
          </FormField>
          {error ? <ErrorBanner message={error} /> : null}
          <button className="btn btn-primary btn-block" type="submit" disabled={busy}>
            {busy ? <Spinner size={16} /> : null}
            {busy ? 'Signing in…' : 'Sign in'}
          </button>
        </form>
        <div className="login-foot">
          <span className="pulse-dot" />
          Self-hosted · Keys stay on your infrastructure
        </div>
      </div>
    </div>
  )
}
