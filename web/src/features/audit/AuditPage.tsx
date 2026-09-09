import { useTranslation } from 'react-i18next'
import { Activity, History } from 'lucide-react'
import type { NotificationSink } from '../../lib/notifications'
import type { Host } from '../../types'
import type { AuditHistoryStore } from './auditHistoryStore'
import { AuditRunsView } from './AuditRunsView'
import { MCPActivityView } from './MCPActivityView'
import type { AuditView } from './useAuditHistory'
import './auditHistory.css'

type AuditPageProps={
	view:AuditView
	onViewChange:(view:AuditView)=>void
	mcpRefreshKey:number
	history:AuditHistoryStore
	hosts:Host[]
	notify:NotificationSink
}

export function AuditPage({view,onViewChange,mcpRefreshKey,history,hosts,notify}:AuditPageProps){
	const {t}=useTranslation()
	return <div className="audit-page page-stack">
		<div className="audit-view-tabs" role="tablist" aria-label={t('audit.views')}>
			<button type="button" role="tab" aria-selected={view==='runs'} className={view==='runs'?'active':''} onClick={()=>onViewChange('runs')}><History size={15}/>{t('audit.runHistory')}</button>
			<button type="button" role="tab" aria-selected={view==='mcp'} className={view==='mcp'?'active':''} onClick={()=>onViewChange('mcp')}><Activity size={15}/>{t('audit.mcpActivity')}</button>
		</div>
		{view==='runs'?<AuditRunsView history={history} hosts={hosts} notify={notify}/>:<MCPActivityView hosts={hosts} refreshKey={mcpRefreshKey}/>}
	</div>
}
