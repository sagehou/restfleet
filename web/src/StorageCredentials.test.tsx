import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, expect, it, vi } from 'vitest'
import { StorageCredentials } from './StorageCredentials'

const credential = {
  id: '0198f1da-2c57-7d3b-9c92-6e2f05293645', name: 'Archive', provider: 'RCLONE_ONEDRIVE',
  remote_name: 'encrypted', status: 'UNTESTED', secret_revision: 2, revision: 3,
  created_at: '2026-09-05T00:00:00Z', updated_at: '2026-09-05T00:00:00Z',
}
const endpoint = '/api/v1/storage-credentials'
const respond = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status })
const onUnauthorized = vi.fn()

afterEach(() => {
  vi.unstubAllGlobals()
  onUnauthorized.mockClear()
})

it('clears imported secrets before a failing request and never caches them', async () => {
  let finish: (response: Response) => void = () => undefined
  const fetch = vi.fn(async (_input: string | URL | Request, init?: RequestInit) => {
    if (init?.method === 'POST') return new Promise<Response>((resolve) => { finish = resolve })
    return respond({ items: [] })
  })
  vi.stubGlobal('fetch', fetch)
  document.cookie = 'restfleet_csrf=csrf-test'
  render(<StorageCredentials canManage onUnauthorized={onUnauthorized} />)
  await screen.findByText(/还没有存储凭据/)
  fireEvent.click(screen.getByRole('button', { name: '导入凭据' }))
  fireEvent.change(screen.getByLabelText('凭据名称'), { target: { value: 'Archive' } })
  fireEvent.change(screen.getByLabelText('Crypt remote 名称'), { target: { value: 'encrypted' } })
  fireEvent.change(screen.getByLabelText('rclone 配置'), { target: { value: 'canary-secret-config' } })
  fireEvent.submit(screen.getByRole('form', { name: '导入存储凭据' }))
  expect(screen.getByLabelText('rclone 配置')).toHaveValue('')
  expect(screen.getByRole('button', { name: '保存中…' })).toBeDisabled()
  finish(respond({ detail: '服务暂时不可用', request_id: 'test-request' }, 503))
  expect(await screen.findByRole('alert')).toHaveTextContent('test-request')
  expect(screen.getByLabelText('rclone 配置')).toHaveValue('')
  const posted = fetch.mock.calls.find(([, init]) => init?.method === 'POST')?.[1]
  expect(JSON.parse(String(posted?.body))).toEqual({ name: 'Archive', remote_name: 'encrypted', rclone_config: 'canary-secret-config' })
  expect(JSON.stringify(localStorage)).not.toContain('canary-secret-config')
})

it('sends the loaded revision and clears replacement input on a conflict', async () => {
  const fetch = vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    if (init?.method === 'POST') return respond({ detail: 'conflict' }, 412)
    return respond(String(input) === endpoint ? { items: [credential] } : credential)
  })
  vi.stubGlobal('fetch', fetch)
  render(<StorageCredentials canManage onUnauthorized={onUnauthorized} />)
  fireEvent.click(await screen.findByRole('button', { name: '查看 Archive' }))
  fireEvent.click(await screen.findByRole('button', { name: '替换凭据' }))
  fireEvent.change(screen.getByLabelText('rclone 配置'), { target: { value: 'replacement-canary' } })
  fireEvent.submit(screen.getByRole('form', { name: '替换存储凭据' }))
  expect(await screen.findByRole('alert')).toHaveTextContent('请刷新详情')
  expect(screen.getByLabelText('rclone 配置')).toHaveValue('')
  const posted = fetch.mock.calls.find(([, init]) => init?.method === 'POST')?.[1]
  expect(new Headers(posted?.headers).get('If-Match')).toBe('"3"')
  expect(screen.getByRole('button', { name: '加密保存' })).toBeEnabled()
})

it('shows metadata without mutation controls to a Viewer', async () => {
  vi.stubGlobal('fetch', vi.fn(async (input: string | URL | Request) => respond(String(input) === endpoint ? { items: [credential] } : credential)))
  render(<StorageCredentials canManage={false} onUnauthorized={onUnauthorized} />)
  fireEvent.click(await screen.findByRole('button', { name: '查看 Archive' }))
  await screen.findByRole('heading', { name: 'Archive' })
  expect(screen.queryByRole('button', { name: '导入凭据' })).not.toBeInTheDocument()
  expect(screen.queryByRole('button', { name: '替换凭据' })).not.toBeInTheDocument()
  expect(screen.queryByRole('button', { name: '禁用凭据' })).not.toBeInTheDocument()
})

it('clears an unsubmitted secret when the page is hidden or the form is cancelled', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => respond({ items: [] })))
  render(<StorageCredentials canManage onUnauthorized={onUnauthorized} />)
  await screen.findByText(/还没有存储凭据/)
  fireEvent.click(screen.getByRole('button', { name: '导入凭据' }))
  fireEvent.change(screen.getByLabelText('rclone 配置'), { target: { value: 'draft-canary' } })
  fireEvent(window, new Event('pagehide'))
  expect(screen.getByLabelText('rclone 配置')).toHaveValue('')
  fireEvent.change(screen.getByLabelText('rclone 配置'), { target: { value: 'draft-canary' } })
  fireEvent.click(screen.getByRole('button', { name: '取消' }))
  fireEvent.click(screen.getByRole('button', { name: '导入凭据' }))
  expect(screen.getByLabelText('rclone 配置')).toHaveValue('')
})

it('requires confirmation to disable and handles expired sessions', async () => {
  const fetch = vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    if (init?.method === 'POST') return respond({}, 401)
    return respond(String(input) === endpoint ? { items: [credential] } : credential)
  })
  vi.stubGlobal('fetch', fetch)
  render(<StorageCredentials canManage onUnauthorized={onUnauthorized} />)
  fireEvent.click(await screen.findByRole('button', { name: '查看 Archive' }))
  fireEvent.click(await screen.findByRole('button', { name: '禁用凭据' }))
  expect(fetch.mock.calls.some(([, init]) => init?.method === 'POST')).toBe(false)
  fireEvent.click(screen.getByRole('button', { name: '确认禁用' }))
  await waitFor(() => expect(onUnauthorized).toHaveBeenCalledOnce())
})
