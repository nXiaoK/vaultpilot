<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, reactive, ref } from 'vue'
import { ChevronRight, ChevronLeft, Folder, File, Link, Search, RefreshCw, ArrowUp, LoaderCircle, X, Home } from 'lucide-vue-next'
import { api, bytes, date } from './api'
import type { Entity } from './api'

const props = defineProps<{ source: Entity }>()
type Entry = { name: string; path: string; kind: string; size: number | null; modified_at: string; mode: string; can_open: boolean; reason?: string; link_target?: string }
const state = reactive({ path: '', items: [] as Entry[], total: 0, next_cursor: '' })
const search = ref('')
const loading = ref(false)
const failure = ref('')
const selected = ref<Entry | null>(null)
const content = ref<{ kind: string; mime?: string; data?: string; text?: string; truncated?: boolean } | null>(null)
const contentLoading = ref(false)
const contentError = ref('')
const inspector = ref<HTMLElement | null>(null)
const cursors = ref([''])
let request: AbortController | undefined
let previewRequest: AbortController | undefined
let stopped = false
const crumbs = computed(() => state.path.split('/').filter(Boolean).map((name, index, parts) => ({ name, path: parts.slice(0, index+1).join('/') })))
const parent = computed(() => state.path.split('/').slice(0,-1).join('/'))
const kind = (entry: Entry) => entry.kind === 'dir' ? '文件夹' : entry.kind === 'symlink' ? '符号链接' : entry.kind === 'special' ? '特殊文件' : '文件'
async function load(path = state.path, reset = true) {
  request?.abort()
  const own = request = new AbortController()
  if (reset) cursors.value = ['']
  loading.value = true; failure.value = ''; selected.value = null; content.value = null; previewRequest?.abort()
  try {
    const query = new URLSearchParams({ path, search: search.value, limit: '100', cursor: cursors.value.at(-1)! })
    const response = await api<typeof state>(`/sources/${props.source.id}/files?${query}`, { signal: own.signal })
    if (!stopped && request === own) Object.assign(state, response)
  } catch(e) { if (!stopped && request === own && !own.signal.aborted) failure.value = e instanceof Error ? e.message : '无法读取目录' }
  finally { if (request === own) loading.value = false }
}
function openDirectory(path: string) { search.value = ''; void load(path) }
function next() { cursors.value.push(state.next_cursor); void load(state.path, false) }
function previous() { cursors.value.pop(); void load(state.path, false) }
async function inspect(entry: Entry) {
  if (entry.kind === 'dir' && entry.can_open) { openDirectory(entry.path); return }
  previewRequest?.abort(); selected.value = entry; content.value = null; contentError.value = entry.reason ?? ''
  if (entry.kind !== 'file' || !entry.can_open) { contentLoading.value = false; await nextTick(); inspector.value?.scrollIntoView({ block: 'nearest' }); return }
  const own = previewRequest = new AbortController()
  contentLoading.value = true
  try {
    const response = await api<NonNullable<typeof content.value>>(`/sources/${props.source.id}/content?path=${encodeURIComponent(entry.path)}`, { signal: own.signal })
    if (!stopped && previewRequest === own) content.value = response
  } catch(e) { if (!stopped && previewRequest === own && !own.signal.aborted) contentError.value = e instanceof Error ? e.message : '无法读取文件' }
  finally { if (previewRequest === own && !own.signal.aborted && !stopped) { contentLoading.value = false; await nextTick(); inspector.value?.scrollIntoView({ block: 'nearest' }) } }
}
onMounted(() => load(''))
onUnmounted(() => { stopped = true; request?.abort(); previewRequest?.abort() })
</script>

