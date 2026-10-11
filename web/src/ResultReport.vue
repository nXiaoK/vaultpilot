<script setup lang="ts">
import { computed } from 'vue'
import { Check, Cloud, File, ShieldCheck } from 'lucide-vue-next'
import { bytes, date, statusLabel } from './api'
const props=defineProps<{result:any;accounts?:Record<string,any>[];sources?:Record<string,any>[]}>()
const accountName=(id:string)=>props.accounts?.find(a=>a.id===id)?.name??id
const sourceName=(id:string)=>props.sources?.find(source=>source.id===id)?.name??id
const restoreLayoutLabel=(layout:string)=>layout==='direct'?'直接恢复到目标目录':layout==='by_source'?'按备份源分目录':'服务未提供布局'
function outputPath(entry:{source_id:string;path:string}):string { const root=props.result.output_roots?.find((item:{source_id:string;path:string})=>item.source_id===entry.source_id)?.path;return root?`${root.replace(/\/$/,'')}${entry.path?`/${entry.path}`:''}`:'' }
const restore=computed(()=>props.result.restored!==undefined)
const summary=computed(()=>props.result.planning??props.result)
const restoreIssues=computed(()=>Number(props.result.skipped??0)+Number(props.result.failed??0)+Number(props.result.conflicts??0)+Number(props.result.unattempted??0)>0)
const restoreUnknown=computed(()=>props.result.verification_errors==null||props.result.verification_status==='unknown')
const failureEntries=computed(()=>props.result.failure_entries??props.result.failures??[])
const verificationLabel=computed(()=>restoreUnknown.value?'未知 · 引擎结果未能明确区分':props.result.verification_status==='failed'?'已记录校验异常':props.result.verification_status==='not_started'?'尚未开始条目校验':props.result.verification_status==='partial'?'仅完成部分已处理条目的校验':props.result.verification_status==='passed'?(restoreIssues.value?'已处理条目未记录校验异常；其余范围尚未完成':'已处理条目未记录校验异常'):'服务未提供校验结论')
const verificationCount=computed(()=>restoreUnknown.value?'未知':props.result.verification_status==='not_started'?'未检查':props.result.verification_errors??'未提供')
const failureStage=(value:string)=>{const labels:Record<string,string>={engine_restore_or_verify:'引擎恢复或校验',output_write:'写入目标',directory_metadata:'应用目录元数据'};return labels[value]??value??'—'}
const failureCode=(value:string)=>{const labels:Record<string,string>={RESTORE_ENGINE_FAILED:'引擎恢复或校验失败',RESTORE_WRITE_FAILED:'目标写入失败',RESTORE_CONFLICT:'恢复目标冲突',RESTORE_SIZE_MISMATCH:'恢复文件大小不符',RESTORE_LINK_MISMATCH:'恢复链接内容不符',RESTORE_TYPE_MISMATCH:'恢复条目类型不符',RESTORE_UNSAFE_LINK:'链接目标超出安全边界',RESTORE_METADATA_FAILED:'目录元数据应用失败'};return labels[value]??'恢复操作未完成'}
</script>
<template>
  <div class="result-report">
    <template v-if="summary.shards">
      <div class="detail-stats"><div><span>扫描条目</span><strong>{{summary.file_count??'—'}}</strong></div><div><span>源数据逻辑大小</span><strong>{{bytes(summary.logical_bytes)}}</strong></div><div><span>计划分片</span><strong>{{summary.shards.length}}</strong></div></div>
      <div v-for="warning in summary.warnings" :key="warning" class="alert warning"><ShieldCheck :size="16"/><div>{{warning}}</div></div>
      <div class="table-scroll"><table><thead><tr><th>目标账号 / 分片</th><th>文件数</th><th>逻辑大小</th><th>预计新增占用</th></tr></thead><tbody><tr v-for="shard in summary.shards" :key="shard.repository_id"><td><strong>{{accountName(shard.account_id)}}</strong><small class="cell-note">分片 {{shard.repository_id.slice(0,12)}}</small></td><td>{{shard.file_count}}</td><td>{{bytes(shard.logical_bytes)}}</td><td>{{bytes(shard.estimated_growth_bytes)}}</td></tr></tbody></table></div><p class="text-secondary fine">控制资料需要 {{summary.catalog_replicas}} 个不同 Drive 副本。容量估算不依赖乐观的去重或压缩率；云端实际增长以任务结束对账为准。</p>
    </template>
    <template v-else-if="restore">
      <div v-if="restoreIssues||restoreUnknown" class="alert warning"><ShieldCheck :size="18"/><div><strong>{{restoreUnknown?'校验结果尚无法明确判定':'所选恢复范围尚未完整完成'}}</strong><p>请核对已恢复、跳过、冲突、失败和未尝试条目。目标中可能已经存在本次恢复的输出。</p></div></div>
      <div class="restore-report"><div><span>已恢复</span><strong>{{result.restored??'—'}}</strong></div><div><span>已跳过</span><strong>{{result.skipped??'—'}}</strong></div><div><span>冲突</span><strong>{{result.conflicts??'—'}}</strong></div><div><span>失败</span><strong>{{result.failed??'—'}}</strong></div><div><span>未尝试</span><strong>{{result.unattempted??'—'}}</strong></div><div><span>校验异常</span><strong>{{verificationCount}}</strong></div></div>
      <div class="preview-summary"><p><strong>恢复目标</strong><code>{{result.target}}</code></p><p v-if="result.layout"><strong>实际布局</strong>{{restoreLayoutLabel(result.layout)}}</p><p><strong>恢复点</strong><code>{{result.point_id}}</code></p><p><strong>校验方式</strong>{{result.verification||'服务未提供校验方式'}}</p><p><strong>校验结论</strong>{{verificationLabel}}</p></div>
      <div v-if="result.output_roots?.length" class="restore-output-roots"><h3>实际输出目录</h3><p v-for="root in result.output_roots" :key="root.source_id"><span>{{sourceName(root.source_id)}}</span><code>{{root.path}}</code></p></div>
      <div v-if="failureEntries.length" class="restore-failures"><div class="logs-head"><h3>失败与冲突明细</h3><span class="text-secondary">显示 {{failureEntries.length}} / {{result.failure_entry_count??failureEntries.length}} 条</span></div><div class="table-scroll"><table><thead><tr><th>源 / 历史路径与目标</th><th>失败阶段</th><th>原因</th></tr></thead><tbody><tr v-for="(entry,index) in failureEntries" :key="index"><td><small class="cell-note">{{sourceName(entry.source_id)||'—'}}</small><span class="filename">{{entry.path||'源根目录'}}</span><code v-if="outputPath(entry)" class="cell-note restore-output-path">{{outputPath(entry)}}</code></td><td>{{failureStage(entry.stage)}}</td><td><strong>{{entry.error||failureCode(entry.code)}}</strong><small class="cell-note">{{entry.code}}</small></td></tr></tbody></table></div><p v-if="result.failure_entries_truncated||result.truncated||Number(result.failure_entry_count??0)>failureEntries.length" class="text-secondary fine">明细数量达到响应上限。全部已知异常数量以上方统计为准，请结合任务日志排查。</p></div>
      <p class="text-secondary fine">请核对恢复报告与实际文件内容。跳过、未尝试或校验未知的范围不属于完整恢复；当前不支持从失败任务逐文件自动续跑。</p>
    </template>
    <template v-else-if="result.checks"><div class="alert success"><ShieldCheck :size="17"/><div><strong>{{result.mode==='full'?'已执行全量读取检查':result.mode==='sample'?'已执行数据抽查':'已执行仓库结构检查'}}</strong><p>{{result.reads_all_data?'本次检查读取并校验全部数据。':'本次结果不代表全部实际数据文件已被读取验证。'}}</p></div></div><div class="table-scroll"><table><thead><tr><th>仓库</th><th>结果</th><th>检查时间</th></tr></thead><tbody><tr v-for="check in result.checks" :key="check.repository_id"><td><code>{{check.repository_id}}</code></td><td><span class="badge success">{{statusLabel(check.status)}}</span></td><td>{{date(check.checked_at)}}</td></tr></tbody></table></div></template>
    <template v-else-if="result.accounts&&result.retired_repository_ids">
      <div v-if="result.cleanup_pending" class="alert warning"><ShieldCheck :size="18"/><div><strong>批量清空尚未完成</strong><p>已删除的数据无法撤销。请查看各账号结果，选择同一组账号重新预览并执行；其他任务会在清理完成后恢复可用。</p></div></div>
      <div class="detail-stats"><div><span>失效恢复点</span><strong>{{result.deleted_point_ids.length}}</strong></div><div><span>退役仓库</span><strong>{{result.retired_repository_ids.length}}</strong></div><div><span>暂停计划</span><strong>{{result.paused_plan_ids.length}}</strong></div></div>
      <div v-if="!result.accounts.length" class="alert info">正在同步删除记录，尚未开始清空网盘数据。</div>
      <div v-else class="table-scroll"><table><thead><tr><th>存储账号</th><th>清空结果</th><th>说明</th></tr></thead><tbody><tr v-for="item in result.accounts" :key="item.account_id"><td>{{item.name||accountName(item.account_id)}}</td><td><span class="badge" :class="item.cleared?'success':'warning'">{{item.cleared?'已清空':'未完成'}}</span></td><td>{{item.error||item.warning||'当前库备份已清空，保留授权与最新删除记录。'}}</td></tr></tbody></table></div>
      <p class="fine text-secondary">清空后定时计划保持暂停，请核对存储池与容量后手动启用。微软回收站或配额统计延迟可能影响可用容量显示。</p>
    </template>
    <template v-else-if="result.deleted_point_ids"><div v-if="result.cleanup_pending" class="alert warning"><ShieldCheck :size="18"/><div><strong>恢复点删除决策已提交，仓库数据回收仍待执行。</strong><p>请查看任务错误与维护报告，尚未完成的清理不会显示为已经回收。</p></div></div><div class="detail-stats"><div><span>已删除恢复点</span><strong>{{result.deleted_point_ids.length}}</strong></div><div v-if="result.pruned_repositories!=null"><span>已清理仓库</span><strong>{{result.pruned_repositories}}</strong></div></div></template>
    <template v-else-if="result.cleanup_old"><div class="alert" :class="result.old_copy_deleted?'success':'warning'"><Check v-if="result.old_copy_deleted" :size="18"/><ShieldCheck v-else :size="18"/><div><strong>{{result.old_copy_deleted?'选定旧仓库副本已删除':'旧副本删除尚未确认完成'}}</strong><p>{{result.new_location_verified?'当前有效位置已通过完整读取检查。':'请查看任务阶段与日志，确认当前位置校验结果。'}}</p></div></div><div class="preview-summary"><p><strong>仓库</strong><code>{{result.repository_id}}</code></p><p><strong>旧账号</strong>{{accountName(result.old_location?.account_id)}}</p><p><strong>旧位置</strong><code>{{result.old_location?.path}}</code></p></div></template>
    <template v-else-if="result.new_account_id"><div class="alert success"><Check :size="18"/><div>仓库位置映射已更新；{{result.old_copy_preserved?'旧副本保留用于观察。':'请检查任务报告中的位置状态。'}}</div></div><div class="preview-summary"><p><strong>新账号</strong>{{accountName(result.new_account_id)}}</p><p><strong>仓库</strong><code>{{result.repository_id}}</code></p><p><strong>旧位置</strong><code>{{result.old_location}}</code></p></div></template>
    <template v-else><pre>{{JSON.stringify(result,null,2)}}</pre></template>
  </div>
</template>
