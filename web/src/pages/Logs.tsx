import { useCallback, useEffect, useState } from 'react'
import type { FormEvent } from 'react'
import { useSearchParams } from 'react-router-dom'
import { Icon } from '../components/Icon'
import { Modal } from '../components/Modal'
import { CostStatusBadge, EmptyState, ErrorBanner, PageHeader, Spinner, StatusBadge } from '../components/ui'
import { api } from '../lib/api'
import { formatCost, formatDateTime, formatDuration, formatNumber, formatTime } from '../lib/format'
import { useToast } from '../lib/toast'
import type { LogItem, LogsResponse, Token } from '../lib/types'

interface Filters {
  model: string
  status: string
  request_id: string
  token_id: string
}

const emptyFilters: Filters = { model: '', status: '', request_id: '', token_id: '' }
const pageSize = 25

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

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const response = await api.logs({
        page,
        page_size: pageSize,
        model: filters.model.trim() || undefined,
        status: filters.status || undefined,
        request_id: filters.request_id.trim() || undefined,
        token_id: filters.token_id ? Number(filters.token_id) : undefined,
      })
      setData(response)
      setError('')
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load request logs')
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

  const applyFilters = (event: FormEvent) => {
    event.preventDefault()
    const next = { ...draft, request_id: draft.request_id.trim() }
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
      toast.success('Request ID copied')
    } catch {
      toast.error('Cannot access the clipboard; select and copy the value manually')
    }
  }

  const totalPages = data ? Math.max(1, Math.ceil(data.total / data.page_size)) : 1

  return (
    <div className="page">
      <PageHeader title="Request logs" subtitle="Every relayed call with tokens, cost, latency and upstream routing." />
      {error ? <ErrorBanner message={error} /> : null}

      <section className="panel filter-panel">
        <form className="filters" onSubmit={applyFilters}>
          <input
            className="input"
            placeholder="Filter by model"
            value={draft.model}
            onChange={(event) => setDraft({ ...draft, model: event.target.value })}
            aria-label="Filter by model"
          />
          <input
            className="input"
            placeholder="Filter by request ID"
            value={draft.request_id}
            onChange={(event) => setDraft({ ...draft, request_id: event.target.value })}
            aria-label="Filter by request ID"
          />
          <select
            className="input"
            value={draft.status}
            onChange={(event) => setDraft({ ...draft, status: event.target.value })}
            aria-label="Filter by status"
          >
            <option value="">All statuses</option>
            <option value="success">Success</option>
            <option value="error">Error</option>
          </select>
          <select
            className="input"
            value={draft.token_id}
            onChange={(event) => setDraft({ ...draft, token_id: event.target.value })}
            aria-label="Filter by API key"
          >
            <option value="">All API keys</option>
            {tokens.map((token) => (
              <option value={token.id} key={token.id}>
                {token.name}
              </option>
            ))}
          </select>
          <button className="btn btn-primary btn-sm" type="submit">
            <Icon name="search" size={15} /> Apply
          </button>
          <button className="btn btn-ghost btn-sm" type="button" onClick={resetFilters}>
            Reset
          </button>
        </form>
      </section>

      {loading ? (
        <div className="page-loading">
          <Spinner size={22} /> Loading request logs…
        </div>
      ) : !data || data.total === 0 ? (
        <section className="panel">
          <EmptyState
            icon="logs"
            title="No requests match these filters"
            description="Traffic appears here as soon as a key calls the relay. Adjust the filters or send a test request."
          />
        </section>
      ) : (
        <section className="panel">
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th>Time</th>
                  <th>Model</th>
                  <th>API key</th>
                  <th>Channel</th>
                  <th>Status</th>
                  <th className="num">Tokens</th>
                  <th className="num">Cost</th>
                  <th className="num">Latency</th>
                </tr>
              </thead>
              <tbody>
                {data.items.map((log) => (
                  <tr
                    key={log.id}
                    className={`clickable${log.status === 'error' ? ' row-error' : ''}`}
                    onClick={() => setSelected(log)}
                    title="Open request detail"
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
                            ? 'streamed'
                            : ''}
                      </div>
                    </td>
                    <td>{log.token_name || <span className="muted">—</span>}</td>
                    <td>{log.channel_name || <span className="muted">—</span>}</td>
                    <td><StatusBadge status={log.status} /></td>
                    <td className="num">
                      <div>{formatNumber(log.total_tokens)}</div>
                      <div className="cell-sub">
                        {formatNumber(log.prompt_tokens)} in · {formatNumber(log.completion_tokens)} out
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
              Showing {data.items.length} of {formatNumber(data.total)} requests
            </span>
            <div className="pager">
              <button
                className="icon-button"
                disabled={page <= 1}
                onClick={() => setPage((current) => Math.max(1, current - 1))}
                aria-label="Previous page"
              >
                <Icon name="chevronLeft" size={16} />
              </button>
              <span>
                Page {page} / {totalPages}
              </span>
              <button
                className="icon-button"
                disabled={page >= totalPages}
                onClick={() => setPage((current) => Math.min(totalPages, current + 1))}
                aria-label="Next page"
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
        title="Request detail"
        subtitle={selected?.request_id}
        width={640}
        footer={
          <button className="btn btn-ghost" onClick={() => setSelected(null)}>
            Close
          </button>
        }
      >
        {selected ? (
          <div className="detail-grid">
            <div>
              <span>Time</span>
              <strong>{formatDateTime(selected.created_at)}</strong>
            </div>
            <div>
              <span>Status</span>
              <strong className="detail-status">
                <StatusBadge status={selected.status} />
                <span className="muted">HTTP {selected.status_code}</span>
              </strong>
            </div>
            <div className="detail-wide">
              <span>Request ID</span>
              <strong className="request-id-detail">
                <code>{selected.request_id}</code>
                <button className="link-button" type="button" onClick={() => void copyRequestId(selected.request_id)}>
                  Copy
                </button>
              </strong>
            </div>
            <div>
              <span>Model</span>
              <strong className="mono">{selected.model}</strong>
            </div>
            <div>
              <span>Upstream model</span>
              <strong className="mono">{selected.upstream_model || '—'}</strong>
            </div>
            <div>
              <span>Channel</span>
              <strong>{selected.channel_name || '—'}</strong>
            </div>
            <div>
              <span>API key</span>
              <strong>{selected.token_name || '—'}</strong>
            </div>
            <div>
              <span>Prompt tokens</span>
              <strong>{formatNumber(selected.prompt_tokens)}</strong>
            </div>
            <div>
              <span>Completion tokens</span>
              <strong>{formatNumber(selected.completion_tokens)}</strong>
            </div>
            <div>
              <span>Total tokens</span>
              <strong>{formatNumber(selected.total_tokens)}</strong>
            </div>
            <div>
              <span>Cost</span>
              <strong>{formatCost(selected.cost)}</strong>
            </div>
            <div>
              <span>Cost basis</span>
              <strong className="detail-status">
                <CostStatusBadge status={selected.cost_status} />
              </strong>
            </div>
            <div>
              <span>Reserved for this request</span>
              <strong>{formatCost(selected.reserved_cost)}</strong>
            </div>
            <div>
              <span>Latency</span>
              <strong>{formatDuration(selected.latency_ms)}</strong>
            </div>
            <div>
              <span>Streaming</span>
              <strong>{selected.stream ? 'Yes' : 'No'}</strong>
            </div>
            {selected.error ? (
              <div className="detail-wide">
                <span>Error</span>
                <pre>{selected.error}</pre>
              </div>
            ) : null}
          </div>
        ) : null}
      </Modal>
    </div>
  )
}
