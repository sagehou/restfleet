import { type FormEvent, useCallback, useEffect, useRef, useState } from 'react'
import type { components } from './api/schema'
import { ApiError, csrfToken, errorMessage, requestJSON } from './api/client'

type Operation = components['schemas']['Operation']
type Repository = components['schemas']['Repository']
type RepositoryList = components['schemas']['RepositoryList']
type Host = components['schemas']['Host']
type Credential = components['schemas']['StorageCredential']
type CredentialList = components['schemas']['StorageCredentialList']
type Props = { hosts: Host[]; canManage: boolean; onUnauthorized: () => void }

const endpoint = '/api/v1/repositories'
const statusLabels: Record<Repository['status'], string> = {
  PROVISIONING: '待完成初始化', READY: '可用', DEGRADED: '访问异常', LOCKED: '已锁定', DISABLED: '已禁用', ERROR: '异常',
}

export function Repositories({ hosts, canManage, onUnauthorized }: Props) {
  const [items, setItems] = useState<Repository[]>([])
  const [nextCursor, setNextCursor] = useState<string>()
  const [selected, setSelected] = useState<Repository | null>(null)
  const [credentials, setCredentials] = useState<Credential[]>([])
  const [credentialCursor, setCredentialCursor] = useState<string>()
  const [creating, setCreating] = useState(false)
  const [loading, setLoading] = useState(true)
  const [busy, setBusy] = useState(false)
  const [message, setMessage] = useState('')
  const [operation, setOperation] = useState<Operation | null>(null)
  const [operationRefresh, setOperationRefresh] = useState(0)
  const initializeKey = useRef<{ repositoryId: string; key: string } | null>(null)
  const operationId = selected?.last_initialize_operation_id
  const initializing = Boolean(operationId && (!operation || operation.id !== operationId || !operation.finished_at))

  const handleError = useCallback((error: unknown) => {
    if (error instanceof ApiError && error.status === 401) { onUnauthorized(); return }
    setMessage(error instanceof ApiError && error.problem?.code === 'HOST_REPOSITORY_EXISTS'
      ? '该 Host 已有仓库。若刚才网络中断，请刷新列表查看创建结果，不要重复创建。'
      : errorMessage(error))
  }, [onUnauthorized])

  const load = useCallback((cursor?: string, signal?: AbortSignal) =>
    requestJSON<RepositoryList>(cursor ? `${endpoint}?cursor=${encodeURIComponent(cursor)}` : endpoint, { signal })
      .then((page) => {
        if (signal?.aborted) return
        setItems((current) => cursor ? [...current, ...page.items] : page.items)
        setNextCursor(page.next_cursor)
      })
      .catch((error: unknown) => { if (!signal?.aborted) handleError(error) })
      .finally(() => { if (!signal?.aborted) setLoading(false) }),
  [handleError])

  useEffect(() => {
    const controller = new AbortController()
    void load(undefined, controller.signal)
    return () => controller.abort()
  }, [load])

  useEffect(() => {
    if (!operationId) return
    const controller = new AbortController()
    let timer: ReturnType<typeof setTimeout> | undefined
    const poll = () => requestJSON<Operation>(`/api/v1/operations/${operationId}`, { signal: controller.signal })
      .then((result) => {
        if (controller.signal.aborted) return
        setOperation(result)
        if (!result.finished_at) { timer = setTimeout(() => { void poll() }, 1000); return }
        void requestJSON<Repository>(`${endpoint}/${result.repository_id}`, { signal: controller.signal })
          .then((repo) => { if (!controller.signal.aborted) setSelected((current) => current?.id === repo.id ? repo : current) })
          .catch((error: unknown) => { if (!controller.signal.aborted) handleError(error) })
        void load(undefined, controller.signal)
      })
      .catch((error: unknown) => { if (!controller.signal.aborted) handleError(error) })
    void poll()
    return () => { controller.abort(); clearTimeout(timer) }
  }, [operationId, operationRefresh, handleError, load])

  async function initialize() {
    if (!selected || busy || initializing || selected.initialized_at) return
    setBusy(true)
    setMessage('')
    try {
      if (initializeKey.current?.repositoryId !== selected.id) initializeKey.current = { repositoryId: selected.id, key: crypto.randomUUID() }
      const result = await requestJSON<Operation>(`${endpoint}/${selected.id}/initialize`, {
        method: 'POST', headers: { 'X-CSRF-Token': csrfToken(), 'Idempotency-Key': initializeKey.current.key },
      })
      initializeKey.current = null
      setOperation(result)
      setSelected((current) => current && current.id === result.repository_id ? { ...current, last_initialize_operation_id: result.id } : current)
    } catch (error) { handleError(error) }
    finally { setBusy(false) }
  }

  async function loadCredentials(cursor?: string) {
    setBusy(true)
    setMessage('')
    try {
      const page = await requestJSON<CredentialList>(`/api/v1/storage-credentials?limit=200${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`)
      setCredentials((current) => cursor ? [...current, ...page.items] : page.items)
      setCredentialCursor(page.next_cursor)
    } catch (error) { handleError(error) }
    finally { setBusy(false) }
  }

  async function openDetail(id: string) {
    setBusy(true)
    setMessage('')
    setCreating(false)
    try { setSelected(await requestJSON<Repository>(`${endpoint}/${id}`)) }
    catch (error) { handleError(error) }
    finally { setBusy(false) }
  }

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (busy) return
    const form = new FormData(event.currentTarget)
    setBusy(true)
    setMessage('')
    try {
      const repository = await requestJSON<Repository>(endpoint, {
        method: 'POST', headers: { 'X-CSRF-Token': csrfToken() },
        body: JSON.stringify({ name: String(form.get('name') ?? ''), host_id: String(form.get('host_id') ?? ''), storage_credential_id: String(form.get('storage_credential_id') ?? '') }),
      })
      setSelected(repository)
      setCreating(false)
      await load()
    } catch (error) { handleError(error) }
    finally { setBusy(false) }
  }

  function reload(cursor?: string) {
    setLoading(true)
    setMessage('')
    void load(cursor)
  }
  const eligibleHosts = hosts.filter((host) => host.status === 'PENDING' || host.status === 'ACTIVE')
  const eligibleCredentials = credentials.filter((credential) => credential.status !== 'DISABLED')
  const hostName = (id: string) => hosts.find((host) => host.id === id)?.display_name ?? id

  return (
    <section>
      <div className="section-heading">
        <div><p className="eyebrow">STORAGE</p><h1>仓库</h1><p className="muted">每台 Host 使用独立仓库、独立密码和 Gateway 身份；不是共享仓库，也不是只写仓库。</p></div>
        <div className="actions">
          <button className="secondary-button" disabled={loading || busy} onClick={() => reload()}>刷新列表</button>
          {canManage && <button className="primary-button" disabled={busy} onClick={() => { setCreating(true); setSelected(null); void loadCredentials() }}>创建仓库</button>}
        </div>
      </div>
      <p className="warning-text" role="status">当前支持创建仓库和中心初始化。Gateway 接入和 Agent 确认尚未接通，新仓库暂不可备份。</p>
      {message && <p className="error-message" role="alert">{message}</p>}
      {creating && (
        <form aria-label="创建独立仓库" className="credential-form" onSubmit={submit}>
          <label>仓库名称<input name="name" maxLength={128} required /></label>
          <label>所属 Host<select name="host_id" required defaultValue="">
            <option value="" disabled>选择 Host</option>
            {eligibleHosts.map((host) => <option key={host.id} value={host.id}>{host.display_name}</option>)}
          </select></label>
          <label>存储凭据<select name="storage_credential_id" required defaultValue="">
            <option value="" disabled>选择中心存储凭据</option>
            {eligibleCredentials.map((credential) => <option key={credential.id} value={credential.id}>{credential.name}（{credential.status}）</option>)}
          </select></label>
          <p className="muted">密码由中心随机生成并加密保存，不会显示在浏览器中。选择未测试凭据并不证明云端可用。</p>
          {!eligibleHosts.length && <p>请先创建或启用一台 Host。</p>}
          {!busy && !eligibleCredentials.length && <p>没有可选择的凭据，请先导入未禁用的存储凭据。</p>}
          <div className="actions">
            <button className="primary-button" type="submit" disabled={busy || !eligibleHosts.length || !eligibleCredentials.length}>{busy ? '处理中…' : '保存仓库记录'}</button>
            <button className="secondary-button" type="button" disabled={busy} onClick={() => { void loadCredentials(credentialCursor) }}>{credentialCursor ? '加载更多凭据' : '刷新凭据'}</button>
            <button className="table-link" type="button" disabled={busy} onClick={() => setCreating(false)}>取消</button>
          </div>
        </form>
      )}
      {selected && (
        <article className="credential-detail" aria-label="仓库详情">
          <h2>{selected.name}</h2>
          <dl className="detail-list">
            <div><dt>仓库 ID</dt><dd>{selected.id}</dd></div>
            <div><dt>所属 Host</dt><dd>{hostName(selected.host_id)}</dd></div>
            <div><dt>存储凭据 ID</dt><dd>{selected.storage_credential_id}</dd></div>
            <div><dt>状态</dt><dd>{statusLabels[selected.status]}</dd></div>
            <div><dt>中心初始化</dt><dd>{selected.initialized_at ? `已验证（${selected.initialized_at}）` : '尚未完成'}</dd></div>
            <div><dt>Agent 凭据交付</dt><dd>{selected.agent_credential_revision ? `版本 ${selected.agent_credential_revision}，${selected.agent_credential_accepted_at ? `已确认保存（${selected.agent_credential_accepted_at}）` : '等待 Agent 确认'}` : '尚未下发'}</dd></div>
            <div><dt>仓库格式</dt><dd>{selected.format_version ? `v${selected.format_version}` : '尚未验证'}</dd></div>
            <div><dt>Gateway 凭据版本</dt><dd>{selected.gateway_secret_revision}</dd></div>
            <div><dt>Restic 凭据版本</dt><dd>{selected.restic_secret_revision}</dd></div>
            <div><dt>记录版本</dt><dd>{selected.revision}</dd></div>
          </dl>
          <p className="muted">快照和容量尚未采集；Agent 凭据确认仅表示已安全保存，不代表 Gateway 已就绪或备份已成功。</p>
          {operationId && <div role="status" aria-label="初始化任务状态">
            <p>操作 {operationId}：{!operation || operation.id !== operationId ? '正在读取状态' : operation.finished_at
              ? operation.status === 'SUCCEEDED' ? '中心初始化已验证，等待 Gateway 和 Agent 确认' : `初始化未成功（${operation.error_code}）`
              : operation.status === 'QUEUED' ? '等待中心 worker' : '正在初始化'}</p>
          </div>}
          <div className="actions">
            {canManage && selected.status === 'PROVISIONING' && !selected.initialized_at && <button className="primary-button" disabled={busy || initializing} onClick={() => { void initialize() }}>初始化仓库</button>}
            <button className="secondary-button" disabled={busy} onClick={() => { setOperationRefresh((current) => current + 1); void openDetail(selected.id) }}>刷新详情</button>
          </div>
        </article>
      )}
      {loading && <p aria-live="polite">正在加载仓库…</p>}
      {!loading && !message && !items.length && <p className="muted">还没有仓库。创建记录后仍需完成初始化和 Agent 确认。</p>}
      {items.length > 0 && <div className="table-wrap"><table>
        <thead><tr><th>仓库</th><th>Host</th><th>状态</th><th>操作</th></tr></thead>
        <tbody>{items.map((repo) => <tr key={repo.id}>
          <td>{repo.name}</td><td>{hostName(repo.host_id)}</td><td>{statusLabels[repo.status]}</td>
          <td><button className="table-link" disabled={busy} onClick={() => { void openDetail(repo.id) }}>查看 {repo.name}</button></td>
        </tr>)}</tbody>
      </table></div>}
      {nextCursor && <button className="secondary-button" disabled={loading || busy} onClick={() => reload(nextCursor)}>加载更多仓库</button>}
    </section>
  )
}
