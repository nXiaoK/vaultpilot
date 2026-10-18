import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { webcrypto } from 'node:crypto'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'

const { outputText } = ts.transpileModule(readFileSync(new URL('../src/api.ts', import.meta.url), 'utf8'), {
  compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS },
})

// Run the real request code with isolated browser capabilities and a fetch recorder.
function client(crypto, globals = {}, response = { status: 200, ok: true, json: async () => ({ id: 'queued-task' }) }) {
  const requests = []
  const context = vm.createContext({
    exports: {}, crypto, ...globals,
    fetch: async (url, options) => {
      requests.push({ url, ...options })
      return response
    },
  })
  vm.runInContext(outputText, context)
  return { ...context.exports, requests }
}

test('task requests use native randomUUID and preserve the request payload and CSRF token', async () => {
  const native = { randomUUID() { assert.equal(this, native); return 'native-task-key' } }
  const { api, setCSRF, requests } = client(native)
  setCSRF('test-csrf')
  const result = await api('/plans/test/run', { method: 'POST', body: { version: 1 }, task: true })
  assert.equal(result.id, 'queued-task')
  assert.equal(requests[0].url, '/api/plans/test/run')
  assert.equal(requests[0].method, 'POST')
  assert.equal(requests[0].headers['Idempotency-Key'], 'native-task-key')
  assert.equal(requests[0].headers['X-CSRF-Token'], 'test-csrf')
  assert.equal(requests[0].headers['Content-Type'], 'application/json')
  assert.equal(requests[0].body, '{"version":1}')
  assert.equal(requests[0].credentials, 'same-origin')
})

test('HTTP browser without randomUUID submits task requests with unique UUID v4 keys', async () => {
  const crypto = { getRandomValues(bytes) { assert.equal(this, crypto); return webcrypto.getRandomValues(bytes) } }
  const { api, requests } = client(crypto)
  for (let i = 0; i < 100; i++) await api('/plans/test/run', { method: 'POST', task: true })
  const keys = requests.map(request => request.headers['Idempotency-Key'])
  assert.equal(new Set(keys).size, 100)
  for (const key of keys) assert.match(key, /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/)
})

for (const crypto of [undefined, {}]) {
  test(`task requests work with ${crypto ? 'empty' : 'missing'} crypto and do not collide within a clock tick`, async () => {
    const { api, requests } = client(crypto, { Date: { now: () => 123456 }, Math: { random: () => 0.5 } })
    for (let i = 0; i < 100; i++) await api('/plans/test/run', { method: 'POST', task: true })
    const keys = requests.map(request => request.headers['Idempotency-Key'])
    assert.equal(new Set(keys).size, 100)
    for (const key of keys) {
      assert.match(key, /^task-[a-z0-9-]+$/)
      assert.ok(key.length < 200)
    }
  })
}

test('ordinary requests need no crypto and receive no task key', async () => {
  const { api, requests } = client(undefined)
  await api('/plans')
  assert.equal(requests[0].headers['Idempotency-Key'], undefined)
})

test('JSON null error responses become ApiError and release the active request', async () => {
  const { api, ApiError, subscribeActivity } = client(undefined, {}, { status: 503, ok: false, json: async () => null })
  const counts = []
  const unsubscribe = subscribeActivity(items => counts.push(items.length))
  await assert.rejects(api('/status'), error => {
    assert.ok(error instanceof ApiError)
    assert.equal(error.message, '操作未完成')
    assert.equal(error.code, 'request_failed')
    assert.equal(error.retryable, false)
    assert.equal(error.status, 503)
    return true
  })
  unsubscribe()
  assert.deepEqual(counts, [0, 1, 0])
})

test('structured errors retain their server message, code, retry policy and status', async () => {
  const { api, ApiError } = client(undefined, {}, { status: 409, ok: false, json: async () => ({ error: 'version changed', code: 'conflict', retryable: true }) })
  await assert.rejects(api('/accounts'), error => error instanceof ApiError && error.message === 'version changed' && error.code === 'conflict' && error.retryable && error.status === 409)
})

test('successful JSON null remains null', async () => {
  const { api } = client(undefined, {}, { status: 200, ok: true, json: async () => null })
  assert.equal(await api('/optional'), null)
})

test('downloads attach the link and revoke its URL only after the timer fires', () => {
  const calls = []
  let release
  const blob = {}
  const link = { click() { calls.push('click') }, remove() { calls.push('remove') } }
  const { downloadBlob } = client(undefined, {
    URL: { createObjectURL(value) { assert.equal(value, blob); return 'blob:kit' }, revokeObjectURL(value) { calls.push(`revoke:${value}`) } },
    document: { createElement(tag) { assert.equal(tag, 'a'); return link }, body: { appendChild(value) { assert.equal(value, link); calls.push('append') } } },
    setTimeout(fn, delay) { assert.equal(delay, 1000); release = fn },
  })
  downloadBlob(blob, 'recovery.json')
  assert.equal(link.href, 'blob:kit'); assert.equal(link.download, 'recovery.json')
  assert.deepEqual(calls, ['append', 'click', 'remove'])
  release()
  assert.deepEqual(calls, ['append', 'click', 'remove', 'revoke:blob:kit'])
})
