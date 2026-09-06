import { useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Activity, ChevronRight, History, LoaderCircle, Search, Trash2 } from 'lucide-react'
import { DestructiveConfirmDialog } from '../../components/DestructiveConfirmDialog'
import { Empty } from '../../components/PageLayout'
import { localeFor } from '../../lib/i18n'
import type { AuditRunDeleteResult, ChatSession, Host, Run } from '../../types'
import { requestFromRun } from '../tools/request'
import { auditSessionID, directAuditSessionID } from './auditRuns'
import { AuditRunRow } from './AuditRunRow'
import { MCPActivityView } from './MCPActivityView'
import type { AuditView } from './useAuditData'
import { useAuditGroupDisclosure } from './useAuditGroupDisclosure'

type AuditDeleteTarget={kind:'session';id:string;title:string}|{kind:'all'}

type AuditRunsViewProps={
	runs:Run[]
	hosts:Host[]
	sessions:ChatSession[]
	ready:boolean
	error:string
	runsHasMore:boolean
	loadingMore:boolean
	onLoadMoreRuns:()=>Promise<string[]>
	onDeleteRuns:(sessionID?:string|null)=>Promise<AuditRunDeleteResult>
}

type AuditPageProps=AuditRunsViewProps&{
	view:AuditView
	onViewChange:(view:AuditView)=>void
	mcpRefreshKey:number
}

export function AuditPage({view,onViewChange,mcpRefreshKey,...props}:AuditPageProps){
	const {t}=useTranslation()
	return <div className="audit-page page-stack">
		<div className="audit-view-tabs" role="tablist" aria-label={t('audit.views')}>
			<button type="button" role="tab" aria-selected={view==='runs'} className={view==='runs'?'active':''} onClick={()=>onViewChange('runs')}><History size={15}/>{t('audit.runHistory')}</button>
			<button type="button" role="tab" aria-selected={view==='mcp'} className={view==='mcp'?'active':''} onClick={()=>onViewChange('mcp')}><Activity size={15}/>{t('audit.mcpActivity')}</button>
		</div>
		{view==='runs'?<AuditRunsView {...props}/>:<MCPActivityView hosts={props.hosts} refreshKey={mcpRefreshKey}/>}
	</div>
}

