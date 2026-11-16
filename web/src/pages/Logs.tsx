import { useCallback, useEffect, useState } from 'react'
import type { FormEvent } from 'react'
import { useSearchParams } from 'react-router-dom'
import { Icon } from '../components/Icon'
import { Modal } from '../components/Modal'
import { CostStatusBadge, EmptyState, ErrorBanner, PageHeader, Spinner, StatusBadge } from '../components/ui'
import { api } from '../lib/api'
import {
  formatCost,
  formatDateTime,
  formatDuration,
  formatNumber,
  formatTime,
  parseLocalDateTime,
  percent,
} from '../lib/format'
import { t } from '../lib/i18n'
import { useToast } from '../lib/toast'
import type { LogItem, LogQuery, LogsResponse, Token } from '../lib/types'

interface Filters {
  model: string
  status: string
  request_id: string
  token_id: string
  start: string
  end: string
}

const emptyFilters: Filters = { model: '', status: '', request_id: '', token_id: '', start: '', end: '' }
const pageSize = 25

function describePeriod(filters: Filters): string {
  const start = filters.start ? parseLocalDateTime(filters.start) : null
  const end = filters.end ? parseLocalDateTime(filters.end) : null
  if (start !== null && end !== null) return `${formatDateTime(start)} – ${formatDateTime(end)}`
  if (start !== null) return t('From {date}', { date: formatDateTime(start) })
  if (end !== null) return t('Until {date}', { date: formatDateTime(end) })
  return t('All time')
}

function logQuery(filters: Filters): LogQuery {
  const start = filters.start ? parseLocalDateTime(filters.start) : null
  const end = filters.end ? parseLocalDateTime(filters.end) : null
  return {
    model: filters.model.trim() || undefined,
    status: filters.status || undefined,
    request_id: filters.request_id.trim() || undefined,
    token_id: filters.token_id ? Number(filters.token_id) : undefined,
    start: start ?? undefined,
    // The picker has minute precision: include the whole selected end minute.
    end: end !== null ? end + 59 : undefined,
  }
}

