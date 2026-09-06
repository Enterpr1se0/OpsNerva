import i18n from '../../lib/i18n'
import type { Run } from '../../types'
import { displayValue, jsonRecord, limitedRecordEntries, previewText, toolCollectionPreviewItems, toolOutputPreviewChars, type JsonRecord } from './payload'

export function toolLabel(value:string){return i18n.t(`toolNames.${value}`,{defaultValue:value})}
export function runAutoApproved(run?:Run){return run?.ai_review?.kind==='automatic_approval'&&run.ai_review.status==='completed'&&run.ai_review.decision==='allow'}
export function compactScript(script:string){const source=script.length>toolOutputPreviewChars?script.slice(0,toolOutputPreviewChars):script,lines=source.split(/\r?\n/).map(line=>line.trim()).filter(Boolean);if(!lines.length)return i18n.t('tool.shellScript');const first=previewText(lines[0],180);return lines.length===1&&source.length===script.length?first:i18n.t('tool.moreLines',{line:first,count:Math.max(1,lines.length-1)})}
export function formatDuration(value:unknown,run?:Run){if(typeof value==='number'&&Number.isFinite(value))return value>=1e9?`${(value/1e9).toFixed(2)} s`:`${(value/1e6).toFixed(1)} ms`;if(run?.completed_at){const ms=Date.parse(run.completed_at)-Date.parse(run.started_at);if(Number.isFinite(ms))return ms>=1000?`${(ms/1000).toFixed(2)} s`:`${ms} ms`}return'—'}

export function safeToolArgument(value:unknown,key='',depth=0):unknown{
	if(/(?:api[_-]?key|private[_-]?key|authorization|cookie|credential|passphrase|password|secret|token)/i.test(key))return'********'
	if(typeof value==='string')return previewText(value)
	if(depth>=4)return'…'
	if(Array.isArray(value)){
		const visible=value.slice(0,toolCollectionPreviewItems).map(item=>safeToolArgument(item,'',depth+1))
		if(value.length>visible.length)visible.push(i18n.t('tool.previewItemsOmitted',{count:value.length-visible.length}))
		return visible
	}
	const record=jsonRecord(value)
	if(record){
		const {entries,truncated}=limitedRecordEntries(record),visible:Array<[string,unknown]>=entries.map(([childKey,item])=>[childKey,safeToolArgument(item,childKey,depth+1)])
		if(truncated)visible.push(['…',i18n.t('tool.moreItemsOmitted')])
		return Object.fromEntries(visible)
	}
	return value
}

export function toolArgumentSummary(toolName:string|undefined,argumentsValue:JsonRecord|undefined){
	if(!argumentsValue)return''
	if(toolName==='TaskCreate')return argumentsValue.subject?displayValue(argumentsValue.subject):''
	if(toolName==='TaskGet')return argumentsValue.taskId?`#${displayValue(argumentsValue.taskId)}`:''
	if(toolName==='TaskUpdate'){
		const taskID=argumentsValue.taskId||argumentsValue.task_id
		const nextStatus=argumentsValue.status
		return [taskID?`#${displayValue(taskID)}`:'',nextStatus?i18n.t(`statusLabels.${displayValue(nextStatus)}`,{defaultValue:displayValue(nextStatus)}):''].filter(Boolean).join(' · ')
	}
	const preferred=toolName==='web_extract'?['urls']:toolName==='skill'?['skill']:toolName==='ssh_history'?['run_id','query']:['query','action','url','uri','path','name','run_id','task_id']
	for(const key of preferred){
		const value=safeToolArgument(argumentsValue[key],key)
		if(value===undefined||value===null||value==='')continue
		const displayed=displayValue(value)
		return Array.from(displayed).length>180?`${Array.from(displayed).slice(0,180).join('')}…`:displayed
	}
	return''
}
