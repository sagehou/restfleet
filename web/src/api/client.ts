import type { components } from './schema'

type Problem = components['schemas']['Problem']

export class ApiError extends Error {
  readonly status: number
  readonly problem?: Problem

  constructor(status: number, problem?: Problem) {
    super(problem?.detail ?? '请求失败')
    this.status = status
    this.problem = problem
  }
}

export async function requestJSON<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers)
  headers.set('Accept', 'application/json')
  if (init?.body) headers.set('Content-Type', 'application/json')
  const response = await fetch(path, { ...init, headers, credentials: 'same-origin' })
  if (!response.ok) {
    let problem: Problem | undefined
    try { problem = (await response.json()) as Problem } catch { problem = undefined }
    throw new ApiError(response.status, problem)
  }
  return (await response.json()) as T
}

export function csrfToken(): string {
  const prefix = 'restfleet_csrf='
  const value = document.cookie.split(';').map((part) => part.trim()).find((part) => part.startsWith(prefix))
  return value ? decodeURIComponent(value.slice(prefix.length)) : ''
}

export function errorMessage(error: unknown): string {
  if (error instanceof ApiError && error.problem?.request_id) {
    return `${error.message}（请求 ID：${error.problem.request_id}）`
  }
  return error instanceof Error ? error.message : '请求失败，请稍后重试。'
}
