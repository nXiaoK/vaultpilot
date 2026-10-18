import assert from 'node:assert/strict'
import {readFileSync} from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
const source=readFileSync(new URL('../src/App.vue',import.meta.url),'utf8').split('<script setup lang="ts">')[1].split('</script>')[0]
const ast=ts.createSourceFile('polling.ts',source,ts.ScriptTarget.ES2022,true)
const declarations=ast.statements.filter(node=>ts.isFunctionDeclaration(node)&&['readTask','loadLogs','refreshPolling'].includes(node.name.text)).map(node=>node.getText(ast)).join('\n')
const {outputText}=ts.transpileModule(declarations,{compilerOptions:{target:ts.ScriptTarget.ES2022}})
function setup(){
  const shown=[]
  const context=vm.createContext({activeTask:{value:{id:'task'}},detail:{value:null},logs:{offset:0},backgroundFailures:{task:0,logs:0,status:0},authenticated:{value:true},busy:{value:false},modal:{value:''},connectionProblem:{value:false},api:async()=>{throw Error('temporary')},showError:error=>shown.push(error),refreshStatus:async()=>{},refresh:async()=>{}})
  vm.runInContext(outputText,context)
  return{context,shown}
}
for(const method of ['readTask','loadLogs'])test(`${method} suppresses the first two background failures, resets on success and reports manual failures immediately`,async()=>{
  const {context:c,shown}=setup()
  const call=silent=>method==='readTask'?c.readTask(silent):c.loadLogs(0,silent)
  await call(true);await call(true);assert.equal(shown.length,0)
  await call(true);assert.equal(shown.length,1)
  c.api=async()=>({id:'task',items:[],total:0})
  await call(true)
  c.api=async()=>{throw Error('new-failure')}
  await call(true);await call(true);assert.equal(shown.length,1)
  await call(false);assert.equal(shown.length,2)
})
test('task and log polling failures are counted independently',async()=>{
  const {context:c,shown}=setup()
  await c.readTask(true);await c.loadLogs(0,true);await c.readTask(true);await c.loadLogs(0,true)
  assert.equal(shown.length,0)
  await c.readTask(true);assert.equal(shown.length,1)
})
test('status polling shows connection trouble only after three failures and clears the streak on success',async()=>{
  const {context:c}=setup();c.refreshStatus=async()=>{throw Error('offline')}
  await c.refreshPolling();await c.refreshPolling();assert.equal(c.connectionProblem.value,false)
  await c.refreshPolling();assert.equal(c.connectionProblem.value,true)
  c.refreshStatus=async()=>{c.connectionProblem.value=false}
  await c.refreshPolling();assert.equal(c.connectionProblem.value,false)
  c.refreshStatus=async()=>{throw Error('offline')}
  await c.refreshPolling();assert.equal(c.connectionProblem.value,false)
})
