import { useCallback, useEffect, useState } from 'react'
import type { components } from './api/schema'
import { ApiError, errorMessage, requestJSON } from './api/client'

type BackupOperation = components['schemas']['BackupOperation']
type BackupOperationList = components['schemas']['BackupOperationList']
type Props = { canManage: boolean; onUnauthorized: () => void }

const endpoint = '/api/v1/backup-operations'
const statusLabels: Record<BackupOperation['status'], string> = {
  QUEUED: '排队中', RUNNING: '运行中', SUCCEEDED: '成功', SUCCEEDED_WITH_WARNINGS: '成功（有告警）',
  FAILED: '失败', CANCELED: '已取消', MISSED: '错过执行', CONCURRENCY_SKIPPED: '并发跳过',
}

function percent(operation: BackupOperation): string {
  const total = operation.total_bytes ?? 0
  const done = operation.bytes_done ?? 0
  if (!total || !done) return '—'
  return `${Math.min(100, Math.round((done / total) * 100))}%`
}

export function Operations({ onUnauthorized }: Props) {
  const [items, setItems] = useState<BackupOperation[]>([])
  const [nextCursor, setNextCursor] = useState<string>()
  const [selected, setSelected] = useState<BackupOperation | null>(null)
  const [loading, setLoading] = useState(true)
  const [message, setMessage] = useState('')

  const handleError = useCallback((error: unknown) => {
    if (error instanceof ApiError && error.status === 401) { onUnauthorized(); return }
    setMessage(errorMessage(error))
  }, [onUnauthorized])

  const load = useCallback((cursor?: string, signal?: AbortSignal) =>
    requestJSON<BackupOperationList>(cursor ? `${endpoint}?cursor=${encodeURIComponent(cursor)}` : endpoint, { signal })
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

  // Refresh while anything is running so progress moves without manual reload.
  useEffect(() => {
    if (!items.some((item) => item.status === 'RUNNING')) return
    const timer = setInterval(() => { void load() }, 5000)
    return () => clearInterval(timer)
  }, [items, load])

  return (
    <section>
      <div className="section-heading">
        <div>
          <p className="eyebrow">EXECUTION</p>
          <h1>操作</h1>
          <p className="muted">Agent 本地调度并执行，进度与终态回传中心；离线期间的记录会在重连后补报。</p>
        </div>
        <div className="actions">
          <button className="secondary-button" disabled={loading} onClick={() => { setLoading(true); void load() }}>刷新列表</button>
        </div>
      </div>
      {message && <p className="error-message" role="alert">{message}</p>}
      {selected && (
        <article className="credential-detail" aria-label="操作详情">
          <h2>{statusLabels[selected.status]} · {selected.id.slice(0, 8)}</h2>
          <dl className="detail-list">
            <div><dt>操作 ID</dt><dd>{selected.id}</dd></div>
            <div><dt>计划</dt><dd>{selected.plan_id}（revision {selected.plan_revision}）</dd></div>
            <div><dt>触发</dt><dd>{selected.source === 'SCHEDULED' ? '本地调度' : '手动'}</dd></div>
            <div><dt>计划时间</dt><dd>{selected.scheduled_at ?? '—'}</dd></div>
            <div><dt>开始 / 结束</dt><dd>{selected.started_at ?? '—'} / {selected.finished_at ?? '—'}</dd></div>
            <div><dt>进度</dt><dd>{percent(selected)}（{selected.files_done ?? 0}/{selected.total_files ?? 0} 文件，{selected.bytes_done ?? 0}/{selected.total_bytes ?? 0} 字节）</dd></div>
            <div><dt>快照</dt><dd>{selected.snapshot_id || '—'}</dd></div>
            <div><dt>退出码 / 错误</dt><dd>{selected.exit_code ?? '—'} {selected.error_code || ''}</dd></div>
          </dl>
          <div className="actions">
            <button className="secondary-button" onClick={() => setSelected(null)}>关闭详情</button>
          </div>
        </article>
      )}
      {loading && <p aria-live="polite">正在加载操作…</p>}
      {!loading && !message && !items.length && <p className="muted">还没有操作记录。计划到点后由 Agent 本地触发。</p>}
      {items.length > 0 && <div className="table-wrap"><table>
        <thead><tr><th>操作</th><th>状态</th><th>计划时间</th><th>进度</th><th></th></tr></thead>
        <tbody>{items.map((item) => <tr key={item.id}>
          <td>{item.id.slice(0, 8)}</td>
          <td>{statusLabels[item.status]}</td>
          <td>{item.scheduled_at ?? '—'}</td>
          <td>{percent(item)}</td>
          <td><button className="table-link" onClick={() => setSelected(item)}>查看</button></td>
        </tr>)}</tbody>
      </table></div>}
      {nextCursor && <button className="secondary-button" disabled={loading} onClick={() => { setLoading(true); void load(nextCursor) }}>加载更多</button>}
    </section>
  )
}
