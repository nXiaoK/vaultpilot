<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import { ArrowUpRight, FileUp, LoaderCircle, RefreshCw, ShieldCheck } from '@lucide/vue'
import { api, date } from './api'
import type { Entity } from './api'
const props = defineProps<{ account: Entity; customOAuth: boolean; rcloneAvailable: boolean; busy?: boolean }>()
const emit = defineEmits<{ authorized: [account: Entity]; custom: []; resume: []; busy: [value: boolean] }>()
const backend = computed(() => props.account.backend === 'gdrive' ? 'gdrive' : 'onedrive')
const provider = computed(() => backend.value === 'gdrive' ? 'Google' : '微软')
const rcloneType = computed(() => backend.value === 'gdrive' ? 'drive' : 'onedrive')
const callbackURL = computed(() => backend.value === 'gdrive' ? 'http://127.0.0.1:53682/' : 'http://localhost:53682/')
const importPlaceholder = computed(() => `[my_${rcloneType.value}]\ntype = ${rcloneType.value}\ntoken = {…}`)
const method = ref('standard')
const pending = ref(false)
const working = computed(()=>pending.value||props.busy)
const resumeLoading = ref(false)
function resume() { resumeLoading.value=true; emit('resume') }
watch(()=>props.busy, value=>{if(!value)resumeLoading.value=false})
const error = ref('')
const flow = ref<any>(null)
const callback = ref('')
const config = ref('')
const filename = ref('')
const remotes = ref<any[]>([])
const selected = ref('')
let timer: ReturnType<typeof setTimeout> | undefined
let disposed = false
const active = () => flow.value && ['starting', 'waiting', 'exchanging', 'cancelling'].includes(flow.value.status)
const flowPath = () => `/accounts/${props.account.id}/rclone/authorize?flow_id=${encodeURIComponent(flow.value.id)}`
async function run(fn: () => Promise<void>) { if (pending.value) return; pending.value = true; emit('busy', true); error.value = ''; try { await fn() } catch (e) { error.value = e instanceof Error ? e.message : '操作未完成' } finally { pending.value = false; emit('busy', false) } }
async function poll() {
  if (disposed || !active()) return
  try {
    flow.value = await api(flowPath(),{silent:true})
    if (flow.value.status === 'completed') { callback.value = ''; emit('authorized', flow.value.account); return }
    if (['failed', 'cancelled'].includes(flow.value.status)) return
  } catch (e) { error.value = e instanceof Error ? e.message : '无法读取授权状态' }
  if (!disposed) timer = setTimeout(poll, 1200)
}
async function start() { await run(async () => { flow.value = await api(`/accounts/${props.account.id}/rclone/authorize`, { method: 'POST', body: { version: props.account.version } }); callback.value = ''; if (disposed) { await api(flowPath(), { method: 'DELETE', body: {} }); return }; await poll() }) }
async function cancel() { await run(async () => { if (active()) { flow.value = await api(flowPath(), { method: 'DELETE', body: {} }); if (timer) clearTimeout(timer); await poll() }; callback.value = '' }) }
async function submitCallback() { await run(async () => { await api(`/accounts/${props.account.id}/rclone/callback?flow_id=${encodeURIComponent(flow.value.id)}`, { method: 'POST', body: { url: callback.value } }); callback.value = ''; flow.value.status = 'exchanging' }) }
function changedConfig() { remotes.value = []; selected.value = '' }
async function upload(event: Event) { await run(async () => { const file = (event.target as HTMLInputElement).files?.[0]; if (!file) return; if (file.size > 1024 * 1024) throw new Error('配置文件不能超过 1 MiB'); config.value = await file.text(); filename.value = file.name; changedConfig() }); (event.target as HTMLInputElement).value = '' }
async function preview() { await run(async () => { const result = await api('/rclone/import/preview', { method: 'POST', body: { config: config.value, backend: backend.value } }); remotes.value = result.items; selected.value = '' }) }
async function importConfig() { await run(async () => { const account = await api(`/accounts/${props.account.id}/rclone/import`, { method: 'POST', body: { config: config.value, remote: selected.value, version: props.account.version } }); config.value = ''; filename.value = ''; remotes.value = []; if (!disposed) emit('authorized', account) }) }
onUnmounted(() => { emit('busy', false); disposed = true; if (timer) clearTimeout(timer); if (active()) void api(flowPath(), { method: 'DELETE', body: {} }).catch(() => {}); config.value = ''; callback.value = '' })
</script>

