import assert from 'node:assert/strict'
import {register} from 'node:module'

register(new URL('./typescript-loader.mjs',import.meta.url))

const listeners=new Map()
globalThis.document={
	hidden:false,
	addEventListener:(name,listener)=>listeners.set(name,listener),
	removeEventListener:(name,listener)=>{if(listeners.get(name)===listener)listeners.delete(name)},
}
globalThis.window={
	location:{protocol:'http:',host:'opsnerva.test'},
	setTimeout:globalThis.setTimeout.bind(globalThis),
	clearTimeout:globalThis.clearTimeout.bind(globalThis),
}

const sockets=[]
class MockWebSocket{
	static CONNECTING=0
	static OPEN=1
	static CLOSED=3
	constructor(url){this.url=url;this.readyState=MockWebSocket.CONNECTING;sockets.push(this)}
	close(){this.readyState=MockWebSocket.CLOSED;this.onclose?.()}
	send(){}
}
globalThis.WebSocket=MockWebSocket

const shell={
	id:'shell-original',run_id:'',session_id:'',kind:'ssh',surface:'quick',host_id:'host-one',host_name:'one',user:'ops',elevated:false,
	status:'failed',cols:120,rows:32,last_sequence:5,termination_reason:'connection_lost',started_at:'2026-09-09T00:00:00Z',
}
const fetchCalls=[]
globalThis.fetch=async(path,init)=>{
	fetchCalls.push({path,init})
	return{ok:true,status:200,json:async()=>({...shell,status:'running',termination_reason:undefined})}
}

const {reconnectOperatorShell,sshShellCanReconnect}=await import('../../src/features/ssh/shellState.ts')
assert.equal(sshShellCanReconnect(shell),true)
assert.equal(sshShellCanReconnect({...shell,status:'running'}),false)
assert.equal(sshShellCanReconnect({...shell,termination_reason:'remote_exit'}),false)
const firstReconnect=reconnectOperatorShell(shell)
const duplicateReconnect=reconnectOperatorShell(shell)
assert.equal(firstReconnect,duplicateReconnect,'concurrent retries must share one request')
const [reconnected]=await Promise.all([firstReconnect,duplicateReconnect])
assert.equal(reconnected.id,shell.id)
assert.equal(fetchCalls.length,1)
assert.equal(fetchCalls[0].path,'/api/v1/ssh-shells/shell-original/reconnect')
assert.equal(fetchCalls[0].init.method,'POST')

const states=[]
const {openShellStream}=await import('../../src/features/ssh/shellStream.ts')
const stream=openShellStream(shell.id,{
	onState:state=>states.push(state),onEvent:()=>{},onShell:()=>{},onOutput:()=>{},onError:error=>assert.fail(error),onUnavailable:()=>{},
})
assert.equal(sockets.length,1)
const originalSocket=sockets[0]
originalSocket.readyState=MockWebSocket.OPEN
originalSocket.onmessage({data:JSON.stringify({type:'ready',shell:{...shell,status:'running'}})})
const frame=new ArrayBuffer(11)
const view=new DataView(frame);view.setUint8(0,1);view.setBigUint64(2,5n);view.setUint8(10,65)
originalSocket.onmessage({data:frame})
originalSocket.onmessage({data:JSON.stringify({type:'ended',shell})})
assert.equal(states.at(-1),'ended')
stream.retry()
assert.equal(sockets.length,2,'an ended browser stream must be attachable again')
assert.match(sockets[1].url,/\/api\/v1\/ssh-shells\/shell-original\/ws\?after=5$/)
assert.equal(fetchCalls.length,1,'WebSocket reattachment must not create or reconnect another shell')
stream.close()

console.log('PASS shell reconnect keeps one logical ID and reattaches its existing stream cursor')
