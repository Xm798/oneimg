import type { CreatedPersonalAccessToken, CreateTokenRequest, DashboardStats, Image, ManagedUser, PersonalAccessToken, Problem, StorageBucket, StorageRequest, Tag, TagUsage, TrendPoint, UploadBatch, User } from './generated/types.gen'
import { client as generatedClient } from './generated/client.gen'

export type { CreatedPersonalAccessToken, CreateTokenRequest, DashboardStats, FieldProblem, Image, ManagedUser, PersonalAccessToken, Problem, StorageBucket, StorageConfig, StorageRequest, Tag, TagUsage, TrendPoint, UploadBatch, User } from './generated/types.gen'

/** GET /me/passkeys 的列表项（OpenAPI 里还是通用 DataEnvelope） */
export interface PasskeyRecord {
  id: number
  name: string
  created_at: string
  last_used_at: string | null
}

/** 上传趋势的统计粒度：day 为最近 30 天，week 为 12 周，month 为 12 个月，year 为 5 年 */
export type TrendPeriod = 'day' | 'week' | 'month' | 'year'

export type Envelope<T> = { data: T; meta?: { pagination?: { page: number; page_size: number; total: number; total_pages: number } } }

export class ApiError extends Error {
  problem: Problem
  /** 429 时服务器建议等待的秒数（Retry-After） */
  retryAfter?: number
  constructor(problem: Problem, retryAfter?: number) {
    super(problem?.detail || 'API request failed')
    this.name = 'ApiError'
    this.problem = problem
    this.retryAfter = retryAfter
  }
}

/** 未拿到可用响应。result_unknown：请求已发出但成功响应无法解析，记录可能已创建 */
export class TransportError extends Error {
  kind: 'network' | 'aborted' | 'result_unknown'
  constructor(kind: TransportError['kind']) {
    super(kind)
    this.name = 'TransportError'
    this.kind = kind
  }
}

const transportMessages: Record<TransportError['kind'], string | undefined> = {
  network: '网络连接失败',
  aborted: undefined,
  result_unknown: '上传结果未知，请到图库确认',
}

/** 接口错误的提示文字：限流时说明等待时间，其他情况用服务器的 detail */
/** A 403 or 409 means the caller acted on stale roles or permissions; reload before retrying. */
export const isStaleError = (error: unknown) => error instanceof ApiError && [403, 409].includes(Number(error.problem?.status))

export const errorMessage = (error: unknown, fallback: string) => {
  if (error instanceof TransportError) return transportMessages[error.kind] ?? fallback
  if (!(error instanceof ApiError)) return fallback
  if (error.problem?.status === 429 || error.retryAfter) {
    return error.retryAfter ? `操作太频繁，请 ${error.retryAfter} 秒后再试` : '操作太频繁，请稍后再试'
  }
  return error.problem?.detail || fallback
}

/** 字段级错误：{ 字段名: 提示 } */
export const fieldErrors = (error: unknown): Record<string, string> => {
  if (!(error instanceof ApiError)) return {}
  return Object.fromEntries((error.problem?.errors ?? []).map(item => [item.field, item.message]))
}

const baseURL = (import.meta.env.VITE_API_BASE_URL || '').replace(/\/$/, '')

/** 错误响应可能不是 JSON（如代理返回的 HTML）；Retry-After 只接受秒数，HTTP 日期视为未提供 */
const responseError = (status: number, payload: unknown, retryAfterHeader: string | null) => {
  const seconds = Number(retryAfterHeader)
  return new ApiError((payload ?? { status }) as Problem, Number.isFinite(seconds) && seconds > 0 ? seconds : undefined)
}

/** 接口返回的站内地址（如 avatar_url）加上 API 基础地址 */
export const resolveApiURL = (path: string) => (path.startsWith('/api/') ? `${baseURL}${path}` : path)

function csrfToken(): string | undefined {
  const value = document.cookie.split('; ').find(item => item.startsWith('oneimg-csrf='))?.slice('oneimg-csrf='.length)
  return value ? decodeURIComponent(value) : undefined
}

generatedClient.setConfig({ baseUrl: `${baseURL}/api/v1`, credentials: 'include' })
generatedClient.interceptors.request.use(request => {
  if (['GET', 'HEAD', 'OPTIONS'].includes(request.method.toUpperCase())) return request
  const token = csrfToken()
  if (!token) return request
  const headers = new Headers(request.headers)
  headers.set('X-OneImg-CSRF', token)
  return new Request(request, { headers })
})

export { generatedClient }

