import { clientId, keepEquivalent } from '../../lib/utils'
import type { ChatSession, ChatSessionDelta } from '../../types'

export const newSessionMarker = '__new__'

export function newChatSessionID(){return `session_${clientId().replace(/[^A-Za-z0-9]/g,'')}`}
export function contextWindowForSession(tokens:number,window:number,fallback:number){
	return tokens>0?window:(window||fallback)
}


export function rememberSession(id: string) { try { localStorage.setItem('opsnerva.activeSession', id) } catch { /* storage may be disabled */ } }
export function recalledSession() { try { return localStorage.getItem('opsnerva.activeSession') || '' } catch { return '' } }
export function rememberWorkspace(id:string){try{if(id)localStorage.setItem('opsnerva.activeWorkspace',id)}catch{/* storage may be disabled */}}
export function recalledWorkspace(){try{return localStorage.getItem('opsnerva.activeWorkspace')||''}catch{return''}}
export function rememberWorkspacePanelCollapsed(collapsed:boolean){try{localStorage.setItem('opsnerva.chatPanel.workspace',String(collapsed))}catch{/* storage may be disabled */}}
export function recalledWorkspacePanelCollapsed(){try{return localStorage.getItem('opsnerva.chatPanel.workspace')==='true'}catch{return false}}
export function applyChatSessionDelta(current:ChatSession[],delta:ChatSessionDelta){
	const removed=new Set(delta.removed_ids||[])
	const sessions=new Map(current.filter(session=>!removed.has(session.id)).map(session=>[session.id,session]))
	for(const session of delta.sessions||[])sessions.set(session.id,session)
	const next=[...sessions.values()].sort((left,right)=>right.updated_at.localeCompare(left.updated_at)||right.id.localeCompare(left.id)).slice(0,50)
	return keepEquivalent(current,next)
}
