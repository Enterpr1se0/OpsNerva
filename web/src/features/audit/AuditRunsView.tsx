import { useCallback, useEffect, useMemo, useState, useSyncExternalStore } from 'react'
import { useTranslation } from 'react-i18next'
import { History, Search, Trash2 } from 'lucide-react'
import { DestructiveConfirmDialog } from '../../components/DestructiveConfirmDialog'
import { Empty } from '../../components/PageLayout'
import { useDebouncedValue } from '../../lib/hooks'
import type { NotificationSink } from '../../lib/notifications'
import type { Host } from '../../types'
import type { AuditHistoryFilters } from '../../types/audit'
import { AuditFilters } from './AuditFilters'
import type { AuditHistoryStore } from './auditHistoryStore'
import { AuditHistoryGroupCard } from './AuditHistoryGroupCard'
import { AuditPageFeedback } from './AuditPageFeedback'

type AuditDeleteTarget={kind:'session';id:string;title:string}|{kind:'all'}

export function AuditRunsView({history,hosts,notify}:{history:AuditHistoryStore;hosts:Host[];notify:NotificationSink}){
	const {t}=useTranslation()
	const page=useSyncExternalStore(history.groups.subscribe,history.groups.getSnapshot,history.groups.getSnapshot)
	const view=useSyncExternalStore(history.subscribeView,history.getViewSnapshot,history.getViewSnapshot)
	const [filters,setFilters]=useState<AuditHistoryFilters>(()=>({...view.filters}))
	const settledQuery=useDebouncedValue(filters.query,250)
	const invalidRange=!!(filters.startedAfter&&filters.startedBefore&&filters.startedAfter>filters.startedBefore)
	const appliedFilters=useMemo(()=>({query:settledQuery,hostID:filters.hostID,startedAfter:filters.startedAfter,startedBefore:filters.startedBefore}),
		[filters.hostID,filters.startedAfter,filters.startedBefore,settledQuery])
	const hasActiveFilters=Object.values(view.filters).some(Boolean)
	const [deleteTarget,setDeleteTarget]=useState<AuditDeleteTarget|null>(null)
	const [deleting,setDeleting]=useState(false)
	useEffect(()=>{if(!invalidRange)history.setFilters(appliedFilters)},[appliedFilters,history,invalidRange])
	const selectGroup=useCallback((id:string,title:string)=>setDeleteTarget({kind:'session',id,title}),[])
	const confirmDelete=async()=>{
		if(!deleteTarget||deleting)return
		setDeleting(true)
		try{
			const result=await history.deleteRuns(deleteTarget.kind==='session'?deleteTarget.id:undefined)
			notify(result.retained?t('audit.deletedWithRetained',{deleted:result.deleted,retained:result.retained}):t('audit.deleted',{count:result.deleted}))
			setDeleteTarget(null)
		}catch{/* the shared notification channel presents the actionable error */}
		finally{setDeleting(false)}
	}
	return <div className="audit-runs-view page-stack">
		<div className="audit-toolbar">
			<div className="search-box"><Search size={16}/><input aria-label={t('common.search')} value={filters.query} onChange={event=>setFilters(current=>({...current,query:event.target.value}))}/></div>
			<span>{t('audit.loadedGroups',{count:page.items.length})}</span>
			{page.items.length>0&&<button type="button" className="audit-clear-button" onClick={()=>setDeleteTarget({kind:'all'})}><Trash2 size={13}/>{t('audit.clear')}</button>}
		</div>
		<AuditFilters filters={filters} hosts={hosts} invalidRange={invalidRange} onChange={setFilters}/>
		<div className="audit-groups">{page.items.map(group=><AuditHistoryGroupCard key={group.session_id} group={group} history={history} hosts={hosts} expanded={view.expanded.has(group.session_id)} active={view.active} onDelete={selectGroup}/>)}</div>
		<AuditPageFeedback page={page} onRetry={history.refresh} onLoadMore={history.loadMoreGroups} moreLabel={t('audit.loadMoreGroups')}/>
		{page.ready&&page.loading==='idle'&&!page.error&&!page.items.length&&<Empty icon={hasActiveFilters?<Search/>:<History/>} title={t(hasActiveFilters?'audit.noMatch':'audit.emptyTitle')}/>}
		{deleteTarget&&<DestructiveConfirmDialog title={deleteTarget.kind==='session'?t('audit.deleteSessionTitle',{title:deleteTarget.title}):t('audit.deleteAllTitle')} busy={deleting} onCancel={()=>setDeleteTarget(null)} onConfirm={()=>void confirmDelete()}/>}
	</div>
}