export function apiFetch(input: RequestInfo | URL, init: RequestInit = {}): Promise<Response> {
  const headers = new Headers(init.headers)
  const method = (init.method || 'GET').toUpperCase()
  if (!['GET', 'HEAD', 'OPTIONS'].includes(method)) {
    const token = csrfToken()
    if (token) headers.set('X-OneImg-CSRF', token)
  }
  let target = typeof input === 'string' ? input : input.toString()
  if (target.startsWith('/api/v1/')) target = `${baseURL}${target}`
  return fetch(target, { ...init, headers, credentials: 'include' })
}

async function requestEnvelope<T>(path: string, init: RequestInit = {}): Promise<Envelope<T>> {
  const headers = new Headers(init.headers)
  if (init.body && !(init.body instanceof FormData) && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json')
  const response = await apiFetch(`${baseURL}/api/v1${path}`, { ...init, headers })
  if (response.status === 204) return { data: undefined as T }
  const payload = await response.json().catch(() => null)
  if (!response.ok) throw responseError(response.status, payload, response.headers.get('Retry-After'))
  return payload as Envelope<T>
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  return (await requestEnvelope<T>(path, init)).data
}

export const api = {
  getPublicConfig: () => request<Record<string, unknown>>('/public/config'),
  login: (body: { username: string; password: string }) => request<User>('/auth/login', { method: 'POST', body: JSON.stringify(body) }),
  register: (body: { username: string; password: string }) => request<User>('/auth/register', { method: 'POST', body: JSON.stringify(body) }),
  logout: () => request<void>('/auth/logout', { method: 'POST' }),
  me: () => request<User>('/me'),
  updateMe: (body: Record<string, unknown>) => request<{ user: User; reauthentication_required: boolean }>('/me', { method: 'PATCH', body: JSON.stringify(body) }),
  listTags: () => request<TagUsage[]>('/tags'),
  createTag: (name: string) => request<Tag>('/tags', { method: 'POST', body: JSON.stringify({ name }) }),
  updateTag: (id: number, name: string) => request<Tag>(`/tags/${id}`, { method: 'PATCH', body: JSON.stringify({ name }) }),
  deleteTag: (id: number) => request<void>(`/tags/${id}`, { method: 'DELETE' }),
  listImages: (query = '') => request<Image[]>(`/images${query ? `?${query}` : ''}`),
  /** 带分页信息的图片列表 */
  listImagesPage: (query = '') => requestEnvelope<Image[]>(`/images${query ? `?${query}` : ''}`),
  getImage: (id: number) => request<Image>(`/images/${id}`),
  deleteImage: (id: number) => request<void>(`/images/${id}`, { method: 'DELETE' }),
  addImageTag: (imageId: number, tagId: number) => request<void>(`/images/${imageId}/tags/${tagId}`, { method: 'PUT' }),
  removeImageTag: (imageId: number, tagId: number) => request<void>(`/images/${imageId}/tags/${tagId}`, { method: 'DELETE' }),
  updateImageTags: (body: { image_ids: number[]; add_tag_ids?: number[]; remove_tag_ids?: number[] }) => request<unknown>('/images/tags', { method: 'PATCH', body: JSON.stringify(body) }),
  listImageStorages: () => request<{ id: number; name: string; type: string; disabled: boolean }[]>('/image-storages'),
  uploadImages: (form: FormData, onProgress?: (value: number) => void) => uploadWithProgress<UploadBatch>(`${baseURL}/api/v1/images`, form, onProgress),
  importImage: (body: Record<string, unknown>) => request<Image>('/image-imports', { method: 'POST', body: JSON.stringify(body) }),
  uploadOptions: () => request<Record<string, unknown>>('/upload-options'),
  dashboardStats: () => request<DashboardStats>('/stats/dashboard'),
  imageStats: (period: TrendPeriod) => request<TrendPoint[]>(`/stats/images?period=${encodeURIComponent(period)}`),
  listBuckets: () => request<StorageBucket[]>('/storage-buckets'),
  createBucket: (body: StorageRequest) => request<StorageBucket>('/storage-buckets', { method: 'POST', body: JSON.stringify(body) }),
  /** config 只需包含要修改的字段，密钥类字段留空表示不变 */
  updateBucket: (id: number, body: StorageRequest) => request<StorageBucket>(`/storage-buckets/${id}`, { method: 'PATCH', body: JSON.stringify(body) }),
  /** purgeObjects 为 true 时同时清理未被其他记录引用的远端文件 */
  deleteBucket: (id: number, purgeObjects = false) => request<void>(`/storage-buckets/${id}${purgeObjects ? '?purge_objects=true' : ''}`, { method: 'DELETE' }),
  setBucketDisabled: (id: number, disabled: boolean) => request<StorageBucket>(`/storage-buckets/${id}/${disabled ? 'disable' : 'enable'}`, { method: 'POST' }),
  testBucketConnection: (body: StorageRequest & { id?: number }) => request<{ detail: string }>('/storage-connection-tests', { method: 'POST', body: JSON.stringify(body) }),
  listUsers: (query = '') => request<User[]>(`/users${query ? `?${query}` : ''}`),
  /** 带分页信息的用户列表 */
  listUsersPage: (query = '') => requestEnvelope<ManagedUser[]>(`/users${query ? `?${query}` : ''}`),
  createUser: (body: { username: string; password: string; role: number }) => request<User>('/users', { method: 'POST', body: JSON.stringify(body) }),
  /** 改用户名或角色；改为普通用户时后端会清空功能权限 */
  updateUser: (id: number, body: { username?: string; role?: number }) => request<User>(`/users/${id}`, { method: 'PATCH', body: JSON.stringify(body) }),
  setUserPermissions: (id: number, body: { codes: string[]; bucket_ids: number[] }) => request<User>(`/users/${id}/permissions`, { method: 'PUT', body: JSON.stringify(body) }),
  resetUserPassword: (id: number) => request<{ new_password: string }>(`/users/${id}/password-reset`, { method: 'POST' }),
  revokeUserPasskeys: (id: number) => request<void>(`/users/${id}/passkeys/revoke`, { method: 'POST' }),
  deleteUser: (id: number) => request<void>(`/users/${id}`, { method: 'DELETE' }),
  /** 上传后服务器裁成正方形并转为 WebP，返回更新后的用户 */
  updateMyAvatar: (file: File) => {
    const form = new FormData()
    form.append('avatar', file)
    return request<User>('/me/avatar', { method: 'PUT', body: form })
  },
  deleteMyAvatar: () => request<User>('/me/avatar', { method: 'DELETE' }),
  listPasskeys: () => request<PasskeyRecord[]>('/me/passkeys'),
  passkeyRegistrationOptions: (body: { name: string; current_password: string }) => request<{ options: unknown }>('/me/passkeys/registration/options', { method: 'POST', body: JSON.stringify(body) }),
  passkeyRegistrationVerify: (body: unknown) => request<unknown>('/me/passkeys/registration/verify', { method: 'POST', body: JSON.stringify(body) }),
  renamePasskey: (id: number, name: string) => request<unknown>(`/me/passkeys/${id}`, { method: 'PATCH', body: JSON.stringify({ name }) }),
  revokePasskey: (id: number, currentPassword: string) => request<void>(`/me/passkeys/${id}/revoke`, { method: 'POST', body: JSON.stringify({ current_password: currentPassword }) }),
  passkeyLoginOptions: () => request<{ options: unknown }>('/auth/passkeys/login/options', { method: 'POST' }),
  passkeyLoginVerify: (body: unknown) => request<User>('/auth/passkeys/login/verify', { method: 'POST', body: JSON.stringify(body) }),
  listTokens: () => request<PersonalAccessToken[]>('/me/tokens'),
  createToken: (body: CreateTokenRequest) => request<CreatedPersonalAccessToken>('/me/tokens', { method: 'POST', body: JSON.stringify(body) }),
  revokeToken: (id: number) => request<unknown>(`/me/tokens/${id}/revoke`, { method: 'POST' }),
  getSettings: (groups = '') => request<Record<string, unknown>>(`/settings${groups ? `?groups=${encodeURIComponent(groups)}` : ''}`),
  updateSettings: (body: Record<string, unknown>) => request<Record<string, unknown>>('/settings', { method: 'PATCH', body: JSON.stringify(body) }),
}

function uploadWithProgress<T>(url: string, body: FormData, onProgress?: (value: number) => void): Promise<T> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    xhr.open('POST', url)
    xhr.withCredentials = true
    const token = csrfToken()
    if (token) xhr.setRequestHeader('X-OneImg-CSRF', token)
    xhr.upload.onprogress = event => { if (event.lengthComputable) onProgress?.(Math.round(event.loaded / event.total * 100)) }
    xhr.onerror = xhr.ontimeout = () => reject(new TransportError('network'))
    xhr.onabort = () => reject(new TransportError('aborted'))
    xhr.onload = () => {
      let payload: unknown = null
      try { payload = JSON.parse(xhr.responseText || 'null') } catch {}
      if (xhr.status < 200 || xhr.status >= 300) return reject(responseError(xhr.status, payload, xhr.getResponseHeader('Retry-After')))
      if (!payload || typeof payload !== 'object' || !('data' in payload)) return reject(new TransportError('result_unknown'))
      resolve((payload as Envelope<T>).data)
    }
    xhr.send(body)
  })
}

export default api
