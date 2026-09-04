import { FormEvent, useCallback, useEffect, useState } from 'react'
import type { components } from './api/schema'

type Session = components['schemas']['Session']
type BootstrapStatus = components['schemas']['BootstrapStatus']
type DashboardSummary = components['schemas']['DashboardSummary']
type Version = components['schemas']['Version']
type Problem = components['schemas']['Problem']
type Phase = 'loading' | 'bootstrap' | 'login' | 'authenticated' | 'error'
type Page = 'overview' | 'version'
type DataState = 'idle' | 'loading' | 'ready' | 'error'

class ApiError extends Error {
  readonly status: number
  readonly problem?: Problem

  constructor(status: number, problem?: Problem) {
    super(problem?.detail ?? '请求失败')
    this.status = status
    this.problem = problem
  }
}

async function requestJSON<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers)
  headers.set('Accept', 'application/json')
  if (init?.body) {
    headers.set('Content-Type', 'application/json')
  }
  const response = await fetch(path, {
    ...init,
    headers,
    credentials: 'same-origin',
  })
  if (!response.ok) {
    let problem: Problem | undefined
    try {
      problem = (await response.json()) as Problem
    } catch {
      problem = undefined
    }
    throw new ApiError(response.status, problem)
  }
  return (await response.json()) as T
}

function csrfToken(): string {
  const prefix = 'restfleet_csrf='
  const value = document.cookie
    .split(';')
    .map((part) => part.trim())
    .find((part) => part.startsWith(prefix))
  return value ? decodeURIComponent(value.slice(prefix.length)) : ''
}

function errorMessage(error: unknown): string {
  if (error instanceof ApiError && error.problem?.request_id) {
    return `${error.message}（请求 ID：${error.problem.request_id}）`
  }
  return error instanceof Error ? error.message : '请求失败，请稍后重试。'
}

const navigation = [
  'Overview',
  'Hosts',
  'Repositories',
  'Templates',
  'Plans',
  'Snapshots',
  'Operations',
  'Notifications',
  'Audit',
] as const

