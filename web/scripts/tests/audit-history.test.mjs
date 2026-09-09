import assert from 'node:assert/strict'
import {register} from 'node:module'

// Run the actual TypeScript modules without emitting files or adding a test bundler.
register(new URL('./typescript-loader.mjs',import.meta.url))
const {AuditHistoryPage}=await import('../../src/features/audit/auditHistoryPage.ts')
const {AuditHistoryStore}=await import('../../src/features/audit/auditHistoryStore.ts')

const timers=new Map()
let timerID=0
globalThis.setTimeout=(fn,delay)=>{assert.equal(delay,250);timers.set(++timerID,fn);return timerID}
globalThis.clearTimeout=id=>timers.delete(id)
const settle=async()=>{for(let i=0;i<200;i++)await Promise.resolve()}
const flush=async()=>{const callbacks=[...timers.values()];timers.clear();callbacks.forEach(fn=>fn());await settle()}
const stamp=n=>new Date(Date.UTC(2026,0,1)+n*1000).toISOString()
const run=(n,session='a')=>({id:`${session}-${String(n).padStart(5,'0')}`,session_id:session,started_at:stamp(n),request_json:`echo ${n}`,status:'completed',host_id:'host',exit_code:0})
const position=item=>({started_at:item.started_at,id:item.id})
const key=cursor=>`${cursor.started_at}/${cursor.id}`
const desc=(a,b)=>a===b?0:a>b?-1:1

function fixture(count=400){
	const calls=[],listeners=new Set(),holds=[],errors=[]
	let rows=Array.from({length:count},(_,i)=>run(i+1)),now=count,deleteResult,deleteResolve
	const request=(kind,session,input,signal)=>{
		calls.push({kind,session,input:{...input},signal})
		const snapshotAt=input.snapshotAt||stamp(now)
		const matching=rows.filter(row=>row.started_at<=snapshotAt&&row.request_json.includes(input.query))
		let items
		if(kind==='runs')items=matching.filter(row=>row.session_id===session).sort((a,b)=>desc(key(position(a)),key(position(b))))
		else{
			const grouped=new Map()
			for(const row of matching){
				const group=grouped.get(row.session_id)||{session_id:row.session_id,kind:row.session_id?'chat':'direct',title:row.session_id,session_exists:true,latest_started_at:'',run_count:0,pending_count:0}
				group.run_count++;group.latest_started_at=group.latest_started_at>row.started_at?group.latest_started_at:row.started_at
				if(row.status==='approval_required')group.pending_count++
				grouped.set(row.session_id,group)
			}
			items=[...grouped.values()].sort((a,b)=>desc(key({started_at:a.latest_started_at,id:a.session_id}),key({started_at:b.latest_started_at,id:b.session_id})))
		}
		const cursor=item=>kind==='runs'?position(item):{started_at:item.latest_started_at,id:item.session_id}
		if(input.cursor)items=items.filter(item=>key(cursor(item))<key(input.cursor))
		const selected=items.slice(0,input.limit)
		const result=structuredClone({items:selected,snapshotAt,nextCursor:items.length>input.limit?cursor(selected.at(-1)):null})
		const hold=holds.find(item=>!item.started&&item.kind===kind)
		if(hold){hold.started=true;return new Promise((resolve,reject)=>{hold.release=()=>resolve(result);hold.reject=reject})}
		return Promise.resolve(result)
	}
	const api={groups:(input,signal)=>request('groups',null,input,signal),runs:(session,input,signal)=>request('runs',session,input,signal),
		deleteRuns:()=>new Promise(resolve=>{deleteResolve=()=>resolve(deleteResult)})}
	const store=new AuditHistoryStore(api,listener=>{listeners.add(listener);return()=>listeners.delete(listener)},error=>errors.push(error))
	return{api,store,calls,errors,listeners,
		get rows(){return rows},set rows(value){rows=value},get now(){return now},set now(value){now=value},
		hold(kind){const pending={kind,started:false};holds.push(pending);return pending},
		emit(event){for(const listener of listeners)listener(event)},
		deletion(result){deleteResult=result;return()=>deleteResolve()},
	}
}

