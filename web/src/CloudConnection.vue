<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { Cloud, KeyRound, LoaderCircle, LockKeyhole, RefreshCw, ShieldCheck } from '@lucide/vue'
import { api, date } from './api'
import type { Entity } from './api'

type ConnectionState = {
  pending: boolean
  recovery: boolean
  enabled: boolean
  auto_discover: boolean
  interval_seconds: number
  last_checked?: string
  last_success?: string
  last_error?: string
}
type FailedAccount = string | { account_id?: string; id?: string; error?: string; message?: string }
type CloudVault = { vault_id: string; account_ids: string[] }
type Publication = { verified_accounts: (string | { account_id?: string; id?: string })[]; failed_accounts: FailedAccount[] }

const props = defineProps<{ pending: boolean; recovery: boolean; unlocked: boolean; accounts: Entity[] }>()
const emit = defineEmits<{ connected: []; activity: [task?: Entity] }>()
const state = ref<ConnectionState | null>(null)
const busy = ref(false)
const searching = ref(false)
const error = ref('')
const statusError = ref('')
const message = ref('')
const password = ref('')
const agreed = ref(false)
const vaults = ref<CloudVault[]>([])
const selectedVault = ref('')
const searched = ref(false)
const discoveryFailures = ref<FailedAccount[]>([])
const publication = ref<Publication | null>(null)
const accountsReady = computed(() => props.accounts.filter(account => account.verified && account.enabled))
let timer: ReturnType<typeof setTimeout> | undefined
let stopped = false
let refreshing = false
let refreshAgain = false
const requests = new Set<AbortController>()

function accountId(value: string | { account_id?: string; id?: string }): string {
  return typeof value === 'string' ? value : value.account_id || value.id || ''
}
function accountName(id: string): string { return props.accounts.find(account => account.id === id)?.name || id || '未知账号' }
function failureText(value: FailedAccount): string {
  return typeof value === 'string' ? accountName(value) : `${accountName(accountId(value))}：${value.error || value.message || '操作未完成，请检查账号授权后重试'}`
}
async function request<T>(path: string, options: Parameters<typeof api>[1] = {}): Promise<T> {
  const controller = new AbortController()
  requests.add(controller)
  try { return await api<T>(path, { ...options, signal: controller.signal }) }
  finally { requests.delete(controller) }
}
async function refresh() {
  if (stopped) return
  if (refreshing) { refreshAgain = true; return }
  refreshing = true
  try {
    const result = await request<ConnectionState>('/security/cloud-connection',{silent:state.value!==null})
    if (!stopped) {
      const hadState = state.value !== null
      const previousSuccess = state.value?.last_success
      state.value = result
      statusError.value = ''
      if (hadState && result.last_success && previousSuccess !== result.last_success) emit('activity')
    }
  } catch (e) { if (!stopped) statusError.value = e instanceof Error ? e.message : '无法读取云端连接状态' }
  finally { refreshing = false; if (refreshAgain && !stopped) { refreshAgain = false; void refresh() } }
}
async function poll() {
  await refresh()
  if (!stopped) timer = setTimeout(poll, 10000)
}
async function run(operation: () => Promise<void>) {
  if (busy.value) return
  busy.value = true
  error.value = ''
  message.value = ''
  try { await operation() }
  catch (e) { if (!stopped) error.value = e instanceof Error ? e.message : '操作未完成' }
  finally { busy.value = false }
}
async function publish() {
  await run(async () => {
    const result = await request<Publication>('/security/cloud-connection', { method: 'POST', body: { password: password.value } })
    if (stopped) return
    publication.value = { verified_accounts: result.verified_accounts || [], failed_accounts: result.failed_accounts || [] }
    password.value = ''
    agreed.value = false
    message.value = publication.value.verified_accounts.length ? '密码连接资料已发布。其他节点授权相同账号后，可用此备份密码连接。' : '没有账号完成发布，请检查下方失败原因后重试。'
    await refresh()
  })
}
async function searchVaults() {
  await run(async () => {
    searching.value = true
    selectedVault.value = ''
    vaults.value = []
    discoveryFailures.value = []
    searched.value = false
    try {
      const result = await request<{ items: CloudVault[]; failed_accounts: FailedAccount[] }>('/security/cloud-vaults')
      if (stopped) return
      vaults.value = result.items || []
      discoveryFailures.value = result.failed_accounts || []
      searched.value = true
    } finally { searching.value = false }
  })
}
async function connect() {
  if (!vaults.value.some(vault => vault.vault_id === selectedVault.value)) return
  await run(async () => {
    const result = await request<{discovery_error?:string}>('/security/cloud-connect', { method: 'POST', body: { vault_id: selectedVault.value, password: password.value } })
    if (stopped) return
    password.value = ''
    message.value = result.discovery_error || '已连接备份库，正在发现云端恢复点。'
    emit('connected')
    await refresh()
  })
}
async function toggleDiscovery() {
  await run(async () => {
    await request('/security/cloud-connection', { method: 'PUT', body: { enabled: !state.value?.auto_discover } })
    if (stopped) return
    await refresh()
    message.value = state.value?.auto_discover ? '已开启自动发现，服务每分钟检查新的云端备份。' : '已暂停自动发现，可随时手动检查。'
  })
}
async function discoverNow() {
  await run(async () => {
    const task = await request<Entity>('/security/discover', { method: 'POST', body: {}, task: true })
    if (stopped) return
    message.value = '已提交云端发现任务，完成后会更新恢复中心。'
    emit('activity', task)
  })
}
watch(() => [props.pending, props.recovery, props.unlocked], () => { password.value = ''; void refresh() })
onMounted(poll)
onUnmounted(() => { stopped = true; if (timer) clearTimeout(timer); for (const controller of requests) controller.abort(); password.value = '' })
</script>

