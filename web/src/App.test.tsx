import { fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { App } from './App'

const session = {
  user: {
    id: '0198f1da-2c57-7d3b-9c92-6e2f05293643',
    username: 'admin',
    display_name: '管理员',
    role: 'ADMIN',
  },
  idle_expires_at: '2026-09-03T08:30:00Z',
  absolute_expires_at: '2026-09-04T08:00:00Z',
}

function response(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': status >= 400 ? 'application/problem+json' : 'application/json' },
  })
}

function problem(status: number, code: string) {
  return response({
    type: `https://restfleet.dev/problems/${code.toLowerCase()}`,
    title: 'Request failed',
    status,
    detail: '请求失败',
    instance: '/api/v1/auth/session',
    code,
    request_id: '0198f1da-2c57-7d3b-9c92-6e2f05293644',
  }, status)
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('App', () => {
  it('shows the one-time bootstrap form for an empty control plane', async () => {
    vi.stubGlobal('fetch', vi.fn(async (input: string | URL | Request) => {
      const path = String(input)
      if (path === '/api/v1/auth/session') return problem(401, 'AUTHENTICATION_REQUIRED')
      if (path === '/api/v1/bootstrap/status') return response({ bootstrap_required: true })
      throw new Error(`unexpected request: ${path}`)
    }))

    render(<App />)

    expect(await screen.findByRole('heading', { name: '创建管理员' })).toBeInTheDocument()
    expect(screen.getByLabelText('一次性初始化令牌')).toHaveAttribute('type', 'password')
  })

  it('logs in and renders the explicit empty dashboard', async () => {
    vi.stubGlobal('fetch', vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
      const path = String(input)
      if (path === '/api/v1/auth/session') return problem(401, 'AUTHENTICATION_REQUIRED')
      if (path === '/api/v1/bootstrap/status') return response({ bootstrap_required: false })
      if (path === '/api/v1/auth/login' && init?.method === 'POST') return response(session)
      if (path === '/api/v1/dashboard/summary') {
        return response({
          collected_at: '2026-09-03T08:00:00Z',
          hosts: 0,
          agents_online: 0,
          agents_degraded: 0,
          agents_offline: 0,
          plans: 0,
          repositories: 0,
          operations: 0,
        })
      }
      if (path === '/api/v1/version') {
        return response({
          version: '0.1.0-test',
          commit: 'abc123',
          built_at: '2026-09-03T08:00:00Z',
          schema_version: 5,
        })
      }
      if (path === '/api/v1/hosts') return response({ items: [] })
      throw new Error(`unexpected request: ${path}`)
    }))

    render(<App />)
    fireEvent.change(await screen.findByLabelText('用户名'), { target: { value: 'admin' } })
    fireEvent.change(screen.getByLabelText('密码'), { target: { value: 'a-strong-test-password' } })
    fireEvent.click(screen.getByRole('button', { name: '登录' }))

    expect(await screen.findByRole('heading', { name: '控制平面已就绪' })).toBeInTheDocument()
    expect(screen.getByText('管理员')).toBeInTheDocument()
  })

  it('shows a request ID when discovery fails', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => problem(503, 'SERVICE_UNAVAILABLE')))

    render(<App />)

    expect(await screen.findByRole('heading', { name: '暂时无法连接' })).toBeInTheDocument()
    expect(screen.getByText(/请求 ID/)).toBeInTheDocument()
  })

  it('clears the one-time enrollment token before waiting for the Agent', async () => {
    document.cookie = 'restfleet_csrf=test-csrf'
    const host = {
      id: '0198f1da-2c57-7d3b-9c92-6e2f05293645',
      display_name: 'edge-01',
      description: '',
      labels: {},
      timezone: 'UTC',
      status: 'PENDING',
      revision: 1,
      created_at: '2026-09-03T08:00:00Z',
      updated_at: '2026-09-03T08:00:00Z',
    }
    let hosts: typeof host[] = []
    vi.stubGlobal('fetch', vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
      const path = String(input)
      if (path === '/api/v1/auth/session') return response(session)
      if (path === '/api/v1/dashboard/summary') {
        return response({ collected_at: '2026-09-03T08:00:00Z', hosts: hosts.length, agents_online: 0, agents_degraded: 0, agents_offline: hosts.length, plans: 0, repositories: 0, operations: 0 })
      }
      if (path === '/api/v1/version') {
        return response({ version: 'test', commit: 'abc', built_at: '2026-09-03T08:00:00Z', schema_version: 5 })
      }
      if (path === '/api/v1/hosts' && init?.method === 'POST') {
        hosts = [host]
        return response(host, 201)
      }
      if (path === '/api/v1/hosts') return response({ items: hosts })
      if (path.endsWith('/enrollment-tokens') && init?.method === 'POST') {
        return response({
          id: '0198f1da-2c57-7d3b-9c92-6e2f05293646',
          token: 'rfe_one_time_secret',
          fingerprint: '…cret',
          expires_at: '2026-09-03T08:10:00Z',
          install: { native: 'native command', docker: 'docker command' },
        }, 201)
      }
      if (path.endsWith('/agents')) return response({ items: [] })
      throw new Error(`unexpected request: ${path}`)
    }))

    render(<App />)
    fireEvent.click(await screen.findByRole('button', { name: 'Hosts' }))
    fireEvent.click(screen.getByRole('button', { name: '添加第一个 Host' }))
    fireEvent.change(screen.getByLabelText('Host 名称'), { target: { value: 'edge-01' } })
    fireEvent.change(screen.getByLabelText('时区'), { target: { value: 'UTC' } })
    fireEvent.click(screen.getByRole('button', { name: '创建 Host' }))
    fireEvent.click(await screen.findByRole('button', { name: '生成一次性令牌' }))

    expect(await screen.findByText('rfe_one_time_secret')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '我已保存，检查连接' }))
    expect(screen.queryByText('rfe_one_time_secret')).not.toBeInTheDocument()
    expect(screen.getByRole('heading', { name: '等待 Agent 连接' })).toBeInTheDocument()
  })

})