function AuditRunsView({runs,hosts,sessions,ready,error,runsHasMore,loadingMore,onLoadMoreRuns,onDeleteRuns}:AuditRunsViewProps) {
	const {t,i18n:instance}=useTranslation()
	const [query,setQuery]=useState('')
	const [deleteTarget,setDeleteTarget]=useState<AuditDeleteTarget|null>(null)
	const [deleting,setDeleting]=useState(false)
	const filtered=useMemo(()=>{
		const needle=query.toLowerCase()
		return runs.filter(run=>{
			const req=requestFromRun(run)
			const requestText=req?Object.values(req).flat().filter(value=>typeof value==='string').join('\n'):run.request_json
			return requestText.toLowerCase().includes(needle)
		})
	},[query,runs])
	const groups=useMemo(()=>{
		const titles=new Map(sessions.map(session=>[session.id,session.title]))
		const grouped=new Map<string,Run[]>()
		for(const run of filtered){const key=auditSessionID(run),items=grouped.get(key);if(items)items.push(run);else grouped.set(key,[run])}
		return [...grouped.entries()].map(([id,items])=>{
			items.sort((a,b)=>Date.parse(b.started_at)-Date.parse(a.started_at))
			return{id,title:id===directAuditSessionID?t('audit.direct'):id.startsWith('mcp_sess_')?t('audit.mcpRunGroup'):titles.get(id)||t('audit.missingConversation'),runs:items,latest:items[0]?.started_at,pending:items.filter(run=>run.status==='approval_required').length}
		}).sort((a,b)=>Date.parse(b.latest||'')-Date.parse(a.latest||''))
	},[filtered,sessions,t])
	const groupIDs=useMemo(()=>groups.map(group=>group.id),[groups])
	const disclosure=useAuditGroupDisclosure(groupIDs,filtered.length)
	const confirmDelete=async()=>{
		if(!deleteTarget||deleting)return
		setDeleting(true)
		try{
			const result=deleteTarget.kind==='session'?await onDeleteRuns(deleteTarget.id===directAuditSessionID?'':deleteTarget.id):await onDeleteRuns(undefined)
			if(result.scope==='all'){
				const retainedRunIDs=new Set(result.retained_run_ids||[])
				const retainedGroupIDs=new Set(runs.filter(run=>retainedRunIDs.has(run.id)).map(auditSessionID))
				disclosure.forget(groupIDs.filter(id=>!retainedGroupIDs.has(id)))
			}
			else if(result.retained===0)disclosure.forget([deleteTarget.kind==='session'?deleteTarget.id:directAuditSessionID])
			setDeleteTarget(null)
		}catch{/* the application notification channel presents the actionable error */}
		finally{setDeleting(false)}
	}
	const deleteTitle=deleteTarget?.kind==='session'?t('audit.deleteSessionTitle',{title:deleteTarget.title}):t('audit.deleteAllTitle')
	if(!ready)return <div className="audit-loading panel" role="status"><LoaderCircle className="spin" size={16}/><span>{t('common.loading')}</span></div>
	return <div className="audit-runs-view page-stack">
		{error&&<div className="inline-error">{error}</div>}
		<div className="audit-toolbar"><div className="search-box"><Search size={16}/><input aria-label={t('common.search')} value={query} onChange={event=>setQuery(event.target.value)}/></div><span>{t('audit.counts',{sessions:groups.length,runs:filtered.length})}</span>{runs.length>0&&<button type="button" className="audit-clear-button" onClick={()=>setDeleteTarget({kind:'all'})}><Trash2 size={13}/>{t('audit.clear')}</button>}</div>
		<div className="audit-groups">{groups.map(group=><details className="audit-session panel" open={disclosure.expanded.has(group.id)} onToggle={event=>disclosure.setOpen(group.id,event.currentTarget.open)} key={group.id}>
			<summary className="audit-session-summary"><div className="audit-session-glyph"><History size={17}/></div><div className="audit-session-name"><b>{group.title}</b><span>{group.id===directAuditSessionID?t('audit.noSession'):group.id} · {t('audit.lastRun',{date:new Date(group.latest).toLocaleString(localeFor(instance.language))})}</span></div><div className="audit-session-stats">{group.pending>0&&<span className="pending-count"><b>{group.pending}</b> {t('audit.pending')}</span>}<button type="button" className="audit-session-delete danger" onClick={event=>{event.preventDefault();event.stopPropagation();setDeleteTarget({kind:'session',id:group.id,title:group.title})}}><Trash2 size={13}/>{t('audit.deleteSession')}</button></div><ChevronRight className="audit-session-chevron" size={17}/></summary>
			<div className="audit-table"><div className="audit-row audit-head"><span>{t('audit.columns.time')}</span><span>{t('audit.columns.operation')}</span><span>{t('audit.columns.status')}</span><span>{t('audit.columns.host')}</span><span>{t('audit.columns.exit')}</span><span aria-hidden="true"/></div>{group.runs.map(run=><AuditRunRow key={run.id} run={run} hosts={hosts}/>)}</div>
		</details>)}</div>
		{runsHasMore&&<button type="button" className="audit-load-more panel" disabled={loadingMore} onClick={()=>void onLoadMoreRuns().then(disclosure.reveal)}>{loadingMore?<LoaderCircle className="spin" size={14}/>:<History size={14}/>} {t('audit.loadMore')}</button>}
		{!runs.length&&<Empty icon={<History/>} title={t('audit.emptyTitle')}/>}
		{runs.length>0&&!groups.length&&<Empty icon={<Search/>} title={t('audit.noMatch')}/>}
		{deleteTarget&&<DestructiveConfirmDialog title={deleteTitle} busy={deleting} onCancel={()=>setDeleteTarget(null)} onConfirm={()=>void confirmDelete()}/>}
	</div>
}
