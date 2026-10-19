<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { Download, FileUp, LoaderCircle, ShieldCheck } from '@lucide/vue'
import { api, bytes, downloadBlob, type Entity } from './api'
const props = defineProps<{ mode: 'export' | 'import'; accounts: Entity[] }>()
const emit = defineEmits<{ busy: [value: boolean]; changed: [] }>()
const password = ref('')
const adminPassword = ref('')
const repeat = ref('')
const agreed = ref(false)
const selected = ref<string[]>([])
const working = ref(false)
const message = ref('')
const failure = ref('')
const filename = ref('')
const packageData = ref<any>(null)
const preview = ref<{account: Entity; duplicate: boolean}[]>([])
const results = ref<{account: Entity; state: string; error: string}[]>([])
const available = computed(() => props.accounts.filter(a => ['onedrive', 'gdrive'].includes(a.backend) && a.verified))
watch(password, () => { if (props.mode === 'import') { preview.value = []; selected.value = [] }; failure.value = '' })
async function run(fn: () => Promise<void>) {
  if (working.value) return
  working.value = true; emit('busy', true); failure.value = ''; message.value = ''
  try { await fn() } catch (e) { failure.value = e instanceof Error ? e.message : '操作未完成' }
  finally { working.value = false; emit('busy', false) }
}
async function exportFile() {
  await run(async () => {
    if (password.value !== repeat.value) throw new Error('两次输入的保护密码不一致。')
    const encrypted = await api('/accounts/export', {method: 'POST', body: {password: password.value, admin_password: adminPassword.value, account_ids: selected.value}})
    downloadBlob(new Blob([JSON.stringify(encrypted, null, 2)], {type: 'application/json'}), 'vaultpilot-accounts.json')
    password.value = ''; repeat.value = ''; agreed.value = false
    message.value = '加密账号包已下载。将文件交给 B 节点，在“存储账号 → 导入账号”中使用保护密码打开。'
  })
  adminPassword.value = ''
}
async function readFile(event: Event) {
  const input = event.target as HTMLInputElement
  const file = input.files?.[0]; preview.value = []; selected.value = []; packageData.value = null; filename.value = ''; results.value = []; failure.value = ''; message.value = ''
  if (!file) return
  try {
    if (file.size > 2 * 1024 ** 2) throw new Error('账号包不能超过 2 MiB。')
    packageData.value = JSON.parse(await file.text()); filename.value = file.name
  } catch (e) { failure.value = e instanceof Error ? e.message : '文件无法读取'; input.value = '' }
}
async function previewFile() {
  await run(async () => {
    const response = await api('/accounts/import/preview', {method: 'POST', body: {password: password.value, package: packageData.value}})
    preview.value = response.items; selected.value = []
    if (preview.value.every(item => item.duplicate)) message.value = '此文件中的 Drive 均已存在，无需重复导入。'
  })
}
async function validate(item: typeof results.value[number]) {
  item.state = '验证中'; item.error = ''
  try {
    item.account = await api(`/accounts/${item.account.id}/verify`, {method: 'POST', body: {drive_id: item.account.drive_id, version: item.account.version}})
    item.state = '已验证'
  } catch (e) { item.state = '待验证'; item.error = e instanceof Error ? e.message : '验证未完成' }
  emit('changed')
}
async function importFile() {
  await run(async () => {
    const response = await api('/accounts/import', {method: 'POST', body: {password: password.value, package: packageData.value, drive_ids: selected.value}})
    results.value = response.items.map((account: Entity) => ({account, state: '等待验证', error: ''}))
    password.value = ''; packageData.value = null; preview.value = []; agreed.value = false
    emit('changed')
    for (const item of results.value) await validate(item)
    message.value = `已导入 ${response.total} 个账号，跳过 ${response.skipped.length} 个已存在的 Drive。${results.value.some(item => item.error) ? '未通过验证的账号已保留，可重试或重新授权。' : '云端访问验证已完成。'}`
  })
}
onBeforeUnmount(() => { password.value = ''; adminPassword.value = ''; repeat.value = ''; packageData.value = null })
</script>

