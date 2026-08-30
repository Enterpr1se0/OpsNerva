import { useEffect, useMemo, useRef, useState } from 'react'
import { subscribeApplicationEvents } from '../api/appEvents'

type JsonRecord=Record<string,unknown>

export type LiveSSHTaskSnapshot={
	readonly id:string
	readonly revision:number
	readonly task?:JsonRecord
	readonly result?:JsonRecord
	readonly error?:string
}

export type LiveSSHTaskTarget={
	readonly entryID:string
	readonly taskID:string
	readonly status:string
}

type LiveSSHTaskEvent={
	type?:string
	task_id?:string
	revision?:number
	snapshot?:{task?:JsonRecord;result?:JsonRecord;error?:string}
	stream?:string
	offset_bytes?:number
	total_bytes?:number
	content?:string
}

type LiveSSHTaskState={sessionID:string;snapshots:ReadonlyMap<string,LiveSSHTaskSnapshot>}

const liveTaskOutputBufferChars=128<<10
const liveTaskRenderInterval=80
const activeTaskStatuses=new Set(['in_progress','running','approval_required','waiting_for_approval'])
const emptySnapshots:ReadonlyMap<string,LiveSSHTaskSnapshot>=new Map()
const textEncoder=new TextEncoder()

function record(value:unknown):JsonRecord|undefined{
	return value!==null&&typeof value==='object'&&!Array.isArray(value)?value as JsonRecord:undefined
}

function text(value:unknown){return typeof value==='string'?value:''}
function number(value:unknown){return typeof value==='number'&&Number.isFinite(value)?value:0}

export function activeLiveTaskStatus(status:string){return activeTaskStatuses.has(status)}

function snapshotStatus(snapshot:LiveSSHTaskSnapshot|undefined){
	if(!snapshot)return''
	if(snapshot.error&&!text(snapshot.task?.id))return'failed'
	return text(snapshot.task?.status)||text(snapshot.result?.status)
}

function liveTaskSnapshot(taskID:string,value:JsonRecord|undefined):LiveSSHTaskSnapshot|undefined{
	if(!value)return undefined
	const task=record(value.task)
	return{id:taskID,revision:number(task?.revision),task,result:record(value.result),error:text(value.error)}
}

function appendLiveTaskOutput(snapshot:LiveSSHTaskSnapshot,event:LiveSSHTaskEvent){
	const stream=event.stream==='stderr'?'stderr':'stdout'
	const result={...(snapshot.result||{})}
	const totalKey=stream==='stderr'?'stderr_total_bytes':'stdout_total_bytes'
	const offsetKey=stream==='stderr'?'stderr_offset_bytes':'stdout_offset_bytes'
	const omittedKey=stream==='stderr'?'stderr_omitted_bytes':'stdout_omitted_bytes'
	if(number(event.offset_bytes)!==number(result[totalKey]))return snapshot
	let content=text(result[stream])+(event.content||'')
	let omitted=number(result[offsetKey])
	if(content.length>liveTaskOutputBufferChars){
		let start=content.length-liveTaskOutputBufferChars
		const code=content.charCodeAt(start)
		if(code>=0xDC00&&code<=0xDFFF)start++
		omitted+=textEncoder.encode(content.slice(0,start)).byteLength
		content=content.slice(start)
	}
	const revision=number(event.revision)
	result[stream]=content
	result[totalKey]=number(event.total_bytes)
	result[offsetKey]=omitted
	result[omittedKey]=omitted
	if(omitted>0){result.output_limited=true;result.output_view='tail'}
	return{...snapshot,revision,task:{...(snapshot.task||{}),revision},result}
}

function applyLiveTaskEvents(current:ReadonlyMap<string,LiveSSHTaskSnapshot>,events:readonly LiveSSHTaskEvent[]){
	let next:Map<string,LiveSSHTaskSnapshot>|undefined
	const get=(taskID:string)=>next?.get(taskID)||current.get(taskID)
	const set=(taskID:string,snapshot:LiveSSHTaskSnapshot)=>{
		if(!next)next=new Map(current)
		next.set(taskID,snapshot)
	}
	for(const event of events){
		const taskID=event.task_id||''
		if(!taskID)continue
		if(event.type==='status'){
			const snapshot=liveTaskSnapshot(taskID,record(event.snapshot))
			const previous=get(taskID)
			if(snapshot&&(!previous||snapshot.revision>=previous.revision))set(taskID,snapshot)
			continue
		}
		if(event.type!=='output')continue
		const snapshot=get(taskID)
		const revision=number(event.revision)
		if(!snapshot||revision!==snapshot.revision+1)continue
		set(taskID,appendLiveTaskOutput(snapshot,event))
	}
	return next||current
}