let tests=0
async function check(name,fn){
	assert.equal(timers.size,0,`${name}: previous test leaked a timer`)
	await fn();tests++;console.log(`PASS ${name}`)
}

await check('refresh fills 150 new records without losing the previously loaded boundary',async()=>{
	const f=fixture()
	const page=new AuditHistoryPage((input,signal)=>f.api.runs('a',input,signal),position,100,error=>f.errors.push(error))
	page.setActive(true);await page.refresh();await page.loadMore()
	assert.equal(page.getSnapshot().items.length,200)
	const old=page.getSnapshot().items.at(-1)
	f.rows.push(...Array.from({length:150},(_,i)=>run(401+i)));f.now=550
	await page.refresh()
	const state=page.getSnapshot()
	assert.equal(state.items.length,350);assert.equal(state.items.at(-1),old)
	assert.deepEqual(state.items.map(item=>item.id),Array.from({length:350},(_,i)=>run(550-i).id))
	await page.loadMore()
	assert.equal(page.getSnapshot().items.length,450)
	const items=page.getSnapshot().items
	await page.refresh();assert.equal(page.getSnapshot().items,items)
	assert.deepEqual(f.errors,[])
})

await check('older pages stay pinned until explicit revalidation and concurrent clicks share one request',async()=>{
	const f=fixture(),page=new AuditHistoryPage((input,signal)=>f.api.runs('a',input,signal),position,100,()=>{})
	page.setActive(true);await page.refresh()
	f.rows.push(run(401));f.now=401
	const delayed=f.hold('runs'),first=page.loadMore(),second=page.loadMore()
	assert.equal(first,second);assert.equal(f.calls.length,2)
	delayed.release();await first
	assert.equal(page.getSnapshot().items.length,200);assert.equal(page.getSnapshot().snapshotAt,stamp(400))
	assert(!page.getSnapshot().items.some(item=>item.id===run(401).id))
})

await check('a deleted boundary remains a valid cursor without pulling an extra page into view',async()=>{
	const f=fixture(200),page=new AuditHistoryPage((input,signal)=>f.api.runs('a',input,signal),position,100,()=>{})
	page.setActive(true);await page.refresh()
	f.rows=f.rows.filter(item=>item.id!==run(101).id)
	await page.refresh()
	assert.equal(page.getSnapshot().items.length,99);assert.equal(page.getSnapshot().nextCursor.id,run(101).id)
	await page.loadMore();assert.equal(page.getSnapshot().items.length,199)
})

await check('query change rejects a late old response even when transport ignores abort',async()=>{
	const f=fixture(),page=new AuditHistoryPage((input,signal)=>f.api.runs('a',input,signal),position,100,error=>f.errors.push(error))
	page.setActive(true);await page.refresh()
	const delayed=f.hold('runs'),pending=page.loadMore(),signal=f.calls.at(-1).signal
	page.reset('echo 4');assert(signal.aborted)
	await page.refresh();const current=page.getSnapshot()
	delayed.release();assert.equal(await pending,false)
	assert.equal(page.getSnapshot(),current);assert(current.items.every(item=>item.request_json.includes('echo 4')))
	assert.deepEqual(f.errors,[])
})

await check('errors finish loading, preserve displayed history and allow retry',async()=>{
	const f=fixture(),page=new AuditHistoryPage((input,signal)=>f.api.runs('a',input,signal),position,100,error=>f.errors.push(error))
	page.setActive(true);await page.refresh()
	const items=page.getSnapshot().items,delayed=f.hold('runs'),pending=page.refresh()
	delayed.reject(new Error('offline'));await pending
	assert.equal(page.getSnapshot().items,items);assert.equal(page.getSnapshot().loading,'idle');assert(page.getSnapshot().ready)
	assert.equal(f.errors.length,1)
	await page.refresh();assert.equal(page.getSnapshot().error,null)
})

