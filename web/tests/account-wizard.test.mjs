import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'

// Execute the actual wizard actions with a deferred API, independently of Vue
// rendering, to verify request counts and the response version used for verify.
const source = readFileSync(new URL('../src/App.vue', import.meta.url), 'utf8').split('<script setup lang="ts">')[1].split('</script>')[0]
const ast = ts.createSourceFile('wizard.ts', source, ts.ScriptTarget.ES2022, true)
const names = ['updateAccount', 'onRcloneAuthorized', 'loadDrives', 'verifyAccount']
const functions = ast.statements.filter(node => ts.isFunctionDeclaration(node) && names.includes(node.name?.text)).map(node => node.getText(ast)).join('\n')
const { outputText } = ts.transpileModule(functions, {compilerOptions:{target:ts.ScriptTarget.ES2022}})
function setup() {
  const calls = [], pending = []
  const context = vm.createContext({
    activeAccount:{value:{id:'account',version:1}}, accountStep:{value:2},
    data:{accounts:[{id:'account',version:1}]}, catalog:{accounts:[{id:'account',version:1},{id:'other',version:4}]},
    drives:{value:[]}, chosenDrive:{value:'drive'}, accountBudget:{budget:800,reserve:50,maintenance:50}, recoveryMode:{value:false},
    action:fn=>fn(), toast:()=>{},
    api:(path,options)=>{calls.push({path,options});return new Promise((resolve,reject)=>pending.push({resolve,reject}))},
  })
  vm.runInContext(outputText,context)
  return {context,calls,pending}
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
