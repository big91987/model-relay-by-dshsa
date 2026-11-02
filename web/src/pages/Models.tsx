import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { Icon } from '../components/Icon'
import { EmptyState, ErrorBanner, PageHeader, Spinner } from '../components/ui'
import { api } from '../lib/api'
import { formatPrice } from '../lib/format'
import type { ModelInfo } from '../lib/types'

export default function Models() {
  const [models, setModels] = useState<ModelInfo[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [search, setSearch] = useState('')

  useEffect(() => {
    let active = true
    api
      .models()
      .then((response) => {
        if (active) setModels(response.data)
      })
      .catch((err: unknown) => {
        if (active) setError(err instanceof Error ? err.message : 'Failed to load models')
      })
      .finally(() => {
        if (active) setLoading(false)
      })
    return () => {
      active = false
    }
  }, [])

  const query = search.trim().toLowerCase()
  const filtered = models.filter(
    (model) =>
      !query ||
      model.id.toLowerCase().includes(query) ||
      model.channels.some((channel) => channel.toLowerCase().includes(query)),
  )

  return (
    <div className="page">
      <PageHeader
        title="Models"
        subtitle="Public model identifiers exposed by your enabled channels."
        actions={
          <div className="search-box">
            <Icon name="search" size={16} />
            <input
              className="search-input"
              placeholder="Search models or channels"
              value={search}
              onChange={(event) => setSearch(event.target.value)}
            />
          </div>
        }
      />
      {error ? <ErrorBanner message={error} /> : null}

      {loading ? (
        <div className="page-loading">
          <Spinner size={22} /> Loading models…
        </div>
      ) : models.length === 0 ? (
        <section className="panel">
          <EmptyState
            icon="models"
            title="No models available"
            description="Add an enabled channel with at least one model to expose it through /v1/models."
            action={
              <Link className="btn btn-primary" to="/channels">
                <Icon name="plus" size={16} /> Add a channel
              </Link>
            }
          />
        </section>
      ) : (
        <section className="panel">
          <div className="panel-head">
            <div>
              <h2>Model catalog</h2>
              <p>
                {filtered.length} of {models.length} models
                {query ? ` matching “${search.trim()}”` : ''}
              </p>
            </div>
            <span className="badge badge-muted">{models.length} total</span>
          </div>
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th>Model</th>
                  <th>Protocols</th>
                  <th>Channels</th>
                  <th className="num">Input / 1M</th>
                  <th className="num">Output / 1M</th>
                </tr>
              </thead>
              <tbody>
                {filtered.map((model) => (
                  <tr key={model.id}>
                    <td>
                      <div className="cell-title mono">{model.id}</div>
                      <div className="cell-sub">Served by {model.channels.length} channel{model.channels.length === 1 ? '' : 's'}</div>
                    </td>
                    <td>
                      <div className="chip-row">
                        {model.types.map((type) => (
                          <span className={`type-badge type-${type}`} key={type}>
                            {type === 'anthropic' ? 'Anthropic' : 'OpenAI'}
                          </span>
                        ))}
                      </div>
                    </td>
                    <td>
                      <div className="chip-row">
                        {model.channels.map((channel) => (
                          <span className="chip" key={channel}>
                            {channel}
                          </span>
                        ))}
                      </div>
                    </td>
                    <td className="num">{formatPrice(model.input_price)}</td>
                    <td className="num">{formatPrice(model.output_price)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {filtered.length === 0 ? (
            <EmptyState icon="search" title="No matching models" description="Try a different model or channel name." />
          ) : null}
        </section>
      )}
    </div>
  )
}
