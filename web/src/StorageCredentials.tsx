import { type FormEvent, useCallback, useEffect, useRef, useState } from 'react'
import type { components } from './api/schema'
import { ApiError, csrfToken, errorMessage, requestJSON } from './api/client'

type Credential = components['schemas']['StorageCredential']
type CredentialList = components['schemas']['StorageCredentialList']
type Props = { canManage: boolean; onUnauthorized: () => void }

const statusLabels: Record<Credential['status'], string> = {
  UNTESTED: '未测试', HEALTHY: '可用', DEGRADED: '访问异常', EXPIRED: '已过期', DISABLED: '已禁用',
}
const endpoint = '/api/v1/storage-credentials'

export function StorageCredentials({ canManage, onUnauthorized }: Props) {
  const [items, setItems] = useState<Credential[]>([])
  const [nextCursor, setNextCursor] = useState<string>()
  const [selected, setSelected] = useState<Credential | null>(null)
  const [formMode, setFormMode] = useState<'create' | 'replace' | null>(null)
  const [loading, setLoading] = useState(true)
  const [busy, setBusy] = useState(false)
  const [message, setMessage] = useState('')
  const [notice, setNotice] = useState('')
  const [confirmDisable, setConfirmDisable] = useState(false)
  const formRef = useRef<HTMLFormElement>(null)

  const handleError = useCallback((error: unknown) => {
    if (error instanceof ApiError && error.status === 401) { onUnauthorized(); return }
    const conflict = error instanceof ApiError && error.status === 412
    setMessage(conflict
      ? '凭据已被其他操作更新。请刷新详情，再重新提交；输入的秘密已清空。'
      : errorMessage(error))
  }, [onUnauthorized])

  const load = useCallback(async (cursor?: string, signal?: AbortSignal) => {
    setLoading(true)
    setMessage('')
    try {
      const page = await requestJSON<CredentialList>(cursor ? `${endpoint}?cursor=${encodeURIComponent(cursor)}` : endpoint, { signal })
      if (signal?.aborted) return
      setItems((current) => cursor ? [...current, ...page.items] : page.items)
      setNextCursor(page.next_cursor)
    } catch (error) {
      if (!signal?.aborted) handleError(error)
    } finally {
      if (!signal?.aborted) setLoading(false)
    }
  }, [handleError])

  useEffect(() => {
    const controller = new AbortController()
    void load(undefined, controller.signal)
    const clearForm = () => formRef.current?.reset()
    window.addEventListener('pagehide', clearForm)
    return () => {
      controller.abort()
      window.removeEventListener('pagehide', clearForm)
    }
  }, [load])

  async function openDetail(id: string) {
    setBusy(true)
    setMessage('')
    setFormMode(null)
    setConfirmDisable(false)
    try { setSelected(await requestJSON<Credential>(`${endpoint}/${id}`)) }
    catch (error) { handleError(error) }
    finally { setBusy(false) }
  }

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (busy) return
    const form = event.currentTarget
    const fields = new FormData(form)
    const replacing = formMode === 'replace' && selected !== null
    const body = JSON.stringify(replacing
      ? { rclone_config: String(fields.get('rclone_config') ?? '') }
      : { name: String(fields.get('name') ?? ''), remote_name: String(fields.get('remote_name') ?? ''), rclone_config: String(fields.get('rclone_config') ?? '') })
    // Secret input stays outside React state and is cleared before every request, including failures.
    fields.delete('rclone_config')
    form.reset()
    setBusy(true)
    setMessage('')
    setNotice('')
    try {
      const credential = await requestJSON<Credential>(replacing ? `${endpoint}/${selected.id}/replace-secret` : endpoint, {
        method: 'POST',
        headers: { 'X-CSRF-Token': csrfToken(), ...(replacing ? { 'If-Match': `"${selected.revision}"` } : {}) },
        body,
      })
      setFormMode(null)
      setSelected(credential)
      setNotice('配置已加密保存，尚未验证云端连通性。')
      await load()
    } catch (error) { handleError(error) }
    finally { setBusy(false) }
  }

  async function disable() {
    if (!selected || busy) return
    setBusy(true)
    setMessage('')
    setNotice('')
    try {
      const credential = await requestJSON<Credential>(`${endpoint}/${selected.id}/disable`, {
        method: 'POST', headers: { 'X-CSRF-Token': csrfToken(), 'If-Match': `"${selected.revision}"` },
      })
      setSelected(credential)
      setConfirmDisable(false)
      setNotice('凭据已禁用。')
      await load()
    } catch (error) { handleError(error) }
    finally { setBusy(false) }
  }

  return (
    <section aria-labelledby="credentials-title">
      <div className="section-heading">
        <div><p className="eyebrow">中心存储</p><h1 id="credentials-title">存储凭据</h1></div>
        <div className="actions">
          <button disabled={busy || loading} onClick={() => void load()}>刷新列表</button>
          {canManage && <button disabled={busy} onClick={() => { setSelected(null); setFormMode('create'); setMessage(''); setNotice('') }}>导入凭据</button>}
        </div>
      </div>
      <p className="muted">导入 OneDrive 与 Crypt 的配置后，由中心加密保存。当前提供凭据管理；云端连接测试和仓库创建将在后续版本开放。</p>
      {message && <p role="alert" className="error-message">{message}</p>}
      {notice && <p role="status">{notice}</p>}
      {loading && items.length === 0 && <p role="status">正在读取凭据…</p>}
      {!loading && !message && items.length === 0 && <p className="muted">还没有存储凭据。先在可信设备通过 rclone 完成 OneDrive 授权和 Crypt 配置，再导入所需的两个 remote。</p>}
      {items.length > 0 && <div className="table-wrap"><table>
        <thead><tr><th>名称</th><th>存储服务</th><th>状态</th><th>凭据版本</th><th>操作</th></tr></thead>
        <tbody>{items.map((item) => <tr key={item.id}>
          <td>{item.name}</td><td>OneDrive + Crypt</td><td>{statusLabels[item.status]}</td><td>{item.secret_revision}</td>
          <td><button className="table-link" disabled={busy} onClick={() => void openDetail(item.id)}>查看 {item.name}</button></td>
        </tr>)}</tbody>
      </table></div>}
      {nextCursor && <button disabled={loading || busy} onClick={() => void load(nextCursor)}>加载更多</button>}
      {selected && <section className="credential-detail" aria-label="凭据详情">
        <h2>{selected.name}</h2>
        <dl className="detail-list">
          <div><dt>状态</dt><dd>{statusLabels[selected.status]}</dd></div>
          <div><dt>Crypt remote</dt><dd>{selected.remote_name}</dd></div>
          <div><dt>凭据版本 / 记录版本</dt><dd>{selected.secret_revision} / {selected.revision}</dd></div>
          <div><dt>最近更新</dt><dd>{new Date(selected.updated_at).toLocaleString()}</dd></div>
        </dl>
        <div className="actions">
          <button disabled={busy} onClick={() => void openDetail(selected.id)}>刷新详情</button>
          {canManage && selected.status !== 'DISABLED' && <>
            <button disabled={busy} onClick={() => { setFormMode('replace'); setConfirmDisable(false); setNotice('') }}>替换凭据</button>
            <button disabled={busy} onClick={() => { setConfirmDisable(true); setFormMode(null) }}>禁用凭据</button>
          </>}
        </div>
        {confirmDisable && <div role="group" aria-label="确认禁用凭据">
          <p>禁用“{selected.name}”后将停止使用该凭据。此操作不会删除云端数据。</p>
          <button disabled={busy} onClick={() => void disable()}>确认禁用</button>
          <button disabled={busy} onClick={() => setConfirmDisable(false)}>取消</button>
        </div>}
      </section>}
      {formMode && canManage && <form ref={formRef} onSubmit={submit} autoComplete="off" className="credential-form" aria-label={formMode === 'create' ? '导入存储凭据' : '替换存储凭据'}>
        <h2>{formMode === 'create' ? '导入存储凭据' : '替换存储凭据'}</h2>
        {formMode === 'create' && <>
          <label>凭据名称<input name="name" maxLength={128} required /></label>
          <label>Crypt remote 名称<input name="remote_name" pattern="[A-Za-z][A-Za-z0-9_-]{1,63}" maxLength={64} placeholder="encrypted" required /></label>
        </>}
        <label>rclone 配置<textarea name="rclone_config" maxLength={262144} rows={10} required autoComplete="off" spellCheck={false} autoCapitalize="off" /></label>
        <p className="muted">仅粘贴 OneDrive 与 Crypt 两个配置段，最多 256 KiB。支持默认全球 Microsoft 端点及标准文件名/目录加密。替换时保留 drive、路径和 Crypt 密码，只更新 token 或 OAuth client 凭据。</p>
        <p className="muted">提交或离开页面时会清空秘密输入；请自行保管原始配置。</p>
        <div className="actions">
          <button type="submit" disabled={busy}>{busy ? '保存中…' : '加密保存'}</button>
          <button type="button" disabled={busy} onClick={() => { formRef.current?.reset(); setFormMode(null) }}>取消</button>
        </div>
      </form>}
    </section>
  )
}