await check('broken cursors fail with no endless page loop',async()=>{
	let calls=0
	const errors=[],page=new AuditHistoryPage(async()=>{calls++;return{items:[],snapshotAt:stamp(1),nextCursor:position(run(1))}},position,50,error=>errors.push(error))
	page.setActive(true);assert.equal(await page.refresh(),false)
	assert.equal(calls,1);assert.equal(errors.length,1);assert.equal(page.getSnapshot().loading,'idle')
})

await check('revalidation keeps nanosecond cursor precision',async()=>{
	const rows=[{id:'a',started_at:'2026-01-01T00:00:00.123456789Z'},{id:'z',started_at:'2026-01-01T00:00:00.123456788Z'}]
	let available=rows.slice(0,1)
	const page=new AuditHistoryPage(async()=>({items:available,snapshotAt:stamp(1),nextCursor:null}),position,50,()=>{})
	page.setActive(true);await page.refresh();available=rows;await page.refresh()
	assert.deepEqual(page.getSnapshot().items,[rows[0]])
	assert.equal(page.getSnapshot().nextCursor.id,'a')
})

await check('group and command subscriptions are independent; collapsed groups do no background work',async()=>{
	const f=fixture(150),s=f.store
	f.rows.push(run(151,'b'));f.now=151
	s.setActive(true);await settle();assert.equal(f.calls.filter(call=>call.kind==='runs').length,0)
	s.setOpen('a',true);await settle()
	let groupsChanged=0;const unsubscribe=s.groups.subscribe(()=>groupsChanged++)
	await s.getRuns('a').loadMore();assert.equal(groupsChanged,0)
	s.setOpen('a',false)
	const count=f.calls.length
	for(let i=0;i<500;i++)f.emit({type:'event',mode:'delta',data:{type:'run_completed'}})
	assert.equal(timers.size,1);assert.equal(f.calls.length,count)
	await flush();assert.equal(f.calls.length,count+1)
	assert.equal(s.getViewSnapshot().expanded.has('a'),false)
	unsubscribe();s.dispose()
})

await check('reconnect preserves 200 loaded commands and does not clear cards while reading',async()=>{
	const f=fixture(350),s=f.store
	s.setActive(true);await settle();s.setOpen('a',true);await settle()
	for(let i=0;i<3;i++)await s.getRuns('a').loadMore()
	const items=s.getRuns('a').getSnapshot().items;assert.equal(items.length,200)
	f.emit({type:'event',mode:'snapshot',data:{}})
	assert.equal(s.getRuns('a').getSnapshot().items,items)
	await flush();assert.equal(s.getRuns('a').getSnapshot().items,items)
	assert(s.getViewSnapshot().expanded.has('a'));s.dispose()
})

await check('hidden views cancel reads and the only event timer; late errors stay silent',async()=>{
	const f=fixture(),s=f.store
	s.setActive(true);await settle()
	f.emit({type:'event',mode:'delta'});assert.equal(timers.size,1)
	s.setActive(false);assert.equal(timers.size,0);assert.equal(f.listeners.size,0)
	const count=f.calls.length
	await s.refresh();await s.loadMoreGroups();assert.equal(f.calls.length,count)
	s.setActive(true);await settle()
	const hold=f.hold('groups'),pending=s.refresh();await settle()
	assert(hold.started);const signal=f.calls.at(-1).signal
	s.setActive(false);assert(signal.aborted)
	s.setActive(true);await settle();const current=s.groups.getSnapshot()
	hold.reject(new Error('late error'));await pending
	assert.equal(s.groups.getSnapshot(),current);assert.deepEqual(f.errors,[]);s.dispose()
})

