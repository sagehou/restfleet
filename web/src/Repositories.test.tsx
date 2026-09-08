import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, expect, it, vi } from 'vitest'
import { Repositories } from './Repositories'
import type { components } from './api/schema'

const host: components['schemas']['Host'] = {
  id: '0198f1da-2c57-7d3b-9c92-6e2f05293645', display_name: 'edge-01', description: '', labels: {},
  timezone: 'UTC', status: 'PENDING', revision: 1, created_at: '2026-09-07T00:00:00Z', updated_at: '2026-09-07T00:00:00Z',
}
const credential = { id: '0198f1da-2c57-7d3b-9c92-6e2f05293646', name: 'Archive credential', status: 'UNTESTED' }
const repository = {
  id: '0198f1da-2c57-7d3b-9c92-6e2f05293647', name: 'Archive', host_id: host.id, storage_credential_id: credential.id,
  status: 'PROVISIONING', gateway_secret_revision: 1, restic_secret_revision: 1, revision: 1,
  created_at: '2026-09-07T00:00:00Z', updated_at: '2026-09-07T00:00:00Z',
}
const endpoint = '/api/v1/repositories'
const respond = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status })
const onUnauthorized = vi.fn()

afterEach(() => {
  vi.unstubAllGlobals()
  onUnauthorized.mockClear()
})

async function fillCreateForm() {
  fireEvent.click(screen.getByRole('button', { name: '创建仓库' }))
  await screen.findByRole('option', { name: 'Archive credential（UNTESTED）' })
  fireEvent.change(screen.getByLabelText('仓库名称'), { target: { value: 'Archive' } })
  fireEvent.change(screen.getByLabelText('所属 Host'), { target: { value: host.id } })
  fireEvent.change(screen.getByLabelText('存储凭据'), { target: { value: credential.id } })
}

it('creates only metadata with CSRF and never implies that provisioning is ready', async () => {
  let created = false
  const fetch = vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    if (init?.method === 'POST') { created = true; return respond(repository, 201) }
    if (String(input).startsWith('/api/v1/storage-credentials')) return respond({ items: [credential] })
    return respond({ items: created ? [repository] : [] })
  })
  vi.stubGlobal('fetch', fetch)
  document.cookie = 'restfleet_csrf=repository-csrf'
  render(<Repositories hosts={[host]} canManage onUnauthorized={onUnauthorized} />)
  await screen.findByText(/还没有仓库/)
  await fillCreateForm()
  expect(screen.queryByLabelText(/密码/)).not.toBeInTheDocument()
  fireEvent.submit(screen.getByRole('form', { name: '创建独立仓库' }))
  await screen.findByRole('heading', { name: 'Archive' })
  const detail = screen.getByRole('article', { name: '仓库详情' })
  expect(within(detail).getByText('待完成初始化')).toBeInTheDocument()
  expect(within(detail).getByText('尚未验证')).toBeInTheDocument()
  expect(screen.getByRole('status')).toHaveTextContent('新仓库暂不可备份')
  expect(screen.getByRole('button', { name: '初始化仓库' })).toBeEnabled()
  const posted = fetch.mock.calls.find(([, init]) => init?.method === 'POST')?.[1]
  expect(JSON.parse(String(posted?.body))).toEqual({ name: 'Archive', host_id: host.id, storage_credential_id: credential.id })
  expect(new Headers(posted?.headers).get('X-CSRF-Token')).toBe('repository-csrf')
})

it('lets Viewers read details without creation controls or fetching credentials', async () => {
  const fetch = vi.fn(async (input: string | URL | Request) => respond(String(input) === endpoint ? { items: [repository] } : repository))
  vi.stubGlobal('fetch', fetch)
  render(<Repositories hosts={[host]} canManage={false} onUnauthorized={onUnauthorized} />)
  fireEvent.click(await screen.findByRole('button', { name: '查看 Archive' }))
  await screen.findByRole('heading', { name: 'Archive' })
  expect(screen.queryByRole('button', { name: '创建仓库' })).not.toBeInTheDocument()
  expect(screen.queryByRole('button', { name: '初始化仓库' })).not.toBeInTheDocument()
  expect(fetch.mock.calls.some(([path]) => String(path).includes('storage-credentials'))).toBe(false)
})

it('handles an ambiguous retry conflict and an expired session without automatic retries', async () => {
  let expired = false
  const fetch = vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    if (init?.method === 'POST') return expired ? respond({}, 401) : respond({ code: 'HOST_REPOSITORY_EXISTS' }, 409)
    return respond({ items: String(input).startsWith('/api/v1/storage-credentials') ? [credential] : [] })
  })
  vi.stubGlobal('fetch', fetch)
  render(<Repositories hosts={[host]} canManage onUnauthorized={onUnauthorized} />)
  await screen.findByText(/还没有仓库/)
  await fillCreateForm()
  fireEvent.submit(screen.getByRole('form', { name: '创建独立仓库' }))
  expect(await screen.findByRole('alert')).toHaveTextContent('请刷新列表查看创建结果')
  expect(fetch.mock.calls.filter(([, init]) => init?.method === 'POST')).toHaveLength(1)
  expired = true
  fireEvent.submit(screen.getByRole('form', { name: '创建独立仓库' }))
  await waitFor(() => expect(onUnauthorized).toHaveBeenCalledOnce())
})

