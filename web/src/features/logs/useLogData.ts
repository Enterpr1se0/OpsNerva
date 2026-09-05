import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from '../../api/api'
import { subscribeApplicationEvents, type ApplicationLogPayload } from '../../api/appEvents'
import { useDocumentVisible } from '../../lib/hooks'
import i18n from '../../lib/i18n'
import { errorText } from '../../lib/utils'
import type { ServerLogEntry } from '../../types'

export type LogRow={id:number;entry:ServerLogEntry}
type LogData={rows:LogRow[];components:string[];minimumLevel:string;file:string}
const logLimit=500
const logRenderInterval=100

export function useLogData(level:string,component:string,query:string,live:boolean){
	const visible=useDocumentVisible()
	const [data,setData]=useState<LogData>({rows:[],components:[],minimumLevel:'debug',file:''})
	const [loading,setLoading]=useState(false)
	const [error,setError]=useState('')
	const nextRowID=useRef(0)
	const reloadRef=useRef<(()=>void)|null>(null)
	const refresh=useCallback(()=>reloadRef.current?.(),[])

	useEffect(()=>{
		if(!visible){setLoading(false);return}
		let disposed=false
		let revision=0
		let timer:number|undefined
		let pending:LogRow[]=[]
		const pendingComponents=new Set<string>()
		let request:AbortController|undefined
		const filters={level,component,q:query,limit:logLimit}
		const rows=(entries:ServerLogEntry[])=>entries.slice(0,logLimit).map(entry=>({id:++nextRowID.current,entry}))
		const clearPending=()=>{
			if(timer!==undefined)window.clearTimeout(timer)
			timer=undefined;pending=[];pendingComponents.clear()
		}
		const snapshot=(value:ApplicationLogPayload)=>{
			clearPending()
			setData({rows:rows(value.entries||[]),components:value.components||[],minimumLevel:value.minimum_level||'debug',file:value.file||''})
			setError('');setLoading(false)
		}
		const load=async()=>{
			request?.abort()
			const controller=new AbortController()
			request=controller
			const startedRevision=revision
			setLoading(true)
			try{
				const value=await api.logs(filters,controller.signal)
				// An arriving stream event is newer than this HTTP snapshot.
				if(!disposed&&!controller.signal.aborted&&revision===startedRevision)snapshot(value)
			}catch(cause){if(!disposed&&!controller.signal.aborted)setError(errorText(cause))}
			finally{if(!disposed&&request===controller)setLoading(false)}
		}
		const flush=()=>{
			const additions=pending
			const components=[...pendingComponents]
			clearPending()
			setData(current=>{
				const added=components.filter(name=>!current.components.includes(name))
				return {...current,rows:[...additions,...current.rows].slice(0,logLimit),components:added.length?[...current.components,...added].sort():current.components}
			})
		}
		reloadRef.current=()=>void load()
		setError('');setLoading(true)
		let unsubscribe: (()=>void)|undefined
		if(!live)void load()
		else unsubscribe=subscribeApplicationEvents<ApplicationLogPayload>('logs',event=>{
			if(event.type==='error'){setError(event.error||i18n.t('logs.streamFailed'));setLoading(false);return}
			if(event.type!=='event'||!event.data)return
			revision++
			if(event.mode==='snapshot'){snapshot(event.data);return}
			const entries=event.data.entries||[]
			if(!entries.length)return
			// Each server batch is already newest first; newer batches precede older ones.
			pending=[...rows(entries),...pending].slice(0,logLimit)
			for(const entry of entries)if(entry.component)pendingComponents.add(entry.component)
			setError('');setLoading(false)
			if(timer===undefined)timer=window.setTimeout(flush,logRenderInterval)
		},{logs:filters})
		return()=>{
			disposed=true;reloadRef.current=null
			request?.abort();unsubscribe?.();clearPending()
		}
	},[level,component,query,live,visible])

	return {...data,loading,error,refresh}
}