await check('deletion invalidates a delayed older page; deleted rows cannot reappear',async()=>{
	const f=fixture(150),s=f.store
	s.setActive(true);await settle();s.setOpen('a',true);await settle()
	const hold=f.hold('runs'),pending=s.getRuns('a').loadMore(),signal=f.calls.at(-1).signal
	f.rows=[]
	f.emit({type:'event',mode:'delta',data:{id:'delete-1',type:'audit_records_deleted',data:{scope:'session',session_id:'a',deleted:150,retained:0}}})
	assert(signal.aborted);assert.equal(s.getRuns('a').getSnapshot().items.length,0)
	hold.release();await pending;await flush()
	assert.equal(s.groups.getSnapshot().items.length,0);assert.equal(s.getRuns('a').getSnapshot().items.length,0)
	s.dispose()
})

await check('WebSocket then REST deletion reports are deduplicated without removing newer records',async()=>{
	const f=fixture(50),s=f.store
	s.setActive(true);await settle();s.setOpen('a',true);await settle()
	const result={audit_event_id:'delete-2',scope:'session',session_id:'a',deleted:49,retained:1,retained_run_ids:[run(50).id]}
	const release=f.deletion(result),deleting=s.deleteRuns('a')
	f.rows=[run(50)]
	f.emit({type:'event',mode:'delta',data:{id:result.audit_event_id,type:'audit_records_deleted',data:result}})
	f.rows.push(run(51));f.now=51
	await flush();assert.equal(s.getRuns('a').getSnapshot().items.length,2)
	const state=s.getRuns('a').getSnapshot();release();await deleting
	assert.equal(s.getRuns('a').getSnapshot(),state);assert.equal(timers.size,0)
	s.dispose()
})

await check('REST then WebSocket reports are deduplicated and snapshot deletion metadata is not replayed',async()=>{
	const f=fixture(50),s=f.store
	s.setActive(true);await settle();s.setOpen('a',true);await settle()
	const result={audit_event_id:'delete-3',scope:'all',deleted:49,retained:1,retained_run_ids:[run(50).id]}
	f.rows=[run(50)];s.applyDeletion(result);await flush()
	f.rows.push(run(51));f.now=51;await s.refresh()
	const state=s.getRuns('a').getSnapshot()
	f.emit({type:'event',mode:'delta',data:{id:result.audit_event_id,type:'audit_records_deleted',data:result}})
	assert.equal(s.getRuns('a').getSnapshot(),state);assert.equal(timers.size,0)
	f.emit({type:'event',mode:'snapshot',data:{id:'old-delete',type:'audit_records_deleted',data:{scope:'all',deleted:999,retained:0}}})
	assert.equal(s.getRuns('a').getSnapshot().items,state.items)
	await flush();assert.equal(s.getRuns('a').getSnapshot().items.length,2);s.dispose()
})

await check('search reset cancels old group and command requests while preserving disclosure choices',async()=>{
	const f=fixture(100),s=f.store
	s.setActive(true);await settle();s.setOpen('a',true);await settle()
	const hold=f.hold('groups'),pending=s.refresh();await settle()
	s.setQuery('echo 1');await settle()
	const current=s.groups.getSnapshot();hold.release();await pending
	assert.equal(s.groups.getSnapshot(),current);assert(s.getViewSnapshot().expanded.has('a'))
	assert(s.getRuns('a').getSnapshot().items.every(item=>item.request_json.includes('echo 1')))
	s.dispose()
})

await check('group pagination restores open groups as they re-enter the loaded window',async()=>{
	const f=fixture(0),s=f.store
	f.rows=Array.from({length:45},(_,i)=>run(i+1,`session-${i+1}`));f.now=45
	s.setOpen('session-1',true)
	s.setActive(true);await settle();assert.equal(s.groups.getSnapshot().items.length,20)
	await s.loadMoreGroups();assert.equal(s.groups.getSnapshot().items.length,40)
	f.emit({type:'event',mode:'snapshot'});await flush();assert.equal(s.groups.getSnapshot().items.length,40)
	await s.loadMoreGroups();assert.equal(s.groups.getSnapshot().items.length,45)
	assert.equal(s.getRuns('session-1').getSnapshot().items.length,1)
	assert(s.getViewSnapshot().expanded.has('session-1'));s.dispose()
})

