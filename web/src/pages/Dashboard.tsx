import { useCallback, useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { Icon, type IconName } from '../components/Icon'
import { EmptyState, ErrorBanner, PageHeader, Spinner, StatusBadge } from '../components/ui'
import { api } from '../lib/api'
import { formatCompact, formatCost, formatDuration, formatNumber, formatTime, percent } from '../lib/format'
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
    <div className="chart" role="img" aria-label="Requests per day for the last seven days">
      {series.map((day) => {
        const rawHeight = (day.requests / max) * 100
        const height = day.requests === 0 ? 2 : Math.max(7, rawHeight)
        const weekday = new Date(`${day.date}T00:00:00`).toLocaleDateString('en-US', { weekday: 'short' })
        return (
          <div className="chart-col" key={day.date}>
            <div className="chart-value">{day.requests ? formatCompact(day.requests) : ''}</div>
            <div className="chart-track">
              <div
                className="chart-bar"
                style={{ height: `${height}%` }}
                title={`${day.date}: ${formatNumber(day.requests)} requests, ${formatNumber(day.errors)} errors`}
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
      setError(err instanceof Error ? err.message : 'Failed to load dashboard')
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
        Loading dashboard…
      </div>
    )
  }
  if (!data) {
    return (
      <div className="page">
        <PageHeader title="Dashboard" subtitle="Traffic and spend across your relay." />
        <ErrorBanner message={error || 'Dashboard unavailable'} />
        <button className="btn btn-primary" onClick={() => void load()}>
          <Icon name="refresh" size={16} /> Try again
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
        title="Dashboard"
        subtitle="Traffic, tokens and spend across every upstream channel."
        actions={
          <button className="btn btn-ghost" onClick={() => void load(true)} disabled={refreshing}>
            <Icon name="refresh" size={16} className={refreshing ? 'spin' : undefined} />
            Refresh
          </button>
        }
      />
      {error ? <ErrorBanner message={error} /> : null}

      <div className="stat-grid">
        <StatCard
          label="Requests"
          value={formatNumber(totals.requests)}
          hint={`${formatNumber(today.requests)} today`}
          icon="activity"
        />
        <StatCard
          label="Tokens"
          value={formatCompact(totals.total_tokens)}
          hint={`${formatCompact(today.total_tokens)} today`}
          icon="spark"
        />
        <StatCard
          label="Spend"
          value={formatCost(totals.cost)}
          hint={
            totals.unverified_cost > 0
              ? `${formatCost(today.cost)} today · ${formatCost(totals.unverified_cost)} unverified`
              : `${formatCost(today.cost)} today`
          }
          icon="dollar"
        />
        <StatCard
          label="Error rate"
          value={totals.requests ? `${errorRate.toFixed(1)}%` : '—'}
          hint={`${formatNumber(totals.errors)} failed requests`}
          icon="alert"
          tone={errorRate > 5 ? 'danger' : undefined}
        />
      </div>

      <div className="dashboard-grid">
        <section className="panel chart-panel">
          <div className="panel-head">
            <div>
              <h2>Traffic</h2>
              <p>Requests per day, last 7 days</p>
            </div>
            <span className="badge badge-muted">7 days</span>
          </div>
          <TrafficChart series={series} />
          <div className="chart-legend">
            <span><span className="legend-swatch" /> Successful</span>
            <span><span className="legend-swatch error" /> Failures (stacked on top)</span>
          </div>
        </section>

        <section className="panel">
          <div className="panel-head">
            <div>
              <h2>Top models</h2>
              <p>By successful requests this week</p>
            </div>
          </div>
          {top_models.length === 0 ? (
            <EmptyState
              icon="models"
              title="No traffic yet"
              description="Send your first request through the relay to populate this board."
              action={
                <Link className="btn btn-ghost" to="/tokens">
                  Create an API key
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
                    <span>{formatCompact(item.tokens)} tok</span>
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
            <Icon name="channels" size={17} /> Channels
          </div>
          <div className="mini-value">
            {formatNumber(channels.active)}
            <span> / {formatNumber(channels.total)} active</span>
          </div>
          <Link to="/channels">
            Manage channels <Icon name="arrowRight" size={14} />
          </Link>
        </div>
        <div className="mini-card">
          <div className="mini-head">
            <Icon name="key" size={17} /> API keys
          </div>
          <div className="mini-value">
            {formatNumber(tokens.active)}
            <span> / {formatNumber(tokens.total)} active</span>
          </div>
          <Link to="/tokens">
            Manage keys <Icon name="arrowRight" size={14} />
          </Link>
        </div>
        <div className="mini-card">
          <div className="mini-head">
            <Icon name="shield" size={17} /> Average latency
          </div>
          <div className="mini-value">
            {recent_logs.length
              ? formatDuration(Math.round(recent_logs.reduce((sum, item) => sum + item.latency_ms, 0) / recent_logs.length))
              : '—'}
          </div>
          <span className="mini-foot">Across recent requests</span>
        </div>
      </div>

      <section className="panel">
        <div className="panel-head">
          <div>
            <h2>Recent requests</h2>
            <p>Latest relayed calls, newest first</p>
          </div>
          <Link className="btn btn-ghost btn-sm" to="/logs">
            View logs <Icon name="arrowRight" size={14} />
          </Link>
        </div>
        {recent_logs.length === 0 ? (
          <EmptyState
            icon="activity"
            title="No requests recorded"
            description="Once a key sends a chat completion, the request appears here."
          />
        ) : (
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th>Time</th>
                  <th>Model</th>
                  <th>Status</th>
                  <th className="num">Tokens</th>
                  <th className="num">Cost</th>
                  <th className="num">Latency</th>
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
