import { useCallback, useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { Icon, type IconName } from '../components/Icon'
import { EmptyState, ErrorBanner, PageHeader, Spinner, StatusBadge } from '../components/ui'
import { api } from '../lib/api'
import { formatCompact, formatCost, formatDuration, formatNumber, formatTime, percent } from '../lib/format'
import { currentLanguage, t } from '../lib/i18n'
import type { Dashboard as DashboardData, DayStat } from '../lib/types'

function StatCard({
  label,
  value,
  hint,
  icon,
  tone,
}: {
  label: string
  value: string
  hint: string
  icon: IconName
  tone?: 'danger'
}) {
  return (
    <div className="stat-card">
      <div className="stat-top">
        <span className="stat-label">{label}</span>
        <span className="stat-icon">
          <Icon name={icon} size={17} />
        </span>
      </div>
      <div className={`stat-value${tone === 'danger' ? ' danger' : ''}`}>{value}</div>
      <div className="stat-hint">{hint}</div>
    </div>
  )
}

function TrafficChart({ series }: { series: DayStat[] }) {
  const max = Math.max(1, ...series.map((day) => day.requests))
  return (
    <div className="chart" role="img" aria-label={t('Requests per day for the last seven days')}>
      {series.map((day) => {
        const rawHeight = (day.requests / max) * 100
        const height = day.requests === 0 ? 2 : Math.max(7, rawHeight)
        const weekday = new Date(`${day.date}T00:00:00`).toLocaleDateString(
          currentLanguage() === 'zh' ? 'zh-CN' : 'en-US',
          { weekday: 'short' },
        )
        return (
          <div className="chart-col" key={day.date}>
            <div className="chart-value">{day.requests ? formatCompact(day.requests) : ''}</div>
            <div className="chart-track">
              <div
                className="chart-bar"
                style={{ height: `${height}%` }}
                title={t('{date}: {requests} requests, {errors} errors', {
                  date: day.date,
                  requests: formatNumber(day.requests),
                  errors: formatNumber(day.errors),
                })}
              >
                {day.errors > 0 ? (
                  <div
                    className="chart-bar-errors"
                    style={{ height: `${(day.errors / Math.max(1, day.requests)) * 100}%` }}
                  />
                ) : null}
              </div>
            </div>
            <div className="chart-label">{weekday}</div>
          </div>
        )
      })}
    </div>
  )
}

export default function Dashboard() {
  const [data, setData] = useState<DashboardData | null>(null)
  const [loading, setLoading] = useState(true)
  const [refreshing, setRefreshing] = useState(false)
  const [error, setError] = useState('')

  const load = useCallback(async (quiet = false) => {
    if (quiet) setRefreshing(true)
    else setLoading(true)
    try {
      setData(await api.dashboard())
      setError('')
    } catch (err) {
      setError(err instanceof Error ? err.message : t('Failed to load dashboard'))
    } finally {
      setLoading(false)
      setRefreshing(false)
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  if (loading && !data) {
    return (
      <div className="page-loading">
        <Spinner size={22} />
        {t('Loading dashboard…')}
      </div>
    )
  }
  if (!data) {
    return (
      <div className="page">
        <PageHeader title={t('Dashboard')} subtitle={t('Traffic and spend across your relay.')} />
        <ErrorBanner message={error || t('Dashboard unavailable')} />
        <button className="btn btn-primary" onClick={() => void load()}>
          <Icon name="refresh" size={16} /> {t('Try again')}
        </button>
      </div>
    )
  }

  const { totals, today, series, top_models, recent_logs, channels, tokens } = data
  const errorRate = totals.requests ? (totals.errors / totals.requests) * 100 : 0
  const topMax = Math.max(1, ...top_models.map((item) => item.requests))

  return (
    <div className="page">
      <PageHeader
        title={t('Dashboard')}
        subtitle={t('Traffic, tokens and spend across every upstream channel.')}
        actions={
          <button className="btn btn-ghost" onClick={() => void load(true)} disabled={refreshing}>
            <Icon name="refresh" size={16} className={refreshing ? 'spin' : undefined} />
            {t('Refresh')}
          </button>
        }
      />
      {error ? <ErrorBanner message={error} /> : null}

      <div className="stat-grid">
        <StatCard
          label={t('Requests')}
          value={formatNumber(totals.requests)}
          hint={t('{n} today', { n: formatNumber(today.requests) })}
          icon="activity"
        />
        <StatCard
          label={t('Tokens')}
          value={formatCompact(totals.total_tokens)}
          hint={t('{n} today', { n: formatCompact(today.total_tokens) })}
          icon="spark"
        />
        <StatCard
          label={t('Spend')}
          value={formatCost(totals.cost)}
          hint={
            totals.unverified_cost > 0
              ? t('{cost} today · {unverified} unverified', {
                  cost: formatCost(today.cost),
                  unverified: formatCost(totals.unverified_cost),
                })
              : t('{n} today', { n: formatCost(today.cost) })
          }
          icon="dollar"
        />
        <StatCard
          label={t('Error rate')}
          value={totals.requests ? `${errorRate.toFixed(1)}%` : "—"}
          hint={t('{n} failed requests', { n: formatNumber(totals.errors) })}
          icon="alert"
          tone={errorRate > 5 ? 'danger' : undefined}
        />
      </div>

      <div className="dashboard-grid">
        <section className="panel chart-panel">
          <div className="panel-head">
            <div>
              <h2>{t('Traffic')}</h2>
              <p>{t('Requests per day, last 7 days')}</p>
            </div>
            <span className="badge badge-muted">{t('7 days')}</span>
          </div>
          <TrafficChart series={series} />
          <div className="chart-legend">
            <span><span className="legend-swatch" /> {t('Successful')}</span>
            <span><span className="legend-swatch error" /> {t('Failures (stacked on top)')}</span>
          </div>
        </section>

        <section className="panel">
          <div className="panel-head">
            <div>
              <h2>{t('Top models')}</h2>
              <p>{t('By successful requests this week')}</p>
            </div>
          </div>
          {top_models.length === 0 ? (
            <EmptyState
              icon="models"
              title={t('No traffic yet')}
              description={t('Send your first request through the relay to populate this board.')}
              action={
                <Link className="btn btn-ghost" to="/tokens">
                  {t('Create an API key')}
                </Link>
              }
            />
          ) : (
            <div className="model-list">
              {top_models.map((item, index) => (
                <div className="model-row" key={item.model}>
                  <span className="model-rank">{String(index + 1).padStart(2, '0')}</span>
                  <div className="model-main">
                    <div className="model-name" title={item.model}>{item.model}</div>
                    <div className="model-meter">
                      <span style={{ width: `${percent(item.requests, topMax)}%` }} />
                    </div>
                  </div>
                  <div className="model-stats">
                    <strong>{formatNumber(item.requests)}</strong>
                    <span>{t('{n} tok', { n: formatCompact(item.tokens) })}</span>
                  </div>
                </div>
              ))}
            </div>
          )}
        </section>
      </div>

      <div className="mini-grid">
        <div className="mini-card">
          <div className="mini-head">
            <Icon name="channels" size={17} /> {t('Channels')}
          </div>
          <div className="mini-value">
            {formatNumber(channels.active)}
            <span> / {t('{n} active', { n: formatNumber(channels.total) })}</span>
          </div>
          <Link to="/channels">
            {t('Manage channels')} <Icon name="arrowRight" size={14} />
          </Link>
        </div>
        <div className="mini-card">
          <div className="mini-head">
            <Icon name="key" size={17} /> {t('API keys')}
          </div>
          <div className="mini-value">
            {formatNumber(tokens.active)}
            <span> / {t('{n} active', { n: formatNumber(tokens.total) })}</span>
          </div>
          <Link to="/tokens">
            {t('Manage keys')} <Icon name="arrowRight" size={14} />
          </Link>
        </div>
        <div className="mini-card">
          <div className="mini-head">
            <Icon name="shield" size={17} /> {t('Average latency')}
          </div>
          <div className="mini-value">
            {recent_logs.length
              ? formatDuration(Math.round(recent_logs.reduce((sum, item) => sum + item.latency_ms, 0) / recent_logs.length))
              : "—"}
          </div>
          <span className="mini-foot">{t('Across recent requests')}</span>
        </div>
      </div>

      <section className="panel">
        <div className="panel-head">
          <div>
            <h2>{t('Recent requests')}</h2>
            <p>{t('Latest relayed calls, newest first')}</p>
          </div>
          <Link className="btn btn-ghost btn-sm" to="/logs">
            {t('View logs')} <Icon name="arrowRight" size={14} />
          </Link>
        </div>
        {recent_logs.length === 0 ? (
          <EmptyState
            icon="activity"
            title={t('No requests recorded')}
            description={t('Once a key sends a chat completion, the request appears here.')}
          />
        ) : (
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th>{t('Time')}</th>
                  <th>{t('Model')}</th>
                  <th>{t('Status')}</th>
                  <th className="num">{t('Tokens')}</th>
                  <th className="num">{t('Cost')}</th>
                  <th className="num">{t('Latency')}</th>
                </tr>
              </thead>
              <tbody>
                {recent_logs.map((log) => (
                  <tr key={log.id}>
                    <td className="muted">{formatTime(log.created_at)}</td>
                    <td className="mono">{log.model}</td>
                    <td><StatusBadge status={log.status} /></td>
                    <td className="num">{formatNumber(log.total_tokens)}</td>
                    <td className="num">{formatCost(log.cost)}</td>
                    <td className="num muted">{formatDuration(log.latency_ms)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </div>
  )
}