export function useLiveSSHTasks(visible:boolean,sessionID:string,targets:readonly LiveSSHTaskTarget[]){
	const [state,setState]=useState<LiveSSHTaskState>({sessionID:'',snapshots:emptySnapshots})
	const pendingEvents=useRef<LiveSSHTaskEvent[]>([])
	const flushTimer=useRef<number|undefined>(undefined)
	const snapshots=state.sessionID===sessionID?state.snapshots:emptySnapshots
	const targetTaskIDs=useMemo(()=>[...new Set(targets.map(target=>target.taskID))].sort(),[targets])
	const targetTaskIDsKey=targetTaskIDs.join('\0')
	const watchingTaskIDs=useMemo(()=>targetTaskIDs.filter(taskID=>{
		const target=targets.find(candidate=>candidate.taskID===taskID)
		return!!target&&activeLiveTaskStatus(snapshotStatus(snapshots.get(taskID))||target.status)
	}),[snapshots,targetTaskIDsKey,targets])
	const watchingTaskIDsKey=watchingTaskIDs.join('\0')

	useEffect(()=>{
		const allowed=new Set(targetTaskIDs)
		setState(current=>{
			if(current.sessionID!==sessionID)return{sessionID,snapshots:emptySnapshots}
			let changed=false
			const next=new Map<string,LiveSSHTaskSnapshot>()
			for(const[taskID,snapshot]of current.snapshots){if(allowed.has(taskID))next.set(taskID,snapshot);else changed=true}
			return changed?{sessionID,snapshots:next}:current
		})
	},[sessionID,targetTaskIDsKey])

	useEffect(()=>{
		if(flushTimer.current!==undefined){window.clearTimeout(flushTimer.current);flushTimer.current=undefined}
		pendingEvents.current=[]
		if(!visible||!sessionID||!watchingTaskIDs.length)return
		const watched=new Set(watchingTaskIDs)
		const flush=()=>{
			if(flushTimer.current!==undefined)window.clearTimeout(flushTimer.current)
			flushTimer.current=undefined
			const events=pendingEvents.current
			pendingEvents.current=[]
			if(events.length)setState(current=>{
				const source=current.sessionID===sessionID?current.snapshots:emptySnapshots
				const next=applyLiveTaskEvents(source,events)
				return next===source&&current.sessionID===sessionID?current:{sessionID,snapshots:next}
			})
		}
		const unsubscribe=subscribeApplicationEvents<JsonRecord|LiveSSHTaskEvent>('tasks',event=>{
			if(event.type!=='event'||!event.data)return
			if(event.mode==='snapshot'){
				pendingEvents.current=[]
				if(flushTimer.current!==undefined){window.clearTimeout(flushTimer.current);flushTimer.current=undefined}
				const payload=record(event.data)
				setState(current=>{
					const next=new Map(current.sessionID===sessionID?current.snapshots:emptySnapshots)
					for(const taskID of watchingTaskIDs){
						const snapshot=liveTaskSnapshot(taskID,record(payload?.[taskID]))
						if(snapshot)next.set(taskID,snapshot);else next.delete(taskID)
					}
					return{sessionID,snapshots:next}
				})
				return
			}
			const update=event.data as LiveSSHTaskEvent
			if(!update.task_id||!watched.has(update.task_id))return
			pendingEvents.current.push(update)
			if(update.type==='status')flush()
			else if(flushTimer.current===undefined)flushTimer.current=window.setTimeout(flush,liveTaskRenderInterval)
		},{sessionId:sessionID,taskIds:watchingTaskIDs})
		return()=>{
			unsubscribe()
			if(flushTimer.current!==undefined){window.clearTimeout(flushTimer.current);flushTimer.current=undefined}
			pendingEvents.current=[]
		}
	},[sessionID,visible,watchingTaskIDsKey])

	return snapshots
}