export function App() {
  const [phase, setPhase] = useState<Phase>('loading')
  const [session, setSession] = useState<Session | null>(null)
  const [summary, setSummary] = useState<DashboardSummary | null>(null)
  const [version, setVersion] = useState<Version | null>(null)
  const [dataState, setDataState] = useState<DataState>('idle')
  const [page, setPage] = useState<Page>('overview')
  const [message, setMessage] = useState('')
  const [retryKey, setRetryKey] = useState(0)

  const loadAuthenticatedData = useCallback(async () => {
    setDataState('loading')
    try {
      const [nextSummary, nextVersion] = await Promise.all([
        requestJSON<DashboardSummary>('/api/v1/dashboard/summary'),
        requestJSON<Version>('/api/v1/version'),
      ])
      setSummary(nextSummary)
      setVersion(nextVersion)
      setDataState('ready')
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) {
        setSession(null)
        setPhase('login')
        return
      }
      setMessage(errorMessage(error))
      setDataState('error')
    }
  }, [])

  useEffect(() => {
    let active = true
    void (async () => {
      setPhase('loading')
      setMessage('')
      try {
        const currentSession = await requestJSON<Session>('/api/v1/auth/session')
        if (!active) return
        setSession(currentSession)
        setPhase('authenticated')
        await loadAuthenticatedData()
      } catch (error) {
        if (!active) return
        if (error instanceof ApiError && error.status === 401) {
          try {
            const status = await requestJSON<BootstrapStatus>('/api/v1/bootstrap/status')
            if (!active) return
            setPhase(status.bootstrap_required ? 'bootstrap' : 'login')
          } catch (statusError) {
            if (!active) return
            setMessage(errorMessage(statusError))
            setPhase('error')
          }
          return
        }
        setMessage(errorMessage(error))
        setPhase('error')
      }
    })()
    return () => {
      active = false
    }
  }, [loadAuthenticatedData, retryKey])

  async function submitBootstrap(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const formElement = event.currentTarget
    const form = new FormData(formElement)
    const bootstrapToken = String(form.get('bootstrap_token') ?? '')
    const password = String(form.get('password') ?? '')
    setMessage('')
    try {
      const authenticated = await requestJSON<Session>('/api/v1/bootstrap', {
        method: 'POST',
        headers: { 'X-RestFleet-Bootstrap-Token': bootstrapToken },
        body: JSON.stringify({
          username: String(form.get('username') ?? ''),
          display_name: String(form.get('display_name') ?? ''),
          password,
        }),
      })
      formElement.reset()
      setSession(authenticated)
      setPhase('authenticated')
      await loadAuthenticatedData()
    } catch (error) {
      formElement.reset()
      setMessage(errorMessage(error))
    }
  }

  async function submitLogin(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const formElement = event.currentTarget
    const form = new FormData(formElement)
    setMessage('')
    try {
      const authenticated = await requestJSON<Session>('/api/v1/auth/login', {
        method: 'POST',
        body: JSON.stringify({
          username: String(form.get('username') ?? ''),
          password: String(form.get('password') ?? ''),
        }),
      })
      formElement.reset()
      setSession(authenticated)
      setPhase('authenticated')
      await loadAuthenticatedData()
    } catch (error) {
      formElement.reset()
      setMessage(errorMessage(error))
    }
  }

  async function logout() {
    setMessage('')
    try {
      const response = await fetch('/api/v1/auth/logout', {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'X-CSRF-Token': csrfToken() },
      })
      if (!response.ok) {
        const problem = (await response.json()) as Problem
        throw new ApiError(response.status, problem)
      }
      setSession(null)
      setSummary(null)
      setVersion(null)
      setPhase('login')
    } catch (error) {
      setMessage(errorMessage(error))
    }
  }

  if (phase === 'loading') {
    return <CenteredState title="正在连接控制平面" detail="正在检查初始化和登录状态。" />
  }

  if (phase === 'error') {
    return (
      <CenteredState title="暂时无法连接" detail={message}>
        <button className="primary-button" onClick={() => setRetryKey((value) => value + 1)}>
          重试
        </button>
      </CenteredState>
    )
  }

  if (phase === 'bootstrap') {
    return (
      <AuthLayout
        eyebrow="首次启动"
        title="创建管理员"
        detail="初始化令牌只用于这一次操作；创建成功后会永久失效。"
      >
        <form onSubmit={submitBootstrap}>
          <label>
            管理员用户名
            <input name="username" minLength={3} maxLength={64} pattern="[A-Za-z0-9._-]+" autoComplete="username" required />
          </label>
          <label>
            显示名称
            <input name="display_name" maxLength={128} autoComplete="name" required />
          </label>
          <label>
            管理员密码
            <input name="password" type="password" minLength={12} maxLength={1024} autoComplete="new-password" required />
          </label>
          <label>
            一次性初始化令牌
            <input name="bootstrap_token" type="password" maxLength={4096} autoComplete="off" required />
          </label>
          {message && <InlineError message={message} />}
          <button className="primary-button" type="submit">创建管理员并登录</button>
        </form>
      </AuthLayout>
    )
  }

  if (phase === 'login') {
    return (
      <AuthLayout eyebrow="RESTFLEET" title="登录控制平面" detail="使用管理员账户继续。">
        <form onSubmit={submitLogin}>
          <label>
            用户名
            <input name="username" maxLength={64} autoComplete="username" required />
          </label>
          <label>
            密码
            <input name="password" type="password" maxLength={1024} autoComplete="current-password" required />
          </label>
          {message && <InlineError message={message} />}
          <button className="primary-button" type="submit">登录</button>
        </form>
      </AuthLayout>
    )
  }

  return (
    <div className="app-shell">
      <aside className="sidebar">
        <div>
          <p className="eyebrow">RESTFLEET</p>
          <p className="product-name">备份控制平面</p>
        </div>
        <nav aria-label="主导航">
          <button className={page === 'overview' ? 'nav-item active' : 'nav-item'} onClick={() => setPage('overview')}>
            Overview
          </button>
          {navigation.slice(1).map((item) => (
            <button className="nav-item" disabled key={item} title="后续里程碑提供">
              {item}
            </button>
          ))}
          <button className={page === 'version' ? 'nav-item active' : 'nav-item'} onClick={() => setPage('version')}>
            Settings
          </button>
        </nav>
        <div className="account">
          <span>{session?.user.display_name}</span>
          <small>{session?.user.role}</small>
          <button className="text-button" onClick={logout}>退出登录</button>
        </div>
      </aside>
      <main className="content">
        {message && <InlineError message={message} />}
        {page === 'overview' ? (
          <Overview summary={summary} state={dataState} retry={loadAuthenticatedData} />
        ) : (
          <VersionView version={version} state={dataState} />
        )}
      </main>
    </div>
  )
}

