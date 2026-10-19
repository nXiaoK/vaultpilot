import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'

// Execute the actual wizard actions with a deferred API, independently of Vue
// rendering, to verify request counts and the response version used for verify.
const source = readFileSync(new URL('../src/App.vue', import.meta.url), 'utf8').split('<script setup lang="ts">')[1].split('</script>')[0]
const ast = ts.createSourceFile('wizard.ts', source, ts.ScriptTarget.ES2022, true)
const names = ['openAccount', 'createAccount', 'updateAccount', 'onRcloneAuthorized', 'loadDrives', 'verifyAccount', 'editAccountSettings', 'saveAccountSettings', 'saveForm']
const functions = ast.statements.filter(node => ts.isFunctionDeclaration(node) && names.includes(node.name?.text)).map(node => node.getText(ast)).join('\n')
const computedNames = ['accountBackend', 'accountProvider', 'accountSteps', 'poolStorageWarning']
const derived = ast.statements.filter(node => ts.isVariableStatement(node) && node.declarationList.declarations.some(declaration => computedNames.includes(declaration.name.getText(ast)))).map(node => node.getText(ast)).join('\n')
const { outputText } = ts.transpileModule(functions + '\n' + derived + '\nglobalThis.derived={accountSteps,poolStorageWarning}', {compilerOptions:{target:ts.ScriptTarget.ES2022}})
function setup() {
  const calls = [], pending = [], loaded = []
  const context = vm.createContext({
    activeAccount:{value:{id:'account',version:1}}, accountStep:{value:2},
    form:{},modal:{value:''},error:{value:null},computed:fn=>({get value(){return fn()}}),loadList:async name=>loaded.push(name),
    data:{accounts:[{id:'account',version:1}]}, catalog:{accounts:[{id:'account',version:1},{id:'other',version:4}]},
    drives:{value:[]}, chosenDrive:{value:'drive'}, accountBudget:{budget:800,reserve:50,maintenance:50}, recoveryMode:{value:false},
    action:fn=>fn(), toast:()=>{},
    api:(path,options)=>{calls.push({path,options});return new Promise((resolve,reject)=>pending.push({resolve,reject}))},
  })
  vm.runInContext(outputText,context)
  return {context,calls,pending,loaded}
}
test('authorization reads Drive once and advances using returned account without reloading lists',async()=>{
  const {context:c,calls,pending}=setup()
  const operation=c.onRcloneAuthorized({id:'account',version:2})
  assert.equal(c.activeAccount.value.version,2)
  assert.equal(c.accountStep.value,2)
  assert.equal(calls.length,1)
  const account={id:'account',version:3,account_email:'member@outlook.com'}
  pending[0].resolve({items:[{id:'drive'}],account});await operation
  assert.equal(calls.length,1)
  assert.equal(c.accountStep.value,3)
  assert.equal(c.activeAccount.value,account)
  assert.equal(c.data.accounts[0],account)
  assert.equal(c.catalog.accounts[0],account)
  assert.equal(c.catalog.accounts[1].version,4)
  const verification=c.verifyAccount()
  assert.equal(calls[1].options.body.version,3)
  assert.equal(calls[1].options.body.drive_id,'drive')
  pending[1].resolve({...account,version:4,verified:true});await verification
  assert.equal(calls.length,2)
  assert.equal(c.accountStep.value,4)
  assert.equal(c.data.accounts[0].verified,true)
})
test('verification failure keeps current step and account for retry',async()=>{
  const {context:c,pending}=setup();c.accountStep.value=3
  const operation=c.verifyAccount();const rejected=assert.rejects(operation,/cloud error/)
  pending[0].reject(new Error('cloud error'));await rejected
  assert.equal(c.accountStep.value,3)
  assert.equal(c.activeAccount.value.version,1)
})
test('updating account on another page does not insert it into the visible page',()=>{
  const {context:c}=setup();c.data.accounts=[{id:'other',version:4}]
  c.updateAccount({id:'account',version:3})
  assert.equal(c.data.accounts.length,1)
  assert.equal(c.data.accounts[0].id,'other')
  assert.equal(c.catalog.accounts[0].version,3)
})