<template>
  <section class="panel cloud-connection" aria-label="多节点云端连接">
    <div class="panel-head">
      <h2><Cloud :size="19"/>{{pending?'连接已有云端备份库':recovery?'恢复节点 · 云端发现':'多节点密码连接'}}</h2>
      <span class="badge" :class="pending?'warning':recovery&&state?.auto_discover?'success':state?.enabled?'success':'neutral'">{{!state?'正在读取状态':pending?'等待连接':recovery?(state?.auto_discover?'自动发现已开启':'手动发现'):state?.enabled?'已启用':'尚未启用'}}</span>
    </div>
    <div class="cloud-body">
      <div class="cloud-flow" aria-label="A 节点备份到云端，B 节点发现并恢复">
        <span><strong>A · 备份节点</strong><small>写入加密备份</small></span><span class="flow-arrow">→</span><span><Cloud :size="21"/><strong>OneDrive</strong></span><span class="flow-arrow">→</span><span><strong>B · 恢复节点</strong><small>发现恢复点 · 按需恢复</small></span>
      </div>
      <p class="cloud-description">{{pending?'先授权 A 节点使用的 OneDrive 账号，再查找备份库并输入 A 的备份密码。无需传递恢复包。':recovery?'此节点只读取与恢复。自动发现会更新恢复点列表，文件恢复仍由你选择范围与本地目标后执行。':'启用后，其他节点可授权相同 OneDrive 账号，并使用此库的备份密码连接。一个备份节点可配合多个恢复节点。'}}</p>

      <div v-if="error||statusError" class="alert danger" role="alert">{{error||statusError}}</div>
      <div v-if="message" class="alert" role="status">{{message}}</div>
      <div v-if="!unlocked&&!pending" class="alert warning"><LockKeyhole :size="17"/><span>请先解锁本节点，再连接、发布或发现云端备份。自动发现也会在锁定时暂停。</span></div>
      <div v-if="!accountsReady.length" class="alert warning"><ShieldCheck :size="17"/><span>请到「存储账号」添加、授权并验证原备份使用的账号，再回到这里。完整恢复需要访问备份涉及的所有账号。</span></div>

      <template v-if="pending">
        <div class="cloud-actions"><button class="button secondary" :disabled="busy||!accountsReady.length" @click="searchVaults"><LoaderCircle v-if="searching" class="spin" :size="16"/><RefreshCw v-else :size="16"/>{{searched?'重新查找云端备份库':'查找云端备份库'}}</button></div>
        <div v-if="searched&&!vaults.length" class="cloud-empty">没有发现可连接的备份库。请确认 A 节点已经启用「多节点密码连接」，并且这里授权的是 A 使用的原账号及 Drive。</div>
        <fieldset v-if="vaults.length" class="vault-picker"><legend>请选择要连接的备份库</legend><label v-for="vault in vaults" :key="vault.vault_id" class="vault-option" :class="{selected:selectedVault===vault.vault_id}"><input v-model="selectedVault" type="radio" name="cloud-vault" :value="vault.vault_id" :disabled="busy"/><span><strong>{{vault.account_ids.map(accountName).join('、')||'已授权账号'}}</strong><code>{{vault.vault_id}}</code></span></label></fieldset>
        <div v-if="discoveryFailures.length" class="cloud-failures" role="status"><strong>以下账号未能完成查找</strong><ul><li v-for="(failure,index) in discoveryFailures" :key="index">{{failureText(failure)}}</li></ul></div>
        <form v-if="vaults.length" class="cloud-form" @submit.prevent="connect"><label class="field">A 节点的备份密码<input v-model="password" type="password" autocomplete="off" :disabled="busy" required minlength="12" maxlength="1024" placeholder="输入原备份库的解锁密码"/><small>密码用于解锁选中的备份库；本节点管理员登录密码须与备份密码不同。</small></label><button class="button primary" :disabled="busy||!accountsReady.length||!selectedVault||!password"><LoaderCircle v-if="busy" class="spin" :size="16"/><KeyRound v-else :size="16"/>连接并建立恢复节点</button></form>
      </template>

      <template v-else-if="recovery">
        <div class="discovery-setting"><div><strong>每分钟自动发现新备份</strong><p>服务运行且节点解锁时检查云端目录，完成后更新恢复中心。</p></div><button class="button" :class="state?.auto_discover?'secondary':'primary'" :disabled="busy||!state||!unlocked||!accountsReady.length" :aria-pressed="Boolean(state?.auto_discover)" @click="toggleDiscovery">{{state?.auto_discover?'暂停自动发现':'开启自动发现'}}</button></div>
        <dl class="discovery-facts"><div><dt>最近检查</dt><dd>{{date(state?.last_checked)}}</dd></div><div><dt>最近发现成功</dt><dd>{{date(state?.last_success)}}</dd></div></dl>
        <div v-if="state?.last_error" class="alert warning" role="status"><span>最近检查未完成：{{state.last_error}}<small>请检查账号授权与网络，修复后可立即检查或等待下一次自动发现。</small></span></div>
        <button class="button secondary" :disabled="busy||!unlocked||!accountsReady.length" @click="discoverNow"><LoaderCircle v-if="busy" class="spin" :size="16"/><RefreshCw v-else :size="16"/>立即发现云端备份</button>
      </template>

      <template v-else>
        <form class="cloud-form" @submit.prevent="publish"><label class="field">当前备份库的原始解锁密码<input v-model="password" type="password" autocomplete="off" :disabled="busy" required minlength="12" maxlength="1024" placeholder="输入创建备份库时设置的密码"/><small>服务会先验证密码，再向已验证并启用的账号发布加密连接资料。</small></label><label class="check-label cloud-agreement"><input v-model="agreed" type="checkbox" :disabled="busy"/><span>我确认将加密的备份库解锁资料保存在云端，允许持有相同账号访问权限和备份密码的节点连接为恢复节点。</span></label><button class="button primary" :disabled="busy||!unlocked||!accountsReady.length||!password||!agreed"><LoaderCircle v-if="busy" class="spin" :size="16"/><Cloud v-else :size="16"/>{{state?.enabled?'重新发布连接资料':'启用并发布密码连接'}}</button></form>
        <div v-if="publication?.verified_accounts.length" class="publication-success"><ShieldCheck :size="16"/><span>已发布到：{{publication.verified_accounts.map(value=>accountName(accountId(value))).join('、')}}</span></div>
        <div v-if="publication?.failed_accounts.length" class="cloud-failures" role="status"><strong>以下账号发布失败，请修复后重新发布</strong><ul><li v-for="(failure,index) in publication.failed_accounts" :key="index">{{failureText(failure)}}</li></ul></div>
        <p class="cloud-footnote">连接资料以备份密码加密，不保存明文密码或账号令牌。仍建议保留离线恢复包，以便云端连接资料不可用时恢复。</p>
      </template>
    </div>
  </section>