<template>
  <div class="source-browser">
    <div class="source-browser-intro"><Folder :size="22"/><div><strong>{{source.name}}</strong><code>{{source.path}}</code></div><span class="badge neutral">只读浏览</span></div>
    <p class="browser-description">浏览服务器上当前存在的内容，包含隐藏文件。备份范围仍由源配置与计划规则决定。</p>
    <div class="browser-toolbar"><button aria-label="返回上一级" class="icon-button" title="返回上一级" :disabled="!state.path||loading" @click="openDirectory(parent)"><ArrowUp :size="18"/></button><nav aria-label="目录导航"><button title="源目录" @click="openDirectory('')"><Home :size="15"/>根目录</button><template v-for="crumb in crumbs" :key="crumb.path"><ChevronRight :size="12"/><button @click="openDirectory(crumb.path)">{{crumb.name}}</button></template></nav><button aria-label="刷新文件列表" class="icon-button" title="刷新文件列表" :disabled="loading" @click="load()"><RefreshCw :size="17" :class="{spin:loading}"/></button></div>
    <form class="browser-search" @submit.prevent="load()"><div class="search-input"><Search :size="16"/><input v-model="search" placeholder="搜索当前目录中的文件或文件夹" aria-label="搜索当前目录"/></div><button class="button secondary small" :disabled="loading">搜索</button></form>
    <div v-if="failure" class="alert danger">{{failure}}<button class="text-button" @click="load()">重试</button></div>
    <div class="source-file-list table-scroll" :aria-busy="loading">
      <div v-if="loading" class="browser-loading"><LoaderCircle class="spin" :size="20"/>正在读取目录…</div>
      <table><thead><tr><th>名称</th><th>大小</th><th>修改时间</th></tr></thead><tbody>
        <tr v-for="entry in state.items" :key="entry.path" :class="{selected:selected?.path===entry.path}"><td><button class="source-file-name" :disabled="loading" :title="entry.reason||entry.name" @click="inspect(entry)"><Folder v-if="entry.kind==='dir'" :size="20"/><Link v-else-if="entry.kind==='symlink'" :size="18"/><File v-else :size="18"/><span>{{entry.name}}<small v-if="!entry.can_open">{{entry.reason||kind(entry)}}</small></span><ChevronRight v-if="entry.kind==='dir'&&entry.can_open" :size="14"/></button></td><td>{{entry.kind==='dir'?'—':bytes(entry.size)}}</td><td>{{date(entry.modified_at)}}</td></tr>
        <tr v-if="!loading&&!failure&&!state.items.length"><td colspan="3" class="empty-cell">{{search?'当前目录没有匹配条目。':'此目录为空。'}}</td></tr>
      </tbody></table>
    </div>
    <div class="pagination"><span>共 {{state.total}} 项 · 第 {{cursors.length}} 页 · 文件夹优先</span><div><button aria-label="上一页" class="icon-button" title="上一页" :disabled="cursors.length===1||loading" @click="previous"><ChevronLeft :size="17"/></button><button aria-label="下一页" class="icon-button" title="下一页" :disabled="!state.next_cursor||loading" @click="next"><ChevronRight :size="17"/></button></div></div>
    <section v-if="selected" ref="inspector" class="file-inspector"><div class="inspector-head"><h3>{{selected.name}}</h3><button aria-label="收起文件预览" class="icon-button" title="收起文件预览" @click="selected=null;previewRequest?.abort()"><X :size="17"/></button></div><dl><div><dt>类型</dt><dd>{{kind(selected)}}</dd></div><div><dt>大小</dt><dd>{{bytes(selected.size)}}</dd></div><div><dt>权限</dt><dd><code>{{selected.mode||'—'}}</code></dd></div><div><dt>修改时间</dt><dd>{{date(selected.modified_at)}}</dd></div></dl><p v-if="selected.link_target" class="link-target">链接目标：<code>{{selected.link_target}}</code></p><p v-if="contentLoading" class="preview-note"><LoaderCircle class="spin" :size="15"/>正在读取预览…</p><p v-else-if="contentError" class="preview-note">{{contentError}}</p><template v-else-if="content"><pre v-if="content.kind==='text'" class="text-preview">{{content.text}}</pre><img v-else-if="content.kind==='image'" class="image-preview" :src="`data:${content.mime};base64,${content.data}`" :alt="selected.name"/><p v-if="content.truncated" class="preview-note">仅展示前 256 KiB 内容。</p></template></section>
    <p class="browser-description fine">文本预览最多 256 KiB；PNG、JPEG、GIF、WebP 图片最多 5 MiB。其他格式展示属性。</p>
  </div>
