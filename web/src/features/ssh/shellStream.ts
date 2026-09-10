import { sshShellWebSocketURL } from '../../api/api'
import type { SSHShell, SSHShellEvent } from '../../types'
import { sshShellActive } from './utils'

export type ShellStreamState='connecting'|'connected'|'disconnected'|'ended'
export type ShellStream=ReturnType<typeof openShellStream>

export function openShellStream(id:string,callbacks:{
	onState:(state:ShellStreamState)=>void
	onEvent:(event:SSHShellEvent)=>void
	onShell:(shell:SSHShell)=>void
	onOutput:(data:Uint8Array)=>void
	onError:(error:string)=>void
	onUnavailable:()=>void
}){
	let socket:WebSocket|null=null
	let ready=false
	let timer:number|undefined
	let sequence=0
	let attempts=0
	let ended=false
	let disposed=false
	const clearRetry=()=>{if(timer!==undefined)window.clearTimeout(timer);timer=undefined}
	const disconnect=()=>{const previous=socket;socket=null;ready=false;previous?.close()}
	const finish=()=>{ended=true;clearRetry();disconnect();callbacks.onState('ended')}
	const connect=()=>{
		clearRetry()
		if(disposed||ended||document.hidden)return
		disconnect()
		callbacks.onState('connecting')
		const current=new WebSocket(sshShellWebSocketURL(id,sequence))
		socket=current
		current.binaryType='arraybuffer'
		current.onmessage=message=>{
			if(socket!==current)return
			if(message.data instanceof ArrayBuffer){
				if(message.data.byteLength<10)return
				const view=new DataView(message.data)
				if(view.getUint8(0)!==1)return
				const nextSequence=Number(view.getBigUint64(2))
				if(nextSequence<=sequence)return
				sequence=nextSequence
				callbacks.onOutput(new Uint8Array(message.data,10))
				return
			}
			let payload:{type:string;event?:SSHShellEvent;shell?:SSHShell;error?:string}
			try{payload=JSON.parse(String(message.data))}catch{return}
			if(payload.type==='ready'){
				ready=true
				if(payload.shell)callbacks.onShell(payload.shell)
				attempts=0;callbacks.onState('connected')
			}else if(payload.type==='ended'){
				if(payload.shell)callbacks.onShell(payload.shell)
				finish()
			}else if(payload.type==='unavailable'){
				finish();callbacks.onUnavailable()
			}else if(payload.type==='event'&&payload.event){
				const event=payload.event
				if(event.sequence<=sequence)return
				sequence=event.sequence
				if(event.stream==='status'&&!sshShellActive(event.status||''))finish()
				callbacks.onEvent(event)
			}else if(payload.type==='error'&&payload.error)callbacks.onError(payload.error)
		}
		current.onclose=()=>{
			if(socket!==current)return
			socket=null;ready=false
			if(disposed||ended)return
			callbacks.onState('disconnected')
			if(!document.hidden)timer=window.setTimeout(connect,Math.min(30_000,1000*2**Math.min(attempts++,5)))
		}
	}
	const visibilityChanged=()=>{
		if(document.hidden)clearRetry()
		else if(!socket)connect()
	}
	document.addEventListener('visibilitychange',visibilityChanged)
	connect()
	return{
		send:(command:Record<string,unknown>)=>{
			if(disposed||ended||!ready||socket?.readyState!==WebSocket.OPEN)return false
			socket.send(JSON.stringify(command));return true
		},
		retry:()=>{
			if(disposed)return
			ended=false;attempts=0
			if(socket?.readyState===WebSocket.OPEN||socket?.readyState===WebSocket.CONNECTING)return
			connect()
		},
		close:()=>{disposed=true;clearRetry();disconnect();document.removeEventListener('visibilitychange',visibilityChanged)},
	}
}
