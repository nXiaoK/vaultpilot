import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import { computed, reactive, ref } from 'vue'
const source=readFileSync(new URL('../src/TelegramNotifications.vue',import.meta.url),'utf8').split('<script setup lang="ts">')[1].split('</script>')[0]
const ast=ts.createSourceFile('telegram.ts',source,ts.ScriptTarget.ES2022,true)
const script=ast.statements.filter(node=>!ts.isImportDeclaration(node)).map(node=>node.getText(ast)).join('\n')
const {outputText}=ts.transpileModule(script,{compilerOptions:{target:ts.ScriptTarget.ES2022}})
function setup(){
 const calls=[],pending=[],events=[];let mount,poll
 const context=vm.createContext({Error,ref,reactive,computed,onMounted:fn=>{mount=fn},onUnmounted:()=>{},setInterval:fn=>{poll=fn;return 1},clearInterval:()=>{},defineEmits:()=>((...event)=>events.push(event)),api:(path,options)=>{calls.push({path,options});return new Promise((resolve,reject)=>pending.push({resolve,reject}))}})
 vm.runInContext(outputText+'\nglobalThis.state={settings,info,working,ready,failure,notice,canSend,changedElsewhere,load,save,test,retry,check}',context)
 return {c:context.state,calls,pending,events,mount:()=>mount(),poll:()=>poll()}
}
const saved={version:2,enabled:true,node_name:'A',chat_id:'123456',token_configured:true,backup_success:true,backup_failure:true,account_error:true,recent:[]}
test('loads masked configuration and clears token input after successful save',async()=>{
 const {c,calls,pending,events}=setup()
 const loading=c.load();assert.equal(c.working.value,true);assert.equal(c.canSend.value,false)
 pending[0].resolve(saved);await loading
 assert.equal(c.settings.bot_token,'');assert.equal(c.settings.version,2);assert.equal(c.canSend.value,true)
 c.settings.node_name='B';c.settings.bot_token='new-secret'
 const saving=c.save();assert.equal(calls[1].options.method,'PUT');assert.equal(calls[1].options.body.version,2)
 assert.equal(calls[1].options.body.bot_token,'new-secret')
 await c.save();assert.equal(calls.length,2)
 pending[1].resolve({...saved,version:3,node_name:'B'});await saving
 assert.equal(c.settings.bot_token,'');assert.equal(c.settings.version,3);assert.equal(c.working.value,false)
 assert.equal(events.at(-1)[1],false)
})
test('test message waits for saved settings and shows queued state rather than claiming delivery',async()=>{
 const {c,calls,pending}=setup()
 await c.test();assert.equal(calls.length,0)
 const loading=c.load();pending[0].resolve(saved);await loading
 const sending=c.test();assert.equal(calls[1].path,'/notifications/telegram/test');assert.equal(c.working.value,true)
 pending[1].resolve({status:'queued'});await new Promise(resolve=>setImmediate(resolve))
 pending[2].resolve({...saved,recent:[{status:'queued',kind:'test'}]});await sending
 assert.match(c.notice.value,/已排队/);assert.equal(c.info.value.recent[0].status,'queued')
 const saving=c.save();pending[3].reject(new Error('version conflict'));await saving
 assert.equal(c.working.value,false);assert.equal(c.failure.value,'version conflict')
})
test('status polling preserves unsaved fields and identifies another session changing configuration',async()=>{
 const state=setup();state.mount();state.pending[0].resolve(saved);await new Promise(resolve=>setImmediate(resolve))
 state.c.settings.node_name='Unsaved label';state.c.settings.bot_token='unsaved-token'
 const polling=state.poll();assert.equal(state.calls[1].options.silent,true)
 state.pending[1].resolve({...saved,version:3,node_name:'Other session'});await polling
 assert.equal(state.c.settings.node_name,'Unsaved label');assert.equal(state.c.settings.bot_token,'unsaved-token')
 assert.equal(state.c.settings.version,2);assert.equal(state.c.changedElsewhere.value,true)
})
