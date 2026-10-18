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
