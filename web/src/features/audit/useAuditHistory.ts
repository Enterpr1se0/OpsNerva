import { useEffect, useState } from 'react'
import { auditHistoryApi } from '../../api/auditHistory'
import { subscribeApplicationEvents } from '../../api/appEvents'
import type { NotificationSink } from '../../lib/notifications'
import { errorText } from '../../lib/utils'
import { AuditHistoryStore } from './auditHistoryStore'

export type AuditView='runs'|'mcp'

// Keep pagination and disclosure across navigation, without subscribing App to
// page updates. Visibility changes only activate/deactivate the external store.
export function useAuditHistory(active:boolean,notify:NotificationSink){
	const [history]=useState(()=>new AuditHistoryStore(auditHistoryApi,
		listener=>subscribeApplicationEvents('audit',listener),
		error=>notify(errorText(error),'error')))
	useEffect(()=>{
		if(!active)return
		const sync=()=>history.setActive(document.visibilityState==='visible')
		sync()
		document.addEventListener('visibilitychange',sync)
		return()=>{document.removeEventListener('visibilitychange',sync);history.setActive(false)}
	},[active,history])
	return history
}
