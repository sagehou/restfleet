import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, expect, it, vi } from 'vitest'
import { StorageCredentials } from './StorageCredentials'

const credential = {
  id: '0198f1da-2c57-7d3b-9c92-6e2f05293645', name: 'Async', provider: 'RCLONE_ONEDRIVE',
  remote_name: 'encrypted', status: 'UNTESTED', secret_revision: 1, revision: 1,
  created_at: '2026-09-05T00:00:00Z', updated_at: '2026-09-05T00:00:00Z',
}
const operation = {
  id: '0198f1da-2c57-7d3b-9c92-6e2f05293646', type: 'CREDENTIAL_TEST', source: 'USER',
  storage_credential_id: credential.id, requested_by_user_id: credential.id,
  status: 'RUNNING', attempt: 1, secret_revision: 1, error_code: '',
  created_at: credential.created_at,
}
const endpoint = '/api/v1/storage-credentials'
const respond = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status })
afterEach(() => { vi.unstubAllGlobals() })

it('queues one operation, polls progress and refreshes metadata after success', async () => {
  let complete = false
  const fetch = vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    const path = String(input)
    if (init?.method === 'POST') return respond({ ...operation, status: 'QUEUED' }, 202)
    if (path.startsWith('/api/v1/operations/')) return respond(complete
      ? { ...operation, status: 'SUCCEEDED', finished_at: credential.updated_at } : operation)
    const current = complete ? { ...credential, status: 'HEALTHY', revision: 3, last_test_operation_id: operation.id, last_tested_at: credential.updated_at, last_test_result: 'SUCCEEDED' } : credential
    return respond(path === endpoint ? { items: [current] } : current)
  })
  vi.stubGlobal('fetch', fetch)
  document.cookie = 'restfleet_csrf=csrf-test'
  render(<StorageCredentials canManage onUnauthorized={vi.fn()} />)
  fireEvent.click(await screen.findByRole('button', { name: '查看 Async' }))
  fireEvent.click(await screen.findByRole('button', { name: '测试连接' }))
  await screen.findByText(/正在测试/)
  expect(screen.getByRole('button', { name: '测试进行中…' })).toBeDisabled()
  complete = true
  fireEvent.click(screen.getByRole('button', { name: '刷新测试状态' }))
  await screen.findByText(new RegExp('操作 ' + operation.id + '：读取成功'))
  await waitFor(() => expect(screen.getAllByText('可用').length).toBeGreaterThan(0))
  const posts = fetch.mock.calls.filter(([, init]) => init?.method === 'POST')
  expect(posts).toHaveLength(1)
  expect(posts[0][1]?.body).toBeUndefined()
  expect(new Headers(posts[0][1]?.headers).get('X-CSRF-Token')).toBe('csrf-test')
  expect(new Headers(posts[0][1]?.headers).get('Idempotency-Key')).toBeTruthy()
})

it('resumes a persisted operation without submitting again and shows safe failure codes', async () => {
  const current = { ...credential, last_test_operation_id: operation.id }
  const fetch = vi.fn(async (input: string | URL | Request) => respond(String(input).startsWith('/api/v1/operations/')
    ? { ...operation, status: 'FAILED', error_code: 'CREDENTIAL_CHANGED', finished_at: credential.updated_at }
    : String(input) === endpoint ? { items: [current] } : current))
  vi.stubGlobal('fetch', fetch)
  render(<StorageCredentials canManage={false} onUnauthorized={vi.fn()} />)
  fireEvent.click(await screen.findByRole('button', { name: '查看 Async' }))
  await screen.findByText(/测试未成功（CREDENTIAL_CHANGED）/)
  expect(screen.queryByRole('button', { name: '测试连接' })).not.toBeInTheDocument()
  expect(fetch.mock.calls.some(([input]) => String(input).endsWith('/test'))).toBe(false)
})

it('reuses the idempotency key after an ambiguous network failure', async () => {
  const keys: string[] = []
  const fetch = vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    if (init?.method === 'POST') {
      keys.push(new Headers(init.headers).get('Idempotency-Key') ?? '')
      throw new TypeError('network unavailable')
    }
    return respond(String(input) === endpoint ? { items: [credential] } : credential)
  })
  vi.stubGlobal('fetch', fetch)
  render(<StorageCredentials canManage onUnauthorized={vi.fn()} />)
  fireEvent.click(await screen.findByRole('button', { name: '查看 Async' }))
  fireEvent.click(await screen.findByRole('button', { name: '测试连接' }))
  await screen.findByRole('alert')
  await waitFor(() => expect(screen.getByRole('button', { name: '测试连接' })).not.toBeDisabled())
  fireEvent.click(screen.getByRole('button', { name: '测试连接' }))
  await waitFor(() => expect(keys).toHaveLength(2))
  expect(keys[0]).not.toBe('')
  expect(keys[0]).toBe(keys[1])
})
