import { FormEvent, useCallback, useEffect, useState } from 'react'
import type { components } from './api/schema'
import { ApiError, csrfToken, errorMessage, requestJSON } from './api/client'
import { StorageCredentials } from './StorageCredentials'

type Session = components['schemas']['Session']
type BootstrapStatus = components['schemas']['BootstrapStatus']
type DashboardSummary = components['schemas']['DashboardSummary']
type Version = components['schemas']['Version']
type Host = components['schemas']['Host']
type HostList = components['schemas']['HostList']
type Agent = components['schemas']['Agent']
type AgentInventory = components['schemas']['AgentInventory']
type AgentList = components['schemas']['AgentList']
type EnrollmentTokenCreated = components['schemas']['EnrollmentTokenCreated']
type Problem = components['schemas']['Problem']
type Phase = 'loading' | 'bootstrap' | 'login' | 'authenticated' | 'error'
type Page = 'overview' | 'hosts' | 'credentials' | 'version'
type DataState = 'idle' | 'loading' | 'ready' | 'error'

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
  const [hosts, setHosts] = useState<Host[]>([])
  const [dataState, setDataState] = useState<DataState>('idle')
  const [page, setPage] = useState<Page>('overview')
  const [message, setMessage] = useState('')
  const [retryKey, setRetryKey] = useState(0)
  const storageSessionExpired = useCallback(() => { setSession(null); setPhase('login') }, [])

  const loadAuthenticatedData = useCallback(async () => {
    setDataState('loading')
    try {
      const [nextSummary, nextVersion, nextHosts] = await Promise.all([
        requestJSON<DashboardSummary>('/api/v1/dashboard/summary'),
        requestJSON<Version>('/api/v1/version'),
        requestJSON<HostList>('/api/v1/hosts'),
      ])
      setSummary(nextSummary)
      setVersion(nextVersion)
      setHosts(nextHosts.items)
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
      setHosts([])
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
          <button className={page === 'hosts' ? 'nav-item active' : 'nav-item'} onClick={() => setPage('hosts')}>
            Hosts
          </button>
          <button className={page === 'credentials' ? 'nav-item active' : 'nav-item'} onClick={() => setPage('credentials')}>
            存储凭据
          </button>
          {navigation.slice(2).map((item) => (
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
        {page === 'overview' && (
          <Overview summary={summary} state={dataState} retry={loadAuthenticatedData} />
        )}
        {page === 'hosts' && (
          <HostsView hosts={hosts} state={dataState} reload={loadAuthenticatedData} />
        )}
        {page === 'credentials' && <StorageCredentials canManage={session?.user.role === 'ADMIN'} onUnauthorized={storageSessionExpired} />}
        {page === 'version' && (
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
        <Metric label="Agents online" value={summary.agents_online} />
        <Metric label="Agents degraded" value={summary.agents_degraded} />
        <Metric label="Agents offline" value={summary.agents_offline} />
        <Metric label="Plans" value={summary.plans} />
        <Metric label="Repositories" value={summary.repositories} />
        <Metric label="Operations" value={summary.operations} />
      </dl>
      {isEmpty && (
        <div className="empty-state">
          <span className="status-mark" aria-hidden="true">✓</span>
          <h2>控制平面已就绪</h2>
          <p>当前还没有受管 Host。请进入 Hosts 创建主机并生成一次性注册令牌。</p>
        </div>
      )}
    </section>
  )
}

function Metric({ label, value }: { label: string; value: number }) {
  return <div className="metric-card"><dt>{label}</dt><dd>{value}</dd></div>
}

function HostsView({
  hosts,
  state,
  reload,
}: {
  hosts: Host[]
  state: DataState
  reload: () => Promise<void>
}) {
  const [selectedID, setSelectedID] = useState<string | null>(null)
  const [wizard, setWizard] = useState(false)
  const [draftHost, setDraftHost] = useState<Host | null>(null)
  const [oneTime, setOneTime] = useState<EnrollmentTokenCreated | null>(null)
  const [step, setStep] = useState<'host' | 'token' | 'secret' | 'wait'>('host')
  const [agents, setAgents] = useState<Agent[]>([])
  const [healthByHost, setHealthByHost] = useState<Record<string, string>>({})
  const [message, setMessage] = useState('')
  const selected = hosts.find((host) => host.id === selectedID) ?? null

  useEffect(() => {
    let active = true
    // ponytail: M3 reuses the existing per-Host endpoint until fleet pagination adds a joined summary.
    void Promise.all(hosts.map(async (host) => {
      const result = await requestJSON<AgentList>(`/api/v1/hosts/${host.id}/agents`)
      return [host.id, result.items[0]?.health ?? 'UNKNOWN'] as const
    })).then((entries) => {
      if (active) setHealthByHost(Object.fromEntries(entries))
    }).catch(() => {
      if (active) setHealthByHost({})
    })
    return () => {
      active = false
    }
  }, [hosts])

  function closeWizard() {
    setOneTime(null)
    setDraftHost(null)
    setAgents([])
    setStep('host')
    setMessage('')
    setWizard(false)
  }

  async function createHost(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    setMessage('')
    const form = new FormData(event.currentTarget)
    try {
      const host = await requestJSON<Host>('/api/v1/hosts', {
        method: 'POST',
        headers: { 'X-CSRF-Token': csrfToken() },
        body: JSON.stringify({
          display_name: String(form.get('display_name') ?? ''),
          description: String(form.get('description') ?? ''),
          timezone: String(form.get('timezone') ?? ''),
          labels: parseLabels(String(form.get('labels') ?? '')),
        }),
      })
      setDraftHost(host)
      setStep('token')
      await reload()
    } catch (error) {
      setMessage(errorMessage(error))
    }
  }

  async function createToken(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!draftHost) return
    setMessage('')
    const form = new FormData(event.currentTarget)
    try {
      const token = await requestJSON<EnrollmentTokenCreated>(
        `/api/v1/hosts/${draftHost.id}/enrollment-tokens`,
        {
          method: 'POST',
          headers: { 'X-CSRF-Token': csrfToken() },
          body: JSON.stringify({ expires_in_seconds: Number(form.get('ttl') ?? 600) }),
        },
      )
      setOneTime(token)
      setStep('secret')
    } catch (error) {
      setMessage(errorMessage(error))
    }
  }

  async function checkAgent() {
    if (!draftHost) return
    setMessage('')
    try {
      const result = await requestJSON<AgentList>(`/api/v1/hosts/${draftHost.id}/agents`)
      setAgents(result.items)
      if (result.items.length > 0) {
        await reload()
      }
    } catch (error) {
      setMessage(errorMessage(error))
    }
  }

  if (selected) {
    return <HostDetail host={selected} back={() => setSelectedID(null)} reload={reload} />
  }

  return (
    <section>
      <header className="page-header">
        <div>
          <p className="eyebrow">MANAGED MACHINES</p>
          <h1>Hosts</h1>
        </div>
        <button className="primary-button" onClick={() => setWizard(true)}>添加 Host</button>
      </header>
      {state === 'loading' && <p className="muted">正在加载 Hosts…</p>}
      {state === 'ready' && hosts.length === 0 && !wizard && (
        <div className="empty-state">
          <h2>尚未接入 Host</h2>
          <p>先创建 Host，再用短期一次性令牌让 Agent 从受管机主动连接。</p>
          <button className="secondary-button" onClick={() => setWizard(true)}>添加第一个 Host</button>
        </div>
      )}
      {hosts.length > 0 && (
        <div className="table-wrap">
          <table>
            <thead><tr><th>Name</th><th>Agent</th><th>Timezone</th><th>Labels</th></tr></thead>
            <tbody>
              {hosts.map((host) => (
                <tr key={host.id}>
                  <td><button className="table-link" onClick={() => setSelectedID(host.id)}>{host.display_name}</button></td>
                  <td><StatusBadge status={healthByHost[host.id] ?? 'UNKNOWN'} /></td>
                  <td>{host.timezone}</td>
                  <td>{Object.entries(host.labels).map(([key, value]) => `${key}=${value}`).join(', ') || '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {wizard && (
        <div className="wizard" aria-labelledby="wizard-title">
          <div className="wizard-header">
            <div><p className="eyebrow">ADD HOST · {stepLabel(step)}</p><h2 id="wizard-title">安全接入 Agent</h2></div>
            <button className="text-dark-button" onClick={closeWizard}>关闭</button>
          </div>
          {message && <InlineError message={message} />}
          {step === 'host' && (
            <form onSubmit={createHost}>
              <label>Host 名称<input name="display_name" maxLength={128} required /></label>
              <label>描述<input name="description" maxLength={1024} /></label>
              <label>时区<input name="timezone" defaultValue={Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'} required /></label>
              <label>标签（逗号分隔 key=value）<input name="labels" placeholder="env=prod, region=ap-east" /></label>
              <button className="primary-button" type="submit">创建 Host</button>
            </form>
          )}
          {step === 'token' && draftHost && (
            <form onSubmit={createToken}>
              <p>Host <strong>{draftHost.display_name}</strong> 已创建。请选择令牌有效期。</p>
              <label>有效期
                <select name="ttl" defaultValue="600">
                  <option value="300">5 分钟</option>
                  <option value="600">10 分钟</option>
                  <option value="1800">30 分钟</option>
                  <option value="3600">60 分钟</option>
                </select>
              </label>
              <button className="primary-button" type="submit">生成一次性令牌</button>
            </form>
          )}
          {step === 'secret' && oneTime && (
            <div className="secret-panel">
              <p className="warning-text">此令牌与安装命令只显示一次。离开本步骤后无法再次查看。</p>
              <p className="muted">命令不会携带令牌。运行所选命令后，在标准输入粘贴上方令牌、回车，再按 Ctrl-D。</p>
              <label>Enrollment token<code className="secret-code">{oneTime.token}</code></label>
              <label>Native Linux<code className="command-code">{oneTime.install.native}</code></label>
              <label>Docker<code className="command-code">{oneTime.install.docker}</code></label>
              <p className="muted">过期时间：{new Date(oneTime.expires_at).toLocaleString()}</p>
              <button className="primary-button" onClick={() => { setOneTime(null); setStep('wait') }}>我已保存，检查连接</button>
            </div>
          )}
          {step === 'wait' && draftHost && (
            <div className="secret-panel" aria-live="polite">
              {agents.length === 0 ? (
                <>
                  <h3>等待 Agent 连接</h3>
                  <p className="muted">在目标主机执行刚才保存的命令，然后检查连接。</p>
                  <button className="secondary-button" onClick={() => void checkAgent()}>检查连接</button>
                </>
              ) : (
                <>
                  <h3>Agent 已接入</h3>
                  <dl className="detail-list compact-list">
                    <div><dt>Fingerprint</dt><dd><code>{agents[0].public_key_fingerprint}</code></dd></div>
                    <div><dt>Hostname</dt><dd>{agents[0].hostname}</dd></div>
                    <div><dt>Platform</dt><dd>{agents[0].os}/{agents[0].arch}</dd></div>
                  </dl>
                  <button className="primary-button" onClick={closeWizard}>完成</button>
                </>
              )}
            </div>
          )}
        </div>
      )}
    </section>
  )
}

function HostDetail({ host, back, reload }: { host: Host; back: () => void; reload: () => Promise<void> }) {
  const [agents, setAgents] = useState<Agent[]>([])
  const [inventory, setInventory] = useState<AgentInventory | null>(null)
  const [message, setMessage] = useState('')

  useEffect(() => {
    let active = true
    const inventoryRequest = requestJSON<AgentInventory>(`/api/v1/hosts/${host.id}/inventory`)
      .catch((error: unknown) => {
        if (error instanceof ApiError && error.status === 404) return null
        throw error
      })
    void Promise.all([
      requestJSON<AgentList>(`/api/v1/hosts/${host.id}/agents`),
      inventoryRequest,
    ]).then(([agentList, latestInventory]) => {
      if (!active) return
      setAgents(agentList.items)
      setInventory(latestInventory)
    }).catch((error: unknown) => {
      if (active) setMessage(errorMessage(error))
    })
    return () => {
      active = false
    }
  }, [host.id])

  async function changeStatus() {
    setMessage('')
    const action = host.status === 'DISABLED' ? 'enable' : 'disable'
    try {
      await requestJSON<Host>(`/api/v1/hosts/${host.id}/${action}`, {
        method: 'POST',
        headers: {
          'X-CSRF-Token': csrfToken(),
          'If-Match': String(host.revision),
        },
      })
      await reload()
      back()
    } catch (error) {
      setMessage(errorMessage(error))
    }
  }

  return (
    <section>
      <button className="text-dark-button" onClick={back}>← 返回 Hosts</button>
      <header className="page-header">
        <div><p className="eyebrow">HOST DETAIL</p><h1>{host.display_name}</h1></div>
        <button className="secondary-button" onClick={() => void changeStatus()}>
          {host.status === 'DISABLED' ? '启用 Host' : '禁用 Host'}
        </button>
      </header>
      {message && <InlineError message={message} />}
      <dl className="detail-list">
        <div><dt>状态</dt><dd><StatusBadge status={host.status} /></dd></div>
        <div><dt>时区</dt><dd>{host.timezone}</dd></div>
        <div><dt>描述</dt><dd>{host.description || '—'}</dd></div>
        <div><dt>Revision</dt><dd>{host.revision}</dd></div>
      </dl>
      <h2 className="section-title">Agent</h2>
      {agents.length === 0 ? <p className="muted">尚无 Agent 接入。</p> : agents.map((agent) => (
        <dl className="detail-list" key={agent.id}>
          <div><dt>运行健康</dt><dd><StatusBadge status={agent.health} /></dd></div>
          <div><dt>身份状态</dt><dd><StatusBadge status={agent.status} /></dd></div>
          <div><dt>配置版本</dt><dd>{agent.accepted_revision} / {agent.desired_revision}{agent.accepted_revision === agent.desired_revision ? '（已同步）' : '（等待同步）'}</dd></div>
          <div><dt>最后心跳</dt><dd>{agent.last_seen_at ? new Date(agent.last_seen_at).toLocaleString() : '尚未收到'}</dd></div>
          <div><dt>运行时长</dt><dd>{formatDuration(agent.uptime_seconds)}</dd></div>
          <div><dt>状态盘可用</dt><dd>{formatBytes(agent.state_free_bytes)}</dd></div>
          <div><dt>时钟偏差</dt><dd>{agent.clock_offset_ms} ms</dd></div>
          <div><dt>诊断代码</dt><dd>{agent.heartbeat_error_code || agent.config_error_code || '—'}{agent.config_error_field ? ` · ${agent.config_error_field}` : ''}</dd></div>
          <div><dt>Fingerprint</dt><dd><code>{agent.public_key_fingerprint}</code></dd></div>
          <div><dt>Hostname</dt><dd>{agent.hostname}</dd></div>
          <div><dt>平台</dt><dd>{agent.os}/{agent.arch}</dd></div>
          <div><dt>证书到期</dt><dd>{new Date(agent.certificate_not_after).toLocaleString()}</dd></div>
        </dl>
      ))}
      <h2 className="section-title">Inventory</h2>
      {!inventory ? <p className="muted">尚未收到系统清单。</p> : (
        <dl className="detail-list">
          <div><dt>采集时间</dt><dd>{new Date(inventory.captured_at).toLocaleString()}</dd></div>
          <div><dt>系统</dt><dd>{inventory.os_release}</dd></div>
          <div><dt>Kernel</dt><dd>{inventory.kernel}</dd></div>
          <div><dt>架构</dt><dd>{inventory.cpu_arch}</dd></div>
          <div><dt>Agent</dt><dd>{inventory.agent_version}</dd></div>
          <div><dt>Restic</dt><dd>{inventory.restic_version || '未检测'}</dd></div>
          <div><dt>运行环境</dt><dd>{inventory.containerized ? '容器' : '原生 Linux'}</dd></div>
          <div><dt>能力</dt><dd>{inventory.capabilities.join(', ') || '—'}</dd></div>
        </dl>
      )}
    </section>
  )
}

function StatusBadge({ status }: { status: string }) {
  const labels: Record<string, string> = {
    PENDING: '等待接入', ACTIVE: '身份有效', DISABLED: '已禁用', REVOKED: '已撤销',
    ONLINE: '在线', DEGRADED: '异常', OFFLINE: '离线', UNKNOWN: '未接入',
  }
  return <span className={`status-badge status-${status.toLowerCase()}`}>{labels[status] ?? status}</span>
}

function formatBytes(value: number): string {
  if (value < 1024) return `${value} B`
  const units = ['KiB', 'MiB', 'GiB', 'TiB']
  let amount = value
  let unit = -1
  do {
    amount /= 1024
    unit++
  } while (amount >= 1024 && unit < units.length - 1)
  return `${amount.toFixed(1)} ${units[unit]}`
}

function formatDuration(seconds: number): string {
  const days = Math.floor(seconds / 86400)
  const hours = Math.floor((seconds % 86400) / 3600)
  return days > 0 ? `${days} 天 ${hours} 小时` : `${hours} 小时`
}

function stepLabel(step: 'host' | 'token' | 'secret' | 'wait') {
  return { host: '1/4', token: '2/4', secret: '3/4', wait: '4/4' }[step]
}

function parseLabels(value: string): Record<string, string> {
  const labels: Record<string, string> = {}
  for (const part of value.split(',').map((item) => item.trim()).filter(Boolean)) {
    const separator = part.indexOf('=')
    if (separator < 1) throw new Error('标签格式必须是 key=value。')
    labels[part.slice(0, separator).trim()] = part.slice(separator + 1).trim()
  }
  return labels
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
