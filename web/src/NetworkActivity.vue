<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { ArrowUpRight, ArrowDownToLine, Activity } from 'lucide-vue-next'
import { api, bytes } from './api'

type Sample = { at: string; upload_bytes_per_second: number; download_bytes_per_second: number }
type Metrics = { available: boolean; supported: boolean; server_time: string; history: Sample[] }
const metrics = ref<Metrics | null>(null)
const disconnected = ref(false)
const selected = ref<Sample | null>(null)
let timer: ReturnType<typeof setTimeout> | undefined
let stopped = false
let request: AbortController | undefined
const current = computed(() => metrics.value?.history.at(-1))
const available = computed(() => !disconnected.value && metrics.value?.available)
const scale = computed(() => Math.max(1024, ...(metrics.value?.history ?? []).flatMap(s => [s.upload_bytes_per_second, s.download_bytes_per_second])))
const end = computed(() => Date.parse(metrics.value?.server_time ?? '') || Date.now())
const x = (sample: Sample) => Math.max(0, Math.min(900, 900 * (Date.parse(sample.at) - end.value + 60000) / 60000))
const y = (value: number) => 164 - 144 * value / scale.value
const rate = (value: number | undefined) => value === undefined ? '—' : `${bytes(Math.round(value))}/s`
function line(direction: 'upload_bytes_per_second' | 'download_bytes_per_second') {
  let previous = 0
  return (metrics.value?.history ?? []).map(s => {
    const at = Date.parse(s.at)
    const command = !previous || at - previous > 2500 ? 'M' : 'L'
    previous = at
    return `${command}${x(s).toFixed(1)},${y(s[direction]).toFixed(1)}`
  }).join(' ')
}
function inspect(event: PointerEvent) {
  const rect = (event.currentTarget as SVGElement).getBoundingClientRect()
  const at = end.value - 60000 + (event.clientX - rect.left) / rect.width * 60000
  selected.value = (metrics.value?.history ?? []).reduce<Sample | null>((best, sample) => !best || Math.abs(Date.parse(sample.at)-at) < Math.abs(Date.parse(best.at)-at) ? sample : best, null)
}
async function refresh() {
  request = new AbortController()
  try { const result = await api<Metrics>('/metrics/network', { signal: request.signal, silent: true }); if (!stopped) { metrics.value = result; disconnected.value = false; selected.value = null } }
  catch { if (!stopped) disconnected.value = true }
  finally { if (!stopped) timer = setTimeout(refresh, 2000) }
}
onMounted(refresh)
onUnmounted(() => { stopped = true; if (timer) clearTimeout(timer); request?.abort() })
</script>

<template>
  <section class="panel network-panel" aria-label="实时网络速度">
    <div class="panel-head"><h2><Activity :size="18"/>实时传输速度<span class="label-sub">最近 1 分钟</span></h2><span class="tiny-label">{{disconnected?'连接中断':available?'每秒采样 · 实时更新':metrics?.supported?'正在采样':'等待网络采样'}}</span></div>
    <div class="network-rates">
      <div><span class="upload-label"><ArrowUpRight :size="17"/>上传 · 出站</span><strong>{{rate(available ? current?.upload_bytes_per_second : undefined)}}</strong></div>
      <div><span class="download-label"><ArrowDownToLine :size="17"/>下载 · 入站</span><strong>{{rate(available ? current?.download_bytes_per_second : undefined)}}</strong></div>
      <p>节点网络流量<br><small>包含备份、恢复及其他网络请求</small></p>
    </div>
    <div class="network-chart">
      <div class="chart-axis"><span>{{rate(scale)}}</span><span>{{rate(scale/2)}}</span><span>0</span></div>
      <div class="chart-plot">
        <svg viewBox="0 0 900 180" preserveAspectRatio="none" role="img" aria-label="最近60秒上传与下载速率曲线" @pointermove="inspect" @pointerleave="selected=null">
          <path d="M0,20H900 M0,92H900 M0,164H900" class="chart-grid"/>
          <path :d="line('upload_bytes_per_second')" class="upload-line"/>
          <path :d="line('download_bytes_per_second')" class="download-line"/>
          <template v-if="selected"><path :d="`M${x(selected)},20V164`" class="chart-guide"/><circle :cx="x(selected)" :cy="y(selected.upload_bytes_per_second)" r="3" class="upload-point"/><circle :cx="x(selected)" :cy="y(selected.download_bytes_per_second)" r="3" class="download-point"/></template>
        </svg>
        <div v-if="!metrics?.history.length" class="chart-empty">{{disconnected?'暂时无法获取速率数据':metrics&&!metrics.supported?'当前系统无法读取网络计数器，Docker / Linux 部署支持此功能。':'正在收集真实速率数据…'}}</div>
        <div class="chart-times"><span>60 秒前</span><span>30 秒前</span><span>现在</span></div>
      </div>
    </div>
    <div class="network-caption"><span v-if="selected">{{new Date(selected.at).toLocaleTimeString('zh-CN')}} · 上传 {{rate(selected.upload_bytes_per_second)}} · 下载 {{rate(selected.download_bytes_per_second)}}</span><span v-else>将指针移到曲线上查看速率 · 不包含回环接口流量</span></div>
  </section>
</template>

<style scoped>
.network-panel{margin-bottom:26px}.panel-head h2{display:flex;align-items:center;gap:9px}.network-rates{display:flex;align-items:center;gap:55px;padding:24px 26px 10px}.network-rates>div span{display:flex;align-items:center;gap:7px;font-size:11px}.upload-label{color:#62885b}.download-label{color:#668db1}.network-rates strong{display:block;font-size:26px;font-weight:500;letter-spacing:-.5px;margin:11px 0;color:#3e5540;font-variant-numeric:tabular-nums}.network-rates p{margin-left:auto;font-size:11px;color:#879c7b;line-height:1.9}.network-rates small{font-size:10px;color:#a2ae98}.network-chart{display:flex;gap:14px;padding:10px 26px 0}.chart-axis{display:flex;flex-direction:column;justify-content:space-between;width:86px;flex-shrink:0;font-size:9px;color:#98a78e;padding:14px 0 35px;text-align:right}.chart-plot{position:relative;flex:1;min-width:0}.chart-plot svg{display:block;width:100%;height:170px;overflow:visible}.chart-grid{stroke:#eaf0e2;fill:none;stroke-dasharray:4 5;vector-effect:non-scaling-stroke}.upload-line,.download-line{stroke-width:2;fill:none;stroke-linejoin:round;vector-effect:non-scaling-stroke}.upload-line{stroke:#80a86f}.download-line{stroke:#81a5c8}.upload-point{fill:#80a86f}.download-point{fill:#81a5c8}.chart-guide{stroke:#bac7b1;stroke-dasharray:4 4;vector-effect:non-scaling-stroke}.chart-times{display:flex;justify-content:space-between;font-size:9px;color:#9eac93;margin-top:5px}.chart-empty{position:absolute;inset:20px 0 30px;display:grid;place-items:center;font-size:11px;color:#8d9e81;padding:20px;text-align:center}.network-caption{padding:16px 26px 22px;font-size:10px;color:#93a586;min-height:49px}@media(max-width:600px){.network-rates{gap:25px;padding:20px 18px 0;flex-wrap:wrap}.network-rates strong{font-size:21px}.network-rates p{margin:0;width:100%}.network-rates p br{display:none}.network-chart{padding:10px 18px 0;gap:9px}.chart-axis{width:68px;font-size:8px}.network-caption{padding:16px 18px 20px;font-size:9px}}
</style>
