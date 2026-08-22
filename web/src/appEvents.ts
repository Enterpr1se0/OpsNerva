import type { ServerLogResponse } from './types'

export type ApplicationEventTopic='connections'|'approvals'|'sessions'|'chat_state'|'audit'|'mcp_activity'|'health'|'logs'
export type ApplicationLogSubscription={level?:string;component?:string;q?:string;limit?:number}
export type ApplicationEventSubscription={logs?:ApplicationLogSubscription;sessionId?:string;mcpSessionId?:string}
export type ApplicationEvent<T=unknown>={type:'event'|'error'|'heartbeat';topic?:ApplicationEventTopic;mode?:'snapshot'|'delta';sequence?:number;data?:T;error?:string}

type ApplicationEventListener=(event:ApplicationEvent)=>void
type ListenerRegistration={listener:ApplicationEventListener;options?:ApplicationEventSubscription}

class ApplicationEventClient{
	private listeners=new Map<ApplicationEventTopic,Map<symbol,ListenerRegistration>>()
	private socket:WebSocket|null=null
	private reconnectTimer:number|undefined
	private reconnectDelay=1000
	private lastSequence=0

	subscribe<T>(topic:ApplicationEventTopic,listener:(event:ApplicationEvent<T>)=>void,options?:ApplicationEventSubscription){
		const id=Symbol(topic)
		const registrations=this.listeners.get(topic)||new Map<symbol,ListenerRegistration>()
		registrations.set(id,{listener:listener as ApplicationEventListener,options})
		this.listeners.set(topic,registrations)
		this.ensureConnected()
		this.sendSubscription()
		return()=>{
			const current=this.listeners.get(topic)
			current?.delete(id)
			if(!current?.size)this.listeners.delete(topic)
			if(this.listeners.size)this.sendSubscription()
			else this.disconnect()
		}
	}

	private ensureConnected(){
		if(this.socket&&this.socket.readyState<=WebSocket.OPEN)return
		if(this.reconnectTimer!==undefined)return
		this.connect()
	}

	private connect(){
		if(!this.listeners.size)return
		const protocol=window.location.protocol==='https:'?'wss:':'ws:'
		const socket=new WebSocket(`${protocol}//${window.location.host}/api/v1/events/ws`)
		this.socket=socket
		this.lastSequence=0
		socket.onopen=()=>{this.reconnectDelay=1000;this.sendSubscription()}
		socket.onmessage=message=>{
			try{
				const event=JSON.parse(String(message.data)) as ApplicationEvent
				if(event.sequence&&event.sequence<=this.lastSequence)return
				if(event.sequence)this.lastSequence=event.sequence
				if(!event.topic)return
				for(const registration of this.listeners.get(event.topic)?.values()||[])registration.listener(event)
			}catch{/* reconnect supplies fresh snapshots after malformed messages or transport loss */}
		}
		socket.onclose=()=>{
			if(this.socket===socket)this.socket=null
			if(this.listeners.size&&this.reconnectTimer===undefined){
				const delay=document.visibilityState==='visible'?this.reconnectDelay:Math.max(5000,this.reconnectDelay)
				this.reconnectDelay=Math.min(15_000,this.reconnectDelay*2)
				this.reconnectTimer=window.setTimeout(()=>{this.reconnectTimer=undefined;this.connect()},delay)
			}
		}
	}

	private sendSubscription(){
		if(this.socket?.readyState!==WebSocket.OPEN)return
		const topics=Array.from(this.listeners.keys()).sort()
		const logs=Array.from(this.listeners.get('logs')?.values()||[]).at(-1)?.options?.logs
		const sessionId=Array.from(this.listeners.get('chat_state')?.values()||[]).at(-1)?.options?.sessionId
		const mcpSessionId=Array.from(this.listeners.get('mcp_activity')?.values()||[]).at(-1)?.options?.mcpSessionId
		this.socket.send(JSON.stringify({type:'subscribe',topics,logs,session_id:sessionId,mcp_session_id:mcpSessionId}))
	}

	private disconnect(){
		if(this.reconnectTimer!==undefined){window.clearTimeout(this.reconnectTimer);this.reconnectTimer=undefined}
		this.socket?.close()
		this.socket=null
		this.lastSequence=0
		this.reconnectDelay=1000
	}
}

const applicationEventClient=new ApplicationEventClient()

export function subscribeApplicationEvents<T>(topic:ApplicationEventTopic,listener:(event:ApplicationEvent<T>)=>void,options?:ApplicationEventSubscription){
	return applicationEventClient.subscribe(topic,listener,options)
}

export type ApplicationLogEvent=ApplicationEvent<ServerLogResponse>
