import { useCallback, useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api } from '../../api/api'
import { subscribeApplicationEvents } from '../../api/appEvents'
import type { NotificationSink } from '../../lib/notifications'
import { errorText } from '../../lib/utils'
import type { AuditRunDeleteResult, ChatSession, Run } from '../../types'
import { applyAuditRunDeletion, auditSessionID } from './auditRuns'

export type AuditView='runs'|'mcp'

type AuditPageCursor={hasMore:boolean;timestamp:string;id:string}
type AuditStateSnapshot={id?:string;type?:string;data?:AuditRunDeleteResult}

type AuditDataOptions={
	active:boolean
	refreshHosts:()=>Promise<void>
	notify:NotificationSink
}

function mergeLatestPage<T extends{id:string}>(latest:T[],current:T[],hasMore:boolean,timestamp:(item:T)=>string){
	if(!hasMore)return latest
	const tail=latest.at(-1)
	if(!tail)return current
	const tailTime=timestamp(tail)
	const retained=current.filter(item=>{
		const value=timestamp(item)
		return value<tailTime||value===tailTime&&item.id<tail.id
	})
	const seen=new Set(latest.map(item=>item.id))
	return [...latest,...retained.filter(item=>!seen.has(item.id))]
}

export function useAuditData({active,refreshHosts,notify}:AuditDataOptions){
	const {t}=useTranslation()
	const [runs,setRuns]=useState<Run[]>([])
	const [sessions,setSessions]=useState<ChatSession[]>([])
	const [cursor,setCursor]=useState<AuditPageCursor>({hasMore:false,timestamp:'',id:''})
	const [ready,setReady]=useState(false)
	const [error,setError]=useState('')
	const [loadingMore,setLoadingMore]=useState(false)
	const refreshRef=useRef<Promise<void>|null>(null)
	const refreshQueuedRef=useRef(false)
	const initializedRef=useRef(false)
	const extendedRef=useRef(false)

	const reportError=useCallback((cause:unknown)=>{
		const message=errorText(cause)
		setError(message)
		notify(message,'error')
	},[notify])
	const refreshRuns=useCallback(async(reset=false)=>{
		const page=await api.runSummaries()
		setRuns(current=>reset?page.runs:mergeLatestPage(page.runs,current,page.has_more,item=>item.started_at))
		if(reset||!extendedRef.current){
			setCursor({hasMore:page.has_more,timestamp:page.next_started_at||'',id:page.next_id||''})
			extendedRef.current=false
		}
	},[])
	const refreshSessions=useCallback(async()=>setSessions(await api.chatSessions()),[])
	const refresh=useCallback(function refreshAudit():Promise<void>{
		if(refreshRef.current){refreshQueuedRef.current=true;return refreshRef.current}
		refreshQueuedRef.current=false
		setError('')
		const reset=!initializedRef.current
		const task=Promise.all([refreshRuns(reset),refreshHosts(),refreshSessions()])
			.then(()=>{initializedRef.current=true})
			.catch(reportError)
			.finally(()=>{
				setReady(true)
				if(refreshRef.current===task)refreshRef.current=null
				if(refreshQueuedRef.current){refreshQueuedRef.current=false;void refreshAudit()}
			})
		refreshRef.current=task
		return task
	},[refreshHosts,refreshRuns,refreshSessions,reportError])
	const applyDeletion=useCallback((result:AuditRunDeleteResult)=>{
		setRuns(current=>applyAuditRunDeletion(current,result))
		if(result.scope==='all'){
			setCursor({hasMore:false,timestamp:'',id:''})
			extendedRef.current=false
		}
	},[])
	const loadMore=useCallback(async():Promise<string[]>=>{
		if(loadingMore||!cursor.hasMore||!cursor.timestamp||!cursor.id)return[]
		setLoadingMore(true);setError('')
		try{
			const page=await api.runSummaries({cursorStartedAt:cursor.timestamp,cursorID:cursor.id})
			setRuns(current=>{
				const seen=new Set(current.map(item=>item.id))
				return[...current,...page.runs.filter(item=>!seen.has(item.id))]
			})
			extendedRef.current=true
			setCursor({hasMore:page.has_more,timestamp:page.next_started_at||'',id:page.next_id||''})
			return[...new Set(page.runs.map(auditSessionID))]
		}catch(cause){reportError(cause);return[]}
		finally{setLoadingMore(false)}
	},[cursor,loadingMore,reportError])
	const deleteRuns=useCallback(async(sessionID?:string|null):Promise<AuditRunDeleteResult>=>{
		try{
			const result=await api.deleteAuditRuns(sessionID)
			applyDeletion(result)
			notify(result.retained?t('audit.deletedWithRetained',{deleted:result.deleted,retained:result.retained}):t('audit.deleted',{count:result.deleted}))
			return result
		}catch(cause){reportError(cause);throw cause}
	},[applyDeletion,notify,reportError,t])

	useEffect(()=>{if(active)void refresh()},[active,refresh])
	useEffect(()=>{
		if(!active)return
		let initialSnapshot=true
		return subscribeApplicationEvents<AuditStateSnapshot>('audit',event=>{
			if(event.type==='error'){reportError(event.error||'Audit event stream failed');return}
			if(event.type!=='event')return
			if(initialSnapshot&&event.mode==='snapshot'){initialSnapshot=false;return}
			if(event.mode==='snapshot'){void refreshRuns(true).catch(reportError);return}
			if(event.data?.type==='audit_records_deleted'&&event.data.data)applyDeletion(event.data.data)
			else void refresh()
		})
	},[active,applyDeletion,refresh,refreshRuns,reportError])

	return{runs,sessions,ready,error,runsHasMore:cursor.hasMore,loadingMore,refresh,loadMore,deleteRuns}
}
