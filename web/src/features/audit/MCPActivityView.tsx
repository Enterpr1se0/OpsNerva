import { useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Activity, LoaderCircle, Search } from 'lucide-react'
import { Empty } from '../../components/PageLayout'
import { localeFor } from '../../lib/i18n'
import type { Host } from '../../types'
import { MCPActivityCallCard } from './MCPActivityCallCard'
import { useMCPActivity } from './useMCPActivity'

export function MCPActivityView({hosts,refreshKey}:{hosts:Host[];refreshKey:number}){
	const {t,i18n:instance}=useTranslation()
	const {sessions,selectedID,setSelectedID,calls,outputs,loading,error}=useMCPActivity(refreshKey)
	const [query,setQuery]=useState('')
	const selected=sessions.find(session=>session.id===selectedID)
	const visibleCalls=useMemo(()=>{const needle=query.trim().toLowerCase();return needle?calls.filter(call=>`${call.tool_name}\n${call.arguments_json}\n${call.status}\n${call.error||''}`.toLowerCase().includes(needle)):calls},[calls,query])
	if(loading)return <div className="audit-loading panel" role="status"><LoaderCircle className="spin" size={16}/><span>{t('common.loading')}</span></div>
	return <div className="mcp-activity-layout">
		<aside className="mcp-session-list panel">
			<header><span>{t('audit.mcpSessions')}</span><em>{sessions.length}</em></header>
			<div>{sessions.map(session=><button type="button" className={session.id===selectedID?'active':''} onClick={()=>setSelectedID(session.id)} key={session.id}><span><b>{session.client_name||t('audit.mcpClient')}</b>{session.running_calls>0&&<em>{session.running_calls}</em>}</span><code title={session.id}>{session.id}</code><small>{session.transport||'—'} · {t('audit.mcpCalls',{count:session.call_count})} · {new Date(session.last_seen_at).toLocaleString(localeFor(instance.language))}</small></button>)}</div>
			{!sessions.length&&<Empty icon={<Activity/>} title={t('audit.noMCPActivity')}/>}
		</aside>
		<section className="mcp-call-panel">
			<div className="audit-toolbar"><div className="search-box"><Search size={16}/><input aria-label={t('common.search')} value={query} onChange={event=>setQuery(event.target.value)}/></div>{selected&&<span><code>{selected.client_name||t('audit.mcpClient')}</code> · {visibleCalls.length}</span>}</div>
			{error&&<div className="inline-error">{error}</div>}
			<div className="mcp-call-list">{visibleCalls.map(call=><MCPActivityCallCard key={call.id} call={call} output={outputs[call.id]} hosts={hosts}/>)}</div>
			{selected&&!calls.length&&<Empty icon={<Activity/>} title={t('audit.noMCPCalls')}/>}
			{selected&&calls.length>0&&!visibleCalls.length&&<Empty icon={<Search/>} title={t('audit.noMatch')}/>}
			{!selected&&sessions.length>0&&<Empty icon={<Activity/>} title={t('audit.selectMCPSession')}/>}
		</section>
	</div>
}
