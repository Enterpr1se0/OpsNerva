import { memo, useSyncExternalStore } from 'react'
import { useTranslation } from 'react-i18next'
import { ChevronRight, History, Trash2 } from 'lucide-react'
import { localeFor } from '../../lib/i18n'
import type { Host } from '../../types'
import type { AuditHistoryGroup } from '../../types/audit'
import type { AuditHistoryStore } from './auditHistoryStore'
import { AuditPageFeedback } from './AuditPageFeedback'
import { AuditRunRow } from './AuditRunRow'

type AuditHistoryGroupCardProps={
	group:AuditHistoryGroup
	history:AuditHistoryStore
	hosts:Host[]
	expanded:boolean
	active:boolean
	onDelete:(id:string,title:string)=>void
}

export const AuditHistoryGroupCard=memo(function AuditHistoryGroupCard({group,history,hosts,expanded,active,onDelete}:AuditHistoryGroupCardProps){
	const {t,i18n:instance}=useTranslation()
	const records=history.getRuns(group.session_id)
	const page=useSyncExternalStore(records.subscribe,records.getSnapshot,records.getSnapshot)
	const title=group.kind==='direct'?t('audit.direct'):group.title||(group.kind==='mcp'?t('audit.mcpRunGroup'):t('audit.missingConversation'))
	return <details className="audit-session panel" open={expanded}>
		<summary className="audit-session-summary" onClick={event=>{event.preventDefault();history.setOpen(group.session_id,!expanded)}}>
			<div className="audit-session-glyph"><History size={17}/></div>
			<div className="audit-session-name"><b>{title}</b><span>{group.kind==='direct'?t('audit.noSession'):group.session_id} · {t('audit.lastRun',{date:new Date(group.latest_started_at).toLocaleString(localeFor(instance.language))})}</span></div>
			<div className="audit-session-stats">
				{group.pending_count>0&&<span className="pending-count"><b>{group.pending_count}</b> {t('audit.pending')}</span>}
				<button type="button" className="audit-session-delete danger" onClick={event=>{event.preventDefault();event.stopPropagation();onDelete(group.session_id,title)}}><Trash2 size={13}/>{t('audit.deleteSession')}</button>
			</div>
			<ChevronRight className="audit-session-chevron" size={17}/>
		</summary>
		<div className="audit-table">
			{page.items.length>0&&<div className="audit-row audit-head"><span>{t('audit.columns.time')}</span><span>{t('audit.columns.operation')}</span><span>{t('audit.columns.status')}</span><span>{t('audit.columns.host')}</span><span>{t('audit.columns.exit')}</span><span aria-hidden="true"/></div>}
			{page.items.map(run=><AuditRunRow key={run.id} run={run} hosts={hosts} active={active&&expanded}/>)}
			{expanded&&<AuditPageFeedback page={page} onRetry={()=>records.refresh(history.groups.getSnapshot().snapshotAt)} onLoadMore={()=>records.loadMore()} moreLabel={t('audit.loadMore')}/>}
		</div>
	</details>
})
