import { memo } from 'react'
import { useTranslation } from 'react-i18next'
import { ChevronRight, ListChecks } from 'lucide-react'
import { CompactTable } from '../tools/components/CompactTable'
import { jsonRecord, limitedRecordEntries, parseRecord, textValue } from '../tools/payload'
import { safeToolArgument, toolArgumentSummary, toolLabel } from '../tools/summary'
import { toolContentStatus } from './chatEntries'
import type { ChatEntry, TaskToolEntryGroup } from './types'
import { useChatCardDisclosure, type ChatDisclosurePositionHandler } from './useChatCardDisclosure'

function taskToolArguments(entry:ChatEntry){
	return jsonRecord(jsonRecord(parseRecord(entry.content)._display)?.arguments)
}

function taskToolEntryStatus(entry:ChatEntry){
	return toolContentStatus(entry.content)||(entry.transient?'in_progress':entry.status||'completed')
}

function taskToolGroupStatus(entries:ChatEntry[]){
	const statuses=entries.map(taskToolEntryStatus)
	if(statuses.includes('in_progress'))return'in_progress'
	for(const status of ['failed','rejected','denied'])if(statuses.includes(status))return status
	if(statuses.includes('partial'))return'partial'
	if(statuses.includes('approval_required'))return'approval_required'
	return statuses.at(-1)||'completed'
}

function taskToolOperationSummary(entry:ChatEntry,argumentsValue=taskToolArguments(entry)){
	const payload=parseRecord(entry.content)
	const task=jsonRecord(payload.task)||jsonRecord(jsonRecord(payload.result)?.task)
	const taskID=textValue(task?.id)
	const summary=toolArgumentSummary(entry.tool,argumentsValue)||textValue(task?.subject)
	return entry.tool==='TaskCreate'&&taskID?[`#${taskID}`,summary].filter(Boolean).join(' · '):summary||toolLabel(entry.tool||'')
}

export const TaskToolGroupCard=memo(function TaskToolGroupCard({group,onDisclosure}:{group:TaskToolEntryGroup;onDisclosure:ChatDisclosurePositionHandler}){
	const {t}=useTranslation()
	const disclosure=useChatCardDisclosure(onDisclosure,'tool-summary-chevron')
	const status=taskToolGroupStatus(group.entries)
	const operations=group.entries.map(entry=>{
		const argumentsValue=taskToolArguments(entry)
		const safeArguments=argumentsValue?Object.fromEntries(limitedRecordEntries(argumentsValue).entries.map(([key,value])=>[key,safeToolArgument(value,key)])):undefined
		return{summary:taskToolOperationSummary(entry,argumentsValue),arguments:safeArguments,status:taskToolEntryStatus(entry)}
	})
	const summary=operations.map(operation=>operation.summary).join(' · ')
	return <details className={`tool-event tool-event-rich task-tool-group ${status}`} open={disclosure.expanded} onTransitionEnd={disclosure.finishTransition}>
		<summary onClick={event=>{event.preventDefault();disclosure.toggle(event.currentTarget)}}><div className="tool-summary-icon"><ListChecks size={15}/></div><div className="tool-summary-copy"><div className="tool-summary-heading"><b>{toolLabel(group.tool)}</b><span className="task-tool-group-count">{t('agentTasks.operationCount',{count:group.entries.length})}</span><code title={summary}>{summary}</code></div></div><div className="tool-summary-statuses"><span className={`tool-status ${status}`} key={status}>{t(`statusLabels.${status}`,{defaultValue:status.replaceAll('_',' ')})}</span></div><ChevronRight className="tool-summary-chevron" size={14}/></summary>
		{disclosure.renderBody&&<div className="tool-event-body"><CompactTable title={t('agentTasks.operations')} columns={[t('agentTasks.operation'),t('tool.actualParameters'),t('common.status')]} rows={operations.map(operation=>[operation.summary,operation.arguments,t(`statusLabels.${operation.status}`,{defaultValue:operation.status.replaceAll('_',' ')})])}/></div>}
	</details>
},(previous,next)=>previous.onDisclosure===next.onDisclosure&&previous.group.tool===next.group.tool&&previous.group.entries.length===next.group.entries.length&&previous.group.entries.every((entry,index)=>entry===next.group.entries[index]))
