import { type FormEvent, useCallback, useEffect, useState } from 'react'
import type { components } from './api/schema'
import { ApiError, csrfToken, errorMessage, requestJSON } from './api/client'
import { describeCron } from './cron'

type Plan = components['schemas']['Plan']
type PlanList = components['schemas']['PlanList']
type Template = components['schemas']['Template']
type TemplateList = components['schemas']['TemplateList']
type Repository = components['schemas']['Repository']
type RepositoryList = components['schemas']['RepositoryList']
type Agent = components['schemas']['Agent']
type AgentList = components['schemas']['AgentList']
type Host = components['schemas']['Host']
type Props = { hosts: Host[]; canManage: boolean; onUnauthorized: () => void }

const endpoint = '/api/v1/plans'
const statusLabels: Record<Plan['status'], string> = {
  DRAFT: '草稿', PENDING_APPLY: '待应用', ACTIVE: '已启用', PAUSED: '已暂停', INVALID: '配置无效', ARCHIVED: '已归档',
}
const healthLabels: Record<Plan['backup_health'], string> = {
  UNKNOWN: '未知', HEALTHY: '健康', OVERDUE: '已逾期', FAILED: '失败', NEVER_RUN: '从未运行', PAUSED: '已暂停',
}

export function Plans({ hosts, canManage, onUnauthorized }: Props) {
  const [items, setItems] = useState<Plan[]>([])
  const [nextCursor, setNextCursor] = useState<string>()
  const [templates, setTemplates] = useState<Template[]>([])
  const [repositories, setRepositories] = useState<Repository[]>([])
  const [agents, setAgents] = useState<Agent[]>([])
  const [hostID, setHostID] = useState('')
  const [selected, setSelected] = useState<Plan | null>(null)
  const [creating, setCreating] = useState(false)
  const [loading, setLoading] = useState(true)
  const [busy, setBusy] = useState(false)
  const [message, setMessage] = useState('')

  const handleError = useCallback((error: unknown) => {
    if (error instanceof ApiError && error.status === 401) { onUnauthorized(); return }
    if (error instanceof ApiError && error.problem?.code === 'PLAN_BINDING_INVALID') {
      setMessage('Agent 与仓库必须属于所选 Host。')
      return
    }
    if (error instanceof ApiError && error.problem?.code === 'PLAN_STATE_INVALID') {
      setMessage('当前状态不允许这个操作，请刷新后重试。')
      return
    }
    setMessage(errorMessage(error))
  }, [onUnauthorized])

  const load = useCallback((cursor?: string, signal?: AbortSignal) =>
    requestJSON<PlanList>(cursor ? `${endpoint}?cursor=${encodeURIComponent(cursor)}` : endpoint, { signal })
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
    void requestJSON<TemplateList>(`/api/v1/templates`, { signal: controller.signal })
      .then((page) => { if (!controller.signal.aborted) setTemplates(page.items) })
      .catch((error: unknown) => { if (!controller.signal.aborted) handleError(error) })
    void requestJSON<RepositoryList>(`/api/v1/repositories`, { signal: controller.signal })
      .then((page) => { if (!controller.signal.aborted) setRepositories(page.items) })
      .catch((error: unknown) => { if (!controller.signal.aborted) handleError(error) })
    return () => controller.abort()
  }, [load, handleError])

  useEffect(() => {
    if (!hostID) return
    const controller = new AbortController()
    void requestJSON<AgentList>(`/api/v1/hosts/${hostID}/agents`, { signal: controller.signal })
      .then((page) => { if (!controller.signal.aborted) setAgents(page.items) })
      .catch((error: unknown) => { if (!controller.signal.aborted) handleError(error) })
    return () => controller.abort()
  }, [hostID, handleError])

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (busy) return
    const form = new FormData(event.currentTarget)
    setBusy(true)
    setMessage('')
    try {
      const plan = await requestJSON<Plan>(endpoint, {
        method: 'POST',
        headers: { 'X-CSRF-Token': csrfToken() },
        body: JSON.stringify({
          name: String(form.get('name') ?? ''),
          host_id: String(form.get('host_id') ?? ''),
          agent_id: String(form.get('agent_id') ?? ''),
          repository_id: String(form.get('repository_id') ?? ''),
          template_id: String(form.get('template_id') ?? ''),
          template_revision: Number(form.get('template_revision') ?? 1),
        }),
      })
      setSelected(plan)
      setCreating(false)
      await load()
    } catch (error) { handleError(error) }
    finally { setBusy(false) }
  }

  async function transition(plan: Plan, action: 'apply' | 'pause' | 'resume') {
    if (busy) return
    setBusy(true)
    setMessage('')
    try {
      const updated = await requestJSON<Plan>(`${endpoint}/${plan.id}/${action}`, {
        method: 'POST',
        headers: { 'X-CSRF-Token': csrfToken(), 'If-Match': `"${plan.desired_revision}"` },
      })
      setSelected(updated)
      await load()
    } catch (error) { handleError(error) }
    finally { setBusy(false) }
  }

  async function backupNow(plan: Plan) {
    if (busy) return
    setBusy(true)
    setMessage('')
    try {
      const operation = await requestJSON<components['schemas']['BackupOperation']>(`${endpoint}/${plan.id}/backup`, {
        method: 'POST',
        headers: { 'X-CSRF-Token': csrfToken(), 'Idempotency-Key': crypto.randomUUID() },
      })
      setMessage(`已排队手动备份，操作 ${operation.id.slice(0, 8)}；Agent 下次心跳时执行。`)
    } catch (error) { handleError(error) }
    finally { setBusy(false) }
  }

  function reload() {
    setLoading(true)
    setMessage('')
    void load()
  }

  const hostName = (id: string) => hosts.find((host) => host.id === id)?.display_name ?? id
  const templateName = (id: string) => templates.find((template) => template.id === id)?.name ?? id
  const hostRepositories = repositories.filter((repo) => !hostID || repo.host_id === hostID)
  const eligibleHosts = hosts.filter((host) => host.status === 'PENDING' || host.status === 'ACTIVE')

  return (
    <section>
      <div className="section-heading">
        <div>
          <p className="eyebrow">AUTOMATION</p>
          <h1>计划</h1>
          <p className="muted">计划把 Host、Agent、仓库和模板 revision 绑定在一起；配置在中心合并并存档，Agent 只接收结果。</p>
        </div>
        <div className="actions">
          <button className="secondary-button" disabled={loading || busy} onClick={reload}>刷新列表</button>
          {canManage && <button className="primary-button" disabled={busy} onClick={() => { setCreating(true); setSelected(null) }}>创建计划</button>}
        </div>
      </div>
      <p className="warning-text" role="status">计划不会自动开始备份：先 apply 到 ACTIVE，Agent 接受配置后按计划执行。</p>
      {message && <p className="error-message" role="alert">{message}</p>}
      {creating && (
        <form aria-label="创建计划" className="credential-form" onSubmit={submit}>
          <label>计划名称<input name="name" maxLength={128} required /></label>
          <label>所属 Host<select name="host_id" required value={hostID} onChange={(event) => { setHostID(event.target.value); setAgents([]) }}>
            <option value="" disabled>选择 Host</option>
            {eligibleHosts.map((host) => <option key={host.id} value={host.id}>{host.display_name}</option>)}
          </select></label>
          <label>Agent<select name="agent_id" required defaultValue="" disabled={!hostID}>
            <option value="" disabled>选择 Agent</option>
            {agents.map((agent) => <option key={agent.id} value={agent.id}>{agent.id.slice(0, 8)}（{agent.status}）</option>)}
          </select></label>
          <label>仓库<select name="repository_id" required defaultValue="" disabled={!hostID}>
            <option value="" disabled>选择仓库</option>
            {hostRepositories.map((repo) => <option key={repo.id} value={repo.id}>{repo.name}（{repo.status}）</option>)}
          </select></label>
          <label>模板<select name="template_id" required defaultValue="">
            <option value="" disabled>选择模板</option>
            {templates.filter((template) => !template.archived_at).map((template) => <option key={template.id} value={template.id}>{template.name}</option>)}
          </select></label>
          <label>模板 revision<input name="template_revision" type="number" min={1} defaultValue={1} required /></label>
          {!eligibleHosts.length && <p>请先创建或启用一台 Host。</p>}
          {hostID && !agents.length && <p>该 Host 还没有已注册 Agent。</p>}
          {hostID && !hostRepositories.length && <p>该 Host 还没有仓库，先到「仓库」页创建。</p>}
          <div className="actions">
            <button className="primary-button" type="submit" disabled={busy}>{busy ? '处理中…' : '保存计划'}</button>
            <button className="table-link" type="button" disabled={busy} onClick={() => setCreating(false)}>取消</button>
          </div>
        </form>
      )}
      {selected && (
        <article className="credential-detail" aria-label="计划详情">
          <h2>{selected.name}</h2>
          <dl className="detail-list">
            <div><dt>计划 ID</dt><dd>{selected.id}</dd></div>
            <div><dt>Host</dt><dd>{hostName(selected.host_id)}</dd></div>
            <div><dt>模板</dt><dd>{templateName(selected.template_id)}（revision {selected.template_revision}）</dd></div>
            <div><dt>状态</dt><dd>{statusLabels[selected.status]}</dd></div>
            <div><dt>desired / accepted</dt><dd>{selected.desired_revision} / {selected.accepted_revision}</dd></div>
            <div><dt>调度</dt><dd>{describeCron(selected.effective_config.schedule_cron)}（{selected.effective_config.schedule_timezone}）</dd></div>
            <div><dt>路径</dt><dd>{selected.effective_config.paths.join('、')}</dd></div>
            <div><dt>备份健康</dt><dd>{healthLabels[selected.backup_health]}</dd></div>
          </dl>
          <p className="muted">effective config 是存档快照；模板后续变更不会自动影响此计划。</p>
          <div className="actions">
            {canManage && (selected.status === 'DRAFT' || selected.status === 'PENDING_APPLY' || selected.status === 'INVALID') &&
              <button className="primary-button" disabled={busy} onClick={() => { void transition(selected, 'apply') }}>应用配置</button>}
            {canManage && selected.status === 'PAUSED' &&
              <button className="primary-button" disabled={busy} onClick={() => { void transition(selected, 'resume') }}>恢复调度</button>}
            {canManage && selected.status === 'ACTIVE' &&
              <button className="secondary-button" disabled={busy} onClick={() => { void transition(selected, 'pause') }}>暂停调度</button>}
            {canManage && selected.status === 'ACTIVE' &&
              <button className="secondary-button" disabled={busy} onClick={() => { void backupNow(selected) }}>立即备份</button>}
            <button className="secondary-button" disabled={busy} onClick={reload}>刷新详情</button>
          </div>
        </article>
      )}
      {loading && <p aria-live="polite">正在加载计划…</p>}
      {!loading && !message && !items.length && <p className="muted">还没有计划。先准备模板、Host 和仓库。</p>}
      {items.length > 0 && <div className="table-wrap"><table>
        <thead><tr><th>计划</th><th>Host</th><th>状态</th><th>desired</th><th>操作</th></tr></thead>
        <tbody>{items.map((plan) => <tr key={plan.id}>
          <td>{plan.name}</td>
          <td>{hostName(plan.host_id)}</td>
          <td>{statusLabels[plan.status]}</td>
          <td>{plan.desired_revision}</td>
          <td><button className="table-link" disabled={busy} onClick={() => setSelected(plan)}>查看 {plan.name}</button></td>
        </tr>)}</tbody>
      </table></div>}
      {nextCursor && <button className="secondary-button" disabled={loading || busy} onClick={() => { setLoading(true); void load(nextCursor) }}>加载更多计划</button>}
    </section>
  )
}
