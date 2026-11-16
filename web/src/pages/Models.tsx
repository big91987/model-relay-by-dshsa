import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { Icon } from '../components/Icon'
import { EmptyState, ErrorBanner, PageHeader, Spinner } from '../components/ui'
import { api } from '../lib/api'
import { channelTypeLabel, formatPrice } from '../lib/format'
import type { ModelInfo } from '../lib/types'
import { t } from '../lib/i18n'

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
        if (active) setError(err instanceof Error ? err.message : t("Failed to load models"))
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
        title={t("Models")}
        subtitle={t("Public model identifiers exposed by your enabled channels.")}
        actions={
          <>
            <button
              type="button"
              className="btn btn-ghost favorites-filter"
              aria-pressed={favoritesOnly}
              title={favoritesOnly ? t("Show all models") : t("Show favorite models only")}
              onClick={() => setFavoritesOnly((value) => !value)}
            >
              <Icon name="star" size={15} />
              {t("Favorites only")}
              {favoriteCount > 0 ? (
                <span className="favorites-filter-count">{favoriteCount}</span>
              ) : null}
            </button>
            <div className="search-box">
              <Icon name="search" size={16} />
              <input
                className="search-input"
                placeholder={t("Search models or channels")}
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
          <Spinner size={22} /> {t("Loading models…")}
        </div>
      ) : models.length === 0 ? (
        <section className="panel">
          <EmptyState
            icon="models"
            title={t("No models available")}
            description={t("Add an enabled channel with at least one model to expose it through /v1/models.")}
            action={
              <Link className="btn btn-primary" to="/channels">
                <Icon name="plus" size={16} /> {t("Add a channel")}
              </Link>
            }
          />
        </section>
      ) : (
        <section className="panel">
          <div className="panel-head">
            <div>
              <h2>{t("Model catalog")}</h2>
              <p>
                {t(
                  favoritesOnly
                    ? query
                      ? '{shown} of {total} models · favorites only matching “{query}”'
                      : '{shown} of {total} models · favorites only'
                    : query
                      ? '{shown} of {total} models matching “{query}”'
                      : '{shown} of {total} models',
                  { shown: filtered.length, total: models.length, query: search.trim() },
                )}
              </p>
            </div>
            <span className="badge badge-muted">{t('{count} total', { count: models.length })}</span>
          </div>
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th className="favorite-col">
                    <span className="sr-only">{t("Favorite")}</span>
                  </th>
                  <th>{t("Model")}</th>
                  <th>{t("Protocols")}</th>
                  <th>{t("Channels")}</th>
                  <th className="num">{t("Input / 1M")}</th>
                  <th className="num">{t("Output / 1M")}</th>
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
                              ? t('Remove {model} from favorites', { model: model.id })
                              : t('Add {model} to favorites', { model: model.id })
                          }
                          title={favorite ? t("Remove from favorites") : t("Add to favorites")}
                          onClick={() => toggleFavorite(model.id)}
                        >
                          <Icon name="star" size={16} />
                        </button>
                      </td>
                      <td>
                        <div className="cell-title mono">{model.id}</div>
                        <div className="cell-sub">
                          {model.channels.length === 1
                            ? t('Served by {count} channel', { count: model.channels.length })
                            : t('Served by {count} channels', { count: model.channels.length })}
                        </div>
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
                        {isMediaModel(model) ? (
                          <span className="muted">{t("Not metered")}</span>
                        ) : model.price_modes?.includes('unset') ? (
                          <span className="badge badge-danger">{t("Unpriced")}</span>
                        ) : model.price_modes?.length > 0 && model.price_modes.every((mode) => mode === 'free') ? (
                          <span className="badge badge-success">{t("Free")}</span>
                        ) : (
                          formatPrice(model.input_price)
                        )}
                      </td>
                      <td className="num">
                        {isMediaModel(model) ? (
                          <span className="muted">{t("Not metered")}</span>
                        ) : model.price_modes?.includes('unset') ? (
                          <span className="badge badge-danger">{t("Unpriced")}</span>
                        ) : model.price_modes?.length > 0 && model.price_modes.every((mode) => mode === 'free') ? (
                          <span className="badge badge-success">{t("Free")}</span>
                        ) : (
                          formatPrice(model.output_price)
                        )}
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
                title={t("No favorite models yet")}
                description={t("Star the models you use most, then use Favorites only to filter the catalog.")}
                action={
                  <button type="button" className="btn btn-ghost btn-sm" onClick={showAllModels}>
                    {t("Show all models")}
                  </button>
                }
              />
            ) : favoritesOnly ? (
              <EmptyState
                icon="search"
                title={t("No matching favorite models")}
                description={t("Try a different model or channel name, or show all models.")}
                action={
                  <button type="button" className="btn btn-ghost btn-sm" onClick={showAllModels}>
                    {t("Show all models")}
                  </button>
                }
              />
            ) : (
              <EmptyState
                icon="search"
                title={t("No matching models")}
                description={t("Try a different model or channel name.")}
              />
            )
          ) : null}
        </section>
      )}
    </div>
  )
}
