import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as Vue from 'vue'
import { parse } from '@vue/compiler-sfc'
import { compile } from '@vue/compiler-dom'
import { renderToString } from '@vue/server-renderer'

const descriptor = parse(readFileSync(new URL('../src/RcloneAuthorization.vue', import.meta.url), 'utf8')).descriptor
const ast = ts.createSourceFile('authorization.ts', descriptor.scriptSetup.content, ts.ScriptTarget.ES2022, true)
const script = ast.statements.filter(node => !ts.isImportDeclaration(node)).map(node => node.getText(ast)).join('\n')
const { outputText } = ts.transpileModule(script, { compilerOptions: { target: ts.ScriptTarget.ES2022 } })
const { code } = compile(descriptor.template.content, { mode: 'function', prefixIdentifiers: true })
const render = vm.runInNewContext(`(function(){${code}})()`, { Vue })
function setup(t, backend) {
  const calls = [], events = [], pending = []
  const props = Vue.reactive({ account: { id: 'account', name: '备份账号', version: 2, backend }, customOAuth: true, rcloneAvailable: true, busy: false })
  const scope = Vue.effectScope(); t.after(() => scope.stop())
  const context = vm.createContext({
    Error, encodeURIComponent, setTimeout: () => 1, clearTimeout: () => {},
    ref: Vue.ref, computed: Vue.computed, watch: Vue.watch, onUnmounted: () => {},
    defineProps: () => props, defineEmits: () => ((...event) => events.push(event)),
    api: (path, options) => { calls.push({ path, options }); return new Promise((resolve, reject) => pending.push({ resolve, reject })) },
  })
  scope.run(() => vm.runInContext(outputText + '\nglobalThis.state={backend,provider,rcloneType,callbackURL,importPlaceholder,method,pending,working,resumeLoading,error,flow,callback,config,filename,remotes,selected,active,start,cancel,submitCallback,preview,importConfig,resume,upload,changedConfig}', context))
  const state = context.state
  async function html() {
    const app = Vue.createSSRApp({ setup: () => ({ ...props, ...state, date: () => 'expiry' }), render })
    for (const name of ['ShieldCheck', 'FileUp', 'LoaderCircle', 'ArrowUpRight', 'RefreshCw']) app.component(name, { render: () => null })
    return renderToString(app)
  }
  return { props, state, calls, events, pending, html }
}
for (const [backend, provider, type, callback] of [
  ['gdrive', 'Google', 'drive', 'http://127.0.0.1:53682/'],
  ['onedrive', '微软', 'onedrive', 'http://localhost:53682/'],
]) {
  test(`${backend} renders its provider, loopback callback and import example`, async t => {
    const { state, html } = setup(t, backend)
    let markup = await html()
    assert.ok(markup.includes(`在${provider}页面登录并同意授权`))
    assert.equal(markup.includes('高级：使用部署者注册的微软应用'), backend === 'onedrive')
    state.flow.value = { id: 'flow', status: 'waiting', authorization_url: 'https://example.test/auth' }
    markup = await html()
    assert.ok(markup.includes(`打开${provider}授权页面`))
    assert.ok(markup.includes(callback))
    state.flow.value = null; state.method.value = 'import'
    markup = await html()
    assert.ok(markup.includes(`rclone authorize ${type}`))
    assert.ok(markup.includes(`type = ${type}`))
    if (backend === 'gdrive') assert.doesNotMatch(markup, /微软|Microsoft|localhost:53682/)
  })
  test(`${backend} import preview includes backend and only imports the selected remote`, async t => {
    const { state, calls, pending, events } = setup(t, backend)
    state.config.value = '[remote]\ntype = ' + type
    state.selected.value = 'stale'
    const preview = state.preview()
    assert.equal(calls[0].path, '/rclone/import/preview')
    assert.equal(calls[0].options.body.backend, backend)
    assert.equal(state.pending.value, true)
    pending[0].resolve({ items: [{ name: 'remote' }] }); await preview
    assert.equal(state.selected.value, '')
    state.selected.value = 'remote'
    const importing = state.importConfig()
    assert.equal(calls[1].path, '/accounts/account/rclone/import')
    assert.equal(calls[1].options.body.remote, 'remote')
    assert.equal(calls[1].options.body.version, 2)
    const account = { id: 'account', backend, version: 3 }
    pending[1].resolve(account); await importing
    assert.equal(state.config.value, '')
    assert.equal(state.remotes.value.length, 0)
    assert.equal(events.find(event => event[0] === 'authorized')[1], account)
    assert.equal(state.pending.value, false)
  })
}
test('failed Google preview keeps input available and releases busy state', async t => {
  const { state, pending, events } = setup(t, 'gdrive')
  state.config.value = 'invalid config'
  const operation = state.preview()
  pending[0].reject(new Error('invalid scope')); await operation
  assert.equal(state.config.value, 'invalid config')
  assert.equal(state.error.value, 'invalid scope')
  assert.equal(state.pending.value, false)
  assert.deepEqual(events.at(-1), ['busy', false])
})
