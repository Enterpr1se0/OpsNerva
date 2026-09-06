import { memo } from 'react'
import { useTranslation } from 'react-i18next'
import { Check, ChevronRight, CircleDot, ListChecks, LoaderCircle, ShieldAlert } from 'lucide-react'
import type { AgentTaskList } from '../../types'
import type { SessionTaskRow } from './taskEntries'

export const SessionTasks=memo(function SessionTasks({tasks,rows,expanded,onExpanded}:{tasks:AgentTaskList;rows:SessionTaskRow[];expanded:boolean;onExpanded:(expanded:boolean)=>void}){
	const {t}=useTranslation()
	const completed=rows.filter(row=>row.status==='completed').length
	const current=rows.find(row=>row.status==='in_progress')?.task||rows.find(row=>row.status==='pending')?.task
	const blocked=rows.filter(row=>row.status==='blocked').length
	const state=current?'active':blocked?'blocked':'completed'
  const progress=tasks.items.length?Math.round(completed/tasks.items.length*100):0
	return <details className={`session-tasks ${state}`} open={expanded} onToggle={event=>onExpanded(event.currentTarget.open)}><summary><span className="task-list-icon"><ListChecks size={16}/></span><span className="task-list-summary"><b>{t('agentTasks.title')}</b><small>{current?current.active_form||current.subject:blocked?t('agentTasks.blocked',{count:blocked}):`${completed}/${tasks.items.length}`}</small></span><span className="task-list-progress"><i><em style={{width:`${progress}%`}}/></i><b>{progress}%</b></span><span className={`task-list-state ${state}`} key={state}>{t(`statusLabels.${state}`,{defaultValue:state})}</span><ChevronRight size={14}/></summary></details>
})

export const SessionTaskItems=memo(function SessionTaskItems({rows}:{rows:SessionTaskRow[]}){
	const {t}=useTranslation()
	const blocked=rows.some(row=>row.status==='blocked')&&!rows.some(row=>row.status==='in_progress'||row.status==='pending')
	return <section className={`session-task-view ${blocked?'blocked':'active'}`}><ul className="session-task-items">{rows.map(({task,blockers,status})=><li className={status} key={task.id}><span className="task-item-marker">{status==='completed'?<Check size={12}/>:status==='in_progress'?<LoaderCircle size={12}/>:status==='blocked'?<ShieldAlert size={12}/>:<CircleDot size={10}/>}</span><div title={task.description}><b>{task.subject}</b>{status==='blocked'&&<small>{t('agentTasks.blocked',{count:blockers.length})}</small>}</div><em>{task.owner||t(`statusLabels.${status}`,{defaultValue:status.replace('_',' ')})}</em></li>)}</ul></section>
})
