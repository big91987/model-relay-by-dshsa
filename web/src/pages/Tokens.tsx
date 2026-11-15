import { useCallback, useEffect, useState } from 'react'
import { Icon } from '../components/Icon'
import { Modal } from '../components/Modal'
import { EmptyState, ErrorBanner, FormField, PageHeader, Spinner, StatusBadge } from '../components/ui'
import { api } from '../lib/api'
import { formatCost, formatTime, localDateTimeValue, parseLocalDateTime, relativeTime } from '../lib/format'
import { t } from '../lib/i18n'
import { useToast } from '../lib/toast'
import type { Token, TokenInput, TokenStatus } from '../lib/types'

interface TokenFormState {
  name: string
  unlimited: boolean
  quota: string
  daily_quota: string
  allowed_models: string
  rpm_limit: string
  tpm_limit: string
  max_concurrency: string
  expires_at: string
  status: TokenStatus
}

const emptyForm: TokenFormState = {
  name: '',
  unlimited: true,
  quota: '',
  daily_quota: '',
  allowed_models: '',
  rpm_limit: '0',
  tpm_limit: '0',
  max_concurrency: '0',
  expires_at: '',
  status: 'active',
}

function tokenToForm(token: Token): TokenFormState {
  return {
    name: token.name,
    unlimited: token.quota < 0,
    quota: token.quota < 0 ? '' : String(token.quota),
    daily_quota: token.daily_quota > 0 ? String(token.daily_quota) : '',
    allowed_models: token.allowed_models.join(', '),
    rpm_limit: String(token.rpm_limit),
    tpm_limit: String(token.tpm_limit),
    max_concurrency: String(token.max_concurrency),
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
    daily_quota: Number.parseFloat(form.daily_quota) || 0,
    allowed_models: parseModelList(form.allowed_models),
    rpm_limit: Number.parseInt(form.rpm_limit, 10) || 0,
    tpm_limit: Number.parseInt(form.tpm_limit, 10) || 0,
    max_concurrency: Number.parseInt(form.max_concurrency, 10) || 0,
    expires_at: parseLocalDateTime(form.expires_at),
    status: form.status,
  }
}

function payloadFromToken(token: Token, overrides: Partial<TokenInput> = {}): TokenInput {
  return {
    name: token.name,
    quota: token.quota,
    daily_quota: token.daily_quota,
    allowed_models: token.allowed_models,
    rpm_limit: token.rpm_limit,
    tpm_limit: token.tpm_limit,
    max_concurrency: token.max_concurrency,
    expires_at: token.expires_at,
    status: token.status,
    ...overrides,
  }
}

function quotaLabel(token: Token): string {
  if (token.quota < 0) return t('Unlimited')
  return `${formatCost(token.used_quota)} / ${formatCost(token.quota)}`
}

function quotaProgress(token: Token): number {
  if (token.quota < 0) return 0
  if (token.quota === 0) return 100
  return Math.min(100, (token.used_quota / token.quota) * 100)
}