await check('events arriving during a refresh schedule one follow-up, not one request per event',async()=>{
	const f=fixture(),s=f.store
	s.setActive(true);await settle()
	const hold=f.hold('groups'),pending=s.refresh();await settle()
	const count=f.calls.length
	for(let i=0;i<100;i++)f.emit({type:'event',mode:'delta'})
	assert.equal(f.calls.length,count);assert.equal(timers.size,0)
	hold.release();await pending;assert.equal(timers.size,1)
	await flush();assert.equal(f.calls.length,count+1);assert.equal(timers.size,0);s.dispose()
})

await check('a changed snapshot fails atomically without committing half of a refreshed window',async()=>{
	const f=fixture(),errors=[]
	let mismatch=false
	const page=new AuditHistoryPage(async(input,signal)=>{
		const result=await f.api.runs('a',input,signal)
		if(mismatch&&input.cursor)result.snapshotAt=stamp(999)
		return result
	},position,100,error=>errors.push(error))
	page.setActive(true);await page.refresh();await page.loadMore()
	const items=page.getSnapshot().items;mismatch=true
	assert.equal(await page.refresh(),false);assert.equal(page.getSnapshot().items,items)
	assert.equal(errors.length,1);assert.equal(page.getSnapshot().loading,'idle')
})

await check('a group moving between refresh pages keeps one correctly ordered row',async()=>{
	const a={id:'a',started_at:stamp(100)},b={id:'b',started_at:stamp(10)},c={id:'c',started_at:stamp(90)}
	let changed=false
	const page=new AuditHistoryPage(async input=>{
		if(!changed)return{items:[a,c,b],snapshotAt:stamp(200),nextCursor:null}
		return input.cursor?{items:[{...a,started_at:stamp(50)},b],snapshotAt:stamp(200),nextCursor:null}:
			{items:[a,c],snapshotAt:stamp(200),nextCursor:position(c)}
	},position,50,()=>{})
	page.setActive(true);await page.refresh();changed=true;await page.refresh()
	assert.deepEqual(page.getSnapshot().items.map(item=>item.id),['c','a','b'])
})

await check('the HTTP client carries search, snapshot, abort signal and empty direct cursor IDs',async()=>{
	const {auditHistoryApi}=await import('../../src/api/auditHistory.ts')
	const originalFetch=globalThis.fetch,calls=[]
	globalThis.fetch=async(path,init)=>{
		calls.push({url:new URL(path,'http://local'),init})
		return new Response(JSON.stringify({groups:[],runs:[],snapshot_at:stamp(10),has_more:false}),{status:200})
	}
	try{
		const controller=new AbortController()
		const input={query:'echo 100% & _',limit:20,snapshotAt:stamp(10),cursor:{started_at:stamp(9),id:''}}
		await auditHistoryApi.groups(input,controller.signal)
		await auditHistoryApi.runs('',input,controller.signal)
		assert.equal(calls[0].url.pathname,'/api/v1/audit/groups')
		assert.equal(calls[1].url.pathname,'/api/v1/audit/runs')
		for(const {url,init} of calls){
			assert.equal(url.searchParams.get('q'),input.query)
			assert.equal(url.searchParams.get('snapshot_at'),stamp(10))
			assert.equal(url.searchParams.get('cursor_started_at'),stamp(9))
			assert(url.searchParams.has('cursor_id'));assert.equal(url.searchParams.get('cursor_id'),'')
			assert.equal(init.signal,controller.signal);assert.equal(init.credentials,'same-origin')
		}
		assert(calls[1].url.searchParams.has('session_id'));assert.equal(calls[1].url.searchParams.get('session_id'),'')
		const {api}=await import('../../src/api/api.ts')
		await api.runDetail('run/with space',controller.signal)
		assert.equal(calls[2].url.pathname,'/api/v1/runs/run%2Fwith%20space')
		assert.equal(calls[2].init.signal,controller.signal)
	}finally{globalThis.fetch=originalFetch}
})

