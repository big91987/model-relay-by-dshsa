import type { ReactNode } from 'react'
import { Icon, type IconName } from './Icon'

export function PageHeader({
  title,
  subtitle,
  actions,
}: {
  title: string
  subtitle?: string
  actions?: ReactNode
}) {
  return (
    <header className="page-header">
      <div>
        <h1>{title}</h1>
        {subtitle ? <p>{subtitle}</p> : null}
      </div>
      {actions ? <div className="page-actions">{actions}</div> : null}
    </header>
  )
}

export function Spinner({ size = 16 }: { size?: number }) {
  return <span className="spinner" style={{ width: size, height: size }} aria-label="Loading" />
}

export function StatusBadge({ status }: { status: string }) {
  let tone: 'success' | 'danger' | 'muted' = 'muted'
  if (status === 'success' || status === 'enabled' || status === 'active') tone = 'success'
  if (status === 'error' || status === 'disabled') tone = 'danger'
  const label = status ? status.charAt(0).toUpperCase() + status.slice(1) : 'Unknown'
  return (
    <span className={`badge badge-${tone}`}>
      <span className={`badge-dot badge-dot-${tone}`} />
      {label}
    </span>
  )
}

const costStatusLabels: Record<string, string> = {
  actual: 'Actual',
  estimated: 'Estimated',
  unverified: 'Unverified',
  free: 'No charge',
  pending: 'Pending',
}

export function CostStatusBadge({ status }: { status: string }) {
  let tone: 'success' | 'danger' | 'muted' = 'muted'
  if (status === 'actual') tone = 'success'
  if (status === 'unverified') tone = 'danger'
  const label = costStatusLabels[status] ?? status
  return (
    <span className={`badge badge-${tone}`}>
      <span className={`badge-dot badge-dot-${tone}`} />
      {label}
    </span>
  )
}

export function ErrorBanner({ message }: { message: string }) {
  return (
    <div className="error-banner" role="alert">
      <Icon name="alert" size={16} />
      <span>{message}</span>
    </div>
  )
}

export function EmptyState({
  icon = 'spark',
  title,
  description,
  action,
}: {
  icon?: IconName
  title: string
  description?: string
  action?: ReactNode
}) {
  return (
    <div className="empty-state">
      <div className="empty-icon">
        <Icon name={icon} size={22} />
      </div>
      <h3>{title}</h3>
      {description ? <p>{description}</p> : null}
      {action ? <div className="empty-action">{action}</div> : null}
    </div>
  )
}

export function FormField({
  label,
  hint,
  children,
  htmlFor,
  wide,
}: {
  label: string
  hint?: string
  children: ReactNode
  htmlFor?: string
  wide?: boolean
}) {
  return (
    <label className={`field${wide ? ' wide' : ''}`} htmlFor={htmlFor}>
      <span className="field-label">{label}</span>
      {children}
      {hint ? <span className="field-hint">{hint}</span> : null}
    </label>
  )
}
