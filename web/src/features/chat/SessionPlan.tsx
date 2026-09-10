import { memo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Check, ChevronRight, CircleDot, ListChecks, LoaderCircle } from 'lucide-react'
import { useDocumentVisible } from '../../lib/hooks'
import type { AgentPlan } from '../../types'
import { useSessionPlan } from './sessionPlanState'

// Keep the subscription and its updates below ChatPage's render boundary.
export const SessionPlanPanel=memo(function SessionPlanPanel({sessionID,active,visible}:{sessionID:string;active:boolean;visible:boolean}) {
	const documentVisible=useDocumentVisible()
	const panelVisible=visible&&documentVisible
	const plan=useSessionPlan(visible,sessionID)
	return <div className="session-plan-slot">{plan&&<SessionPlan key={`${plan.session_id}:${plan.created_at}`} plan={plan} active={active} visible={panelVisible}/>}</div>
})

export const SessionPlan=memo(function SessionPlan({plan,active,visible}:{plan:AgentPlan;active:boolean;visible:boolean}) {
	const {t}=useTranslation()
	const [expanded,setExpanded]=useState(false)
	const finished=plan.steps.filter(step=>step.status==='completed'||step.status==='skipped').length
	const current=plan.steps.find(step=>step.status==='in_progress')
	const state=plan.status==='completed'?'completed':active?'active':'paused'
	const progress=plan.steps.length?Math.round(finished/plan.steps.length*100):0
	return <details className={`session-plan ${state}`} open={expanded} onToggle={event=>setExpanded(event.currentTarget.open)}>
		<summary><span className="plan-icon"><ListChecks size={16}/></span><span className="plan-summary-copy"><b>{plan.goal}</b><small>{current?.title||t('plan.completed',{completed:finished,total:plan.steps.length})}</small></span><span className="plan-progress"><i><em style={{width:`${progress}%`}}/></i><b>{progress}%</b></span><span className="plan-state">{t(`statusLabels.${state}`)}</span><ChevronRight size={14}/></summary>
		{expanded&&<ul className="session-plan-steps">{plan.steps.map(step=>{
			const status=step.status==='in_progress'&&!active?'paused':step.status
			const animate=step.status==='in_progress'&&active&&visible
			return <li className={status} key={step.number}><span className="plan-step-marker">{step.status==='completed'?<Check size={12}/>:step.status==='skipped'?<ChevronRight size={12}/>:animate?<LoaderCircle className="spin" size={12}/>:<CircleDot size={10}/>}</span><b title={step.description||undefined}>{step.title}</b><em>{t(`statusLabels.${status}`)}</em></li>
		})}</ul>}
	</details>
})
