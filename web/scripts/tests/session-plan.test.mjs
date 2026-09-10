import assert from 'node:assert/strict'
import {register} from 'node:module'
import {createElement} from 'react'
import {renderToStaticMarkup} from 'react-dom/server'

register(new URL('./typescript-loader.mjs',import.meta.url))
globalThis.window={location:{protocol:'http:',host:'opsnerva.test'}}
globalThis.document={documentElement:{lang:''},visibilityState:'visible'}
globalThis.localStorage={getItem:()=> 'en',setItem:()=>{}}
const {SessionPlan}=await import('../../src/features/chat/SessionPlan.tsx')
const {subscribeSessionPlan}=await import('../../src/features/chat/sessionPlanState.ts')
const {buildToolEventView}=await import('../../src/features/tools/toolEvent.ts')
const {historyEntries}=await import('../../src/features/chat/chatEntries.ts')
const plan={session_id:'s',goal:'Repair service',status:'active',created_at:'2026-09-10T00:00:00Z',updated_at:'2026-09-10T00:00:01Z',steps:[
	{number:1,title:'Inspect',status:'completed'},
	{number:2,title:'Repair',status:'in_progress'},
	{number:3,title:'Verify',status:'pending'},
]}
const render=(value,active)=>renderToStaticMarkup(createElement(SessionPlan,{plan:value,active,visible:true}))
let html=render(plan,false)
assert(html.includes('session-plan paused'))
assert(html.includes('Paused'))
assert(html.includes('Repair service'))
assert(html.includes('33%'))
assert(!html.includes('open=""'),'historical plans should not automatically cover chat')
assert(!html.includes('class="spin"'),'paused plans must not animate')
assert(!html.includes('blocked'))
assert(render(plan,true).includes('session-plan active'))
const completed={...plan,status:'completed',steps:plan.steps.map(step=>({...step,status:'completed'}))}
html=render(completed,false)
assert(html.includes('session-plan completed'))
assert(html.includes('100%'))
assert(html.includes('3 / 3 completed'))
const toolResult={status:'completed',plan}
const toolEntry={id:'plan-tool',kind:'tool',tool:'ops_plan_create',content:JSON.stringify(toolResult)}
const toolView=buildToolEventView({entry:toolEntry,storedPayload:toolResult,hosts:[],liveSSHTaskOwner:false,t:key=>key})
assert.equal(toolView.status,'completed','tool completion is independent of plan activity')
assert.equal(toolView.toolLive,false)
assert.equal(toolView.commandSummary,plan.goal)
const [historyEntry]=historyEntries([{id:'plan-tool',role:'tool',tool_name:'ops_plan_create',content:toolEntry.content,created_at:plan.updated_at}],'s')
assert.equal(historyEntry.transient,false,'restored plan tools must not become running tools in history')
const sockets=[]
class MockWebSocket{
	static CONNECTING=0
	static OPEN=1
	static CLOSED=3
	constructor(url){this.url=url;this.readyState=MockWebSocket.CONNECTING;this.sent=[];sockets.push(this)}
	close(){this.readyState=MockWebSocket.CLOSED;this.onclose?.()}
	send(value){this.sent.push(JSON.parse(value))}
}
globalThis.WebSocket=MockWebSocket
const received=[]
const unsubscribe=subscribeSessionPlan('s',next=>received.push(next))
await Promise.resolve()
assert.equal(sockets.length,1)
const socket=sockets[0]
socket.readyState=MockWebSocket.OPEN
socket.onopen()
assert.deepEqual(socket.sent.at(-1).topics,['chat_state'])
assert.equal(socket.sent.at(-1).session_id,'s')
const send=(sequence,data)=>socket.onmessage({data:JSON.stringify({type:'event',topic:'chat_state',sequence,data})})
send(1,{plan:null})
send(2,{active:true})
assert.deepEqual(received,[null],'unrelated deltas must not clear plan state')
send(3,{plan})
send(4,{plan:completed})
send(3,{plan})
assert.equal(received.length,3,'the shared stream rejects stale or duplicate events')
assert.deepEqual(received.at(-1),completed)
send(5,{plan:{...plan,session_id:'another-session'}})
assert.equal(received.length,3,'ignore plans belonging to another session')
unsubscribe()
await Promise.resolve()
assert.equal(socket.readyState,MockWebSocket.CLOSED,'release the subscription when hidden or unmounted')
send(6,{plan})
assert.equal(received.length,3,'stopped subscriptions must not update state')
console.log('PASS sequential plan rendering, paused activity, completed history and event lifecycle')