test('Google wizard creates its storage group, selects a root explicitly and verifies the current version',async()=>{
  const {context:c,calls,pending,loaded}=setup()
  c.openAccount();c.form.name='Google One';c.form.backend='gdrive';c.form.storage_group='  家庭容量  '
  assert.deepEqual(Array.from(c.derived.accountSteps.value),['账号信息','Google 授权','云端硬盘与验证','完成'])
  const creating=c.createAccount()
  assert.equal(calls[0].options.body.backend,'gdrive')
  assert.equal(calls[0].options.body.storage_group,'家庭容量')
  pending[0].resolve({id:'google',backend:'gdrive',version:1});await creating
  assert.equal(c.accountStep.value,2);assert.deepEqual(loaded,['accounts'])
  const authorizing=c.onRcloneAuthorized({id:'google',backend:'gdrive',version:2})
  assert.equal(calls[1].path,'/accounts/google/drives')
  pending[1].resolve({items:[{id:'root',name:'我的云端硬盘',supported:true}],account:{id:'google',backend:'gdrive',version:3}});await authorizing
  assert.equal(c.accountStep.value,3);assert.equal(c.chosenDrive.value,'')
  c.chosenDrive.value='root'
  const verifying=c.verifyAccount()
  assert.equal(calls[2].options.body.drive_id,'root');assert.equal(calls[2].options.body.version,3)
  pending[2].resolve({id:'google',backend:'gdrive',version:4,verified:true});await verifying
  assert.equal(c.accountStep.value,4);assert.equal(c.activeAccount.value.verified,true)
})
test('account settings can change or clear a Google storage group without adding it to OneDrive requests',async()=>{
  const {context:c,calls,pending}=setup()
  c.editAccountSettings({id:'google',backend:'gdrive',version:7,budget_bytes:1024**3,cloud_reserve_bytes:0,maintenance_reserve_bytes:0,storage_group:'family'})
  c.form.storage_group='   '
  const saving=c.saveAccountSettings()
  assert.equal(calls[0].options.body.storage_group,'');assert.equal(calls[0].options.body.version,7)
  pending[0].resolve({});await saving
  c.openAccount();c.form.name='OneDrive';c.form.storage_group='unused'
  const creating=c.createAccount()
  assert.equal(Object.hasOwn(calls[1].options.body,'storage_group'),false)
  pending[1].resolve({id:'one',backend:'onedrive',version:1});await creating
  assert.deepEqual(Array.from(c.derived.accountSteps.value),['账号信息','微软授权','Drive 与验证','完成'])
})
test('equal known Google capacities warn only for selected ungrouped accounts and do not block saving',async()=>{
  const {context:c,calls,pending}=setup()
  c.catalog.accounts=[{id:'a',backend:'gdrive',total_bytes:100},{id:'b',backend:'gdrive',total_bytes:'100'},{id:'one',backend:'onedrive',total_bytes:100},{id:'unknown',backend:'gdrive',total_bytes:0}]
  Object.assign(c.form,{name:'mixed',account_ids:['a','b'],shard_target_gib:1});c.modal.value='pool'
  assert.equal(c.derived.poolStorageWarning.value,true)
  const saving=c.saveForm();assert.equal(calls[0].path,'/pools')
  pending[0].resolve({});await saving
  c.catalog.accounts[1].storage_group='family';assert.equal(c.derived.poolStorageWarning.value,false)
  c.catalog.accounts[1].storage_group='  ';assert.equal(c.derived.poolStorageWarning.value,true)
  c.form.account_ids=['a','one','unknown'];assert.equal(c.derived.poolStorageWarning.value,false)
  c.catalog.accounts[0].total_bytes=0;c.form.account_ids=['a','unknown'];assert.equal(c.derived.poolStorageWarning.value,false)
})
