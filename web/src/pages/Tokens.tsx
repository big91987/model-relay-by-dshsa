import { useCallback, useEffect, useState } from 'react'
import { Icon } from '../components/Icon'
import { Modal } from '../components/Modal'
import { EmptyState, ErrorBanner, FormField, PageHeader, Spinner, StatusBadge } from '../components/ui'
import { api } from '../lib/api'
import { formatCost, formatTime, localDateTimeValue, parseLocalDateTime, relativeTime } from '../lib/format'
import { useToast } from '../lib/toast'
import type { Token, TokenInput, TokenStatus } from '../lib/types'

interface TokenFormState {
  name: string
  unlimited: boolean
  quota: string
  allowed_models: string
  rpm_limit: string
  expires_at: string
  status: TokenStatus
}

const emptyForm: TokenFormState = {
  name: '',
  unlimited: true,
  quota: '',
  allowed_models: '',
  rpm_limit: '0',
  expires_at: '',
  status: 'active',
}

function tokenToForm(token: Token): TokenFormState {
  return {
    name: token.name,
    unlimited: token.quota < 0,
    quota: token.quota < 0 ? '' : String(token.quota),
    allowed_models: token.allowed_models.join(', '),
    rpm_limit: String(token.rpm_limit),
    expires_at: localDateTimeValue(token.expires_at),
    status: token.status,
  }
}

function parseModelList(value: string): string[] {
  return value
    .split(/[\n,]/)
    .map((item) => item.trim())
    .filter(Boolean)
}

function toPayload(form: TokenFormState): TokenInput {
  return {
    name: form.name.trim(),
    quota: form.unlimited ? -1 : Number.parseFloat(form.quota) || 0,
    allowed_models: parseModelList(form.allowed_models),
    rpm_limit: Number.parseInt(form.rpm_limit, 10) || 0,
    expires_at: parseLocalDateTime(form.expires_at),
    status: form.status,
  }
}

function payloadFromToken(token: Token, overrides: Partial<TokenInput> = {}): TokenInput {
  return {
    name: token.name,
    quota: token.quota,
    allowed_models: token.allowed_models,
    rpm_limit: token.rpm_limit,
    expires_at: token.expires_at,
    status: token.status,
    ...overrides,
  }
}

function quotaLabel(token: Token): string {
  if (token.quota < 0) return 'Unlimited'
  return `${formatCost(token.used_quota)} / ${formatCost(token.quota)}`
}

function quotaProgress(token: Token): number {
  if (token.quota < 0) return 0
  if (token.quota === 0) return 100
  return Math.min(100, (token.used_quota / token.quota) * 100)
}