await check('small complete histories open by default but refresh and deletion respect manual collapse',async()=>{
	const f=fixture(3),s=f.store
	s.setActive(true);await settle()
	assert(s.getViewSnapshot().expanded.has('a'));assert.equal(s.getRuns('a').getSnapshot().items.length,3)
	s.setOpen('a',false)
	const calls=f.calls.filter(call=>call.kind==='runs').length
	await s.refresh();assert(!s.getViewSnapshot().expanded.has('a'))
	f.rows=[run(3)]
	s.applyDeletion({scope:'session',session_id:'a',deleted:2,retained:1,retained_run_ids:[run(3).id]});await flush()
	assert(!s.getViewSnapshot().expanded.has('a'))
	assert.equal(f.calls.filter(call=>call.kind==='runs').length,calls)
	s.setActive(false);s.setActive(true);await settle()
	assert(!s.getViewSnapshot().expanded.has('a'));s.dispose()
})

await check('a small first page is not mistaken for the complete history',async()=>{
	const f=fixture(0),s=f.store
	f.rows=Array.from({length:21},(_,i)=>run(i+1,`session-${i+1}`));f.now=21
	s.setActive(true);await settle()
	assert.equal(s.getViewSnapshot().expanded.size,0)
	assert.equal(f.calls.filter(call=>call.kind==='runs').length,0)
	s.setOpen('session-21',true);await settle();s.setOpen('session-21',false)
	await s.loadMoreGroups()
	assert.equal(s.groups.getSnapshot().items.length,21)
	assert(!s.getViewSnapshot().expanded.has('session-21'));s.dispose()
})

await check('closing a group cancels its pending page without resetting other groups',async()=>{
	const f=fixture(150),s=f.store
	f.rows.push(run(151,'b'));f.now=151
	s.setActive(true);await settle();s.setOpen('a',true);s.setOpen('b',true);await settle()
	const other=s.getRuns('b').getSnapshot(),groups=s.groups.getSnapshot()
	const hold=f.hold('runs'),pending=s.getRuns('a').loadMore(),signal=f.calls.at(-1).signal
	s.setOpen('a',false);assert(signal.aborted);hold.release();await pending
	assert.equal(s.getRuns('a').getSnapshot().items.length,50)
	assert.equal(s.getRuns('a').getSnapshot().loading,'idle')
	assert.equal(s.getRuns('b').getSnapshot(),other);assert.equal(s.groups.getSnapshot(),groups)
	s.setOpen('a',true);await settle();await s.getRuns('a').loadMore()
	assert.equal(s.getRuns('a').getSnapshot().items.length,100);s.dispose()
})

assert.equal(timers.size,0)

await check('failed older pages retain their cursor and resume the same page on retry',async()=>{
	const f=fixture(),page=new AuditHistoryPage((input,signal)=>f.api.runs('a',input,signal),position,50,()=>{})
	page.setActive(true);await page.refresh()
	const cursor=page.getSnapshot().nextCursor,items=page.getSnapshot().items
	const hold=f.hold('runs'),pending=page.loadMore()
	hold.reject(new Error('offline'));await pending
	assert.equal(page.getSnapshot().failedOperation,'more')
	assert.equal(page.getSnapshot().nextCursor,cursor);assert.equal(page.getSnapshot().items,items)
	await page.loadMore()
	assert.deepEqual(f.calls.at(-1).input.cursor,cursor);assert.equal(page.getSnapshot().items.length,100)
	assert.equal(page.getSnapshot().failedOperation,null)
	const refreshHold=f.hold('runs'),refresh=page.refresh()
	refreshHold.reject(new Error('offline'));await refresh
	assert.equal(page.getSnapshot().failedOperation,'refresh')
	page.reset('new query');assert.equal(page.getSnapshot().failedOperation,null)
})

