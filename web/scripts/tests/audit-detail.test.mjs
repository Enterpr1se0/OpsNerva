import assert from 'node:assert/strict'
import {register} from 'node:module'

register(new URL('./typescript-loader.mjs',import.meta.url))
const {AuditRunDetailResource}=await import('../../src/features/audit/auditRunDetailResource.ts')
const settle=async()=>{for(let i=0;i<20;i++)await Promise.resolve()}
const summary={id:'run-1',status:'running'}
const calls=[]
const resource=new AuditRunDetailResource((id,signal)=>new Promise((resolve,reject)=>calls.push({id,signal,resolve,reject})))
resource.setInput(summary,false);assert.equal(calls.length,0)
resource.setInput(summary,true);assert.equal(calls.length,1);assert(resource.getSnapshot().loading)
resource.setInput(summary,true);assert.equal(calls.length,1,'do not duplicate pending detail requests')
resource.setInput(summary,false);assert(calls[0].signal.aborted);assert(!resource.getSnapshot().loading)
calls[0].resolve({run:{...summary,stdout_redacted:'stale'}});await settle()
assert.equal(resource.getSnapshot().run,null,'ignore late success after closing or hiding')

resource.setInput(summary,true)
calls[1].resolve({run:{...summary,stdout_redacted:'partial'}});await settle()
assert.equal(resource.getSnapshot().run.stdout_redacted,'partial')
resource.setInput(summary,false);resource.setInput(summary,true)
assert.equal(calls.length,2,'unchanged cached details reopen without fetching')
const complete={...summary,status:'completed',completed_at:'2026-01-01T00:00:01Z'}
resource.setInput(complete,true)
assert.equal(resource.getSnapshot().run,null,'a changed summary invalidates stale output')
assert.equal(calls.length,3);calls[2].resolve({run:{...complete,stdout_redacted:'finished'}});await settle()
assert.equal(resource.getSnapshot().run.stdout_redacted,'finished')

resource.setInput({...complete,id:'run-2'},true)
resource.cancel();calls[3].reject(new Error('late error'));await settle()
assert(!resource.getSnapshot().loading);assert.equal(resource.getSnapshot().error,null)
const next={...complete,id:'run-3'}
resource.setInput(next,true);calls[4].reject(new Error('offline'));await settle()
assert(!resource.getSnapshot().loading);assert.equal(resource.getSnapshot().error.message,'offline')
resource.setInput(next,true);assert.equal(calls.length,5,'no automatic retry loop after an error')
const retry=resource.retry();calls[5].resolve({run:next});await retry
assert.equal(resource.getSnapshot().run.id,'run-3');assert.equal(resource.getSnapshot().error,null)

resource.setInput({...next,id:'run-4'},true)
resource.setInput({...next,id:'run-5'},true)
assert(calls[6].signal.aborted)
calls[7].resolve({run:{...next,id:'run-5'}});await settle()
calls[6].resolve({run:{...next,id:'run-4'}});await settle()
assert.equal(resource.getSnapshot().run.id,'run-5','an earlier source cannot replace the new run detail')
resource.cancel()
console.log('PASS lazy audit details, summary invalidation, close/hide cancellation, late success/error rejection, cache reuse and explicit retry')
