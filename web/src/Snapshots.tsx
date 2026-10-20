import { useCallback, useEffect, useState } from 'react'
import type { components } from './api/schema'
import { ApiError, errorMessage, requestJSON } from './api/client'

type Snapshot = components['schemas']['Snapshot']
type SnapshotList = components['schemas']['SnapshotList']
type Props = { onUnauthorized: () => void }

function size(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  const units = ['KB', 'MB', 'GB', 'TB']
  let value = bytes / 1024
  let index = 0
  while (value >= 1024 && index < units.length - 1) { value /= 1024; index += 1 }
  return `${value.toFixed(1)} ${units[index]}`
}

export function Snapshots({ onUnauthorized }: Props) {
  const [items, setItems] = useState<Snapshot[]>([])
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
          <p className="muted">成功备份后由终态结果建立索引；浏览文件树、下载与恢复在后续阶段接入。</p>
        </div>
        <div className="actions">
          <button className="secondary-button" disabled={loading} onClick={() => { setLoading(true); void load() }}>刷新列表</button>
        </div>
      </div>
      {message && <p className="error-message" role="alert">{message}</p>}
      {loading && <p aria-live="polite">正在加载快照…</p>}
      {!loading && !message && !items.length && <p className="muted">还没有快照。计划成功执行一次备份后会出现在这里。</p>}
      {items.length > 0 && <div className="table-wrap"><table>
        <thead><tr><th>快照</th><th>计划</th><th>采集时间</th><th>新增文件</th><th>新增数据</th></tr></thead>
        <tbody>{items.map((item) => <tr key={`${item.plan_id}-${item.snapshot_id}`}>
          <td>{item.snapshot_id.slice(0, 8)}</td>
          <td>{item.plan_id.slice(0, 8)}</td>
          <td>{new Date(item.captured_at).toLocaleString()}</td>
          <td>{item.files_new}</td>
          <td>{size(item.bytes_added)}</td>
        </tr>)}</tbody>
      </table></div>}
    </section>
  )
}