function limitsLabel(token: Token): string {
  const limits: string[] = []
  if (token.rpm_limit > 0) limits.push(`${token.rpm_limit} RPM`)
  if (token.tpm_limit > 0) limits.push(`${token.tpm_limit.toLocaleString()} TPM`)
  if (token.max_concurrency > 0) limits.push(t('{n} concurrent', { n: token.max_concurrency }))
  return limits.length > 0 ? limits.join(' · ') : t('Unlimited')
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
      setError(err instanceof Error ? err.message : t('Failed to load API keys'))
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
      toast.error(t('Key name is required'))
      return
    }
    if (!form.unlimited && !(Number.parseFloat(form.quota) >= 0)) {
      toast.error(t('Enter a quota, or mark the key as unlimited'))
      return
    }
    if (payload.rpm_limit < 0 || payload.tpm_limit < 0 || payload.max_concurrency < 0) {
      toast.error(t('Rate limits must be zero or greater'))
      return
    }
    if (payload.daily_quota < 0) {
      toast.error(t('Daily spend limit must be zero or greater'))
      return
    }
    if (
      Number.isNaN(payload.rpm_limit) ||
      Number.isNaN(payload.tpm_limit) ||
      Number.isNaN(payload.max_concurrency) ||
      Number.isNaN(payload.daily_quota)
    ) {
      toast.error(t('Limits must be valid numbers'))
      return
    }
    setSaving(true)
    try {
      if (editing) {
        await api.updateToken(editing.id, payload)
        toast.success(t('API key “{name}” updated', { name: payload.name }))
        setModalOpen(false)
        await load()
      } else {
        const result = await api.createToken(payload)
        setModalOpen(false)
        setCreatedKey(result.key)
        toast.success(t('API key “{name}” created', { name: payload.name }))
        await load()
      }
    } catch (err) {
      toast.error(err instanceof Error ? err.message : t('Failed to save API key'))
    } finally {
      setSaving(false)
    }
  }

  const toggleStatus = async (token: Token) => {
    const nextStatus: TokenStatus = token.status === 'active' ? 'disabled' : 'active'
    try {
      await api.updateToken(token.id, payloadFromToken(token, { status: nextStatus }))
      toast.success(t(nextStatus === 'active' ? "API key enabled" : "API key disabled"))
      await load()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : t('Failed to update API key'))
    }
  }

  const copyKey = async () => {
    if (!createdKey) return
    try {
      await navigator.clipboard.writeText(createdKey)
      toast.success(t('Key copied to clipboard'))
    } catch {
      toast.error(t('Could not copy automatically — select the key and copy it manually'))
    }
  }

  const confirmDelete = async () => {
    if (!deleteTarget) return
    setDeleting(true)
    try {
      await api.deleteToken(deleteTarget.id)
      toast.success(t('API key “{name}” deleted', { name: deleteTarget.name }))
      setDeleteTarget(null)
      await load()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : t('Failed to delete API key'))
    } finally {
      setDeleting(false)
    }
  }

  return (
    <div className="page">
      <PageHeader
        title={t('API keys')}
        subtitle={t('Gateway keys authenticate OpenAI-compatible clients. Set per-key balances, daily budgets, model access and rate limits.')}
        actions={
          <button className="btn btn-primary" onClick={openCreate}>
            <Icon name="plus" size={16} /> {t('Create key')}
          </button>
        }
      />
      {error ? <ErrorBanner message={error} /> : null}

      {loading ? (
        <div className="page-loading">
          <Spinner size={22} /> {t('Loading API keys…')}
        </div>
      ) : tokens.length === 0 ? (
        <section className="panel">
          <EmptyState
            icon="key"
            title={t('No API keys yet')}
            description={t('Create a key, then use it as the Bearer token for /v1/chat/completions.')}
            action={
              <button className="btn btn-primary" onClick={openCreate}>
                <Icon name="plus" size={16} /> {t('Create your first key')}
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
                  <th>{t('Key')}</th>
                  <th>{t('Status')}</th>
                  <th>{t('Quota')}</th>
                  <th>{t('Limits')}</th>
                  <th>{t('Last used')}</th>
                  <th>{t('Expires')}</th>
                  <th className="right">{t('Actions')}</th>
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
                          <span className="chip chip-muted">{t('All models')}</span>
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
                          aria-label={t('Toggle {name}', { name: token.name })}
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
                        {token.reserved_quota > 0 ? (
                          <span className="quota-reserved">{t('{cost} in flight', { cost: formatCost(token.reserved_quota) })}</span>
                        ) : null}
                        {token.daily_quota > 0 ? (
                          <span className="quota-daily">
                            {t('Today {used} / {quota}', { used: formatCost(token.daily_used_quota), quota: formatCost(token.daily_quota) })}
                          </span>
                        ) : null}
                      </div>
                    </td>
                    <td className="muted">
                      <span className="limits-label">{limitsLabel(token)}</span>
                    </td>
                    <td className="muted">{relativeTime(token.last_used_at)}</td>
                    <td className="muted">{token.expires_at ? formatTime(token.expires_at) : t('Never')}</td>
                    <td className="right">
                      <div className="row-actions">
                        <button className="icon-button" title={t('Edit key')} onClick={() => openEdit(token)}>
                          <Icon name="edit" size={16} />
                        </button>
                        <button className="icon-button danger" title={t('Delete key')} onClick={() => setDeleteTarget(token)}>
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
        title={editing ? t('Edit API key') : t('Create API key')}
        subtitle={t('Keys are stored as a hash; the plaintext value is shown only once at creation.')}
        width={600}
        footer={
          <>
            <button className="btn btn-ghost" onClick={() => setModalOpen(false)} disabled={saving}>
              {t('Cancel')}
            </button>
            <button className="btn btn-primary" onClick={() => void submit()} disabled={saving}>
              {saving ? <Spinner size={15} /> : null}
              {editing ? t('Save changes') : t('Create key')}
            </button>
          </>
        }
      >
        <div className="form-grid">
          <FormField label={t('Name')} htmlFor="token-name">
            <input
              id="token-name"
              className="input"
              value={form.name}
              onChange={(event) => setField('name', event.target.value)}
              placeholder={t('production-app')}
            />
          </FormField>
          <FormField label={t('Status')}>
            <select
              className="input"
              value={form.status}
              onChange={(event) => setField('status', event.target.value as TokenStatus)}
            >
              <option value="active">{t('Active')}</option>
              <option value="disabled">{t('Disabled')}</option>
            </select>
          </FormField>
          <FormField label={t('Quota (USD)')} hint={t('Spend limit for this key. Mark unlimited for no cap.')}>
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
                {t('Unlimited')}
              </label>
            </div>
          </FormField>
          <FormField
            label={t('Daily spend limit (USD)')}
            hint={t('Budget for the current day, resets at local midnight. 0 means unlimited.')}
          >
            <input
              className="input"
              type="number"
              min={0}
              step="0.01"
              value={form.daily_quota}
              onChange={(event) => setField('daily_quota', event.target.value)}
              placeholder="0"
            />
          </FormField>
          <FormField label={t('Requests per minute (RPM)')} hint={t('0 means unlimited.')}>
            <input
              className="input"
              type="number"
              min={0}
              value={form.rpm_limit}
              onChange={(event) => setField('rpm_limit', event.target.value)}
            />
          </FormField>
          <FormField label={t('Tokens per minute (TPM)')} hint={t('Prompt and completion tokens. 0 means unlimited.')}>
            <input
              className="input"
              type="number"
              min={0}
              value={form.tpm_limit}
              onChange={(event) => setField('tpm_limit', event.target.value)}
            />
          </FormField>
          <FormField label={t('Max concurrency')} hint={t('Requests this key may have in flight. 0 means unlimited.')}>
            <input
              className="input"
              type="number"
              min={0}
              value={form.max_concurrency}
              onChange={(event) => setField('max_concurrency', event.target.value)}
            />
          </FormField>
          <FormField
            label={t('Allowed models')}
            hint={t('Comma separated. Leave empty to allow every model exposed by your channels.')}
            wide
          >
            <input
              className="input mono"
              value={form.allowed_models}
              onChange={(event) => setField('allowed_models', event.target.value)}
              placeholder="gpt-4o, claude-sonnet"
            />
          </FormField>
          <FormField label={t('Expires at')} hint={t('Leave empty for a key that never expires.')}>
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
        title={t('API key created')}
        subtitle={t('Copy this value now. The full key will not be shown again.')}
        width={560}
        footer={
          <button className="btn btn-primary" onClick={() => setCreatedKey(null)}>
            {t('Done')}
          </button>
        }
      >
        <div className="key-reveal">
          <code>{createdKey}</code>
          <button className="icon-button" onClick={() => void copyKey()} title={t('Copy key')}>
            <Icon name="copy" size={16} />
          </button>
        </div>
        <p className="modal-copy">
          {t('Use it as')} <code>Authorization: Bearer &lt;key&gt;</code> {t('against the relay. Store it in your secret manager.')}
        </p>
      </Modal>

      <Modal
        open={deleteTarget !== null}
        onClose={() => setDeleteTarget(null)}
        title={t('Delete API key')}
        subtitle={deleteTarget ? t('“{name}” will stop authenticating immediately.', { name: deleteTarget.name }) : undefined}
        width={440}
        footer={
          <>
            <button className="btn btn-ghost" onClick={() => setDeleteTarget(null)} disabled={deleting}>
              {t('Cancel')}
            </button>
            <button className="btn btn-danger" onClick={() => void confirmDelete()} disabled={deleting}>
              {deleting ? <Spinner size={15} /> : null}
              {t('Delete key')}
            </button>
          </>
        }
      >
        <p className="modal-copy">{t('Request logs keep their key name so historical spend stays readable.')}</p>
      </Modal>
    </div>
  )
}
