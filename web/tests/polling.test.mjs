import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
const {outputText}=ts.transpileModule(readFileSync(new URL('../src/polling.ts',import.meta.url),'utf8'),{compilerOptions:{target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.CommonJS}})
function setup(hidden=false) {
  const timers=new Map(),listeners=new Map();let sequence=0
  const document={hidden,addEventListener:(event,fn)=>listeners.set(event,fn),removeEventListener:(event,fn)=>{if(listeners.get(event)===fn)listeners.delete(event)}}
  const context=vm.createContext({exports:{},document,setTimeout:(fn,delay)=>{timers.set(++sequence,{fn,delay});return sequence},clearTimeout:id=>timers.delete(id)})
  vm.runInContext(outputText,context)
  return {...context.exports,timers,listeners,visibility(hidden){document.hidden=hidden;listeners.get('visibilitychange')?.()},async tick(){const[id,timer]=timers.entries().next().value;timers.delete(id);await timer.fn()}}
}
test('hidden tabs pause polling and becoming visible refreshes immediately before resuming the interval',async()=>{
  const c=setup();let calls=0
  const stop=c.pollWhileVisible(async()=>{calls++},5000,false)
  assert.equal(calls,0);assert.equal(c.timers.size,1)
  c.visibility(true);assert.equal(c.timers.size,0)
  c.visibility(false);assert.equal(calls,1)
  await new Promise(setImmediate);assert.equal([...c.timers.values()][0].delay,5000)
  await c.tick();assert.equal(calls,2)
  stop();assert.equal(c.listeners.size,0);assert.equal(c.timers.size,0)
})
test('polling mounted in a hidden tab waits for visibility and never overlaps an in-flight refresh',async()=>{
  const c=setup(true);let calls=0,finish
  const stop=c.pollWhileVisible(()=>{calls++;return new Promise(resolve=>{finish=resolve})},2000)
  assert.equal(calls,0);assert.equal(c.timers.size,0)
  c.visibility(false);assert.equal(calls,1)
  c.visibility(true);c.visibility(false);assert.equal(calls,1)
  finish();await new Promise(setImmediate);assert.equal([...c.timers.values()][0].delay,0)
  await new Promise(setImmediate);const[id,timer]=c.timers.entries().next().value;c.timers.delete(id);const running=timer.fn()
  assert.equal(calls,2);stop();finish();await running
  assert.equal(c.timers.size,0);assert.equal(c.listeners.size,0)
})
