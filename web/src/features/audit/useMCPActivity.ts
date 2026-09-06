import { useCallback, useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api } from '../../api/api'
import { subscribeApplicationEvents } from '../../api/appEvents'
import { errorText } from '../../lib/utils'
import type { MCPActivityEvent, MCPActivitySnapshot, MCPClientSession, MCPToolCall } from '../../types'

export type MCPCallOutput={stdout:string;stderr:string;transferredBytes:number;totalBytes:number}
const mcpLiveOutputChars=64<<10

function appendMCPOutput(current:string,content:string){
	const next=current+content
	return next.length<=mcpLiveOutputChars?next:next.slice(-mcpLiveOutputChars)
}

export function useMCPActivity(refreshKey:number){
	const {t}=useTranslation()
	const [sessions,setSessions]=useState<MCPClientSession[]>([])
	const [selectedID,setSelectedID]=useState('')
	const [calls,setCalls]=useState<MCPToolCall[]>([])
	const [outputs,setOutputs]=useState<Record<string,MCPCallOutput>>({})
	const [callsSessionID,setCallsSessionID]=useState(selectedID)
	if(callsSessionID!==selectedID){setCallsSessionID(selectedID);setCalls([]);setOutputs({})}
	const [loading,setLoading]=useState(true)
	const [error,setError]=useState('')
	const selectedIDRef=useRef('')
	const pendingEventsRef=useRef<MCPActivityEvent[]>([])
	const eventFrameRef=useRef(0)
	useEffect(()=>{selectedIDRef.current=selectedID},[selectedID])
	useEffect(()=>{
		let active=true
		api.mcpActivity().then(snapshot=>{if(!active)return;setSessions(snapshot.sessions||[]);setSelectedID(current=>current&&snapshot.sessions.some(session=>session.id===current)?current:snapshot.sessions[0]?.id||'');setError('')}).catch(err=>{if(active)setError(errorText(err))}).finally(()=>{if(active)setLoading(false)})
		return()=>{active=false}
	},[refreshKey])
	useEffect(()=>{
		if(!selectedID)return
		let active=true
		api.mcpActivity(selectedID).then(snapshot=>{if(active)setCalls(snapshot.calls||[])}).catch(err=>{if(active)setError(errorText(err))})
		return()=>{active=false}
	},[selectedID])
	const flushEvents=useCallback(()=>{
		eventFrameRef.current=0
		const events=pendingEventsRef.current.splice(0)
		if(!events.length)return
		setSessions(current=>{
			const next=[...current]
			for(const event of events){
				const index=next.findIndex(session=>session.id===event.session_id)
				if(event.type==='call_started'){
					const existing=index>=0?next[index]:undefined
					const session={...(event.session||existing||{id:event.session_id,transport:'',started_at:new Date().toISOString(),last_seen_at:new Date().toISOString(),call_count:0,running_calls:0}),call_count:(existing?.call_count||0)+1,running_calls:(existing?.running_calls||0)+1,last_seen_at:event.call?.started_at||new Date().toISOString()}
					if(index>=0)next[index]=session;else next.push(session)
				}else if(event.type==='call_finished'&&index>=0){
					next[index]={...next[index],running_calls:Math.max(0,next[index].running_calls-1),last_seen_at:event.call?.updated_at||next[index].last_seen_at}
				}
			}
			next.sort((a,b)=>Date.parse(b.last_seen_at)-Date.parse(a.last_seen_at))
			return next
		})
		setCalls(current=>{
			const next=[...current]
			for(const event of events){
				if(event.session_id!==selectedIDRef.current)continue
				const index=next.findIndex(call=>call.id===event.call_id)
				if(event.call){if(index>=0)next[index]=event.call;else next.unshift(event.call)}
				else if(index>=0&&(event.status||event.run_id))next[index]={...next[index],operation_status:event.status||next[index].operation_status,run_id:event.run_id||next[index].run_id,updated_at:new Date().toISOString()}
			}
			return next.sort((a,b)=>Date.parse(b.started_at)-Date.parse(a.started_at))
		})
		setOutputs(current=>{
			let next=current
			for(const event of events){
				if(event.session_id!==selectedIDRef.current||(!event.content&&event.type!=='call_progress'))continue
				if(next===current)next={...current}
				const output=next[event.call_id]||{stdout:'',stderr:'',transferredBytes:0,totalBytes:0}
				next[event.call_id]={...output,
					stdout:event.stream!=='stderr'&&event.content?appendMCPOutput(output.stdout,event.content):output.stdout,
					stderr:event.stream==='stderr'&&event.content?appendMCPOutput(output.stderr,event.content):output.stderr,
					transferredBytes:event.transferred_bytes??output.transferredBytes,totalBytes:event.total_bytes??output.totalBytes}
			}
			return next
		})
	},[])
	useEffect(()=>{
		const unsubscribe=subscribeApplicationEvents<MCPActivitySnapshot|MCPActivityEvent>('mcp_activity',event=>{
			if(event.type==='error'){setError(event.error||t('audit.mcpStreamFailed'));return}
			if(event.type!=='event'||!event.data)return
			if(event.mode==='snapshot'){const snapshot=event.data as MCPActivitySnapshot;setSessions(snapshot.sessions||[]);setSelectedID(current=>current&&snapshot.sessions.some(session=>session.id===current)?current:snapshot.sessions[0]?.id||'');return}
			const activity=event.data as MCPActivityEvent
			pendingEventsRef.current.push(activity);if(!eventFrameRef.current)eventFrameRef.current=window.requestAnimationFrame(flushEvents)
		})
		return()=>{unsubscribe();if(eventFrameRef.current)window.cancelAnimationFrame(eventFrameRef.current);eventFrameRef.current=0;pendingEventsRef.current=[]}
	},[flushEvents,t])

	return{sessions,selectedID,setSelectedID,calls,outputs,loading,error}
}
