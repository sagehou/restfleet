import { useCallback, useEffect, useRef, useState } from 'react'
import type { components } from './api/schema'
import { ApiError, csrfToken, errorMessage, requestJSON } from './api/client'

type BackupOperation = components['schemas']['BackupOperation']
type BackupOperationList = components['schemas']['BackupOperationList']
type Props = { canManage: boolean; onUnauthorized: () => void }

const endpoint = '/api/v1/backup-operations'
const statusLabels: Record<BackupOperation['status'], string> = {
  QUEUED: '排队中', RUNNING: '运行中', SUCCEEDED: '成功', SUCCEEDED_WITH_WARNINGS: '成功（有告警）',
  FAILED: '失败', CANCEL_REQUESTED: '取消请求中', CANCELED: '已取消', LOST: '已丢失', MISSED: '错过执行', CONCURRENCY_SKIPPED: '并发跳过',
}

function terminal(status: BackupOperation['status']): boolean {
  return status === 'SUCCEEDED' || status === 'SUCCEEDED_WITH_WARNINGS' || status === 'FAILED' ||
    status === 'CANCELED' || status === 'LOST' || status === 'MISSED' || status === 'CONCURRENCY_SKIPPED'
}

function retryable(status: BackupOperation['status']): boolean {
  return status === 'FAILED' || status === 'CANCELED' || status === 'LOST' || status === 'MISSED' || status === 'CONCURRENCY_SKIPPED'
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
  const requestKeys = useRef(new Map<string, string>())
  const actionKey = useCallback((action: string, id: string) => {
    const scope = `${action}:${id}`
    let key = requestKeys.current.get(scope)
    if (!key) { key = crypto.randomUUID(); requestKeys.current.set(scope, key) }
    return key
  }, [])

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
        setSelected((current) => current ? page.items.find((item) => item.id === current.id) ?? current : null)
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
        headers: { 'X-CSRF-Token': csrfToken(), 'Idempotency-Key': actionKey('cancel', operation.id) },
        body: JSON.stringify({ reason: '用户取消' }),
      })
      requestKeys.current.delete(`cancel:${operation.id}`)
      setItems((current) => current.map((item) => item.id === updated.id ? updated : item))
      setSelected((current) => current?.id === updated.id ? updated : current)
    } catch (error) {
      handleError(error)
    } finally {
      setBusyID('')
    }
  }, [handleError, actionKey])

  const retry = useCallback(async (operation: BackupOperation) => {
    setBusyID(operation.id)
    setMessage('')
    try {
      const created = await requestJSON<BackupOperation>(`${endpoint}/${operation.id}/retry`, {
        method: 'POST',
        headers: { 'X-CSRF-Token': csrfToken(), 'Idempotency-Key': actionKey('retry', operation.id) },
      })
      requestKeys.current.delete(`retry:${operation.id}`)
      setItems((current) => [created, ...current.filter((item) => item.id !== created.id)])
    } catch (error) {
      handleError(error)
    } finally {
      setBusyID('')
    }
  }, [handleError, actionKey])

  // Refresh while anything is running or cancellation is pending.
  useEffect(() => {
    if (!items.some((item) => item.status === 'QUEUED' || item.status === 'RUNNING' || item.status === 'CANCEL_REQUESTED')) return
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
            <div><dt>快照</dt><dd>{selected.snapshot_id || (selected.status === 'SUCCEEDED' ? '无变化，未创建快照' : '—')}</dd></div>
            <div><dt>退出码 / 错误</dt><dd>{selected.exit_code ?? '—'} {selected.error_code || ''}</dd></div>
            {selected.parent_operation_id && <div><dt>重试关联</dt><dd>第 {selected.attempt ?? 1} 次 · 前次 {selected.parent_operation_id} · 起始 {selected.root_operation_id}</dd></div>}
            {selected.summary && <>
              <div><dt>文件统计</dt><dd>新增 {selected.summary.files_new} · 变化 {selected.summary.files_changed} · 未变 {selected.summary.files_unmodified}</dd></div>
              <div><dt>写入 / 耗时</dt><dd>{selected.summary.data_added} 字节（压缩后 {selected.summary.data_added_packed}）· {selected.summary.total_duration.toFixed(1)} 秒 · {selected.summary.warning_count} 个警告</dd></div>
            </>}
          </dl>
          <OperationLogs key={selected.id} operationID={selected.id} onError={handleError} />
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

function OperationLogs({ operationID, onError }: { operationID: string; onError: (error: unknown) => void }) {
  const [after, setAfter] = useState(0)
  const [page, setPage] = useState<components['schemas']['BackupLogPage']>({ items: [], next_sequence: 0 })
  const [history, setHistory] = useState<number[]>([])
  useEffect(() => {
    const controller = new AbortController()
    const load = () => requestJSON<components['schemas']['BackupLogPage']>(`${endpoint}/${operationID}/logs?after_sequence=${after}&limit=100`, { signal: controller.signal })
      .then((result) => { if (!controller.signal.aborted) setPage(result) })
      .catch((error: unknown) => { if (!controller.signal.aborted) onError(error) })
    void load()
    const timer = setInterval(() => { void load() }, 5000)
    return () => { controller.abort(); clearInterval(timer) }
  }, [operationID, after, onError])
  return <section aria-label="执行日志">
    <h3>执行日志</h3>
    {!page.items.length && <p className="muted">暂无日志；离线记录会在结果确认后继续补报。</p>}
    <ol>{page.items.map((entry) => <li key={entry.sequence}>
      <time>{entry.observed_at}</time> [{entry.section}] {entry.level} · {entry.code}
      {entry.code === 'BACKUP_PROGRESS' && ` · ${entry.files_done} 文件 / ${entry.bytes_done} 字节`}
      {entry.code === 'LOG_TRUNCATED' && ' · 日志已达到上限，保留摘要、错误及最终结果。'}
    </li>)}</ol>
    <div className="actions">
      {history.length > 0 && <button className="secondary-button" onClick={() => { setAfter(history[history.length - 1]); setHistory(history.slice(0, -1)) }}>上一页日志</button>}
      {page.items.length === 100 && <button className="secondary-button" onClick={() => { setHistory([...history, after]); setAfter(page.next_sequence) }}>下一页日志</button>}
    </div>
  </section>
}