export default function Logs() {
  const toast = useToast()
  const [searchParams, setSearchParams] = useSearchParams()
  const initialRequestId = (searchParams.get('request_id') ?? '').trim()
  const [draft, setDraft] = useState<Filters>({ ...emptyFilters, request_id: initialRequestId })
  const [filters, setFilters] = useState<Filters>({ ...emptyFilters, request_id: initialRequestId })
  const [page, setPage] = useState(1)
  const [data, setData] = useState<LogsResponse | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [tokens, setTokens] = useState<Token[]>([])
  const [selected, setSelected] = useState<LogItem | null>(null)
  const [exporting, setExporting] = useState(false)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const response = await api.logs({ page, page_size: pageSize, ...logQuery(filters) })
      setData(response)
      setError('')
    } catch (err) {
      setError(err instanceof Error ? err.message : t('Failed to load request logs'))
    } finally {
      setLoading(false)
    }
  }, [page, filters])

  useEffect(() => {
    void load()
  }, [load])

  useEffect(() => {
    api
      .tokens()
      .then((response) => setTokens(response.data))
      .catch(() => undefined)
  }, [])

  useEffect(() => {
    const requestId = (searchParams.get('request_id') ?? '').trim()
    if (requestId === filters.request_id) return
    setDraft((current) => ({ ...current, request_id: requestId }))
    setFilters((current) => ({ ...current, request_id: requestId }))
    setPage(1)
  }, [searchParams, filters.request_id])

  const validateDraft = (): Filters | null => {
    const next = { ...draft, request_id: draft.request_id.trim() }
    const start = next.start ? parseLocalDateTime(next.start) : null
    const end = next.end ? parseLocalDateTime(next.end) : null
    if (start !== null && end !== null && start > end) {
      toast.error(t('The period start must not be after the period end'))
      return null
    }
    return next
  }

  const applyFilters = (event: FormEvent) => {
    event.preventDefault()
    const next = validateDraft()
    if (!next) return
    setPage(1)
    setFilters(next)
    setSearchParams(next.request_id ? { request_id: next.request_id } : {}, { replace: true })
  }

  const resetFilters = () => {
    setDraft({ ...emptyFilters })
    setPage(1)
    setFilters({ ...emptyFilters })
    setSearchParams({}, { replace: true })
  }

  const copyRequestId = async (requestId: string) => {
    try {
      await navigator.clipboard.writeText(requestId)
      toast.success(t('Request ID copied'))
    } catch {
      toast.error(t('Cannot access the clipboard; select and copy the value manually'))
    }
  }

  const exportCsv = async () => {
    const next = validateDraft()
    if (!next) return
    setPage(1)
    setFilters(next)
    setSearchParams(next.request_id ? { request_id: next.request_id } : {}, { replace: true })
    setExporting(true)
    try {
      const { blob, filename } = await api.exportLogs(logQuery(next))
      const url = URL.createObjectURL(blob)
      const link = document.createElement('a')
      link.href = url
      link.download = filename
      document.body.appendChild(link)
      link.click()
      link.remove()
      URL.revokeObjectURL(url)
      toast.success(t('Exported {filename}', { filename }))
    } catch (err) {
      toast.error(err instanceof Error ? err.message : t('Failed to export request logs'))
    } finally {
      setExporting(false)
    }
  }

  const totalPages = data ? Math.max(1, Math.ceil(data.total / data.page_size)) : 1

  return (
    <div className="page">
      <PageHeader
        title={t('Request logs')}
        subtitle={t('Every relayed call with tokens, cost, latency and upstream routing. Export the filtered detail as CSV for finance reconciliation.')}
        actions={
          <button className="btn btn-ghost" type="button" onClick={() => void exportCsv()} disabled={exporting}>
            <Icon name="download" size={16} /> {exporting ? t('Exporting…') : t('Export CSV')}
          </button>
        }
      />
      {error ? <ErrorBanner message={error} /> : null}

      <section className="panel filter-panel">
        <form className="filters" onSubmit={applyFilters}>
          <input
            className="input"
            placeholder={t('Filter by model')}
            value={draft.model}
            onChange={(event) => setDraft({ ...draft, model: event.target.value })}
            aria-label={t('Filter by model')}
          />
          <input
            className="input"
            placeholder={t('Filter by request ID')}
            value={draft.request_id}
            onChange={(event) => setDraft({ ...draft, request_id: event.target.value })}
            aria-label={t('Filter by request ID')}
          />
          <select
            className="input"
            value={draft.status}
            onChange={(event) => setDraft({ ...draft, status: event.target.value })}
            aria-label={t('Filter by status')}
          >
            <option value="">{t('All statuses')}</option>
            <option value="success">{t('Success')}</option>
            <option value="error">{t('Error')}</option>
            <option value="interrupted">{t('Interrupted')}</option>
            <option value="cancelled">{t('Cancelled')}</option>
          </select>
          <select
            className="input"
            value={draft.token_id}
            onChange={(event) => setDraft({ ...draft, token_id: event.target.value })}
            aria-label={t('Filter by API key')}
          >
            <option value="">{t('All API keys')}</option>
            {tokens.map((token) => (
              <option value={token.id} key={token.id}>
                {token.name}
              </option>
            ))}
          </select>
          <label className="filter-date">
            <span>{t('Period from')}</span>
            <input
              className="input"
              type="datetime-local"
              value={draft.start}
              onChange={(event) => setDraft({ ...draft, start: event.target.value })}
            />
          </label>
          <label className="filter-date">
            <span>{t('Period to')}</span>
            <input
              className="input"
              type="datetime-local"
              value={draft.end}
              onChange={(event) => setDraft({ ...draft, end: event.target.value })}
            />
          </label>
          <button className="btn btn-primary btn-sm" type="submit">
            <Icon name="search" size={15} /> {t('Apply')}
          </button>
          <button className="btn btn-ghost btn-sm" type="button" onClick={resetFilters}>
            {t('Reset')}
          </button>
        </form>
      </section>

      {!loading && data ? (
        <section className="panel cost-summary" aria-label={t('Cost total for the selected period')}>
          <div className="cost-summary-main">
            <span className="cost-summary-label">{t('Total cost')}</span>
            <strong className="cost-summary-value">{formatCost(data.summary.cost)}</strong>
          </div>
          <div className="cost-summary-meta">
            <span>{t('{count} requests', { count: formatNumber(data.summary.requests) })}</span>
            <span>{describePeriod(filters)}</span>
            {data.summary.unverified_cost > 0 ? (
              <span className="cost-summary-unverified">
                {t('{cost} unverified', { cost: formatCost(data.summary.unverified_cost) })}
              </span>
            ) : null}
          </div>
        </section>
      ) : null}

      {!loading && data && data.summary.by_model.length > 0 ? (
        <section className="panel cost-by-model" aria-label={t('Cost by model for the selected period')}>
          <div className="panel-head">
            <div>
              <h2>{t('Cost by model')}</h2>
              <p>{t('Same filters and period as the total above.')}</p>
            </div>
            <span className="badge badge-muted">
              {data.summary.by_model.length === 1
                ? t('{count} model', { count: formatNumber(data.summary.by_model.length) })
                : t('{count} models', { count: formatNumber(data.summary.by_model.length) })}
            </span>
          </div>
          <div className="table-wrap">
            <table className="table cost-by-model-table">
              <thead>
                <tr>
                  <th>{t('Model')}</th>
                  <th className="num">{t('Requests')}</th>
                  <th className="num">{t('Share')}</th>
                  <th className="num">{t('Cost')}</th>
                </tr>
              </thead>
              <tbody>
                {data.summary.by_model.map((item) => (
                  <tr key={item.model || '(unknown)'}>
                    <td>
                      <div className="cell-title mono">{item.model || t('Unknown model')}</div>
                    </td>
                    <td className="num">{formatNumber(item.requests)}</td>
                    <td className="num muted">{percent(item.cost, data.summary.cost).toFixed(1)}%</td>
                    <td className="num">
                      <div className="cost-cell">
                        <span>{formatCost(item.cost)}</span>
                      </div>
                      {item.unverified_cost > 0 ? (
                        <div className="cell-sub cost-unverified">
                          {t('{cost} unverified', { cost: formatCost(item.unverified_cost) })}
                        </div>
                      ) : null}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>
      ) : null}

      {loading ? (
        <div className="page-loading">
          <Spinner size={22} /> {t('Loading request logs…')}
        </div>
      ) : !data || data.total === 0 ? (
        <section className="panel">
          <EmptyState
            icon="logs"
            title={t('No requests match these filters')}
            description={t('Traffic appears here as soon as a key calls the relay. Adjust the filters or send a test request.')}
          />
        </section>
      ) : (
        <section className="panel">
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th>{t('Time')}</th>
                  <th>{t('Model')}</th>
                  <th>{t('API key')}</th>
                  <th>{t('Channel')}</th>
                  <th>{t('Status')}</th>
                  <th className="num">{t('Tokens')}</th>
                  <th className="num">{t('Cost')}</th>
                  <th className="num">{t('Latency')}</th>
                </tr>
              </thead>
              <tbody>
                {data.items.map((log) => (
                  <tr
                    key={log.id}
                    className={`clickable${log.status === 'error' || log.status === 'interrupted' ? ' row-error' : ''}`}
                    onClick={() => setSelected(log)}
                    title={t('Open request detail')}
                  >
                    <td>
                      <div>{formatTime(log.created_at)}</div>
                      <div className="cell-sub mono">{log.request_id.slice(0, 10)}</div>
                    </td>
                    <td>
                      <div className="cell-title mono">{log.model}</div>
                      <div className="cell-sub mono">
                        {log.upstream_model && log.upstream_model !== log.model
                          ? `→ ${log.upstream_model}`
                          : log.stream
                            ? t('streamed')
                            : ''}
                      </div>
                    </td>
                    <td>{log.token_name || <span className="muted">{t('—')}</span>}</td>
                    <td>{log.channel_name || <span className="muted">{t('—')}</span>}</td>
                    <td><StatusBadge status={log.status} /></td>
                    <td className="num">
                      <div>{formatNumber(log.total_tokens)}</div>
                      <div className="cell-sub">
                        {t('{input} in · {output} out', {
                          input: formatNumber(log.prompt_tokens),
                          output: formatNumber(log.completion_tokens),
                        })}
                      </div>
                    </td>
                    <td className="num">
                      <div className="cost-cell">
                        <span>{formatCost(log.cost)}</span>
                        <CostStatusBadge status={log.cost_status} />
                      </div>
                    </td>
                    <td className="num muted">{formatDuration(log.latency_ms)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <div className="table-foot">
            <span className="muted">
              {t('Showing {shown} of {total} requests', {
                shown: data.items.length,
                total: formatNumber(data.total),
              })}
            </span>
            <div className="pager">
              <button
                className="icon-button"
                disabled={page <= 1}
                onClick={() => setPage((current) => Math.max(1, current - 1))}
                aria-label={t('Previous page')}
              >
                <Icon name="chevronLeft" size={16} />
              </button>
              <span>
                {t('Page {page} / {total}', { page, total: totalPages })}
              </span>
              <button
                className="icon-button"
                disabled={page >= totalPages}
                onClick={() => setPage((current) => Math.min(totalPages, current + 1))}
                aria-label={t('Next page')}
              >
                <Icon name="chevronRight" size={16} />
              </button>
            </div>
          </div>
        </section>
      )}

      <Modal
        open={selected !== null}
        onClose={() => setSelected(null)}
        title={t('Request detail')}
        subtitle={selected?.request_id}
        width={640}
        footer={
          <button className="btn btn-ghost" onClick={() => setSelected(null)}>
            {t('Close')}
          </button>
        }
      >
        {selected ? (
          <div className="detail-grid">
            <div>
              <span>{t('Time')}</span>
              <strong>{formatDateTime(selected.created_at)}</strong>
            </div>
            <div>
              <span>{t('Status')}</span>
              <strong className="detail-status">
                <StatusBadge status={selected.status} />
                <span className="muted">HTTP {selected.status_code}</span>
              </strong>
            </div>
            <div className="detail-wide">
              <span>{t('Request ID')}</span>
              <strong className="request-id-detail">
                <code>{selected.request_id}</code>
                <button className="link-button" type="button" onClick={() => void copyRequestId(selected.request_id)}>
                  {t('Copy')}
                </button>
              </strong>
            </div>
            <div>
              <span>{t('Model')}</span>
              <strong className="mono">{selected.model}</strong>
            </div>
            <div>
              <span>{t('Upstream model')}</span>
              <strong className="mono">{selected.upstream_model || t('—')}</strong>
            </div>
            <div>
              <span>{t('Channel')}</span>
              <strong>{selected.channel_name || t('—')}</strong>
            </div>
            <div>
              <span>{t('API key')}</span>
              <strong>{selected.token_name || t('—')}</strong>
            </div>
            <div>
              <span>{t('Prompt tokens')}</span>
              <strong>{formatNumber(selected.prompt_tokens)}</strong>
            </div>
            <div>
              <span>{t('Completion tokens')}</span>
              <strong>{formatNumber(selected.completion_tokens)}</strong>
            </div>
            <div>
              <span>{t('Total tokens')}</span>
              <strong>{formatNumber(selected.total_tokens)}</strong>
            </div>
            <div>
              <span>{t('Cost')}</span>
              <strong>{formatCost(selected.cost)}</strong>
            </div>
            <div>
              <span>{t('Cost basis')}</span>
              <strong className="detail-status">
                <CostStatusBadge status={selected.cost_status} />
              </strong>
            </div>
            <div>
              <span>{t('Reserved for this request')}</span>
              <strong>{formatCost(selected.reserved_cost)}</strong>
            </div>
            <div>
              <span>{t('Latency')}</span>
              <strong>{formatDuration(selected.latency_ms)}</strong>
            </div>
            <div>
              <span>{t('Streaming')}</span>
              <strong>{selected.stream ? t('Yes') : t('No')}</strong>
            </div>
            {selected.error ? (
              <div className="detail-wide">
                <span>{t('Error')}</span>
                <pre>{selected.error}</pre>
              </div>
            ) : null}
          </div>
        ) : null}
      </Modal>
    </div>
  )
}
