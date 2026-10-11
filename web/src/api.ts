export type Entity = Record<string, any> & { id: string; version: number }
export type List = { items: Entity[]; total: number }
export class ApiError extends Error {
  code: string
  retryable: boolean
  status: number
  constructor(message: string, code: string, retryable: boolean, status: number) { super(message); this.code = code; this.retryable = retryable; this.status = status }
}
let csrf = ''
export function setCSRF(value: string) { csrf = value }
let taskSequence = 0
function taskKey(): string {
  const cryptoAPI = globalThis.crypto
  if (typeof cryptoAPI?.randomUUID === 'function') return cryptoAPI.randomUUID()
  if (typeof cryptoAPI?.getRandomValues === 'function') {
    const bytes = cryptoAPI.getRandomValues(new Uint8Array(16))
    bytes[6] = (bytes[6] & 0x0f) | 0x40
    bytes[8] = (bytes[8] & 0x3f) | 0x80
    const hex = Array.from(bytes, byte => byte.toString(16).padStart(2, '0')).join('')
    return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`
  }
  // Task deduplication only; this fallback must not be used for secrets or tokens.
  return `task-${Date.now().toString(36)}-${(++taskSequence).toString(36)}-${Math.random().toString(36).slice(2)}`
}
export type RequestActivity = { id: number; label: string }
const activeRequests = new Map<number, RequestActivity>()
const activityListeners = new Set<(items: RequestActivity[]) => void>()
let requestSequence = 0
function publishActivity() { for (const listener of activityListeners) listener([...activeRequests.values()]) }
export function subscribeActivity(listener: (items: RequestActivity[]) => void) {
  activityListeners.add(listener); listener([...activeRequests.values()])
  return () => { activityListeners.delete(listener) }
}
function requestLabel(path: string, method: string) {
  if (path.includes('/notifications/telegram')) return method === 'GET' ? '正在加载 Telegram 通知设置…' : '正在保存或提交 Telegram 通知…'
  if (path.includes('/accounts/clear')) return path.includes('/preview') ? '正在预览批量清空范围…' : '正在提交批量清空任务…'
  if (/\/accounts\/[^/]+\/drives(?:\?|$)/.test(path)) return '正在读取 OneDrive 账号身份与 Drive 列表…'
  if (path.includes('/rclone/authorize')) return '正在启动或更新微软授权…'
  if (path.includes('/rclone/import') || path.includes('/accounts/import')) return '正在解析或导入账号配置…'
  if (/\/accounts\/[^/]+\/verify/.test(path)) return '正在验证云端访问，请稍候…'
  if (path.includes('/quota')) return '正在查询云端容量…'
  if (path.includes('/files') || path.includes('/browse')) return '正在读取文件目录…'
  if (path.includes('/content')) return '正在读取文件预览…'
  if (path.includes('/logs')) return '正在读取任务日志…'
  if (path.includes('/preview')) return '正在执行预检查，请稍候…'
  if (path.includes('/export') || path.includes('/recovery-kit')) return '正在加密并导出资料…'
  if (path.includes('/auth/login') || path.includes('/setup')) return '正在验证并加载工作空间…'
  if (path.includes('/unlock')) return '正在验证密码并解锁备份库…'
  if (path.includes('/cloud-vaults')) return '正在查找云端备份库…'
  if (path.includes('/cloud-connect') && !path.includes('/cloud-connection')) return '正在验证密码并连接云端备份库…'
  if (path.includes('/cloud-connection')) return method === 'POST' ? '正在加密并发布云端连接资料…' : method === 'PUT' ? '正在保存自动发现设置…' : '正在读取云端连接状态…'
  if (/\/(run|restore|discover|check|migrate|retention)(?:\?|$)/.test(path)) return '正在提交任务，请稍候…'
  return method === 'GET' ? '正在加载数据…' : '正在处理请求，请稍候…'
}
export async function api<T = any>(path: string, options: { method?: string; body?: unknown; task?: boolean; signal?: AbortSignal; silent?: boolean } = {}): Promise<T> {
  const method = options.method ?? 'GET'
  const requestID = ++requestSequence
  if (!options.silent) { activeRequests.set(requestID, {id: requestID, label: requestLabel(path, method)}); publishActivity() }
  try {
    const headers: Record<string, string> = { Accept: 'application/json' }
    if (options.body !== undefined) headers['Content-Type'] = 'application/json'
    if (method !== 'GET') headers['X-CSRF-Token'] = csrf
    if (options.task) headers['Idempotency-Key'] = taskKey()
    let response: Response
    try { response = await fetch('/api' + path, { method, headers, credentials: 'same-origin', body: options.body === undefined ? undefined : JSON.stringify(options.body), signal: options.signal }) }
    catch { throw new ApiError('无法连接服务，请检查网络后重试。', 'network_unavailable', true, 0) }
    if (response.status === 204) return undefined as T
    let data: any
    try { data = await response.json() } catch { throw new ApiError('服务响应无法读取，请检查服务日志。', 'invalid_response', true, response.status) }
    if (!response.ok) throw new ApiError(data.error ?? '操作未完成', data.code ?? 'request_failed', Boolean(data.retryable), response.status)
    return data
  } finally { if (activeRequests.delete(requestID)) publishActivity() }
}
export function bytes(value: unknown): string {
  if (value === undefined || value === null || !Number.isFinite(Number(value))) return '—'
  const n = Number(value)
  if (n < 1024) return `${n} B`
  const units = ['KiB', 'MiB', 'GiB', 'TiB', 'PiB']; const power = Math.min(Math.floor(Math.log(n) / Math.log(1024)), units.length)
  return `${(n / 1024 ** power).toLocaleString('zh-CN', { maximumFractionDigits: 1 })} ${units[power - 1]}`
}
export function date(value: unknown): string { if (!value || String(value).startsWith('0001-')) return '—'; const d = new Date(String(value)); return Number.isNaN(d.getTime()) ? String(value) : d.toLocaleString('zh-CN', { hour12: false }) }
export function dateInZone(value: unknown, timezone: string): string { if (!value) return '—';try{return new Date(String(value)).toLocaleString('zh-CN',{hour12:false,timeZone:timezone})}catch{return date(value)} }
export function statusLabel(value: string): string { const labels: Record<string,string> = {pending:'待处理', queued:'排队中', waiting:'等待中', checking:'检查中', scanning:'扫描中', planning:'规划中', backing_up:'备份中', backup:'备份中', committing:'提交中', completed:'完成', complete:'完成', success:'完成', partial_failed:'部分失败', partial:'部分失败', failed:'失败', cancelled:'已取消', canceled:'已取消', interrupted:'中断待处理', running:'执行中', ready:'就绪', authorized:'已授权 · 待验证', verified:'已验证', healthy:'可用', unverified:'待验证', disabled:'已停用', unavailable:'不可用', error:'异常', waiting_authorization:'等待授权', awaiting_authorization:'等待授权', awaiting_drive_selection:'请选择 Drive', initializing:'初始化仓库', checking_sources:'检查源目录', checking_accounts:'检查账号', allocating:'分配容量', reserving_capacity:'预留容量', publishing:'发布恢复点', discovering:'发现云端目录', rebuilding_index:'重建索引', checking_repositories:'检查仓库', copying_repository:'复制完整仓库', verifying_repository:'校验目标仓库', committing_location:'提交位置映射', recording_deletions:'记录删除决策', pruning:'清理未引用数据', restoring:'恢复中', verifying:'校验中', reconciling:'容量对账', deleted:'已删除', cancelling:'取消中', retry_wait:'等待重试', passed:'检查通过',new:'待初始化',account_missing:'账号缺失',verifying_current_location:'全量校验当前仓库',committing_old_copy_retirement:'提交旧副本退役记录',deleting_old_copy:'删除旧仓库副本',cleanup_pending:'清理待重试',retired:'已退役',committing_clear_index:'同步备份删除记录',clearing_accounts:'批量清空网盘'}; return labels[value] ?? value ?? '—' }
export function isActive(value: string) { return ['pending','queued','waiting','checking','scanning','planning','backing_up','backup','committing','running'].includes(value) }
export function tone(value: string) { return ['failed','partial_failed','partial','interrupted','error','unavailable'].includes(value) ? 'danger' : ['completed','complete','success','ready','verified','healthy'].includes(value) ? 'success' : 'neutral' }
