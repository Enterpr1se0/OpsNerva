import type { ActiveFileTransfer, FileTransferRecord, FileTransferState } from './types'

const emptyRecord:FileTransferRecord={active:null,conflict:null,uploadVersion:0}
const emptyState:FileTransferState={operation:null,uploadVersion:0}
const progressInterval=200
type Listeners=Map<string,Set<()=>void>>

export class FileTransferStore {
	private records=new Map<string,FileTransferRecord>()
	private states=new Map<string,FileTransferState>()
	private progressSnapshots=new Map<string,ActiveFileTransfer|null>()
	private stateListeners:Listeners=new Map()
	private progressListeners:Listeners=new Map()
	private pending=new Set<string>()
	private timer:ReturnType<typeof setTimeout>|undefined

	record=(key:string)=>this.records.get(key)||emptyRecord
	state=(key:string)=>this.states.get(key)||emptyState
	progress=(key:string)=>this.progressSnapshots.get(key)||null
	private subscribe(listeners:Listeners,key:string,listener:()=>void){
		const current=listeners.get(key)||new Set()
		listeners.set(key,current);current.add(listener)
		return()=>{current.delete(listener);if(!current.size)listeners.delete(key)}
	}
	subscribeState=(key:string,listener:()=>void)=>this.subscribe(this.stateListeners,key,listener)
	subscribeProgress=(key:string,listener:()=>void)=>{
		// Reopening a view reads the latest bytes, even if hidden for the whole transfer.
		if(!this.progressListeners.has(key))this.progressSnapshots.set(key,this.record(key).active)
		const unsubscribe=this.subscribe(this.progressListeners,key,listener)
		return()=>{
			unsubscribe()
			if(!this.progressListeners.has(key))this.pending.delete(key)
			if(!this.pending.size)this.stopTimer()
		}
	}
	private notify(listeners:Listeners,key:string){for(const listener of listeners.get(key)||[])listener()}
	private stopTimer(){if(this.timer!==undefined)clearTimeout(this.timer);this.timer=undefined}
	private publishProgress(key:string){
		this.progressSnapshots.set(key,this.record(key).active)
		this.notify(this.progressListeners,key)
	}
	set(key:string,record:FileTransferRecord){
		const previous=this.record(key),state=this.state(key)
		this.records.set(key,record)
		const operation=record.active?.operation||null
		if(operation!==state.operation||record.uploadVersion!==state.uploadVersion){
			this.states.set(key,{operation,uploadVersion:record.uploadVersion})
			this.notify(this.stateListeners,key)
		}
		if(!record.active||!previous.active){
			this.pending.delete(key)
			if(!this.pending.size)this.stopTimer()
			this.publishProgress(key)
		}else if(record.active!==previous.active&&this.progressListeners.has(key)){
			this.pending.add(key)
			if(this.timer===undefined)this.timer=setTimeout(()=>{
				this.timer=undefined
				const keys=[...this.pending];this.pending.clear()
				for(const current of keys)this.publishProgress(current)
			},progressInterval)
		}
	}
	dispose(){this.stopTimer();this.pending.clear();this.stateListeners.clear();this.progressListeners.clear()}
}
