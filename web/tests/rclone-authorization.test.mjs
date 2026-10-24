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
  let unmount = () => {}
  const props = Vue.reactive({ account: { id: 'account', name: '备份账号', version: 2, backend }, customOAuth: true, rcloneAvailable: true, busy: false })
  const scope = Vue.effectScope(); t.after(() => scope.stop())
  const context = vm.createContext({
    Error, encodeURIComponent, setTimeout: () => 1, clearTimeout: () => {},
    ref: Vue.ref, computed: Vue.computed, watch: Vue.watch,
    defineProps: () => props, defineEmits: () => ((...event) => events.push(event)),
    onUnmounted: callback => { unmount = callback },
    api: (path, options) => { calls.push({ path, options }); return new Promise((resolve, reject) => pending.push({ resolve, reject })) },
  })
  scope.run(() => vm.runInContext(outputText + '\nglobalThis.state={backend,provider,rcloneType,callbackURL,importPlaceholder,method,googleClientMode,clientID,clientSecret,googleClientError,googleClientLabel,authorizationBody,pending,working,resumeLoading,error,flow,callback,config,filename,remotes,selected,active,start,cancel,submitCallback,preview,importConfig,resume,upload,changedConfig}', context))
  const state = context.state
  async function html() {
    const app = Vue.createSSRApp({ setup: () => ({ ...props, ...state, date: () => 'expiry' }), render })
    for (const name of ['ShieldCheck', 'FileUp', 'LoaderCircle', 'ArrowUpRight', 'RefreshCw']) app.component(name, { render: () => null })
    return renderToString(app)
  }
  return { props, state, calls, events, pending, html, unmount: () => unmount() }
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
test('Google custom authorization submits a body snapshot and clears client inputs after completion', async t => {
  const { state, calls, pending, events } = setup(t, 'gdrive')
  state.googleClientMode.value = 'custom'
  state.clientID.value = '1234567890.apps.googleusercontent.com'
  state.clientSecret.value = 'GOCSPX-original-secret'
  const operation = state.start()
  assert.equal(calls.length, 1)
  assert.deepEqual(JSON.parse(JSON.stringify(calls[0].options.body)), {
    version: 2,
    client_id: '1234567890.apps.googleusercontent.com',
    client_secret: 'GOCSPX-original-secret',
  })
  pending[0].resolve({ id: 'flow', status: 'waiting', authorization_url: 'https://example.test/auth' })
  await new Promise(resolve => setImmediate(resolve))
  assert.equal(calls[1].path, '/accounts/account/rclone/authorize?flow_id=flow')
  pending[1].resolve({ id: 'flow', status: 'completed', custom_client: true, account: { id: 'account', backend: 'gdrive', version: 3 } })
  await operation
  assert.equal(state.clientID.value, '')
  assert.equal(state.clientSecret.value, '')
  assert.equal(events.find(event => event[0] === 'authorized')[0], 'authorized')
})

test('Google custom authorization clears client inputs after a failed request', async t => {
  const { state, calls, pending } = setup(t, 'gdrive')
  state.googleClientMode.value = 'custom'
  state.clientID.value = '1234567890.apps.googleusercontent.com'
  state.clientSecret.value = 'GOCSPX-original-secret'
  const operation = state.start()
  const body = calls[0].options.body
  pending[0].reject(new Error('quota exceeded'))
  await operation
  assert.equal(body.client_secret, 'GOCSPX-original-secret')
  assert.equal(state.clientID.value, '')
  assert.equal(state.clientSecret.value, '')
  assert.equal(state.error.value, 'quota exceeded')
})

test('Google custom authorization rejects missing or malformed fields without submitting', async t => {
  const { state, calls } = setup(t, 'gdrive')
  state.googleClientMode.value = 'custom'
  state.clientID.value = 'not-a-client'
  const operation = state.start()
  await operation
  assert.equal(calls.length, 0)
  assert.match(state.googleClientError.value, /完整填写/)
  state.clientSecret.value = 'GOCSPX-original-secret'
  await state.start()
  assert.equal(calls.length, 0)
  assert.match(state.googleClientError.value, /Client ID 必须/)
  state.clientID.value = '1234567890.apps.googleusercontent.com'
  state.clientSecret.value = 'secret with spaces'
  await state.start()
  assert.equal(calls.length, 0)
  assert.match(state.googleClientError.value, /Client Secret 不能/)
})

test('reusing an existing Google client omits custom client fields', async t => {
  const { state, calls, pending } = setup(t, 'gdrive')
  const operation = state.start()
  assert.deepEqual(JSON.parse(JSON.stringify(calls[0].options.body)), { version: 2 })
  pending[0].resolve({ id: 'flow', status: 'failed', message: 'cancelled by test' })
  await operation
  assert.equal(state.clientID.value, '')
  assert.equal(state.clientSecret.value, '')
})

test('switching to import clears Google client inputs and OneDrive authorization remains unchanged', async t => {
  const google = setup(t, 'gdrive')
  google.state.googleClientMode.value = 'custom'
  google.state.clientID.value = '1234567890.apps.googleusercontent.com'
  google.state.clientSecret.value = 'GOCSPX-original-secret'
  google.state.method.value = 'import'
  assert.equal(google.state.clientID.value, '')
  assert.equal(google.state.clientSecret.value, '')

  const oneDrive = setup(t, 'onedrive')
  const operation = oneDrive.state.start()
  assert.deepEqual(JSON.parse(JSON.stringify(oneDrive.calls[0].options.body)), { version: 2 })
  oneDrive.pending[0].resolve({ id: 'flow', status: 'failed', message: 'cancelled by test' })
  await operation
  assert.equal(oneDrive.calls.length, 1)
})

test('active Google flow displays the server-reported client type', async t => {
  const { state, html } = setup(t, 'gdrive')
  state.flow.value = { id: 'flow', status: 'waiting', custom_client: true }
  let markup = await html()
  assert.match(markup, /你提供的独立 Google 应用/)
  state.flow.value = { id: 'flow', status: 'waiting', custom_client: false }
  markup = await html()
  assert.match(markup, /共享的 Rclone Google 应用/)
})

test('busy state disables Google client controls and authorization action', async t => {
  const { props, html } = setup(t, 'gdrive')
  props.busy = true
  const markup = await html()
  assert.equal(markup.includes('aria-busy="true"'), false)
  assert.match(markup, /disabled/)
})

test('cancelling or unmounting clears Google client inputs', async t => {
  const cancelled = setup(t, 'gdrive')
  cancelled.state.googleClientMode.value = 'custom'
  cancelled.state.clientID.value = '1234567890.apps.googleusercontent.com'
  cancelled.state.clientSecret.value = 'GOCSPX-original-secret'
  cancelled.state.flow.value = { id: 'flow', status: 'waiting' }
  const operation = cancelled.state.cancel()
  assert.equal(cancelled.state.clientSecret.value, '')
  cancelled.pending[0].resolve({ id: 'flow', status: 'cancelled' })
  await operation

  const unmounted = setup(t, 'gdrive')
  unmounted.state.clientID.value = '1234567890.apps.googleusercontent.com'
  unmounted.state.clientSecret.value = 'GOCSPX-original-secret'
  unmounted.unmount()
  assert.equal(unmounted.state.clientID.value, '')
  assert.equal(unmounted.state.clientSecret.value, '')
})