</template>

<style scoped>
.cloud-connection{margin-bottom:26px}.panel-head h2{display:flex;align-items:center;gap:9px}.cloud-body{padding:24px}.cloud-flow{display:flex;align-items:center;justify-content:center;gap:24px;padding:20px;background:#f6f9ef;border:1px solid #e5ecd7;border-radius:8px}.cloud-flow>span:not(.flow-arrow){display:flex;align-items:center;flex-direction:column;gap:7px;color:#718e61;text-align:center}.cloud-flow strong{font-size:12px;font-weight:500}.cloud-flow small{font-size:10px;color:#9aac8c}.flow-arrow{color:#a1b590;font-size:20px}.cloud-description{font-size:12px;line-height:1.9;color:#879b74;margin:19px 0 22px}.cloud-actions{margin:20px 0}.cloud-form{max-width:650px;margin-top:22px}.cloud-form .field{margin-bottom:19px}.cloud-form input[type=password]{max-width:460px}.cloud-form .button{margin-top:4px}.cloud-agreement{align-items:flex-start;font-size:11px;color:#7e936c;line-height:1.9;margin:20px 0}.cloud-agreement input{flex-shrink:0;margin-top:4px}.vault-picker{border:0;padding:0;margin:22px 0;min-width:0}.vault-picker legend{font-size:12px;color:#708961;margin-bottom:12px}.vault-option{display:flex;align-items:center;gap:13px;padding:16px;border:1px solid #e1e8d7;border-radius:8px;margin-bottom:10px;cursor:pointer;background:#fbfcf8}.vault-option.selected{border-color:#8da785;background:#f0f5e9}.vault-option input{flex-shrink:0}.vault-option span{min-width:0}.vault-option strong{font-size:12px;font-weight:500;color:#657d55;display:block;margin-bottom:7px}.vault-option code{font-size:11px;color:#8ea17c;overflow-wrap:anywhere}.cloud-empty{font-size:12px;line-height:1.9;color:#8c9f7c;background:#f7f9f2;border-radius:8px;padding:18px;margin-top:20px}.cloud-failures{font-size:11px;line-height:1.9;color:#ac8752;border:1px solid #ecdfc9;background:#fcf9f2;border-radius:8px;padding:15px 18px;margin-top:18px;overflow-wrap:anywhere}.cloud-failures strong{font-weight:500}.cloud-failures ul{padding-left:18px;margin:8px 0 0}.publication-success{display:flex;gap:8px;font-size:11px;color:#789565;margin-top:20px;line-height:1.8}.publication-success svg{flex-shrink:0;margin-top:2px}.cloud-footnote{color:#9dae8c;font-size:10px;line-height:1.9;margin:20px 0 0}.discovery-setting{display:flex;align-items:center;justify-content:space-between;gap:20px;padding:20px 0 22px;border-bottom:1px solid #e7eddf}.discovery-setting strong{font-size:13px;color:#6b865a;font-weight:500}.discovery-setting p{font-size:11px;line-height:1.9;color:#96a785;margin-top:9px}.discovery-setting .button{flex-shrink:0}.discovery-facts{display:grid;grid-template-columns:1fr 1fr;gap:25px;margin:23px 0}.discovery-facts dt{font-size:10px;color:#99ab88}.discovery-facts dd{font-size:12px;color:#748d63;margin:10px 0 0}.cloud-body>.alert{margin:18px 0}.cloud-body .alert small{display:block;margin-top:6px;line-height:1.8}@media(max-width:600px){.cloud-body{padding:20px}.cloud-flow{gap:10px;padding:17px 10px}.cloud-flow strong{font-size:10px}.cloud-flow small{font-size:8px}.cloud-flow svg{width:18px}.flow-arrow{font-size:16px}.cloud-description{font-size:11px}.discovery-setting{align-items:flex-start;flex-direction:column;gap:13px}.discovery-facts{grid-template-columns:1fr;gap:20px}.cloud-form .button{font-size:11px}.cloud-agreement{font-size:10px}.vault-option{padding:14px}.vault-option code{font-size:10px}.panel-head{gap:12px;flex-wrap:wrap}.panel-head .badge{font-size:9px}}
</style>