await check('an already loaded group moved into an older page updates rather than leaving a stale duplicate',async()=>{
	const a={id:'a',started_at:stamp(100),count:2},b={id:'b',started_at:stamp(90)},c={id:'c',started_at:stamp(70)}
	const page=new AuditHistoryPage(async input=>input.cursor?
		{items:[{...a,started_at:stamp(80),count:1},c],snapshotAt:stamp(200),nextCursor:null}:
		{items:[a,b],snapshotAt:stamp(200),nextCursor:position(b)},position,2,()=>{})
	page.setActive(true);await page.refresh();await page.loadMore()
	assert.deepEqual(page.getSnapshot().items.map(item=>item.id),['b','a','c'])
	assert.equal(page.getSnapshot().items[1].count,1)
})

await check('a large loaded window batches 1000 events, preserves row references and stops at idle',async()=>{
	const f=fixture(3000),s=f.store
	s.setActive(true);await settle();s.setOpen('a',true);await settle()
	for(let i=0;i<19;i++)await s.getRuns('a').loadMore()
	const before=s.getRuns('a').getSnapshot().items,groups=s.groups.getSnapshot().items
	let groupNotifications=0,runNotifications=0
	const unsubscribeGroups=s.groups.subscribe(()=>groupNotifications++)
	const unsubscribeRuns=s.getRuns('a').subscribe(()=>runNotifications++)
	const callCount=f.calls.length
	for(let i=0;i<1000;i++)f.emit({type:'event',mode:'delta'})
	assert.equal(timers.size,1);await flush();await s.getRuns('a').wait();await settle()
	assert.equal(f.calls.length-callCount,21,'one group page and twenty 50-run pages, not 1000 refreshes')
	assert.equal(s.groups.getSnapshot().items,groups);assert.equal(s.getRuns('a').getSnapshot().items,before)
	assert(groupNotifications<=3);assert(runNotifications<=3)
	assert.equal(timers.size,0)
	console.log(`MEASURE 1000 loaded runs / 1000 events: ${f.calls.length-callCount} requests, ${groupNotifications} group notifications, ${runNotifications} run notifications`)
	unsubscribeGroups();unsubscribeRuns();s.dispose()
})

await check('concurrent group load-more calls share hydration instead of cancelling each other',async()=>{
	const f=fixture(0),s=f.store
	f.rows=Array.from({length:45},(_,i)=>run(i+1,`session-${i+1}`));f.now=45
	s.setOpen('session-1',true);s.setActive(true);await settle();await s.loadMoreGroups()
	const hold=f.hold('runs'),first=s.loadMoreGroups(),second=s.loadMoreGroups()
	await settle()
	assert.equal(f.calls.filter(call=>call.kind==='runs'&&call.session==='session-1').length,1)
	hold.release();await first;await second
	assert.equal(s.getRuns('session-1').getSnapshot().items.length,1);s.dispose()
})

await check('search changes cancel old hydration even if the server returns the same snapshot timestamp',async()=>{
	const f=fixture(0),s=f.store
	f.rows=Array.from({length:45},(_,i)=>run(i+1,`session-${i+1}`));f.now=45
	s.setOpen('session-1',true);s.setOpen('session-2',true)
	s.setActive(true);await settle();await s.loadMoreGroups()
	const hold=f.hold('runs'),pending=s.loadMoreGroups();await settle()
	assert(hold.started)
	s.setQuery('echo 2');await settle()
	const count=f.calls.length,state=s.groups.getSnapshot()
	hold.release();await pending;await settle()
	assert.equal(f.calls.length,count,'old hydration must not fetch sessions outside the new query')
	assert.equal(s.groups.getSnapshot(),state);assert.equal(state.snapshotAt,stamp(45))
	s.dispose()
})

assert.equal(timers.size,0)
console.log(`${tests} audit history state scenarios passed; no polling or leftover timers.`)