it('pages credentials and repositories, excluding disabled inputs', async () => {
  const fetch = vi.fn(async (input: string | URL | Request) => {
    const path = String(input)
    if (path.startsWith('/api/v1/storage-credentials')) return respond(path.includes('cursor=')
      ? { items: [credential] }
      : { items: [{ ...credential, id: 'disabled', name: 'Disabled credential', status: 'DISABLED' }], next_cursor: 'credential-next' })
    return respond(path.includes('cursor=')
      ? { items: [{ ...repository, id: 'second', name: 'Second' }] }
      : { items: [repository], next_cursor: 'repository-next' })
  })
  vi.stubGlobal('fetch', fetch)
  render(<Repositories hosts={[host, { ...host, id: 'disabled', display_name: 'Disabled Host', status: 'DISABLED' }]} canManage onUnauthorized={onUnauthorized} />)
  fireEvent.click(await screen.findByRole('button', { name: '加载更多仓库' }))
  await screen.findByRole('button', { name: '查看 Second' })
  expect(screen.getByRole('button', { name: '查看 Archive' })).toBeInTheDocument()
  fireEvent.click(screen.getByRole('button', { name: '创建仓库' }))
  fireEvent.click(await screen.findByRole('button', { name: '加载更多凭据' }))
  await screen.findByRole('option', { name: 'Archive credential（UNTESTED）' })
  expect(screen.queryByRole('option', { name: /Disabled/ })).not.toBeInTheDocument()
  expect(screen.getByRole('button', { name: '保存仓库记录' })).toBeEnabled()
  expect(fetch.mock.calls.map(([path]) => String(path))).toContain('/api/v1/storage-credentials?limit=200&cursor=credential-next')
})

it('shows a failed list request as an error, not an empty repository list', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => respond({ detail: '数据库不可用', request_id: 'repo-request' }, 503)))
  render(<Repositories hosts={[host]} canManage onUnauthorized={onUnauthorized} />)
  expect(await screen.findByRole('alert')).toHaveTextContent('repo-request')
  expect(screen.queryByText(/还没有仓库/)).not.toBeInTheDocument()
})

it('reuses the initialization key after an ambiguous failure and keeps verified repositories PROVISIONING', async () => {
  const op = { id: '0198f1da-2c57-7d3b-9c92-6e2f05293648', repository_id: repository.id, storage_credential_id: credential.id,
    type: 'REPOSITORY_INITIALIZE', status: 'SUCCEEDED', finished_at: repository.created_at, error_code: '' }
  let attempts = 0
  const keys: (string | null)[] = []
  const fetch = vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    const path = String(input)
    if (init?.method === 'POST') {
      keys.push(new Headers(init.headers).get('Idempotency-Key'))
      expect(new Headers(init.headers).get('X-CSRF-Token')).toBe('initialize-csrf')
      expect(init.body).toBeUndefined()
      if (++attempts === 1) throw new TypeError('network interrupted')
      return respond(op, 202)
    }
    if (path.startsWith('/api/v1/operations/')) return respond(op)
    if (path === endpoint) return respond({ items: [repository] })
    return respond(attempts > 1 ? { ...repository, initialized_at: repository.created_at, format_version: 2, last_initialize_operation_id: op.id } : repository)
  })
  vi.stubGlobal('fetch', fetch)
  document.cookie = 'restfleet_csrf=initialize-csrf'
  render(<Repositories hosts={[host]} canManage onUnauthorized={onUnauthorized} />)
  fireEvent.click(await screen.findByRole('button', { name: '查看 Archive' }))
  fireEvent.click(await screen.findByRole('button', { name: '初始化仓库' }))
  await screen.findByRole('alert')
  fireEvent.click(screen.getByRole('button', { name: '初始化仓库' }))
  await screen.findByText('中心初始化已验证，等待 Gateway 和 Agent 确认', { exact: false })
  await waitFor(() => expect(screen.queryByRole('button', { name: '初始化仓库' })).not.toBeInTheDocument())
  expect(keys).toHaveLength(2)
  expect(keys[0]).toBeTruthy()
  expect(keys[0]).toBe(keys[1])
  expect(within(screen.getByRole('article', { name: '仓库详情' })).getByText('待完成初始化')).toBeInTheDocument()
})

it('restores the latest failed initialization operation when opening a repository', async () => {
  const op = { id: 'last-operation', repository_id: repository.id, status: 'FAILED', finished_at: repository.created_at, error_code: 'REPOSITORY_LOCKED' }
  vi.stubGlobal('fetch', vi.fn(async (input: string | URL | Request) => {
    const path = String(input)
    if (path.startsWith('/api/v1/operations/')) return respond(op)
    return respond(path === endpoint ? { items: [repository] } : { ...repository, last_initialize_operation_id: op.id })
  }))
  render(<Repositories hosts={[host]} canManage onUnauthorized={onUnauthorized} />)
  fireEvent.click(await screen.findByRole('button', { name: '查看 Archive' }))
  await waitFor(() => expect(screen.getByRole('status', { name: '初始化任务状态' })).toHaveTextContent('REPOSITORY_LOCKED'))
  expect(screen.getByRole('button', { name: '初始化仓库' })).toBeEnabled()
})


it('shows a durable credential ACK without treating the repository as READY', async () => {
  vi.stubGlobal('fetch', vi.fn(async (input: string | URL | Request) => respond(String(input) === endpoint
    ? { items: [repository] }
    : { ...repository, initialized_at: repository.created_at, agent_credential_revision: 2, agent_credential_accepted_at: repository.updated_at })))
  render(<Repositories hosts={[host]} canManage onUnauthorized={onUnauthorized} />)
  fireEvent.click(await screen.findByRole('button', { name: '查看 Archive' }))
  expect(await screen.findByText(/版本 2，已确认保存/)).toBeInTheDocument()
  expect(screen.getByText(/不代表 Gateway 已就绪或备份已成功/)).toBeInTheDocument()
})