export default function Tokens() {
  const toast = useToast()
  const [tokens, setTokens] = useState<Token[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [modalOpen, setModalOpen] = useState(false)
  const [editing, setEditing] = useState<Token | null>(null)
  const [form, setForm] = useState<TokenFormState>(emptyForm)
  const [saving, setSaving] = useState(false)
  const [createdKey, setCreatedKey] = useState<string | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<Token | null>(null)
  const [deleting, setDeleting] = useState(false)

  const load = useCallback(async () => {
    try {
      const response = await api.tokens()
      setTokens(response.data)
      setError('')
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load API keys')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  const setField = <K extends keyof TokenFormState>(key: K, value: TokenFormState[K]) => {
    setForm((previous) => ({ ...previous, [key]: value }))
  }

  const openCreate = () => {
    setEditing(null)
    setForm(emptyForm)
    setModalOpen(true)
  }

  const openEdit = (token: Token) => {
    setEditing(token)
    setForm(tokenToForm(token))
    setModalOpen(true)
  }

  const submit = async () => {
    const payload = toPayload(form)
    if (!payload.name) {
      toast.error('Key name is required')
      return
    }
    if (!form.unlimited && !(Number.parseFloat(form.quota) >= 0)) {
      toast.error('Enter a quota, or mark the key as unlimited')
      return
    }
    if (payload.rpm_limit < 0) {
      toast.error('Rate limit must be zero or greater')
      return
    }
    setSaving(true)
    try {
      if (editing) {
        await api.updateToken(editing.id, payload)
        toast.success(`API key “${payload.name}” updated`)
        setModalOpen(false)
        await load()
      } else {
        const result = await api.createToken(payload)
        setModalOpen(false)
        setCreatedKey(result.key)
        toast.success(`API key “${payload.name}” created`)
        await load()
      }
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'Failed to save API key')
    } finally {
      setSaving(false)
    }
  }

  const toggleStatus = async (token: Token) => {
    const nextStatus: TokenStatus = token.status === 'active' ? 'disabled' : 'active'
    try {
      await api.updateToken(token.id, payloadFromToken(token, { status: nextStatus }))
      toast.success(`API key ${nextStatus === 'active' ? 'enabled' : 'disabled'}`)
      await load()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'Failed to update API key')
    }
  }

  const copyKey = async () => {
    if (!createdKey) return
    try {
      await navigator.clipboard.writeText(createdKey)
      toast.success('Key copied to clipboard')
    } catch {
      toast.error('Could not copy automatically — select the key and copy it manually')
    }
  }

  const confirmDelete = async () => {
    if (!deleteTarget) return
    setDeleting(true)
    try {
      await api.deleteToken(deleteTarget.id)
      toast.success(`API key “${deleteTarget.name}” deleted`)
      setDeleteTarget(null)
      await load()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'Failed to delete API key')
    } finally {
      setDeleting(false)
    }
  }

  return (
    <div className="page">
      <PageHeader
        title="API keys"
        subtitle="Gateway keys authenticate OpenAI-compatible clients. Set per-key quotas, model access and rate limits."
        actions={
          <button className="btn btn-primary" onClick={openCreate}>
            <Icon name="plus" size={16} /> Create key
          </button>
        }
      />
      {error ? <ErrorBanner message={error} /> : null}

      {loading ? (
        <div className="page-loading">
          <Spinner size={22} /> Loading API keys…
        </div>
      ) : tokens.length === 0 ? (
        <section className="panel">
          <EmptyState
            icon="key"
            title="No API keys yet"
            description="Create a key, then use it as the Bearer token for /v1/chat/completions."
            action={
              <button className="btn btn-primary" onClick={openCreate}>
                <Icon name="plus" size={16} /> Create your first key
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
                  <th>Key</th>
                  <th>Status</th>
                  <th>Quota</th>
                  <th>Rate limit</th>
                  <th>Last used</th>
                  <th>Expires</th>
                  <th className="right">Actions</th>
                </tr>
              </thead>
              <tbody>
                {tokens.map((token) => (
                  <tr key={token.id}>
                    <td>
                      <div className="cell-title">{token.name}</div>
                      <div className="cell-sub mono">{token.key_prefix}…</div>
                      <div className="chip-row token-models">
                        {token.allowed_models.length === 0 ? (
                          <span className="chip chip-muted">All models</span>
                        ) : (
                          token.allowed_models.slice(0, 2).map((model) => (
                            <span className="chip" key={model}>
                              {model}
                            </span>
                          ))
                        )}
                        {token.allowed_models.length > 2 ? (
                          <span className="chip chip-more">+{token.allowed_models.length - 2}</span>
                        ) : null}
                      </div>
                    </td>
                    <td>
                      <div className="status-toggle">
                        <button
                          className={`switch${token.status === 'active' ? ' on' : ''}`}
                          role="switch"
                          aria-checked={token.status === 'active'}
                          aria-label={`Toggle ${token.name}`}
                          onClick={() => void toggleStatus(token)}
                        >
                          <span />
                        </button>
                        <StatusBadge status={token.status} />
                      </div>
                    </td>
                    <td>
                      <div className="quota-cell">
                        <span>{quotaLabel(token)}</span>
                        {token.quota >= 0 ? (
                          <span className="quota-meter">
                            <span style={{ width: `${quotaProgress(token)}%` }} />
                          </span>
                        ) : null}
                      </div>
                    </td>
                    <td className="muted">{token.rpm_limit > 0 ? `${token.rpm_limit} RPM` : 'Unlimited'}</td>
                    <td className="muted">{relativeTime(token.last_used_at)}</td>
                    <td className="muted">{token.expires_at ? formatTime(token.expires_at) : 'Never'}</td>
                    <td className="right">
                      <div className="row-actions">
                        <button className="icon-button" title="Edit key" onClick={() => openEdit(token)}>
                          <Icon name="edit" size={16} />
                        </button>
                        <button className="icon-button danger" title="Delete key" onClick={() => setDeleteTarget(token)}>
                          <Icon name="trash" size={16} />
                        </button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>
      )}

      <Modal
        open={modalOpen}
        onClose={() => setModalOpen(false)}
        title={editing ? 'Edit API key' : 'Create API key'}
        subtitle="Keys are stored as a hash; the plaintext value is shown only once at creation."
        width={600}
        footer={
          <>
            <button className="btn btn-ghost" onClick={() => setModalOpen(false)} disabled={saving}>
              Cancel
            </button>
            <button className="btn btn-primary" onClick={() => void submit()} disabled={saving}>
              {saving ? <Spinner size={15} /> : null}
              {editing ? 'Save changes' : 'Create key'}
            </button>
          </>
        }
      >
        <div className="form-grid">
          <FormField label="Name" htmlFor="token-name">
            <input
              id="token-name"
              className="input"
              value={form.name}
              onChange={(event) => setField('name', event.target.value)}
              placeholder="production-app"
            />
          </FormField>
          <FormField label="Status">
            <select
              className="input"
              value={form.status}
              onChange={(event) => setField('status', event.target.value as TokenStatus)}
            >
              <option value="active">Active</option>
              <option value="disabled">Disabled</option>
            </select>
          </FormField>
          <FormField label="Quota (USD)" hint="Spend limit for this key. Mark unlimited for no cap.">
            <div className="input-row">
              <input
                className="input"
                type="number"
                min={0}
                step="0.01"
                value={form.quota}
                onChange={(event) => setField('quota', event.target.value)}
                disabled={form.unlimited}
                placeholder="25.00"
              />
              <label className="checkbox">
                <input
                  type="checkbox"
                  checked={form.unlimited}
                  onChange={(event) => setField('unlimited', event.target.checked)}
                />
                Unlimited
              </label>
            </div>
          </FormField>
          <FormField label="Rate limit (RPM)" hint="0 means unlimited.">
            <input
              className="input"
              type="number"
              min={0}
              value={form.rpm_limit}
              onChange={(event) => setField('rpm_limit', event.target.value)}
            />
          </FormField>
          <FormField
            label="Allowed models"
            hint="Comma separated. Leave empty to allow every model exposed by your channels."
            wide
          >
            <input
              className="input mono"
              value={form.allowed_models}
              onChange={(event) => setField('allowed_models', event.target.value)}
              placeholder="gpt-4o, claude-sonnet"
            />
          </FormField>
          <FormField label="Expires at" hint="Leave empty for a key that never expires.">
            <input
              className="input"
              type="datetime-local"
              value={form.expires_at}
              onChange={(event) => setField('expires_at', event.target.value)}
            />
          </FormField>
        </div>
      </Modal>

      <Modal
        open={createdKey !== null}
        onClose={() => setCreatedKey(null)}
        title="API key created"
        subtitle="Copy this value now. The full key will not be shown again."
        width={560}
        footer={
          <button className="btn btn-primary" onClick={() => setCreatedKey(null)}>
            Done
          </button>
        }
      >
        <div className="key-reveal">
          <code>{createdKey}</code>
          <button className="icon-button" onClick={() => void copyKey()} title="Copy key">
            <Icon name="copy" size={16} />
          </button>
        </div>
        <p className="modal-copy">
          Use it as <code>Authorization: Bearer &lt;key&gt;</code> against the relay. Store it in your secret manager.
        </p>
      </Modal>

      <Modal
        open={deleteTarget !== null}
        onClose={() => setDeleteTarget(null)}
        title="Delete API key"
        subtitle={deleteTarget ? `“${deleteTarget.name}” will stop authenticating immediately.` : undefined}
        width={440}
        footer={
          <>
            <button className="btn btn-ghost" onClick={() => setDeleteTarget(null)} disabled={deleting}>
              Cancel
            </button>
            <button className="btn btn-danger" onClick={() => void confirmDelete()} disabled={deleting}>
              {deleting ? <Spinner size={15} /> : null}
              Delete key
            </button>
          </>
        }
      >
        <p className="modal-copy">Request logs keep their key name so historical spend stays readable.</p>
      </Modal>
    </div>
  )
}