<template>
  <div class="account-transfer">
    <p class="section-description">{{mode === 'export' ? '把已验证的 云盘账号迁移到另一台 VaultPilot 节点。账号名称、容量预算与授权一并保存。' : '上传 A 导出的加密账号包，预览并选择需要添加的账号。导入后自动验证原 Drive 的访问。'}}</p>
    <div class="small-note"><ShieldCheck :size="18"/><span>账号包包含云盘访问与刷新凭据，使用独立密码加密。接收节点会获得原授权范围的访问权限；备份密码仍用于连接和解密云端备份。</span></div>
    <div v-if="failure" class="alert danger" role="alert">{{failure}}</div>
    <div v-if="message" class="alert info" role="status">{{message}}</div>
    <form v-if="mode==='export'" @submit.prevent="exportFile">
      <div class="checkbox-list"><label v-for="account in available" :key="account.id" class="check-label"><input v-model="selected" type="checkbox" :value="account.id" :disabled="working"/><span>{{account.name}}<small>{{account.account_email||account.account_display_name||account.drive_name}} · {{bytes(account.budget_bytes)}} 预算</small></span></label><p v-if="!available.length" class="text-secondary">没有可导出的账号，请先验证云盘访问。</p></div>
      <label class="field">当前登录密码<input v-model="adminPassword" type="password" maxlength="1024" autocomplete="current-password" required :disabled="working"/></label>
      <label class="field">账号包保护密码<input v-model="password" type="password" minlength="12" maxlength="1024" autocomplete="new-password" required :disabled="working"/><small>至少 12 个字符，请使用独立密码并告知 B 节点部署者。</small></label>
      <label class="field">再次输入保护密码<input v-model="repeat" type="password" minlength="12" maxlength="1024" autocomplete="new-password" required :disabled="working"/></label>
      <label class="check-label"><input v-model="agreed" type="checkbox" :disabled="working"/>我确认将所选云盘账号的授权共享给接收节点。</label>
      <div class="modal-actions"><button class="button primary" :disabled="working||!selected.length||!agreed"><LoaderCircle v-if="working" class="spin" :size="16"/><Download v-else :size="16"/>下载加密账号包</button></div>
    </form>
    <template v-else>
      <form v-if="!results.length" @submit.prevent="previewFile">
        <label class="field">选择加密账号包<input type="file" accept="application/json,.json" :disabled="working" @change="readFile"/><small v-if="filename">{{filename}}</small></label>
        <label class="field">账号包保护密码<input v-model="password" type="password" minlength="12" maxlength="1024" autocomplete="off" required :disabled="working"/></label>
        <button class="button secondary" :disabled="working||!packageData"><LoaderCircle v-if="working" class="spin" :size="16"/><FileUp v-else :size="16"/>解密并预览账号</button>
      </form>
      <template v-if="preview.length">
        <div class="form-section"><h3>选择要导入的账号</h3><div class="checkbox-list"><label v-for="item in preview" :key="item.account.drive_id" class="check-label"><input v-model="selected" type="checkbox" :value="item.account.drive_id" :disabled="working||item.duplicate"/><span>{{item.account.name}}<small>{{item.account.account_email||item.account.account_display_name||item.account.drive_name}} · {{bytes(item.account.budget_bytes)}} 预算</small><small>{{item.account.drive_id}} · {{item.duplicate?'本节点已存在，将跳过':'新账号'}}</small></span></label></div></div>
        <label class="check-label"><input v-model="agreed" type="checkbox" :disabled="working"/>我确认此节点可以使用所选云盘账号的授权。</label>
        <div class="modal-actions"><button class="button primary" :disabled="working||!selected.length||!agreed" @click="importFile"><LoaderCircle v-if="working" class="spin" :size="16"/><ShieldCheck v-else :size="16"/>导入并验证账号</button></div>
      </template>
      <div v-if="results.length" class="transfer-results"><div v-for="item in results" :key="item.account.id" class="transfer-result"><div><strong>{{item.account.name}}</strong><p>{{item.account.account_email||item.account.account_display_name||item.account.drive_name}}</p><small v-if="item.error">{{item.error}}</small></div><span class="badge" :class="item.state==='已验证'?'success':'neutral'">{{item.state}}</span><button v-if="item.error" class="text-button" :disabled="working" @click="run(()=>validate(item))">重试验证</button></div></div>
      <p v-if="results.length" class="section-description">恢复节点完成验证后，可前往“安全与维护 → 云端连接”，使用 A 的备份密码连接原备份库。云盘授权失效时请在账号卡片中重新授权。</p>
    </template>
  </div>
</template>
