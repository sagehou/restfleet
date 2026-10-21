import { useCallback, useEffect, useState } from 'react'
import type { components } from './api/schema'
import { ApiError, errorMessage, requestJSON, requestJSONResponse } from './api/client'

type Snapshot = components['schemas']['Snapshot']
type SnapshotList = components['schemas']['SnapshotList']
type SnapshotEntry = components['schemas']['SnapshotEntry']
type SnapshotEntries = components['schemas']['SnapshotEntries']
type SnapshotScanPending = components['schemas']['SnapshotScanPending']
type Operation = components['schemas']['Operation']
type Props = { onUnauthorized: () => void }

function size(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  const units = ['KB', 'MB', 'GB', 'TB']
  let value = bytes / 1024
  let index = 0
  while (value >= 1024 && index < units.length - 1) { value /= 1024; index += 1 }
  return `${value.toFixed(1)} ${units[index]}`
}

function breadcrumbs(path: string): { label: string; path: string }[] {
  const crumbs = [{ label: '根目录', path: '/' }]
  let current = ''
  for (const part of path.split('/').filter(Boolean)) {
    current += `/${part}`
    crumbs.push({ label: part, path: current })
  }
  return crumbs
}

function SnapshotBrowser({ snapshot, onClose, onUnauthorized }: {
  snapshot: Snapshot
  onClose: () => void
  onUnauthorized: () => void
}) {
  const [path, setPath] = useState('/')
  const [cursor, setCursor] = useState('')
  const [entries, setEntries] = useState<SnapshotEntries | null>(null)
  const [loading, setLoading] = useState(true)
  const [loadingMore, setLoadingMore] = useState(false)
  const [waiting, setWaiting] = useState(false)
  const [message, setMessage] = useState('')

  const handleError = useCallback((error: unknown) => {
    if (error instanceof ApiError && error.status === 401) { onUnauthorized(); return }
    setMessage(errorMessage(error))
  }, [onUnauthorized])

  const goTo = (next: string) => {
    if (next === path) return
    setLoading(true)
    setLoadingMore(false)
    setWaiting(false)
    setMessage('')
    setEntries(null)
    setCursor('')
    setPath(next)
  }

  useEffect(() => {
    let active = true
    let timer: number | undefined
    const controller = new AbortController()
    const endpoint = `/api/v1/snapshots/${encodeURIComponent(snapshot.snapshot_id)}/entries?path=${encodeURIComponent(path)}&repository_id=${encodeURIComponent(snapshot.repository_id)}${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`
    async function pollOperation(operationID: string, remaining: number) {
      try {
        const operation = await requestJSON<Operation>(`/api/v1/operations/${encodeURIComponent(operationID)}`, { signal: controller.signal })
        if (!active) return
        if (operation.status === 'SUCCEEDED' || operation.status === 'SUCCEEDED_WITH_WARNINGS') {
          await fetchPage(remaining - 1)
          return
        }
        if (operation.status === 'FAILED' || operation.status === 'TIMED_OUT' || operation.status === 'CANCELED' ||
          operation.status === 'LOST' || operation.status === 'REJECTED') {
          setWaiting(false)
          setMessage(`扫描失败：${operation.error_code || operation.status}`)
          return
        }
        if (remaining <= 0) {
          setMessage('扫描时间较长，请稍后重试。')
          return
        }
        timer = window.setTimeout(() => { void pollOperation(operationID, remaining - 1) }, 1500)
      } catch (error) {
        if (active && !controller.signal.aborted) {
          setWaiting(false)
          handleError(error)
        }
      }
    }
    async function fetchPage(remaining: number) {
      try {
        const response = await requestJSONResponse<SnapshotEntries | SnapshotScanPending>(endpoint, { signal: controller.signal })
        if (!active) return
        if (response.status === 202) {
          setLoading(false)
          setLoadingMore(false)
          setWaiting(true)
          const pending = response.body as SnapshotScanPending
          if (remaining <= 0) {
            setMessage('扫描时间较长，请稍后重试。')
            return
          }
          timer = window.setTimeout(() => { void pollOperation(pending.operation_id, remaining - 1) }, 1500)
          return
        }
        setWaiting(false)
        setLoading(false)
        setLoadingMore(false)
        const page = response.body as SnapshotEntries
        if (cursor) {
          setEntries((previous) => previous ? { ...page, items: [...previous.items, ...page.items] } : page)
        } else {
          setEntries(page)
        }
      } catch (error) {
        if (active && !controller.signal.aborted) {
          setLoading(false)
          setLoadingMore(false)
          setWaiting(false)
          handleError(error)
        }
      }
    }
    void fetchPage(60)
    return () => {
      active = false
      controller.abort()
      if (timer !== undefined) window.clearTimeout(timer)
    }
  }, [snapshot.snapshot_id, snapshot.repository_id, path, cursor, handleError])

  const navigate = (entry: SnapshotEntry) => {
    if (entry.type === 'dir') goTo(entry.path)
  }

  return (
    <article className="credential-detail" aria-label="快照文件浏览">
      <div className="section-heading">
        <div>
          <p className="eyebrow">SNAPSHOT</p>
          <h2>{snapshot.snapshot_id.slice(0, 12)}</h2>
          <p className="muted">采集于 {new Date(snapshot.captured_at).toLocaleString()}；浏览结果来自有界扫描缓存。</p>
        </div>
        <div className="actions">
          <button className="secondary-button" onClick={onClose}>关闭浏览</button>
        </div>
      </div>
      <nav aria-label="目录路径">
        {breadcrumbs(path).map((crumb) => (
          <button key={crumb.path} className="table-link" onClick={() => goTo(crumb.path)}>{crumb.label}</button>
        ))}
      </nav>
      {message && <p className="error-message" role="alert">{message}</p>}
      {(loading || waiting) && <p aria-live="polite">{waiting ? '正在读取快照目录，稍后自动刷新…' : '正在加载目录…'}</p>}
      {entries && <>
        {entries.truncated && <p className="muted">{entries.next_cursor
          ? `已加载 ${entries.items.length} 项目录条目，可继续加载。`
          : `目录条目过多，本次仅显示前 ${entries.items.length} 项。`}</p>}
        {!entries.items.length && <p className="muted">这个目录是空的。</p>}
        {entries.items.length > 0 && <div className="table-wrap"><table>
          <thead><tr><th>名称</th><th>类型</th><th>大小</th><th>修改时间</th></tr></thead>
          <tbody>{entries.items.map((entry) => <tr key={entry.path}>
            <td>{entry.type === 'dir'
              ? <button className="table-link" onClick={() => navigate(entry)}>{entry.name}/</button>
              : entry.name}</td>
            <td>{entry.type === 'dir' ? '目录' : entry.type === 'file' ? '文件' : entry.type}</td>
            <td>{entry.type === 'dir' ? '—' : size(entry.size)}</td>
            <td>{new Date(entry.mtime).toLocaleString()}</td>
          </tr>)}</tbody>
        </table></div>}
        {entries.next_cursor && <div className="actions">
          <button className="secondary-button" disabled={loadingMore}
            onClick={() => { setLoadingMore(true); setCursor(entries.next_cursor ?? '') }}>{loadingMore ? '加载中…' : '加载更多'}</button>
        </div>}
        <p className="muted">缓存于 {new Date(entries.collected_at).toLocaleString()}。</p>
      </>}
    </article>
  )
}

