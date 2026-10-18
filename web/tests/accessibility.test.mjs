import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import { parse as parseSFC } from '@vue/compiler-sfc'
import { parse as parseTemplate, NodeTypes } from '@vue/compiler-dom'

const app = readFileSync(new URL('../src/App.vue', import.meta.url), 'utf8')
const source = parseSFC(app).descriptor.scriptSetup.content
const ast = ts.createSourceFile('accessibility.ts', source, ts.ScriptTarget.ES2022, true)
const logic = ast.statements.filter(node =>
  ts.isFunctionDeclaration(node) && node.name?.text === 'closeModal' ||
  ts.isExpressionStatement(node) && ts.isCallExpression(node.expression) && node.expression.expression.getText(ast) === 'watch',
).map(node => node.getText(ast)).join('\n')
const { outputText } = ts.transpileModule(logic, { compilerOptions: { target: ts.ScriptTarget.ES2022 } })
function setup() {
  const calls = []
  class Element { isConnected = true; focus() { calls.push(this) } }
  const trigger = new Element(), input = new Element(), dialog = new Element()
  dialog.querySelector = selector => { assert.match(selector, /:not\(\[disabled\]\)/); return input }
  let focusChanged
  const context = vm.createContext({
    modal: { value: 'source' }, accountStep: { value: 1 }, busy: { value: false }, authorizationBusy: { value: false },
    confirmAction: { value: () => {} }, modalElement: { value: dialog }, modalTrigger: null,
    document: { activeElement: trigger }, HTMLElement: Element, nextTick: async () => {},
    watch: (_sources, handler) => { focusChanged = handler },
  })
  vm.runInContext(outputText, context)
  return { context, calls, trigger, input, dialog, focusChanged }
}
test('opening a modal focuses its first enabled input and closing restores the trigger', async () => {
  const { context: c, calls, trigger, input, focusChanged } = setup()
  await focusChanged(['source'], [''])
  assert.deepEqual(calls, [input])
  c.closeModal()
  assert.equal(c.modal.value, '')
  assert.equal(c.confirmAction.value, null)
  await focusChanged([''], ['source'])
  assert.deepEqual(calls, [input, trigger])
})
test('a confirmation without inputs receives focus without replacing the original trigger', async () => {
  const { context: c, calls, trigger, input, dialog, focusChanged } = setup()
  await focusChanged(['restore-preview'], [''])
  c.modal.value = 'confirm'
  dialog.querySelector = () => null
  await focusChanged(['confirm'], ['restore-preview'])
  assert.deepEqual(calls, [input, dialog])
  c.closeModal()
  await focusChanged([''], ['confirm'])
  assert.equal(calls.at(-1), trigger)
})
for (const flag of ['busy', 'authorizationBusy']) {
  test(`${flag} keeps modal and pending confirmation open`, () => {
    const { context: c } = setup()
    c[flag].value = true
    const confirm = c.confirmAction.value
    c.closeModal()
    assert.equal(c.modal.value, 'source')
    assert.equal(c.confirmAction.value, confirm)
  })
}
test('removed trigger is not focused after closing', async () => {
  const { context: c, calls, trigger, focusChanged } = setup()
  await focusChanged(['source'], [''])
  trigger.isConnected = false
  c.closeModal()
  await focusChanged([''], ['source'])
  assert.equal(calls.length, 1)
})
test('authorization and import requests publish busy state until success or failure', async () => {
  const script = parseSFC(readFileSync(new URL('../src/RcloneAuthorization.vue', import.meta.url), 'utf8')).descriptor.scriptSetup.content
  const ast = ts.createSourceFile('authorization.ts', script, ts.ScriptTarget.ES2022, true)
  const run = ast.statements.find(node => ts.isFunctionDeclaration(node) && node.name?.text === 'run')
  const { outputText } = ts.transpileModule(run.getText(ast), { compilerOptions: { target: ts.ScriptTarget.ES2022 } })
  for (const failure of [false, true]) {
    const events = []
    const context = vm.createContext({ Error, pending: { value: false }, error: { value: '' }, emit: (...event) => events.push(event) })
    vm.runInContext(outputText, context)
    let finish
    const operation = context.run(() => new Promise((resolve, reject) => { finish = () => failure ? reject(new Error('request failed')) : resolve() }))
    assert.deepEqual(events, [['busy', true]])
    assert.equal(context.pending.value, true)
    finish()
    await operation
    assert.deepEqual(events, [['busy', true], ['busy', false]])
    assert.equal(context.pending.value, false)
    assert.equal(context.error.value, failure ? 'request failed' : '')
  }
})
test('icon buttons have explicit accessible names and modal Escape uses the guarded close action', () => {
  for (const file of ['App.vue', 'SourceBrowser.vue']) {
    const root = parseTemplate(parseSFC(readFileSync(new URL(`../src/${file}`, import.meta.url), 'utf8')).descriptor.template.content)
    function visit(node) {
      if (node.type === NodeTypes.ELEMENT) {
        if (node.tag === 'button' && node.children.some(child => child.type === NodeTypes.ELEMENT) && node.children.every(child => child.type !== NodeTypes.INTERPOLATION && (child.type !== NodeTypes.TEXT || !child.content.trim()) && (child.type !== NodeTypes.ELEMENT || /^[A-Z]/.test(child.tag)))) {
          assert.ok(node.props.some(prop => prop.type === NodeTypes.ATTRIBUTE && prop.name === 'aria-label' && prop.value?.content), `${file}: ${node.loc.source}`)
        }
        if (node.props.some(prop => prop.type === NodeTypes.ATTRIBUTE && prop.name === 'role' && prop.value?.content === 'dialog')) {
          const escape = node.props.find(prop => prop.type === NodeTypes.DIRECTIVE && prop.name === 'on' && prop.arg?.content === 'keydown')
          assert.equal(escape?.exp?.content, 'closeModal')
          assert.ok(escape.modifiers.some(modifier => modifier.content === 'esc'))
        }
      }
      for (const child of node.children ?? []) visit(child)
    }
    visit(root)
  }
})