function AuthLayout({
  eyebrow,
  title,
  detail,
  children,
}: {
  eyebrow: string
  title: string
  detail: string
  children: React.ReactNode
}) {
  return (
    <main className="auth-shell">
      <section className="auth-panel" aria-labelledby="auth-title">
        <p className="eyebrow">{eyebrow}</p>
        <h1 id="auth-title">{title}</h1>
        <p className="muted">{detail}</p>
        {children}
      </section>
    </main>
  )
}

function CenteredState({
  title,
  detail,
  children,
}: {
  title: string
  detail: string
  children?: React.ReactNode
}) {
  return (
    <main className="auth-shell">
      <section className="auth-panel compact" aria-live="polite">
        <p className="eyebrow">RESTFLEET</p>
        <h1>{title}</h1>
        <p className="muted">{detail}</p>
        {children}
      </section>
    </main>
  )
}

function InlineError({ message }: { message: string }) {
  return <p className="error-message" role="alert">{message}</p>
}

function Overview({
  summary,
  state,
  retry,
}: {
  summary: DashboardSummary | null
  state: DataState
  retry: () => Promise<void>
}) {
  if (state === 'loading' || state === 'idle') {
    return <section aria-live="polite"><h1>Overview</h1><p className="muted">正在加载控制平面摘要…</p></section>
  }
  if (state === 'error' || !summary) {
    return (
      <section aria-live="polite">
        <h1>Overview</h1>
        <div className="empty-state">
          <h2>摘要加载失败</h2>
          <button className="secondary-button" onClick={() => void retry()}>重新加载</button>
        </div>
      </section>
    )
  }
  const isEmpty = summary.hosts + summary.plans + summary.repositories + summary.operations === 0
  return (
    <section>
      <header className="page-header">
        <div>
          <p className="eyebrow">FLEET STATUS</p>
          <h1>Overview</h1>
        </div>
        <time dateTime={summary.collected_at}>采集于 {new Date(summary.collected_at).toLocaleString()}</time>
      </header>
      <dl className="metric-grid">
        <Metric label="Hosts" value={summary.hosts} />
        <Metric label="Plans" value={summary.plans} />
        <Metric label="Repositories" value={summary.repositories} />
        <Metric label="Operations" value={summary.operations} />
      </dl>
      {isEmpty && (
        <div className="empty-state">
          <span className="status-mark" aria-hidden="true">✓</span>
          <h2>控制平面已就绪</h2>
          <p>当前还没有受管 Host。Host 接入将在下一个里程碑开放。</p>
        </div>
      )}
    </section>
  )
}

function Metric({ label, value }: { label: string; value: number }) {
  return <div className="metric-card"><dt>{label}</dt><dd>{value}</dd></div>
}

function VersionView({ version, state }: { version: Version | null; state: DataState }) {
  return (
    <section>
      <p className="eyebrow">SETTINGS</p>
      <h1>版本信息</h1>
      {state === 'ready' && version ? (
        <dl className="detail-list">
          <div><dt>Server</dt><dd>{version.version}</dd></div>
          <div><dt>Commit</dt><dd><code>{version.commit}</code></dd></div>
          <div><dt>Build time</dt><dd>{version.built_at}</dd></div>
          <div><dt>Schema</dt><dd>v{version.schema_version}</dd></div>
        </dl>
      ) : (
        <p className="muted">版本信息暂不可用。</p>
      )}
    </section>
  )
}
