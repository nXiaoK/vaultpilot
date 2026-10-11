<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { Bell, Check, LoaderCircle, RefreshCw, Send, ShieldCheck } from 'lucide-vue-next'
import { api, date } from './api'
const emit = defineEmits<{ busy: [value: boolean] }>()
const settings = reactive({ version: 0, enabled: false, node_name: 'VaultPilot', chat_id: '', bot_token: '', clear_token: false, backup_success: true, backup_failure: true, account_error: true })
const info = ref<any>(null)
const working = ref(false)
const ready = ref(false)
const failure = ref('')
const notice = ref('')
const changedElsewhere = computed(() => info.value && info.value.version !== settings.version)
const canSend = computed(() => ready.value && info.value?.enabled && info.value?.token_configured && !working.value && !changedElsewhere.value)
let poll: ReturnType<typeof setInterval> | undefined
async function run(fn: () => Promise<void>) {
  if (working.value) return
  working.value = true; emit('busy', true); failure.value = ''; notice.value = ''
  try { await fn() } catch (error) { failure.value = error instanceof Error ? error.message : '通知操作未完成' }
  finally { working.value = false; emit('busy', false) }
}
function apply(value: any) {
  info.value = value
  Object.assign(settings, { version: value.version, enabled: value.enabled, node_name: value.node_name, chat_id: value.chat_id, backup_success: value.backup_success, backup_failure: value.backup_failure, account_error: value.account_error, bot_token: '', clear_token: false })
  ready.value = true
}
async function load() { await run(async () => { apply(await api('/notifications/telegram')) }) }
async function save() { await run(async () => {
  apply(await api('/notifications/telegram', { method: 'PUT', body: { ...settings } }))
  notice.value = 'Telegram 通知设置已保存。'
}) }
async function test() { if (!canSend.value) return; await run(async () => {
  await api('/notifications/telegram/test', { method: 'POST', body: {} })
  notice.value = '测试通知已排队，请查看 Telegram 和下方发送记录。'
  info.value = await api('/notifications/telegram')
}) }
async function retry() { await run(async () => {
  await api('/notifications/telegram/retry', { method: 'POST', body: {} })
  notice.value = '失败的通知已重新排队。'
  info.value = await api('/notifications/telegram')
}) }
async function check() { await run(async () => {
  await api('/notifications/telegram/check-accounts', { method: 'POST', body: {} })
  notice.value = '已提交账号授权检查；发现授权异常时发送通知。'
}) }
const kindLabel = (kind: string) => ({ test: '测试通知', backup_success: '备份成功', backup_failure: '备份异常', account_error: '账号授权异常' }[kind] || kind)
const deliveryLabel = (status: string) => ({ queued: '等待发送 / 重试', sent: '已发送', failed: '发送失败', cancelled: '已取消' }[status] || status)
onMounted(() => {
  void load()
  poll = setInterval(async () => { if (!ready.value || working.value) return; try { info.value = await api('/notifications/telegram', { silent: true }) } catch { /* Manual reload displays an actionable error. */ } }, 5000)
})
onUnmounted(() => { if (poll) clearInterval(poll) })
</script>