</template>

<style scoped>
.source-browser-intro{display:flex;align-items:center;gap:12px;background:#f5f8ef;border:1px solid #e5ecd9;border-radius:8px;padding:16px;color:#849e73}.source-browser-intro>div{min-width:0;flex:1}.source-browser-intro strong{font-size:13px;font-weight:500;display:block}.source-browser-intro code{display:block;font-size:10px;color:#95a98a;margin-top:7px;overflow-wrap:anywhere}.browser-description{font-size:11px;color:#91a184;line-height:1.9;margin:17px 0}.browser-description.fine{font-size:10px;margin-bottom:0}.browser-toolbar{display:flex;align-items:center;gap:8px;border:1px solid var(--line);background:#fafcf6;border-radius:6px;padding:8px}.browser-toolbar nav{display:flex;align-items:center;gap:6px;flex:1;flex-wrap:wrap;min-width:0;font-size:11px;color:#9dab8f}.browser-toolbar nav button{display:flex;align-items:center;gap:6px;color:#6d875e;overflow-wrap:anywhere}.browser-toolbar nav button:hover{text-decoration:underline}.browser-search{display:flex;gap:10px;margin:15px 0}.browser-search .search-input{flex:1;min-width:0}.source-file-list{position:relative;border:1px solid var(--line);border-radius:7px;max-height:380px;min-height:180px;overflow:auto}.source-file-list table{width:100%;font-size:11px}.source-file-list th{position:sticky;top:0;background:#f9fbf5;z-index:1}.source-file-list th,.source-file-list td{padding:12px 15px}.source-file-list th:first-child{width:55%}.source-file-list td{color:#91a183}.source-file-list tr.selected td{background:#f1f6e9}.source-file-name{display:flex;align-items:center;gap:10px;width:100%;text-align:left;color:#627c57}.source-file-name>span{flex:1;min-width:0;overflow-wrap:anywhere}.source-file-name>svg{flex-shrink:0;color:#9ab084}.source-file-name small{display:block;font-size:9px;color:#a6b29a;margin-top:5px}.source-file-name:hover>span{color:#385d35}.browser-loading{position:absolute;inset:0;z-index:2;background:#ffffffd9;display:flex;align-items:center;justify-content:center;gap:10px;font-size:11px;color:#8ea27b}.file-inspector{border:1px solid var(--line);border-radius:8px;padding:17px;margin-top:17px}.inspector-head{display:flex;align-items:center;justify-content:space-between;gap:15px}.inspector-head h3{font-size:13px;color:#628054;overflow-wrap:anywhere;font-weight:500}.file-inspector dl{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:12px;margin:14px 0;font-size:10px}.file-inspector dl>div{display:flex;justify-content:space-between;gap:12px}.file-inspector dt{color:#97a78b}.file-inspector dd{margin:0;color:#728964;overflow-wrap:anywhere}.file-inspector code{font-size:10px}.text-preview{white-space:pre-wrap;overflow-wrap:anywhere;max-height:320px;overflow:auto;border:1px solid #e6eddb;background:#f8faf4;padding:15px;border-radius:6px;font-size:11px;line-height:1.8;color:#5c7351;tab-size:4}.image-preview{display:block;max-width:100%;max-height:360px;object-fit:contain;margin:17px auto}.preview-note,.link-target{display:flex;gap:8px;font-size:11px;color:#97a18d;line-height:1.8;margin:12px 0}.link-target{overflow-wrap:anywhere}@media(max-width:520px){.source-file-list th,.source-file-list td{padding:11px 10px}.source-file-list th:nth-child(3),.source-file-list td:nth-child(3){display:none}.file-inspector dl{grid-template-columns:1fr}.source-browser-intro{padding:13px}.source-browser-intro .badge{font-size:8px}.browser-description{font-size:10px}}
</style>
