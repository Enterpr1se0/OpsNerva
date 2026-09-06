import { memo, useEffect, useState } from 'react'
import { createPortal } from 'react-dom'
import { useTranslation } from 'react-i18next'
import { Edit3, History, LoaderCircle, Plus, Save, ShieldAlert, Trash2, X } from 'lucide-react'
import { localeFor } from '../../lib/i18n'
import type { ChatSession } from '../../types'

export function SessionRenameDialog({session,busy,error,onCancel,onConfirm}:{session:ChatSession;busy:boolean;error:string;onCancel:()=>void;onConfirm:(title:string)=>void}){
	const {t}=useTranslation()
	const [title,setTitle]=useState(session.title)
	const normalized=title.trim()
	useEffect(()=>{const close=(event:KeyboardEvent)=>{if(event.key==='Escape'&&!busy)onCancel()};window.addEventListener('keydown',close);return()=>window.removeEventListener('keydown',close)},[busy,onCancel])
	return createPortal(<div className="connection-dialog-backdrop" onMouseDown={event=>{if(event.target===event.currentTarget&&!busy)onCancel()}}><form className="connection-dialog compact panel session-rename-dialog" noValidate onSubmit={event=>{event.preventDefault();if(normalized&&normalized!==session.title)onConfirm(normalized)}}><header><span><Edit3 size={19}/><span><h2>{t('chat.renameConversation')}</h2></span></span><button type="button" disabled={busy} onClick={onCancel}><X size={15}/></button></header><div className="connection-dialog-fields single"><label><span>{t('chat.sessionTitle')}</span><input value={title} maxLength={80} onChange={event=>setTitle(event.target.value)} autoFocus/></label></div>{error&&<div className="connection-dialog-error"><ShieldAlert size={14}/><span>{error}</span></div>}<footer><button type="button" disabled={busy} onClick={onCancel}>{t('common.cancel')}</button><button className="primary" disabled={busy||!normalized||normalized===session.title}>{busy?<LoaderCircle className="spin" size={13}/>:<Save size={13}/>} {t('common.save')}</button></footer></form></div>,document.body)
}

export const ChatSessionSidebar=memo(function ChatSessionSidebar({sessions,historyError,approvalCounts,activeSessionID,activeCurrentSession,workspaceSwitching,loadingSession,onNew,onOpen,onRename,onDelete}:{sessions:ChatSession[];historyError:string;approvalCounts:Map<string,number>;activeSessionID:string;activeCurrentSession:boolean;workspaceSwitching:boolean;loadingSession:string;onNew:()=>void;onOpen:(id:string)=>void;onRename:(session:ChatSession)=>void;onDelete:(session:ChatSession)=>void}){
	const {t,i18n:instance}=useTranslation()
	return <>
		<header className="sidebar-conversation-head"><span><History size={15}/>{t('chat.conversations')}</span><button className="new-chat-button" onClick={onNew} disabled={workspaceSwitching} title={t('chat.newConversation')} aria-label={t('chat.newConversation')}><Plus size={14}/><span>{t('common.new')}</span></button></header>
		<div className="session-list">
			{historyError&&<div className="history-error">{historyError}</div>}
			{!sessions.length&&!historyError&&<div className="history-empty">{t('chat.noSaved')}</div>}
			{sessions.map(session=>{const pending=approvalCounts.get(session.id)||0;const active=session.active||(session.id===activeSessionID&&activeCurrentSession);return <div className={`session-item ${session.id===activeSessionID?'active':''}`} key={session.id}><button className="session-open" onClick={()=>onOpen(session.id)} disabled={workspaceSwitching||loadingSession===session.id}><b>{session.title}{pending>0&&<em className="session-approval-count">{t('chat.approvalCount',{count:pending})}</em>}{active&&<em className="session-running-count">{t('chat.runningBadge')}</em>}</b><span>{new Date(session.updated_at).toLocaleString(localeFor(instance.language))} · {t('chat.messageCount',{count:session.message_count})}</span></button><div className="session-actions"><button className="session-edit" onClick={()=>onRename(session)} disabled={workspaceSwitching} title={t('chat.renameConversation')} aria-label={t('chat.renameConversation')}><Edit3 size={13}/></button><button className="session-delete" onClick={()=>onDelete(session)} disabled={active||workspaceSwitching} title={active?t('chat.cannotDelete'):t('chat.deleteConversation')}><Trash2 size={13}/></button></div></div>})}
		</div>
	</>
})
