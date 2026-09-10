import { useEffect, useState } from 'react'
import { subscribeApplicationEvents } from '../../api/appEvents'
import type { AgentPlan, ChatState } from '../../types'
import { keepEquivalent } from '../../lib/utils'

export function subscribeSessionPlan(sessionID:string,onPlan:(plan:AgentPlan|null)=>void) {
	return subscribeApplicationEvents<Partial<ChatState>>('chat_state',event=>{
		if(event.type!=='event'||!event.data||!Object.prototype.hasOwnProperty.call(event.data,'plan'))return
		const next=event.data.plan||null
		if(next&&next.session_id!==sessionID)return
		onPlan(next)
	},{sessionId:sessionID})
}

export function useSessionPlan(visible:boolean,sessionID:string) {
	const [plan,setPlan]=useState<AgentPlan|null>(null)
	useEffect(()=>{
		if(!visible||!sessionID)return
		// The shared WebSocket supplies an initial snapshot and ordered deltas.
		// Tool results and HTTP history must not race this authoritative stream.
		return subscribeSessionPlan(sessionID,next=>setPlan(current=>keepEquivalent(current,next)))
	},[sessionID,visible])
	return plan?.session_id===sessionID?plan:null
}
