import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
const { outputText } = ts.transpileModule(readFileSync(new URL('../src/api.ts', import.meta.url), 'utf8'), {compilerOptions: {target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.CommonJS}})
function setup() {
  const pending=[]
  const context=vm.createContext({exports:{},fetch:()=>new Promise((resolve,reject)=>pending.push({resolve,reject}))})
  vm.runInContext(outputText,context)
  return {...context.exports,pending}
}
const success = {status:200,ok:true,json:async()=>({items:[]})}
test('foreground activity starts immediately and remains visible until all overlapping responses are decoded', async()=>{
  const c=setup();const states=[]
  const stop=c.subscribeActivity(items=>states.push(items))
  const first=c.api('/accounts/test/drives')
  const second=c.api('/accounts')
  assert.equal(states.at(-1).length,2)
  assert.match(states.at(-1)[0].label,/Drive/)
  c.pending[1].resolve(success);await second
  assert.equal(states.at(-1).length,1)
  assert.match(states.at(-1)[0].label,/Drive/)
  let finishJSON
  c.pending[0].resolve({...success,json:()=>new Promise(resolve=>{finishJSON=resolve})})
  await new Promise(resolve=>setImmediate(resolve))
  assert.equal(states.at(-1).length,1)
  finishJSON({items:[]});await first
  assert.equal(states.at(-1).length,0)
  stop();const count=states.length
  const third=c.api('/accounts');c.pending[2].resolve(success);await third
  assert.equal(states.length,count)
})
test('background polling never flashes a foreground loading notice',async()=>{
 const c=setup();const states=[];c.subscribeActivity(items=>states.push(items))
 const quiet=c.api('/accounts',{silent:true});const visible=c.api('/accounts/test/drives')
 assert.equal(states.at(-1).length,1)
 c.pending[0].resolve(success);await quiet
 assert.equal(states.at(-1).length,1)
 c.pending[1].resolve(success);await visible
 assert.equal(states.at(-1).length,0)
})
for (const failure of ['network','http','json','abort']) {
 test(`loading state is cleared after ${failure} failure and retry works`,async()=>{
 const c=setup();const states=[];c.subscribeActivity(items=>states.push(items))
 const failed=c.api('/accounts/test/drives')
 const rejected=assert.rejects(failed)
 if(failure==='network'||failure==='abort')c.pending[0].reject(new Error(failure))
 if(failure==='http')c.pending[0].resolve({status:502,ok:false,json:async()=>({error:'unavailable',code:'cloud_access_failed'})})
 if(failure==='json')c.pending[0].resolve({...success,json:async()=>{throw new Error('bad response')}})
 await rejected
 assert.equal(states.at(-1).length,0)
 const retry=c.api('/accounts/test/drives');assert.equal(states.at(-1).length,1)
 c.pending[1].resolve(success);await retry;assert.equal(states.at(-1).length,0)
 })
}
test('loading labels never include URLs, tokens, file paths or query values',async()=>{
 const c=setup();const states=[];c.subscribeActivity(items=>states.push(items))
 const result=c.api('/sources/private/content?path=private-file&token=secret-token')
 assert.match(states.at(-1)[0].label,/预览/)
 assert.doesNotMatch(states.at(-1)[0].label,/private|secret|token|sources/)
 c.pending[0].resolve(success);await result
})
