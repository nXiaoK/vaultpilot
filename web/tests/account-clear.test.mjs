import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { computed, effectScope, nextTick, ref, watch } from 'vue'
import { parse } from '@vue/compiler-sfc'
import { compile } from '@vue/compiler-dom'
import { renderToString } from '@vue/server-renderer'
const descriptor=parse(readFileSync(new URL('../src/AccountClear.vue',import.meta.url),'utf8')).descriptor
const source=descriptor.scriptSetup.content
const {code}=compile(descriptor.template.content,{mode:'function',prefixIdentifiers:true})
const render=vm.runInNewContext(`(function(){${code}})()`,{Vue})
const ast=ts.createSourceFile('clear.ts',source,ts.ScriptTarget.ES2022,true)
const script=ast.statements.filter(node=>!ts.isImportDeclaration(node)).map(node=>node.getText(ast)).join('\n')
const {outputText}=ts.transpileModule(script,{compilerOptions:{target:ts.ScriptTarget.ES2022}})
function setup(t,overrides={}){
 const events=[],calls=[],pending=[]
 const scope=effectScope();t.after(()=>scope.stop())
 const props={accounts:[{id:'one',backend:'onedrive',verified:true,drive_id:'drive-one'},{id:'two',backend:'gdrive',verified:true,drive_id:'drive-two'},{id:'unverified',verified:false}],initialIds:['one'],...overrides}
 const context=vm.createContext({ref,watch,computed,defineProps:()=>props,defineEmits:()=>((...event)=>events.push(event)),api:(path,options)=>{calls.push({path,options});return new Promise((resolve,reject)=>pending.push({resolve,reject}))}})
 scope.run(()=>vm.runInContext(outputText+'\nglobalThis.state={selected,preview,working,confirmation,agreed,canSubmit,available,permanentDelete,failure,inspect,submit,selectAll}',context))
 async function html(){
  const app=Vue.createSSRApp({setup:()=>({...props,...context.state,bytes:value=>String(value??0)}),render})
  for(const name of ['Cloud','HardDrive','LoaderCircle','Search','ShieldCheck','Trash2'])app.component(name,{render:()=>null})
  return renderToString(app)
 }
 return {c:context.state,events,calls,pending,html}
}
const preview={blocked:false,preview_token:'current-preview',point_ids:['cross'],pause_plan_ids:['plan-one'],protected_point_ids:[]}
test('requires explicit preview, acknowledgement and exact confirmation before submitting a task',async t=>{
 const {c,calls,pending,events}=setup(t)
 assert.equal(c.available.value.length,2)
 await c.submit();assert.equal(calls.length,0)
 const inspecting=c.inspect();assert.equal(c.working.value,true)
 assert.equal(events.at(-1)[0],'busy');assert.equal(events.at(-1)[1],true)
 assert.equal(calls[0].path,'/accounts/clear/preview')
 pending[0].resolve(preview);await inspecting
 c.agreed.value=true;c.confirmation.value='wrong';await c.submit();assert.equal(calls.length,1)
 c.confirmation.value='清空所选备份';assert.equal(c.canSubmit.value,true)
 const submitting=c.submit();assert.equal(calls[1].path,'/accounts/clear');assert.equal(calls[1].options.task,true)
 assert.equal(calls[1].options.body.preview_token,'current-preview')
 assert.deepEqual(Array.from(calls[1].options.body.account_ids),['one'])
 pending[1].resolve({id:'clear-task',kind:'account-clear'});await submitting
 assert.equal(events.at(-1)[0],'submitted')
 assert.equal(events.at(-2)[0],'busy');assert.equal(events.at(-2)[1],false)
})
test('changing account selection invalidates the approved preview and resets confirmation',async t=>{
 const {c,pending}=setup(t);const inspecting=c.inspect();pending[0].resolve(preview);await inspecting
 c.agreed.value=true;c.confirmation.value='清空所选备份'
 c.selectAll();await nextTick()
 assert.equal(c.preview.value,null);assert.equal(c.agreed.value,false);assert.equal(c.confirmation.value,'')
 assert.deepEqual(Array.from(c.selected.value),['one','two'])
 assert.equal(c.canSubmit.value,false)
})
test('protected or active-task preview cannot submit and request failure releases loading state',async t=>{
 const {c,calls,pending}=setup(t);const inspecting=c.inspect();pending[0].resolve({...preview,blocked:true});await inspecting
 c.agreed.value=true;c.confirmation.value='清空所选备份';await c.submit();assert.equal(calls.length,1)
 const retry=c.inspect();pending[1].reject(new Error('offline'));await retry
 assert.equal(c.working.value,false);assert.equal(c.preview.value,null)
})

for(const source of ['selected-account','preview-flag']){
 test(`permanent Google deletion warning renders above confirmation from ${source}`,async t=>{
  const {c,html}=setup(t,{initialIds:[source==='selected-account'?'two':'one']})
  c.preview.value={...preview,items:[{name:'Drive',permanent_delete:source==='preview-flag'}],path:'VaultPilot/vault',catalog_account_count:2,repository_ids:[]}
  const markup=await html()
  const warning='Google Drive 中的备份将被永久删除，不进入回收站，无法找回'
  assert.equal(c.permanentDelete.value,true)
  assert.ok(markup.includes(warning))
  assert.ok(markup.indexOf(warning)<markup.indexOf('输入“清空所选备份”确认'))
 })
}
test('OneDrive preview does not display the Google permanent deletion warning',async t=>{
 const {c,html}=setup(t)
 c.preview.value={...preview,items:[{name:'OneDrive',permanent_delete:false}],path:'VaultPilot/vault',catalog_account_count:2,repository_ids:[]}
 assert.equal(c.permanentDelete.value,false)
 assert.doesNotMatch(await html(),/Google Drive 中的备份将被永久删除/)
})
