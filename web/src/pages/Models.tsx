import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { Icon } from '../components/Icon'
import { EmptyState, ErrorBanner, PageHeader, Spinner } from '../components/ui'
import { api } from '../lib/api'
import { channelTypeLabel, formatPrice } from '../lib/format'
import type { ModelInfo } from '../lib/types'

const FAVORITES_STORAGE_KEY = 'mr-favorite-models'

function parseFavorites(raw: string | null): Set<string> {
  if (!raw) return new Set()
  try {
    const value: unknown = JSON.parse(raw)
    if (!Array.isArray(value)) return new Set()
    return new Set(
      value.filter((item): item is string => typeof item === 'string' && item.trim() !== ''),
    )
  } catch {
    return new Set()
  }
}

function readFavorites(): Set<string> {
  try {
    return parseFavorites(window.localStorage.getItem(FAVORITES_STORAGE_KEY))
  } catch {
    return new Set()
  }
}

function writeFavorites(favorites: Set<string>) {
  try {
    window.localStorage.setItem(FAVORITES_STORAGE_KEY, JSON.stringify([...favorites]))
  } catch {
    // Storage can be unavailable in private or restricted browser contexts.
    // The in-memory selection still applies for the current session.
  }
}

function isMediaModel(model: ModelInfo): boolean {
  return model.types.includes('minimax') || model.types.includes('seedance') || model.types.includes('minimax-music')
}

export default function Models() {
  const [models, setModels] = useState<ModelInfo[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [search, setSearch] = useState('')
  const [favorites, setFavorites] = useState<Set<string>>(readFavorites)
  const [favoritesOnly, setFavoritesOnly] = useState(false)

  useEffect(() => {
    writeFavorites(favorites)
  }, [favorites])

  useEffect(() => {
    const onStorage = (event: StorageEvent) => {
      if (event.key === FAVORITES_STORAGE_KEY) {
        setFavorites(parseFavorites(event.newValue))
      }
    }
    window.addEventListener('storage', onStorage)
    return () => window.removeEventListener('storage', onStorage)
  }, [])

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

  const toggleFavorite = (id: string) => {
    setFavorites((current) => {
      const next = new Set(current)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  const showAllModels = () => setFavoritesOnly(false)

  const query = search.trim().toLowerCase()
  const favoriteCount = models.filter((model) => favorites.has(model.id)).length
  const filtered = models.filter((model) => {
    if (favoritesOnly && !favorites.has(model.id)) return false
    return (
      !query ||
      model.id.toLowerCase().includes(query) ||
      model.channels.some((channel) => channel.toLowerCase().includes(query))
    )
  })

  return (
    <div className="page">
      <PageHeader
        title="Models"
        subtitle="Public model identifiers exposed by your enabled channels."
        actions={
          <>
            <button
              type="button"
              className="btn btn-ghost favorites-filter"
              aria-pressed={favoritesOnly}
              title={favoritesOnly ? 'Show all models' : 'Show favorite models only'}
              onClick={() => setFavoritesOnly((value) => !value)}
            >
              <Icon name="star" size={15} />
              Favorites only
              {favoriteCount > 0 ? (
                <span className="favorites-filter-count">{favoriteCount}</span>
              ) : null}
            </button>
            <div className="search-box">
              <Icon name="search" size={16} />
              <input
                className="search-input"
                placeholder="Search models or channels"
                value={search}
                onChange={(event) => setSearch(event.target.value)}
              />
            </div>
          </>
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
                {favoritesOnly ? ' · favorites only' : ''}
                {query ? ` matching “${search.trim()}”` : ''}
              </p>
            </div>
            <span className="badge badge-muted">{models.length} total</span>
          </div>
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th className="favorite-col">
                    <span className="sr-only">Favorite</span>
                  </th>
                  <th>Model</th>
                  <th>Protocols</th>
                  <th>Channels</th>
                  <th className="num">Input / 1M</th>
                  <th className="num">Output / 1M</th>
                </tr>
              </thead>
              <tbody>
                {filtered.map((model) => {
                  const favorite = favorites.has(model.id)
                  return (
                    <tr key={model.id}>
                      <td className="favorite-col">
                        <button
                          type="button"
                          className={`favorite-button${favorite ? ' is-favorite' : ''}`}
                          aria-pressed={favorite}
                          aria-label={
                            favorite
                              ? `Remove ${model.id} from favorites`
                              : `Add ${model.id} to favorites`
                          }
                          title={favorite ? 'Remove from favorites' : 'Add to favorites'}
                          onClick={() => toggleFavorite(model.id)}
                        >
                          <Icon name="star" size={16} />
                        </button>
                      </td>
                      <td>
                        <div className="cell-title mono">{model.id}</div>
                        <div className="cell-sub">Served by {model.channels.length} channel{model.channels.length === 1 ? '' : 's'}</div>
                      </td>
                      <td>
                        <div className="chip-row">
                          {model.types.map((type) => (
                            <span className={`type-badge type-${type}`} key={type}>
                              {channelTypeLabel(type)}
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
                      <td className="num">
                        {isMediaModel(model) ? <span className="muted">Not metered</span> : formatPrice(model.input_price)}
                      </td>
                      <td className="num">
                        {isMediaModel(model) ? <span className="muted">Not metered</span> : formatPrice(model.output_price)}
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
          {filtered.length === 0 ? (
            favoritesOnly && favoriteCount === 0 ? (
              <EmptyState
                icon="star"
                title="No favorite models yet"
                description="Star the models you use most, then use Favorites only to filter the catalog."
                action={
                  <button type="button" className="btn btn-ghost btn-sm" onClick={showAllModels}>
                    Show all models
                  </button>
                }
              />
            ) : favoritesOnly ? (
              <EmptyState
                icon="search"
                title="No matching favorite models"
                description="Try a different model or channel name, or show all models."
                action={
                  <button type="button" className="btn btn-ghost btn-sm" onClick={showAllModels}>
                    Show all models
                  </button>
                }
              />
            ) : (
              <EmptyState
                icon="search"
                title="No matching models"
                description="Try a different model or channel name."
              />
            )
          ) : null}
        </section>
      )}
    </div>
  )
}