<template>
  <div class="oauth-intro"><span class="account-icon"><ShieldCheck :size="30" /></span><h3>连接「{{account.name}}」</h3><p>{{backend==='gdrive'?'使用个人 Google 账号连接“我的云端硬盘”，容量来自 Google One。选择 Rclone 标准授权或导入现有配置，无需注册应用。':'Microsoft 365 Family 使用个人 OneDrive。选择 Rclone 标准授权或导入现有配置，无需开通 Azure 或注册微软应用。'}}</p></div>
  <div class="setup-options auth-methods"><button :class="{selected:method==='standard'}" :disabled="working||active()" @click="method='standard'"><ShieldCheck :size="20"/><strong>Rclone 标准授权</strong><small>在{{provider}}页面登录并同意授权</small></button><button :class="{selected:method==='import'}" :disabled="working||active()" @click="method='import'"><FileUp :size="20"/><strong>导入 Rclone 配置</strong><small>上传文件或粘贴授权结果</small></button></div>
  <div v-if="error" class="alert danger" role="alert">{{error}}</div>
  <template v-if="method==='standard'">
    <div class="small-note"><ShieldCheck :size="17"/><span>授权使用 Rclone 自带的{{provider}}应用。{{provider}}页面会显示它请求的权限；凭据仅在服务器加密保存。</span></div>
    <div v-if="!rcloneAvailable" class="alert warning">服务器未安装 Rclone。可先导入已有配置，但运行备份与刷新授权仍需安装 Rclone。</div>
    <button v-if="!active()" class="button primary full" :disabled="working||!rcloneAvailable" @click="start"><LoaderCircle v-if="pending" class="spin" :size="17"/>{{pending?'正在启动授权…':flow?'重新开始标准授权':'开始 Rclone 标准授权'}}<ArrowUpRight v-if="!pending" :size="17"/></button>
    <template v-if="active()">
      <div class="alert"><LoaderCircle class="spin" :size="18"/><span>{{flow.status==='starting'?'正在启动 Rclone…':flow.status==='exchanging'?'正在交换并加密保存凭据…':flow.status==='cancelling'?'正在取消授权…':`等待${provider}授权`}}<small>有效期至 {{date(flow.expires_at)}}</small></span></div>
      <a v-if="flow.authorization_url" class="button primary full" :href="flow.authorization_url" target="_blank" rel="noopener noreferrer">打开{{provider}}授权页面<ArrowUpRight :size="17"/></a>
      <div v-if="flow.status==='waiting'" class="callback-help"><p>登录并同意后，{{provider}}会跳转到 <code>{{callbackURL}}</code>。</p><p>本机运行时通常自动完成。远程服务器或 Docker 部署时，可能显示“无法访问此网站”；请复制该页地址栏的完整地址，粘贴到下方提交。</p><label class="field">授权后的完整回调地址<textarea v-model="callback" rows="3" spellcheck="false" autocomplete="off" :placeholder="`${callbackURL}?code=…&state=…`"/><small>该地址包含临时授权码，请勿分享给他人。</small></label><button class="button primary full" :disabled="working||!callback.trim()" @click="submitCallback">提交回调并完成授权</button></div>
      <button class="button secondary full" :disabled="working||flow.status==='cancelling'" @click="cancel">取消本次授权</button>
    </template>
    <div v-if="flow&&!active()" class="alert" :class="flow.status==='failed'?'danger':''">{{flow.message}}</div>
  </template>
  <template v-else>
    <p class="section-description">支持未加密的 <code>rclone.conf</code>、<code>rclone config dump</code> JSON 和 <code>rclone authorize {{rcloneType}}</code> 的授权结果。多账号配置每次选择一个 remote 导入。</p>
    <label class="upload-area"><FileUp :size="21"/><span>{{filename||'选择 Rclone 配置文件'}}</span><input type="file" accept=".conf,.json,.txt,text/plain,application/json" :disabled="working" @change="upload"/></label>
    <label class="field">或粘贴配置 / 授权结果<textarea v-model="config" rows="6" spellcheck="false" autocomplete="off" :placeholder="importPlaceholder" @input="filename=''; changedConfig()"/><small>包含访问凭据，请仅在可信的 HTTPS 或本机页面操作。文件不会原样保存。</small></label>
    <button class="button secondary full" :disabled="working||!config.trim()" @click="preview"><LoaderCircle v-if="pending" class="spin" :size="16"/>{{pending?'正在解析配置…':'解析并预览可导入账号'}}</button>
    <template v-if="remotes.length"><label class="field">选择本次导入的 remote<select v-model="selected"><option value="" disabled>请选择一个 remote</option><option v-for="remote in remotes" :key="remote.name" :value="remote.name">{{remote.name}} {{remote.custom_client?'（自定义应用）':'（Rclone 标准应用）'}} {{remote.drive_id?`· ${remote.drive_id}`:''}}</option></select><small>导入的 Drive 信息仅供参考，下一步仍需从{{provider}}实际返回的列表选择并验证。</small></label><button class="button primary full" :disabled="working||!selected" @click="importConfig"><LoaderCircle v-if="working" class="spin" :size="16"/>{{working?'正在保存并读取 Drive…':'加密保存并读取 Drive'}}</button></template>
  </template>
  <button class="button secondary full" :disabled="working||active()" @click="resume" :aria-busy="Boolean(busy&&resumeLoading)"><LoaderCircle v-if="busy&&resumeLoading" class="spin" :size="16"/>{{busy&&resumeLoading?'正在读取 Drive…':'已授权，继续读取 Drive'}}<RefreshCw v-if="!busy||!resumeLoading" :size="16"/></button>
  <details v-if="backend==='onedrive'&&customOAuth&&!active()" class="callback-help"><summary>高级：使用部署者注册的微软应用</summary><p>当前实例已配置自定义 OAuth 应用，可沿用原网页授权方式。</p><button class="button secondary" :disabled="working" @click="emit('custom')">使用自定义应用授权<ArrowUpRight :size="16"/></button></details>
</template>
