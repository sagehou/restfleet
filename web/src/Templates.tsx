import { type FormEvent, useCallback, useEffect, useState } from 'react'
import type { components } from './api/schema'
import { ApiError, csrfToken, errorMessage, requestJSON } from './api/client'
import { describeCron } from './cron'

type Template = components['schemas']['Template']
type TemplateList = components['schemas']['TemplateList']
type TemplateConfig = components['schemas']['TemplateConfig']
type Props = { canManage: boolean; onUnauthorized: () => void }

const endpoint = '/api/v1/templates'

function pathsFrom(text: string): string[] {
  return text.split(/[\n,]/).map((item) => item.trim()).filter(Boolean)
}

export function Templates({ canManage, onUnauthorized }: Props) {
  const [items, setItems] = useState<Template[]>([])
  const [nextCursor, setNextCursor] = useState<string>()
  const [selected, setSelected] = useState<Template | null>(null)
  const [creating, setCreating] = useState(false)
  const [loading, setLoading] = useState(true)
  const [busy, setBusy] = useState(false)
  const [message, setMessage] = useState('')

  const handleError = useCallback((error: unknown) => {
    if (error instanceof ApiError && error.status === 401) { onUnauthorized(); return }
    if (error instanceof ApiError && error.status === 409 && error.problem?.code === 'TEMPLATE_IN_USE') {
      setMessage('还有未归档的计划引用此模板，先处理依赖计划再归档。')
      return
    }
    setMessage(errorMessage(error))
  }, [onUnauthorized])

  const load = useCallback((cursor?: string, signal?: AbortSignal) =>
    requestJSON<TemplateList>(cursor ? `${endpoint}?cursor=${encodeURIComponent(cursor)}` : endpoint, { signal })
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

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (busy) return
    const form = new FormData(event.currentTarget)
    const config: TemplateConfig = {
      paths: pathsFrom(String(form.get('paths') ?? '')),
      schedule_cron: String(form.get('schedule_cron') ?? ''),
      schedule_timezone: String(form.get('schedule_timezone') ?? ''),
      jitter_seconds: Number(form.get('jitter_seconds') ?? 0),
      misfire_grace_seconds: Number(form.get('misfire_grace_seconds') ?? 900),
      exclude_caches: form.get('exclude_caches') === 'on',
      one_file_system: form.get('one_file_system') === 'on',
      exclude_patterns: pathsFrom(String(form.get('exclude_patterns') ?? '')),
      enabled_by_default: true,
    }
    setBusy(true)
    setMessage('')
    try {
      const template = await requestJSON<Template>(endpoint, {
        method: 'POST',
        headers: { 'X-CSRF-Token': csrfToken() },
        body: JSON.stringify({ name: String(form.get('name') ?? ''), description: String(form.get('description') ?? ''), config }),
      })
      setSelected(template)
      setCreating(false)
      await load()
    } catch (error) { handleError(error) }
    finally { setBusy(false) }
  }

  async function archive(template: Template) {
    if (busy) return
    setBusy(true)
    setMessage('')
    try {
      const updated = await requestJSON<Template>(`${endpoint}/${template.id}/archive`, {
        method: 'POST',
        headers: { 'X-CSRF-Token': csrfToken(), 'If-Match': `"${template.revision}"` },
      })
      setSelected(updated)
      await load()
    } catch (error) { handleError(error) }
    finally { setBusy(false) }
  }

  function reload() {
    setLoading(true)
    setMessage('')
    void load()
  }

  return (
    <section>
      <div className="section-heading">
        <div>
          <p className="eyebrow">AUTOMATION</p>
          <h1>模板</h1>
          <p className="muted">模板保存备份目录与调度；每次修改产生不可变 revision，不会自动应用到已有计划。</p>
        </div>
        <div className="actions">
          <button className="secondary-button" disabled={loading || busy} onClick={reload}>刷新列表</button>
          {canManage && <button className="primary-button" disabled={busy} onClick={() => { setCreating(true); setSelected(null) }}>创建模板</button>}
        </div>
      </div>
      <p className="warning-text" role="status">模板只描述意图；真正执行备份与本地调度仍待 M5/M6 接线。</p>
      {message && <p className="error-message" role="alert">{message}</p>}
      {creating && (
        <form aria-label="创建模板" className="credential-form" onSubmit={submit}>
          <label>模板名称<input name="name" maxLength={128} required /></label>
          <label>描述<input name="description" maxLength={1024} /></label>
          <label>备份路径（每行或逗号分隔）<textarea name="paths" required rows={3} placeholder="/srv/data" /></label>
          <label>排除规则（可选）<textarea name="exclude_patterns" rows={2} placeholder="*.tmp" /></label>
          <label>Cron（5 字段）<input name="schedule_cron" required defaultValue="0 3 * * *" /></label>
          <label>时区<input name="schedule_timezone" required defaultValue="Asia/Shanghai" /></label>
          <label>Jitter（秒）<input name="jitter_seconds" type="number" min={0} max={3600} defaultValue={0} /></label>
          <label>Misfire 宽限（秒）<input name="misfire_grace_seconds" type="number" min={0} max={86400} defaultValue={900} /></label>
          <label><input name="exclude_caches" type="checkbox" /> 排除缓存目录</label>
          <label><input name="one_file_system" type="checkbox" /> 不跨文件系统</label>
          <div className="actions">
            <button className="primary-button" type="submit" disabled={busy}>{busy ? '处理中…' : '保存模板'}</button>
            <button className="table-link" type="button" disabled={busy} onClick={() => setCreating(false)}>取消</button>
          </div>
        </form>
      )}
      {selected && (
        <article className="credential-detail" aria-label="模板详情">
          <h2>{selected.name}</h2>
          <dl className="detail-list">
            <div><dt>模板 ID</dt><dd>{selected.id}</dd></div>
            <div><dt>当前 revision</dt><dd>{selected.revision}</dd></div>
            <div><dt>描述</dt><dd>{selected.description || '（无）'}</dd></div>
            <div><dt>备份路径</dt><dd>{selected.config.paths.join('、')}</dd></div>
            <div><dt>调度</dt><dd>{describeCron(selected.config.schedule_cron)}（{selected.config.schedule_cron}，{selected.config.schedule_timezone}）</dd></div>
            <div><dt>抖动 / 宽限</dt><dd>{selected.config.jitter_seconds ?? 0}s / {selected.config.misfire_grace_seconds ?? 900}s</dd></div>
            <div><dt>状态</dt><dd>{selected.archived_at ? `已归档（${selected.archived_at}）` : '使用中'}</dd></div>
          </dl>
          <p className="muted">修改模板会产生新 revision；计划需要显式应用才会更新 effective config。</p>
          <div className="actions">
            {canManage && !selected.archived_at && <button className="secondary-button" disabled={busy} onClick={() => { void archive(selected) }}>归档模板</button>}
            <button className="secondary-button" disabled={busy} onClick={reload}>刷新详情</button>
          </div>
        </article>
      )}
      {loading && <p aria-live="polite">正在加载模板…</p>}
      {!loading && !message && !items.length && <p className="muted">还没有模板。先创建模板，再为计划选择 revision。</p>}
      {items.length > 0 && <div className="table-wrap"><table>
        <thead><tr><th>模板</th><th>调度</th><th>revision</th><th>操作</th></tr></thead>
        <tbody>{items.map((template) => <tr key={template.id}>
          <td>{template.name}</td>
          <td>{describeCron(template.config.schedule_cron)}</td>
          <td>{template.revision}</td>
          <td><button className="table-link" disabled={busy} onClick={() => setSelected(template)}>查看 {template.name}</button></td>
        </tr>)}</tbody>
      </table></div>}
      {nextCursor && <button className="secondary-button" disabled={loading || busy} onClick={() => { setLoading(true); void load(nextCursor) }}>加载更多模板</button>}
    </section>
  )
}
