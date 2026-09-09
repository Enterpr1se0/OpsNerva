import assert from 'node:assert/strict'
import { register } from 'node:module'
import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'

register(new URL('./typescript-loader.mjs',import.meta.url))
// This is component/server rendering, not a browser or WebView launch.
globalThis.window={}
globalThis.document={documentElement:{lang:''}}
globalThis.localStorage={getItem:()=> 'en',setItem:()=>{}}
globalThis.fetch=()=>{throw new Error('Rendering must not fetch audit or tool details')}
const {AuditHistoryStore}=await import('../../src/features/audit/auditHistoryStore.ts')
const {AuditRunsView}=await import('../../src/features/audit/AuditRunsView.tsx')
const {AuditPageFeedback}=await import('../../src/features/audit/AuditPageFeedback.tsx')
const {buildToolEventView}=await import('../../src/features/tools/toolEvent.ts')
const {default:i18n}=await import('../../src/lib/i18n.ts')

const stamp=n=>new Date(Date.UTC(2026,0,1)+n*1000).toISOString()
const group=(id,kind='chat',title=id)=>({session_id:id,kind,title,session_exists:true,latest_started_at:stamp(100),run_count:120,pending_count:0})
const run=n=>({id:`run-${n}`,session_id:'old-session',host_id:'host',status:'completed',started_at:stamp(n),request_json:JSON.stringify({mode:'program',program:'echo',args:[String(n)]}),exit_code:0})
const groups=[group('old-session','chat','Joined old title'),group('mcp_sess_1','mcp','External Agent'),group('','direct','')]
const rows=Array.from({length:50},(_,i)=>run(100-i))
const calls=[]
const store=new AuditHistoryStore({
	groups:async()=>({items:groups,snapshotAt:stamp(200),nextCursor:{started_at:stamp(100),id:''}}),
	runs:async(session,input)=>{calls.push({session,input});return{items:rows,snapshotAt:input.snapshotAt,nextCursor:{started_at:stamp(51),id:'run-51'}}},
	deleteRuns:async()=>{throw new Error('Rendering must not delete')},
},()=>()=>{},error=>{throw error})
const settle=async()=>{for(let i=0;i<50;i++)await Promise.resolve()}
const render=()=>renderToStaticMarkup(createElement(AuditRunsView,{history:store,hosts:[],notify:()=>{}}))

let html=render()
assert(html.includes('aria-label="Search"'));assert(!html.includes('class="spin"'))
store.setActive(true);await settle();html=render()
assert(html.includes('Load earlier sessions'));assert(!html.includes('Load earlier records'))
assert(html.includes('Joined old title'));assert(html.includes('External Agent'));assert(html.includes('Direct / legacy operations'))
assert(!html.includes('Deleted or unavailable'));assert(!html.includes('RUNS'));assert.equal(calls.length,0)

store.setOpen('old-session',true);await settle();html=render()
assert(html.includes('class="audit-session panel" open=""'))
assert(html.includes('Load earlier records'));assert(html.includes('Load earlier sessions'))
assert.equal((html.match(/class="audit-row"/g)||[]).length,50)
assert.equal(calls.length,1);assert.equal(calls[0].session,'old-session')
assert.equal(calls[0].input.snapshotAt,store.groups.getSnapshot().snapshotAt)
store.setOpen('old-session',false);html=render()
assert(!html.includes('class="audit-session panel" open=""'))
assert(!html.includes('Load earlier records'));assert(!html.includes('class="spin"'))
assert.equal((html.match(/class="audit-row"/g)||[]).length,50,'keep the disclosure body for the existing CSS collapse transition')
store.dispose()

const feedback=page=>renderToStaticMarkup(createElement(AuditPageFeedback,{page,onRetry:async()=>{},onLoadMore:async()=>{},moreLabel:'More'}))
assert(feedback({loading:'refresh',error:null,nextCursor:null}).includes('role="status"'))
const loadingMore=feedback({loading:'more',error:null,nextCursor:{started_at:stamp(1),id:'run-1'}})
assert(loadingMore.includes('<button'));assert(loadingMore.includes('disabled=""'));assert(loadingMore.includes('More'))
const failed=feedback({loading:'idle',error:new Error('offline'),nextCursor:null})
assert(failed.includes('offline'));assert(failed.includes('Retry'));assert(!failed.includes('class="spin"'))
assert.equal(feedback({loading:'idle',error:null,nextCursor:null}),'')

const entry={id:'tool-1',kind:'tool',tool:'ssh_run_script',content:'',startedAt:1234}
const request={host_id:'host',mode:'script',script:'echo test',reason:'check service',sudo:true}
const payload={run_id:'run-1',status:'completed',stdout:'test',stderr:'',exit_code:0,duration:5e6,auto_approved:true,_display:{host_id:'host',request,arguments:request}}
const input={entry,storedPayload:payload,hosts:[{id:'host',name:'Host One',user:'root'}],liveSSHTaskOwner:false,t:i18n.t.bind(i18n)}
const view=buildToolEventView(input)
assert.equal(view.script,'echo test');assert.equal(view.purpose,'check service');assert.equal(view.hostName,'Host One')
assert.equal(view.stdout,'test');assert.equal(view.exitCode,0);assert.equal(view.duration,'5.0 ms')
assert.equal(view.status,'completed');assert(view.autoApproved);assert.equal(view.startedAt,1234)
const failedTool=buildToolEventView({...input,storedPayload:{...payload,status:'failed',error:'interrupted'}})
assert.equal(failedTool.status,'failed');assert.equal(failedTool.stderr,'interrupted')
const live=buildToolEventView({...input,storedPayload:{...payload,status:'running'}})
assert.equal(live.status,'in_progress');assert(live.toolLive)
console.log('PASS audit component rendering, independent page controls, disclosure structure, bounded feedback and self-contained tool payloads')
