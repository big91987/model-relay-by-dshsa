import { useCallback, useEffect, useRef, useState } from 'react'
import { Icon } from '../components/Icon'
import { Modal } from '../components/Modal'
import { EmptyState, ErrorBanner, FormField, PageHeader, Spinner, StatusBadge } from '../components/ui'
import { api } from '../lib/api'
import { channelTypeLabel } from '../lib/format'
import { useToast } from '../lib/toast'
import type { Channel, ChannelInput, ChannelStatus, ChannelType, UpstreamModelsInput } from '../lib/types'

interface ChannelFormState {
  name: string
  type: ChannelType
  base_url: string
  api_key: string
  models: string
  mapping: string
  priority: string
  weight: string
  status: ChannelStatus
  input_price: string
  output_price: string
}

const emptyForm: ChannelFormState = {
  name: '',
  type: 'openai',
  base_url: '',
  api_key: '',
  models: '',
  mapping: '',
  priority: '0',
  weight: '1',
  status: 'enabled',
  input_price: '0',
  output_price: '0',
}

function isMediaProtocol(type: ChannelType): boolean {
  return type === 'minimax' || type === 'seedance' || type === 'minimax-music'
}

function toForm(channel: Channel): ChannelFormState {
  return {
    name: channel.name,
    type: channel.type,
    base_url: channel.base_url,
    api_key: '',
    models: channel.models.join(', '),
    mapping: Object.entries(channel.model_mapping)
      .map(([key, value]) => `${key}=${value}`)
      .join('\n'),
    priority: String(channel.priority),
    weight: String(channel.weight),
    status: channel.status,
    input_price: String(channel.input_price),
    output_price: String(channel.output_price),
  }
}

function parseModels(value: string): string[] {
  return value
    .split(/[\n,]/)
    .map((item) => item.trim())
    .filter(Boolean)
}

function parseMapping(value: string): Record<string, string> {
  const result: Record<string, string> = {}
  for (const line of value.split('\n')) {
    const trimmed = line.trim()
    if (!trimmed) continue
    const index = trimmed.indexOf('=')
    if (index <= 0) continue
    const key = trimmed.slice(0, index).trim()
    const upstream = trimmed.slice(index + 1).trim()
    if (key && upstream) result[key] = upstream
  }
  return result
}

function toPayload(form: ChannelFormState): ChannelInput {
  const payload: ChannelInput = {
    name: form.name.trim(),
    type: form.type,
    base_url: form.base_url.trim(),
    models: parseModels(form.models),
    model_mapping: parseMapping(form.mapping),
    priority: Number.parseInt(form.priority, 10) || 0,
    weight: Math.max(1, Number.parseInt(form.weight, 10) || 1),
    status: form.status,
    input_price: Number.parseFloat(form.input_price) || 0,
    output_price: Number.parseFloat(form.output_price) || 0,
  }
  if (form.api_key.trim()) payload.api_key = form.api_key.trim()
  return payload
}

