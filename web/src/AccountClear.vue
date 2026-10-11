<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { Cloud, HardDrive, LoaderCircle, Search, ShieldCheck, Trash2 } from 'lucide-vue-next'
import { api, bytes, type Entity } from './api'
const props = defineProps<{ accounts: Entity[]; initialIds?: string[] }>()
const emit = defineEmits<{ busy: [value: boolean]; submitted: [task: Entity] }>()
const available = computed(() => props.accounts.filter(account => account.verified && account.drive_id))
const selected = ref<string[]>([...(props.initialIds ?? [])])
const working = ref(false)
const failure = ref('')
const preview = ref<any>(null)
const confirmation = ref('')
const agreed = ref(false)
const expectedConfirmation = '清空所选备份'
const canSubmit = computed(() => Boolean(preview.value && !preview.value.blocked && agreed.value && confirmation.value === expectedConfirmation && !working.value))
watch(selected, () => { preview.value = null; confirmation.value = ''; agreed.value = false; failure.value = '' }, { deep: true })
async function run(fn: () => Promise<void>) {
  if (working.value) return
  working.value = true; emit('busy', true); failure.value = ''
  try { await fn() } catch (error) { failure.value = error instanceof Error ? error.message : '操作未完成' }
  finally { working.value = false; emit('busy', false) }
}
async function inspect() { await run(async () => {
  preview.value = null; confirmation.value = ''; agreed.value = false
  preview.value = await api('/accounts/clear/preview', { method: 'POST', body: { account_ids: [...selected.value] } })
}) }
async function submit() {
  if (!canSubmit.value) return
  let task: Entity | undefined
  await run(async () => {
    task = await api('/accounts/clear', { method: 'POST', task: true, body: { account_ids: [...selected.value], preview_token: preview.value.preview_token, confirmation: confirmation.value } })
  })
  if (task) emit('submitted', task)
}
function selectAll() { selected.value = selected.value.length === available.value.length ? [] : available.value.map(account => account.id) }
</script>

<template>
  <div class="account-clear">
    <p class="section-description">选择一个或多个网盘，清空它们在当前备份库中的备份数据。适合测试结束后重新开始备份。</p>
    <div class="alert warning"><ShieldCheck :size="20"/><div><strong>删除无法撤销</strong><p>仅清理当前库的仓库文件和旧索引。网盘其他目录、本地备份源、账号授权与节点连接信息会保留。跨盘恢复点可能随所选仓库一起失效。</p></div></div>
    <div v-if="failure" class="alert danger" role="alert">{{failure}}</div>
    <details class="clear-selection" :open="!preview"><summary>选择要清空的账号 · 已选 {{selected.length}} 个</summary><div class="panel-head"><h3>已验证的存储账号</h3><button class="text-button" :disabled="working||!available.length" @click="selectAll">{{selected.length===available.length&&available.length?'取消全选':'全选已验证账号'}}</button></div>
    <div class="checkbox-list"><label v-for="account in available" :key="account.id" class="check-label"><input v-model="selected" type="checkbox" :value="account.id" :disabled="working"/><Cloud v-if="account.backend==='onedrive'" :size="18"/><HardDrive v-else :size="18"/><span>{{account.name}}<small>{{account.account_email||account.account_display_name||account.drive_id}}</small><small>上次查询的项目占用：{{account.quota_at?bytes(account.project_used_bytes):'未查询'}}</small></span></label><p v-if="!available.length" class="text-secondary">请先完成存储账号访问验证。</p></div></details>
    <button class="button secondary" :disabled="working||!selected.length" @click="inspect"><LoaderCircle v-if="working" class="spin" :size="16"/><Search v-else :size="16"/>预览清空范围</button>
    <template v-if="preview">
      <div class="clear-preview" aria-live="polite">
        <p class="clear-account-names"><strong>所选账号：</strong>{{preview.items.map((item:any)=>item.email?`${item.name} (${item.email})`:item.name).join('、')}}</p><div class="path-bar"><Cloud :size="17"/><code>{{preview.path}}</code></div>
        <p class="fine text-secondary">将删除此目录下 repositories 中的全部备份文件，以及旧的 catalog 索引；保留最新删除记录和 connect.vpkey 连接入口。更新索引需要 {{preview.catalog_account_count}} 个已验证账号在线，包含已停用账号。</p>
        <div v-if="preview.resume" class="alert warning">这是上次未完成的清理。请保留同一组账号重新执行；已完成的删除不会恢复。</div>
        <div class="detail-stats"><div><span>选择的网盘</span><strong>{{preview.items.length}}</strong></div><div><span>失效的恢复点</span><strong>{{preview.point_ids.length}}</strong></div><div><span>暂停的计划</span><strong>{{preview.pause_plan_ids.length}}</strong></div></div>
        <p>将退役 {{preview.repository_ids.length}} 个失去可用副本的仓库；已有其他已验证副本的仓库继续保留恢复能力。</p>
        <div v-if="preview.blocked" class="alert danger" role="alert"><div><strong>当前无法清空</strong><p v-for="reason in preview.reasons" :key="reason">{{reason}}</p><small v-if="preview.protected_point_ids.length">受保护恢复点：{{preview.protected_point_ids.join('、')}}</small></div></div>
        <template v-else>
          <label class="check-label"><input v-model="agreed" type="checkbox" :disabled="working"/>我确认删除所选网盘的当前库备份，并暂停受影响的定时计划。</label>
          <label class="field">输入“清空所选备份”确认<input v-model="confirmation" :disabled="working" autocomplete="off" placeholder="清空所选备份"/></label>
          <div class="modal-actions"><button class="button danger" :disabled="!canSubmit" @click="submit"><LoaderCircle v-if="working" class="spin" :size="16"/><Trash2 v-else :size="16"/>提交批量清空任务</button></div>
        </template>
      </div>
    </template>
  </div>
</template>

<style scoped>
.clear-selection{margin:20px 0}.clear-selection summary{cursor:pointer;font-weight:600}.clear-selection .panel-head{margin-top:12px;padding:8px 0}.clear-account-names{overflow-wrap:anywhere}
.clear-preview{margin-top:24px;border-top:1px solid var(--line);padding-top:20px}
.clear-preview .check-label{margin:20px 0}
.clear-preview .field{margin-top:16px}
.clear-preview .path-bar code{white-space:normal;overflow-wrap:anywhere}
</style>
