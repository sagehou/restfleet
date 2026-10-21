import { useCallback, useEffect, useState } from 'react'
import type { components } from './api/schema'
import { ApiError, csrfToken, errorMessage, requestJSON } from './api/client'

type BackupOperation = components['schemas']['BackupOperation']
type BackupOperationList = components['schemas']['BackupOperationList']
type Props = { canManage: boolean; onUnauthorized: () => void }

const endpoint = '/api/v1/backup-operations'
const statusLabels: Record<BackupOperation['status'], string> = {
  QUEUED: '排队中', RUNNING: '运行中', SUCCEEDED: '成功', SUCCEEDED_WITH_WARNINGS: '成功（有告警）',
  FAILED: '失败', CANCEL_REQUESTED: '取消请求中', CANCELED: '已取消', MISSED: '错过执行', CONCURRENCY_SKIPPED: '并发跳过',
}

function terminal(status: BackupOperation['status']): boolean {
  return status === 'SUCCEEDED' || status === 'SUCCEEDED_WITH_WARNINGS' || status === 'FAILED' ||
    status === 'CANCELED' || status === 'MISSED' || status === 'CONCURRENCY_SKIPPED'
}

function retryable(status: BackupOperation['status']): boolean {
  return status === 'FAILED' || status === 'CANCELED' || status === 'MISSED' || status === 'CONCURRENCY_SKIPPED'
}

function percent(operation: BackupOperation): string {
  const total = operation.total_bytes ?? 0
  const done = operation.bytes_done ?? 0
  if (!total || !done) return '—'
  return `${Math.min(100, Math.round((done / total) * 100))}%`
}

export function Operations({ canManage, onUnauthorized }: Props) {
  const [items, setItems] = useState<BackupOperation[]>([])
  const [nextCursor, setNextCursor] = useState<string>()
  const [selected, setSelected] = useState<BackupOperation | null>(null)
  const [loading, setLoading] = useState(true)
  const [message, setMessage] = useState('')
  const [busyID, setBusyID] = useState('')

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

  const cancel = useCallback(async (operation: BackupOperation) => {
    if (!window.confirm(`取消操作 ${operation.id.slice(0, 8)}？运行中的备份会在 Agent 收到请求后停止。`)) return
    setBusyID(operation.id)
    setMessage('')
    try {
      const updated = await requestJSON<BackupOperation>(`${endpoint}/${operation.id}/cancel`, {
        method: 'POST',
        headers: { 'X-CSRF-Token': csrfToken() },
        body: JSON.stringify({ reason: '用户取消' }),
      })
      setItems((current) => current.map((item) => item.id === updated.id ? updated : item))
      setSelected((current) => current?.id === updated.id ? updated : current)
    } catch (error) {
      handleError(error)
    } finally {
      setBusyID('')
    }
  }, [handleError])

  const retry = useCallback(async (operation: BackupOperation) => {
    setBusyID(operation.id)
    setMessage('')
    try {
      const created = await requestJSON<BackupOperation>(`${endpoint}/${operation.id}/retry`, {
        method: 'POST',
        headers: { 'X-CSRF-Token': csrfToken() },
      })
      setItems((current) => [created, ...current])
    } catch (error) {
      handleError(error)
    } finally {
      setBusyID('')
    }
  }, [handleError])

  // Refresh while anything is running or cancellation is pending.
  useEffect(() => {
    if (!items.some((item) => item.status === 'RUNNING' || item.status === 'CANCEL_REQUESTED')) return
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
            {canManage && !terminal(selected.status) && selected.status !== 'CANCEL_REQUESTED' && (
              <button className="secondary-button" disabled={busyID === selected.id} onClick={() => { void cancel(selected) }}>取消</button>
            )}
            {canManage && retryable(selected.status) && (
              <button className="secondary-button" disabled={busyID === selected.id} onClick={() => { void retry(selected) }}>重试</button>
            )}
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
          <td>
            <button className="table-link" onClick={() => setSelected(item)}>查看</button>
            {canManage && !terminal(item.status) && item.status !== 'CANCEL_REQUESTED' && (
              <button className="table-link" disabled={busyID === item.id} onClick={() => { void cancel(item) }}>取消</button>
            )}
            {canManage && retryable(item.status) && (
              <button className="table-link" disabled={busyID === item.id} onClick={() => { void retry(item) }}>重试</button>
            )}
          </td>
        </tr>)}</tbody>
      </table></div>}
      {nextCursor && <button className="secondary-button" disabled={loading} onClick={() => { setLoading(true); void load(nextCursor) }}>加载更多</button>}
    </section>
  )
}
