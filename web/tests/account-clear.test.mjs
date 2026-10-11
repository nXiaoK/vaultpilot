import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import { computed, effectScope, nextTick, ref, watch } from 'vue'
const source=readFileSync(new URL('../src/AccountClear.vue',import.meta.url),'utf8').split('<script setup lang="ts">')[1].split('</script>')[0]
const ast=ts.createSourceFile('clear.ts',source,ts.ScriptTarget.ES2022,true)
const script=ast.statements.filter(node=>!ts.isImportDeclaration(node)).map(node=>node.getText(ast)).join('\n')
const {outputText}=ts.transpileModule(script,{compilerOptions:{target:ts.ScriptTarget.ES2022}})
function setup(t){
 const events=[],calls=[],pending=[]
 const scope=effectScope();t.after(()=>scope.stop())
 const context=vm.createContext({ref,watch,computed,defineProps:()=>({accounts:[{id:'one',verified:true,drive_id:'drive-one'},{id:'two',verified:true,drive_id:'drive-two'},{id:'unverified',verified:false}],initialIds:['one']}),defineEmits:()=>((...event)=>events.push(event)),api:(path,options)=>{calls.push({path,options});return new Promise((resolve,reject)=>pending.push({resolve,reject}))}})
 scope.run(()=>vm.runInContext(outputText+'\nglobalThis.state={selected,preview,working,confirmation,agreed,canSubmit,available,inspect,submit,selectAll}',context))
 return {c:context.state,events,calls,pending}
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