<template>
  <section class="panel telegram-notifications" aria-label="Telegram 通知设置" :aria-busy="working">
    <div class="panel-head"><h2><Bell :size="21"/>Telegram 通知</h2><button class="text-button" :disabled="working" @click="load"><RefreshCw :size="15"/>重新加载</button></div>
    <div class="telegram-content">
      <p class="section-description">将备份结果和存储账号授权异常发送到指定 Telegram 用户。每个节点单独配置，可用节点名称区分 A/B 机器。</p>
      <div v-if="failure" class="alert danger" role="alert">{{failure}}</div><div v-if="notice" class="alert success" role="status"><Check :size="18"/>{{notice}}</div>
      <div v-if="changedElsewhere" class="alert warning">设置已被其他会话修改，请重新加载后再保存。</div>
      <form @submit.prevent="save">
        <label class="check-label"><input v-model="settings.enabled" type="checkbox" :disabled="working||!ready"/>启用 Telegram 通知</label>
        <div class="form-grid">
          <label class="field">节点名称<input v-model="settings.node_name" maxlength="80" :disabled="working||!ready" placeholder="例如：A · 备份服务器"/></label>
          <label class="field">接收用户 Chat ID<input v-model="settings.chat_id" inputmode="numeric" :disabled="working||!ready" placeholder="个人用户的数字 Chat ID"/><small>填写数字 ID，不能填 @用户名；用户须先私聊机器人发送 /start。</small></label>
        </div>
        <label class="field">Bot Token<input v-model="settings.bot_token" type="password" autocomplete="new-password" :disabled="working||!ready" :placeholder="info?.token_configured?'已配置 · 留空保留现有 Token':'粘贴 BotFather 提供的 Token'"/><small>Token 加密保存在本节点，不会回显，也不随恢复包或账号导出。</small></label>
        <label v-if="info?.token_configured" class="check-label"><input v-model="settings.clear_token" type="checkbox" :disabled="working"/>删除已保存的 Token（请同时关闭通知，或填写新 Token）</label>
        <div class="telegram-events"><label class="check-label"><input v-model="settings.backup_success" type="checkbox" :disabled="working||!ready"/>备份成功</label><label class="check-label"><input v-model="settings.backup_failure" type="checkbox" :disabled="working||!ready"/>备份失败 / 取消 / 中断</label><label class="check-label"><input v-model="settings.account_error" type="checkbox" :disabled="working||!ready"/>存储账号授权异常</label></div>
        <div class="row-actions"><button class="button primary" :disabled="working||!ready||changedElsewhere"><LoaderCircle v-if="working" class="spin" :size="16"/>保存通知设置</button><button type="button" class="button secondary" :disabled="!canSend" @click="test"><Send :size="16"/>发送测试通知</button><button type="button" class="text-button" :disabled="!canSend||!info?.account_error" @click="check">立即检查账号授权</button></div>
      </form>
      <div class="small-note"><ShieldCheck :size="18"/><span>通知包含节点、计划或账号名称、任务标识与结果统计；不发送文件清单、密码或授权凭据。备份重试后只发送最终结果。每 15 分钟检查已启用且已验证的 OneDrive 账号；同一授权异常只提醒一次，重新授权或恢复正常后可再次告警。关闭通知或更换 Token / 接收用户会取消旧的待发送通知，已在发送中的请求可能仍会完成。</span></div>
      <p class="fine text-secondary">测试通知和授权检查使用已保存的配置；修改设置后请先保存。</p>
      <details class="telegram-help"><summary>如何取得 Bot Token 和用户 Chat ID</summary><ol><li>在 Telegram 找到官方 @BotFather，发送 /newbot，按提示创建机器人并复制 Token。</li><li>接收用户打开新机器人私聊，发送 /start。</li><li>在自己的浏览器访问 https://api.telegram.org/bot&lt;Token&gt;/getUpdates，在 message.chat.id 中找到该用户的数字 ID。不要把含 Token 的地址分享给别人。若结果为空，再向机器人发送一条消息后刷新。</li><li>填写 Token 和 Chat ID，启用通知并保存，再发送测试通知。如果该机器人已设置 webhook，getUpdates 无法使用；请通过已有机器人服务取得 Chat ID。</li></ol></details>
      <div class="panel-head telegram-record-head"><h3>最近发送记录</h3><button class="text-button" :disabled="!canSend||!info?.failed_count" @click="retry">重试失败通知</button></div>
      <p v-if="info?.queued_count||info?.failed_count" class="fine text-secondary">待发送 {{info.queued_count}} 条 · 发送失败 {{info.failed_count}} 条；下方显示最近 10 条记录。</p>
      <div v-if="info?.recent?.length" class="table-scroll"><table><thead><tr><th>事件</th><th>状态</th><th>时间</th><th>说明</th></tr></thead><tbody><tr v-for="item in info.recent" :key="item.id"><td>{{kindLabel(item.kind)}}</td><td><span class="badge" :class="item.status==='sent'?'success':item.status==='failed'?'danger':'neutral'">{{deliveryLabel(item.status)}}</span><small class="cell-note">已尝试 {{item.attempts}} 次</small></td><td>{{date(item.sent_at||item.created_at)}}</td><td>{{item.last_error||'—'}}</td></tr></tbody></table></div><p v-else class="text-secondary">{{ready?'尚无通知记录。':'正在加载通知设置…'}}</p>
    </div>
  </section>
</template>

<style scoped>
.telegram-notifications{margin-bottom:24px}.telegram-content{padding:24px}.telegram-events{display:flex;flex-wrap:wrap;gap:16px;margin:20px 0}.telegram-content form>.check-label{margin-bottom:18px}.telegram-content .small-note{margin-top:24px}.telegram-help{margin-top:20px}.telegram-help summary{cursor:pointer;font-weight:600}.telegram-help li{margin:10px 0;overflow-wrap:anywhere;line-height:1.7}.telegram-record-head{margin-top:24px;padding:12px 0}.telegram-notifications h2{display:flex;align-items:center;gap:10px}
@media(max-width:640px){.telegram-content{padding:16px}.telegram-events{display:grid;gap:12px}}
</style>