export default function Channels() {
  const toast = useToast()
  const [channels, setChannels] = useState<Channel[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [modalOpen, setModalOpen] = useState(false)
  const [editing, setEditing] = useState<Channel | null>(null)
  const [form, setForm] = useState<ChannelFormState>(emptyForm)
  const [saving, setSaving] = useState(false)
  const [testing, setTesting] = useState<number | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<Channel | null>(null)
  const [deleting, setDeleting] = useState(false)
  const [fetchingModels, setFetchingModels] = useState(false)
  const [upstreamModels, setUpstreamModels] = useState<string[] | null>(null)
  const [selectedModels, setSelectedModels] = useState<string[]>([])
  const [modelsError, setModelsError] = useState('')
  const modelFetchRequest = useRef(0)

  const load = useCallback(async () => {
    try {
      const response = await api.channels()
      setChannels(response.data)
      setError('')
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load channels')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  const setField = <K extends keyof ChannelFormState>(key: K, value: ChannelFormState[K]) => {
    setForm((previous) => ({ ...previous, [key]: value }))
    if (key === 'type' || key === 'base_url' || key === 'api_key') {
      // A pending or shown model list belongs to the previous provider config.
      modelFetchRequest.current += 1
      setFetchingModels(false)
      setUpstreamModels(null)
      setSelectedModels([])
      setModelsError('')
    }
  }

  const resetModelFetch = () => {
    modelFetchRequest.current += 1
    setFetchingModels(false)
    setUpstreamModels(null)
    setSelectedModels([])
    setModelsError('')
  }

  const openCreate = () => {
    setEditing(null)
    setForm(emptyForm)
    resetModelFetch()
    setModalOpen(true)
  }

  const openEdit = (channel: Channel) => {
    setEditing(channel)
    setForm(toForm(channel))
    resetModelFetch()
    setModalOpen(true)
  }

  const currentModels = parseModels(form.models)
  const importableModels = upstreamModels
    ? upstreamModels.filter((model) => !currentModels.includes(model))
    : []
  const allImportableSelected =
    importableModels.length > 0 && importableModels.every((model) => selectedModels.includes(model))
  const canFetchModels = form.base_url.trim() !== '' && (editing !== null || form.api_key.trim() !== '')

  const fetchUpstreamModels = async () => {
    if (!canFetchModels) {
      toast.error('Enter the base URL and API key before fetching models')
      return
    }
    const requestID = modelFetchRequest.current + 1
    modelFetchRequest.current = requestID
    setFetchingModels(true)
    setModelsError('')
    const payload: UpstreamModelsInput = {
      type: form.type,
      base_url: form.base_url.trim(),
    }
    if (form.api_key.trim()) payload.api_key = form.api_key.trim()
    if (editing) payload.channel_id = editing.id
    try {
      const response = await api.fetchUpstreamModels(payload)
      if (modelFetchRequest.current !== requestID) return
      setUpstreamModels(response.models)
      setSelectedModels([])
    } catch (err) {
      if (modelFetchRequest.current !== requestID) return
      setUpstreamModels(null)
      setSelectedModels([])
      setModelsError(err instanceof Error ? err.message : 'Failed to fetch models from the upstream provider')
    } finally {
      if (modelFetchRequest.current === requestID) setFetchingModels(false)
    }
  }

  const toggleModel = (model: string) => {
    setSelectedModels((previous) =>
      previous.includes(model) ? previous.filter((item) => item !== model) : [...previous, model],
    )
  }

  const toggleAllModels = () => {
    setSelectedModels(allImportableSelected ? [] : [...importableModels])
  }

  const importSelectedModels = () => {
    const additions = selectedModels.filter((model) => !currentModels.includes(model))
    if (additions.length === 0) {
      toast.error('The selected models are already in the list')
      return
    }
    const trimmed = form.models.trim()
    const separator = !trimmed ? '' : /[\n,]\s*$/.test(form.models) ? ' ' : ', '
    setField('models', trimmed + separator + additions.join(', '))
    setSelectedModels([])
    toast.success(`Imported ${additions.length} model${additions.length === 1 ? '' : 's'}`)
  }

  const submit = async () => {
    const payload = toPayload(form)
    if (!payload.name) {
      toast.error('Channel name is required')
      return
    }
    if (!payload.base_url) {
      toast.error('Base URL is required')
      return
    }
    if (!editing && !payload.api_key) {
      toast.error('API key is required for a new channel')
      return
    }
    if (payload.models.length === 0 && Object.keys(payload.model_mapping).length === 0) {
      toast.error('Add at least one model or a model mapping')
      return
    }
    setSaving(true)
    try {
      if (editing) {
        await api.updateChannel(editing.id, payload)
        toast.success(`Channel “${payload.name}” updated`)
      } else {
        await api.createChannel(payload)
        toast.success(`Channel “${payload.name}” created`)
      }
      setModalOpen(false)
      await load()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'Failed to save channel')
    } finally {
      setSaving(false)
    }
  }

  const toggleStatus = async (channel: Channel) => {
    const nextStatus: ChannelStatus = channel.status === 'enabled' ? 'disabled' : 'enabled'
    try {
      await api.updateChannel(channel.id, toPayload(toForm({ ...channel, status: nextStatus })))
      toast.success(`Channel ${nextStatus}`)
      await load()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'Failed to update channel')
    }
  }

  const testConnection = async (channel: Channel) => {
    setTesting(channel.id)
    try {
      const result = await api.testChannel(channel.id)
      if (result.ok) {
        toast.success(`Connected to ${channel.name} in ${result.latency_ms} ms`)
      } else {
        toast.error(
          `${channel.name} failed${result.status_code ? ` (HTTP ${result.status_code})` : ''}: ${result.message}`,
        )
      }
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'Connection test failed')
    } finally {
      setTesting(null)
    }
  }

  const confirmDelete = async () => {
    if (!deleteTarget) return
    setDeleting(true)
    try {
      await api.deleteChannel(deleteTarget.id)
      toast.success(`Channel “${deleteTarget.name}” deleted`)
      setDeleteTarget(null)
      await load()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'Failed to delete channel')
    } finally {
      setDeleting(false)
    }
  }

  return (
    <div className="page">
      <PageHeader
        title="Channels"
        subtitle="Upstream providers that power your relay. Higher priority wins; equal priorities load-balance by weight."
        actions={
          <button className="btn btn-primary" onClick={openCreate}>
            <Icon name="plus" size={16} /> Add channel
          </button>
        }
      />
      {error ? <ErrorBanner message={error} /> : null}

      {loading ? (
        <div className="page-loading">
          <Spinner size={22} /> Loading channels…
        </div>
      ) : channels.length === 0 ? (
        <section className="panel">
          <EmptyState
            icon="channels"
            title="No upstream channels"
            description="Add an OpenAI-compatible, Anthropic, MiniMax or Seedance video provider to start serving models."
            action={
              <button className="btn btn-primary" onClick={openCreate}>
                <Icon name="plus" size={16} /> Add your first channel
              </button>
            }
          />
        </section>
      ) : (
        <section className="panel">
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th>Channel</th>
                  <th>Type</th>
                  <th>Models</th>
                  <th>Routing</th>
                  <th>Price / 1M</th>
                  <th>Status</th>
                  <th className="right">Actions</th>
                </tr>
              </thead>
              <tbody>
                {channels.map((channel) => {
                  const publicModels = [...channel.models, ...Object.keys(channel.model_mapping)]
                  const uniqueModels = [...new Set(publicModels)]
                  return (
                    <tr key={channel.id}>
                      <td>
                        <div className="cell-title">{channel.name}</div>
                        <div className="cell-sub mono">{channel.base_url}</div>
                        <div className="cell-sub">Key {channel.api_key_hint}</div>
                      </td>
                      <td>
                        <span className={`type-badge type-${channel.type}`}>
                          {channelTypeLabel(channel.type)}
                        </span>
                      </td>
                      <td>
                        <div className="chip-row">
                          {uniqueModels.slice(0, 3).map((model) => (
                            <span className="chip" key={model} title={model}>
                              {model}
                            </span>
                          ))}
                          {uniqueModels.length > 3 ? (
                            <span className="chip chip-more">+{uniqueModels.length - 3}</span>
                          ) : null}
                        </div>
                      </td>
                      <td className="muted">
                        P{channel.priority} · W{channel.weight}
                      </td>
                      <td>
                        {isMediaProtocol(channel.type) ? (
                          <span className="muted">Not metered</span>
                        ) : (
                          <div className="price-cell">
                            <span>${channel.input_price.toFixed(2)}</span>
                            <span>${channel.output_price.toFixed(2)}</span>
                          </div>
                        )}
                      </td>
                      <td>
                        <div className="status-toggle">
                          <button
                            className={`switch${channel.status === 'enabled' ? ' on' : ''}`}
                            role="switch"
                            aria-checked={channel.status === 'enabled'}
                            aria-label={`Toggle ${channel.name}`}
                            onClick={() => void toggleStatus(channel)}
                          >
                            <span />
                          </button>
                          <StatusBadge status={channel.status} />
                        </div>
                      </td>
                      <td className="right">
                        <div className="row-actions">
                          <button
                            className="icon-button"
                            title="Test connection"
                            onClick={() => void testConnection(channel)}
                            disabled={testing === channel.id}
                          >
                            {testing === channel.id ? <Spinner size={15} /> : <Icon name="test" size={16} />}
                          </button>
                          <button className="icon-button" title="Edit channel" onClick={() => openEdit(channel)}>
                            <Icon name="edit" size={16} />
                          </button>
                          <button
                            className="icon-button danger"
                            title="Delete channel"
                            onClick={() => setDeleteTarget(channel)}
                          >
                            <Icon name="trash" size={16} />
                          </button>
                        </div>
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        </section>
      )}

      <Modal
        open={modalOpen}
        onClose={() => setModalOpen(false)}
        title={editing ? 'Edit channel' : 'Add channel'}
        subtitle="Providers are called with their native protocol; the relay exposes a single OpenAI-compatible API."
        width={700}
        footer={
          <>
            <button className="btn btn-ghost" onClick={() => setModalOpen(false)} disabled={saving}>
              Cancel
            </button>
            <button className="btn btn-primary" onClick={() => void submit()} disabled={saving}>
              {saving ? <Spinner size={15} /> : null}
              {editing ? 'Save changes' : 'Create channel'}
            </button>
          </>
        }
      >
        <div className="form-grid">
          <FormField label="Name" htmlFor="channel-name">
            <input
              id="channel-name"
              className="input"
              value={form.name}
              onChange={(event) => setField('name', event.target.value)}
              placeholder="OpenAI production"
            />
          </FormField>
          <FormField label="Protocol">
            <select
              className="input"
              value={form.type}
              onChange={(event) => setField('type', event.target.value as ChannelType)}
            >
              <option value="openai">OpenAI-compatible</option>
              <option value="anthropic">Anthropic Messages</option>
              <option value="minimax">MiniMax H3 (video)</option>
              <option value="seedance">Seedance (Volcano Ark, video)</option>
              <option value="minimax-music">MiniMax Music (audio)</option>
            </select>
          </FormField>
          <FormField
            label="Base URL"
            htmlFor="channel-url"
            wide
            hint={
              form.type === 'minimax'
                ? 'MiniMax API root, e.g. https://api.minimax.io or https://api.minimaxi.com'
                : form.type === 'minimax-music'
                  ? 'MiniMax API root, e.g. https://api.minimax.io or https://api.minimaxi.com'
                  : form.type === 'seedance'
                    ? 'Volcano Ark API root including the version path, e.g. https://ark.cn-beijing.volces.com/api/v3'
                    : 'Include the API version path, e.g. https://api.openai.com/v1'
            }
          >
            <input
              id="channel-url"
              className="input mono"
              value={form.base_url}
              onChange={(event) => setField('base_url', event.target.value)}
              placeholder={
                form.type === 'minimax'
                  ? 'https://api.minimax.io'
                  : form.type === 'minimax-music'
                    ? 'https://api.minimax.io'
                    : form.type === 'seedance'
                      ? 'https://ark.cn-beijing.volces.com/api/v3'
                      : 'https://api.openai.com/v1'
              }
            />
          </FormField>
          <FormField
            label="API key"
            htmlFor="channel-key"
            wide
            hint={editing ? `Leave blank to keep the current key (${editing.api_key_hint})` : 'Encrypted at rest with SECRET_KEY'}
          >
            <input
              id="channel-key"
              className="input mono"
              type="password"
              value={form.api_key}
              onChange={(event) => setField('api_key', event.target.value)}
              placeholder={editing ? '••••••••' : 'sk-…'}
              autoComplete="new-password"
            />
          </FormField>
          <FormField
            label="Models"
            htmlFor="channel-models"
            hint={
              form.type === 'minimax'
                ? 'MiniMax models, e.g. MiniMax-H3. Fetch the live list and keep your manual entries.'
                : form.type === 'minimax-music'
                  ? 'MiniMax Music models, e.g. music-3.0. Fetch the live list and keep your manual entries.'
                  : form.type === 'seedance'
                    ? 'Seedance model IDs, e.g. doubao-seedance-1-0-pro-250528. Fetch the live list and keep your manual entries.'
                    : 'Upstream model names, separated by commas or new lines. Importing keeps your manual entries.'
            }
            action={
              <button
                type="button"
                className="btn btn-ghost btn-xs"
                onClick={() => void fetchUpstreamModels()}
                disabled={fetchingModels || !canFetchModels}
                title={
                  canFetchModels
                    ? 'Fetch the model list from the upstream provider'
                    : 'Enter the base URL and API key first'
                }
              >
                {fetchingModels ? <Spinner size={13} /> : <Icon name="refresh" size={13} />}
                {fetchingModels ? 'Fetching…' : 'Fetch models'}
              </button>
            }
          >
            <textarea
              id="channel-models"
              className="input textarea mono"
              rows={2}
              value={form.models}
              onChange={(event) => setField('models', event.target.value)}
              placeholder={
                form.type === 'minimax'
                  ? 'MiniMax-H3'
                  : form.type === 'minimax-music'
                    ? 'music-3.0'
                    : form.type === 'seedance'
                      ? 'doubao-seedance-1-0-pro-250528'
                      : 'gpt-4o, gpt-4o-mini'
              }
            />
          </FormField>
          <FormField
            label="Model mapping"
            htmlFor="channel-mapping"
            hint="Optional aliases as public=upstream, one per line."
          >
            <textarea
              id="channel-mapping"
              className="input textarea mono"
              rows={2}
              value={form.mapping}
              onChange={(event) => setField('mapping', event.target.value)}
              placeholder={
                form.type === 'minimax'
                  ? 'my-video=MiniMax-H3'
                  : form.type === 'minimax-music'
                    ? 'my-song=music-3.0'
                    : form.type === 'seedance'
                      ? 'my-video=doubao-seedance-1-0-pro-250528'
                      : 'claude-sonnet=claude-3-7-sonnet-latest'
              }
            />
          </FormField>
          {modelsError ? (
            <div className="field wide">
              <ErrorBanner message={modelsError} />
              <span className="field-hint">
                Your current models and aliases were not changed. Check the base URL and API key, then try again.
              </span>
            </div>
          ) : upstreamModels !== null ? (
            <div className="field wide">
              <div className="model-picker">
                <div className="model-picker-head">
                  <div className="model-picker-heading">
                    <span className="model-picker-title">Upstream models</span>
                    <span className="model-picker-sub">
                      {upstreamModels.length === 0
                        ? 'No models returned'
                        : `${upstreamModels.length} fetched · ${importableModels.length} not added`}
                    </span>
                  </div>
                  {importableModels.length > 0 ? (
                    <button type="button" className="btn btn-ghost btn-xs" onClick={toggleAllModels}>
                      {allImportableSelected ? 'Clear selection' : 'Select all'}
                    </button>
                  ) : null}
                </div>
                {upstreamModels.length === 0 ? (
                  <p className="model-picker-empty">
                    The upstream provider returned an empty model list. You can still enter model names manually.
                  </p>
                ) : (
                  <div className="model-picker-list">
                    {upstreamModels.map((model) => {
                      const alreadyAdded = currentModels.includes(model)
                      return (
                        <label
                          key={model}
                          className={`model-option${alreadyAdded ? ' is-added' : ''}`}
                          title={model}
                        >
                          <input
                            type="checkbox"
                            checked={alreadyAdded || selectedModels.includes(model)}
                            disabled={alreadyAdded}
                            onChange={() => toggleModel(model)}
                          />
                          <span className="model-option-name">{model}</span>
                          {alreadyAdded ? <span className="model-option-tag">added</span> : null}
                        </label>
                      )
                    })}
                  </div>
                )}
                <div className="model-picker-foot">
                  <span className="field-hint">Aliases in model mapping are left untouched.</span>
                  <button
                    type="button"
                    className="btn btn-primary btn-xs"
                    onClick={importSelectedModels}
                    disabled={selectedModels.length === 0}
                  >
                    <Icon name="plus" size={13} />
                    {selectedModels.length > 0 ? `Add ${selectedModels.length} selected` : 'Add selected'}
                  </button>
                </div>
              </div>
            </div>
          ) : null}
          <FormField label="Priority" htmlFor="channel-priority" hint="Higher priority is tried first.">
            <input
              id="channel-priority"
              className="input"
              type="number"
              min={0}
              value={form.priority}
              onChange={(event) => setField('priority', event.target.value)}
            />
          </FormField>
          <FormField label="Weight" htmlFor="channel-weight" hint="Load-balancing weight among equal priorities.">
            <input
              id="channel-weight"
              className="input"
              type="number"
              min={1}
              value={form.weight}
              onChange={(event) => setField('weight', event.target.value)}
            />
          </FormField>
          {!isMediaProtocol(form.type) ? (
            <>
              <FormField label="Input price" htmlFor="channel-input-price" hint="USD per 1M prompt tokens.">
                <input
                  id="channel-input-price"
                  className="input"
                  type="number"
                  min={0}
                  step="0.01"
                  value={form.input_price}
                  onChange={(event) => setField('input_price', event.target.value)}
                />
              </FormField>
              <FormField label="Output price" htmlFor="channel-output-price" hint="USD per 1M completion tokens.">
                <input
                  id="channel-output-price"
                  className="input"
                  type="number"
                  min={0}
                  step="0.01"
                  value={form.output_price}
                  onChange={(event) => setField('output_price', event.target.value)}
                />
              </FormField>
            </>
          ) : null}
          <FormField label="Status">
            <select
              className="input"
              value={form.status}
              onChange={(event) => setField('status', event.target.value as ChannelStatus)}
            >
              <option value="enabled">Enabled</option>
              <option value="disabled">Disabled</option>
            </select>
          </FormField>
        </div>
      </Modal>

      <Modal
        open={deleteTarget !== null}
        onClose={() => setDeleteTarget(null)}
        title="Delete channel"
        subtitle={deleteTarget ? `“${deleteTarget.name}” will stop serving requests immediately.` : undefined}
        width={440}
        footer={
          <>
            <button className="btn btn-ghost" onClick={() => setDeleteTarget(null)} disabled={deleting}>
              Cancel
            </button>
            <button className="btn btn-danger" onClick={() => void confirmDelete()} disabled={deleting}>
              {deleting ? <Spinner size={15} /> : null}
              Delete channel
            </button>
          </>
        }
      >
        <p className="modal-copy">
          Past request logs keep their channel name and pricing snapshot. This action cannot be undone.
        </p>
      </Modal>
    </div>
  )
}
