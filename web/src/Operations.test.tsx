import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, expect, it, vi } from 'vitest'
import { Operations } from './Operations'

const operation = {
  id: '0198f1da-2c57-7d3b-9c92-6e2f05293645', plan_id: '0198f1da-2c57-7d3b-9c92-6e2f05293646',
  agent_id: '0198f1da-2c57-7d3b-9c92-6e2f05293647', type: 'BACKUP', source: 'SCHEDULED',
  status: 'SUCCEEDED', plan_revision: 1, created_at: '2026-11-06T00:00:00Z', updated_at: '2026-11-06T00:00:00Z',
}
const respond = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status })
afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks() })

it('shows unchanged success, complete summary, retry lineage and paged logs', async () => {
  const current = { ...operation, attempt: 2, parent_operation_id: operation.agent_id, root_operation_id: operation.agent_id,
    summary: { files_new: 2, files_changed: 3, files_unmodified: 4, data_added: 512, data_added_packed: 256, total_duration: 1.5, warning_count: 0 } }
  const fetch = vi.fn(async (input: string | URL | Request) => {
    const path = String(input)
    if (!path.includes('/logs?')) return respond({ items: [current] })
    const after = Number(new URL(path, 'https://restfleet.example').searchParams.get('after_sequence'))
    const items = Array.from({ length: after === 0 ? 100 : 1 }, (_, i) => ({ sequence: after + i + 1,
      observed_at: operation.created_at, section: 'backup', level: 'INFO', code: after ? 'LOG_TRUNCATED' : 'BACKUP_PROGRESS', files_done: i, bytes_done: i * 100 }))
    return respond({ items, next_sequence: items[items.length - 1].sequence })
  })
  vi.stubGlobal('fetch', fetch)
  render(<Operations canManage onUnauthorized={vi.fn()} />)
  fireEvent.click(await screen.findByRole('button', { name: '查看' }))
  await screen.findByText('无变化，未创建快照')
  expect(screen.getByText('新增 2 · 变化 3 · 未变 4')).toBeInTheDocument()
  expect(screen.getByText(/第 2 次/)).toBeInTheDocument()
  fireEvent.click(await screen.findByRole('button', { name: '下一页日志' }))
  await screen.findByText(/日志已达到上限/)
  fireEvent.click(screen.getByRole('button', { name: '上一页日志' }))
  await screen.findByRole('button', { name: '下一页日志' })
})

it('preserves retry idempotency after ambiguous delivery and shows partial-success warnings', async () => {
  const keys: string[] = []
  let succeeded = false
  const fetch = vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    if (init?.method === 'POST') {
      keys.push(new Headers(init.headers).get('Idempotency-Key') ?? '')
      if (!succeeded) throw new TypeError('network unavailable')
      return respond({ ...operation, id: operation.agent_id, status: 'QUEUED' }, 202)
    }
    if (String(input).includes('/logs?')) return respond({ items: [], next_sequence: 0 })
    return respond({ items: [{ ...operation, status: 'FAILED', error_code: 'REPOSITORY_LOCKED' }, { ...operation, id: operation.plan_id, status: 'SUCCEEDED_WITH_WARNINGS', error_code: 'PARTIAL_READ' }] })
  })
  vi.stubGlobal('fetch', fetch)
  render(<Operations canManage onUnauthorized={vi.fn()} />)
  await screen.findByText('成功（有告警）')
  fireEvent.click(await screen.findByRole('button', { name: '重试' }))
  await screen.findByRole('alert')
  await waitFor(() => expect(screen.getByRole('button', { name: '重试' })).not.toBeDisabled())
  succeeded = true
  fireEvent.click(screen.getByRole('button', { name: '重试' }))
  await screen.findByText('排队中')
  expect(keys).toHaveLength(2)
  expect(keys[0]).toBeTruthy()
  expect(keys[1]).toBe(keys[0])
})
