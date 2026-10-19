import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'

function actions(file, names) {
  const source = readFileSync(new URL(file, import.meta.url), 'utf8').split('<script setup lang="ts">')[1].split('</script>')[0]
  const ast = ts.createSourceFile(file, source, ts.ScriptTarget.ES2022, true)
  const functions = ast.statements.filter(node => ts.isFunctionDeclaration(node) && names.includes(node.name?.text)).map(node => node.getText(ast)).join('\n')
  return ts.transpileModule(functions, {compilerOptions: {target: ts.ScriptTarget.ES2022}}).outputText
}
const recoveryActions = actions('../src/App.vue', ['exportKit', 'closeModal'])
const accountActions = actions('../src/AccountTransfer.vue', ['run', 'exportFile'])

for (const failed of [false, true]) {
  test(`recovery export sends current login password and clears it after ${failed ? 'failure' : 'success'}`, async () => {
    const calls = []
    const context = vm.createContext({
      form: {password: 'new-package-password', admin_password: 'current-login-password'},
      status: {value: {vault_id: 'vault'}}, modal: {value: 'export'}, busy: {value: false}, authorizationBusy: {value: false}, confirmAction: {value: null},
      action: async fn => { try { await fn() } catch {} }, toast: () => {}, Blob, downloadBlob: () => {},
      api: async (path, options) => { calls.push({path, options}); if (failed) throw new Error('reauth failed'); return {format: 'encrypted'} },
    })
    vm.runInContext(recoveryActions, context)
    await context.exportKit()
    assert.equal(calls[0].path, '/security/recovery-kit')
    assert.equal(calls[0].options.body.admin_password, 'current-login-password')
    assert.equal(calls[0].options.body.password, 'new-package-password')
    assert.equal(context.form.admin_password, '')
    context.form.admin_password = 'entered-again'
    context.closeModal()
    assert.equal(context.form.admin_password, '')
  })
  test(`account export sends current login password and clears it after ${failed ? 'failure' : 'success'}`, async () => {
    const calls = []
    const downloads = []
    const context = vm.createContext({
      password: {value: 'new-package-password'}, repeat: {value: 'new-package-password'}, adminPassword: {value: 'current-login-password'},
      agreed: {value: true}, selected: {value: ['account']}, working: {value: false}, failure: {value: ''}, message: {value: ''}, emit: () => {},
      Error, Blob, URL: {createObjectURL: () => 'blob:export', revokeObjectURL: () => {}}, document: {createElement: () => ({click: () => {}})}, setTimeout: () => {},
      downloadBlob: (_blob, name) => downloads.push(name),
      api: async (path, options) => { calls.push({path, options}); if (failed) throw new Error('reauth failed'); return {format: 'encrypted'} },
    })
    vm.runInContext(accountActions, context)
    await context.exportFile()
    assert.equal(calls[0].path, '/accounts/export')
    assert.equal(calls[0].options.body.admin_password, 'current-login-password')
    assert.equal(calls[0].options.body.password, 'new-package-password')
    assert.equal(context.adminPassword.value, '')
    assert.equal(context.working.value, false)
    assert.deepEqual(downloads, failed ? [] : ['vaultpilot-accounts.json'])
    assert.equal(context.failure.value, failed ? 'reauth failed' : '')
    if (!failed) assert.match(context.message.value, /加密账号包已下载/)
  })
}

test('account export offers verified Google Drive and OneDrive accounts',()=>{
  const source=readFileSync(new URL('../src/AccountTransfer.vue',import.meta.url),'utf8').split('<script setup lang="ts">')[1].split('</script>')[0]
  const ast=ts.createSourceFile('transfer.ts',source,ts.ScriptTarget.ES2022,true)
  const declaration=ast.statements.find(node=>ts.isVariableStatement(node)&&node.declarationList.declarations.some(item=>item.name.getText(ast)==='available'))
  const {outputText}=ts.transpileModule(declaration.getText(ast)+'\nglobalThis.availableAccounts=available',{compilerOptions:{target:ts.ScriptTarget.ES2022}})
  const context=vm.createContext({computed:fn=>({get value(){return fn()}}),props:{accounts:[{id:'one',backend:'onedrive',verified:true},{id:'google',backend:'gdrive',verified:true},{id:'pending',backend:'gdrive',verified:false},{id:'local',backend:'local',verified:true}]}})
  vm.runInContext(outputText,context)
  assert.deepEqual(Array.from(context.availableAccounts.value,item=>item.id),['one','google'])
})
