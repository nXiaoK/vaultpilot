import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
const source=readFileSync(new URL('../src/App.vue',import.meta.url),'utf8').split('<script setup lang="ts">')[1].split('</script>')[0]
const ast=ts.createSourceFile('maintenance.ts',source,ts.ScriptTarget.ES2022,true)
const computed=ast.statements.find(node=>ts.isVariableStatement(node)&&node.declarationList.declarations.some(declaration=>declaration.name.getText(ast)==='migrationTargetAccounts')).getText(ast)
const {outputText}=ts.transpileModule(computed+'\nglobalThis.targets=migrationTargetAccounts',{compilerOptions:{target:ts.ScriptTarget.ES2022}})
test('migration targets belong to the selected repository plan pool and remain enabled and verified',()=>{
  const context=vm.createContext({computed:fn=>({get value(){return fn()}}),maintenanceForm:{repository_id:'repo'},data:{repos:[{id:'repo',plan_id:'plan',account_id:'a'}]},catalog:{plans:[{id:'plan',pool_id:'pool'}],pools:[{id:'pool',account_ids:['a','b','disabled','unverified']}],accounts:[{id:'a',verified:true,enabled:true},{id:'b',verified:true,enabled:true},{id:'outside',verified:true,enabled:true},{id:'disabled',verified:true,enabled:false},{id:'unverified',verified:false,enabled:true}]}})
  vm.runInContext(outputText,context)
  assert.deepEqual(Array.from(context.targets.value,a=>a.id),['b'])
  context.catalog.pools=[]
  assert.equal(context.targets.value.length,0)
  context.maintenanceForm.repository_id='missing'
  assert.equal(context.targets.value.length,0)
})

test('retry cleanup asks for confirmation and submits only an empty selection for the chosen plan',async()=>{
  const declaration=ast.statements.find(node=>ts.isFunctionDeclaration(node)&&node.name.text==='retryRetentionCleanup').getText(ast)
  const {outputText}=ts.transpileModule(declaration,{compilerOptions:{target:ts.ScriptTarget.ES2022}})
  const calls=[],opened=[]
  let confirm
  const context=vm.createContext({maintenanceForm:{plan_id:'plan'},retentionSelection:{value:['unrelated-point']},askConfirm:(title,text,fn)=>{confirm=fn},api:async(path,options)=>{calls.push({path,options});return{id:'retry-task'}},openTask:async task=>opened.push(task)})
  vm.runInContext(outputText,context)
  context.retryRetentionCleanup()
  assert.equal(calls.length,0)
  context.maintenanceForm.plan_id='different-plan'
  await confirm()
  assert.equal(calls[0].path,'/maintenance/retention')
  assert.deepEqual(JSON.parse(JSON.stringify(calls[0].options.body)),{plan_id:'plan',point_ids:[],confirm:true})
  assert.equal(calls[0].options.task,true)
  assert.equal(opened[0].id,'retry-task')
})