export function Snapshots({ onUnauthorized }: Props) {
  const [items, setItems] = useState<Snapshot[]>([])
  const [selected, setSelected] = useState<Snapshot | null>(null)
  const [loading, setLoading] = useState(true)
  const [message, setMessage] = useState('')

  const handleError = useCallback((error: unknown) => {
    if (error instanceof ApiError && error.status === 401) { onUnauthorized(); return }
    setMessage(errorMessage(error))
  }, [onUnauthorized])

  const load = useCallback((signal?: AbortSignal) =>
    requestJSON<SnapshotList>('/api/v1/snapshots?limit=100', { signal })
      .then((page) => { if (!signal?.aborted) setItems(page.items) })
      .catch((error: unknown) => { if (!signal?.aborted) handleError(error) })
      .finally(() => { if (!signal?.aborted) setLoading(false) }),
  [handleError])

  useEffect(() => {
    const controller = new AbortController()
    void load(controller.signal)
    return () => controller.abort()
  }, [load])

  return (
    <section>
      <div className="section-heading">
        <div>
          <p className="eyebrow">RECOVERY</p>
          <h1>快照</h1>
          <p className="muted">成功备份后由终态结果建立索引；文件树按需扫描，下载与恢复在后续阶段接入。</p>
        </div>
        <div className="actions">
          <button className="secondary-button" disabled={loading} onClick={() => { setLoading(true); void load() }}>刷新列表</button>
        </div>
      </div>
      {message && <p className="error-message" role="alert">{message}</p>}
      {selected && <SnapshotBrowser key={`${selected.repository_id}-${selected.snapshot_id}`} snapshot={selected} onClose={() => setSelected(null)} onUnauthorized={onUnauthorized} />}
      {loading && <p aria-live="polite">正在加载快照…</p>}
      {!loading && !message && !items.length && <p className="muted">还没有快照。计划成功执行一次备份后会出现在这里。</p>}
      {items.length > 0 && <div className="table-wrap"><table>
        <thead><tr><th>快照</th><th>计划</th><th>采集时间</th><th>新增文件</th><th>新增数据</th><th></th></tr></thead>
        <tbody>{items.map((item) => <tr key={`${item.repository_id}-${item.snapshot_id}`}>
          <td>{item.snapshot_id.slice(0, 8)}</td>
          <td>{item.plan_id ? item.plan_id.slice(0, 8) : 'UNMANAGED'}</td>
          <td>{new Date(item.captured_at).toLocaleString()}</td>
          <td>{item.files_new}</td>
          <td>{size(item.bytes_added)}</td>
          <td><button className="table-link" onClick={() => setSelected(item)}>浏览</button></td>
        </tr>)}</tbody>
      </table></div>}
    </section>
  )
}
