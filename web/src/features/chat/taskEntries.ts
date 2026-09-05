import { type LiveSSHTaskTarget } from '../../lib/liveTasks'
import type { AgentTaskList } from '../../types'
import { type ChatEntry, type TaskToolEntryGroup, type ChatRenderItem } from './types'
import { jsonRecord, parseRecord, textValue } from '../tools/payload'

export function tasksFromToolContent(content:string):AgentTaskList|undefined{
  try{const value=JSON.parse(content) as {tasks?:AgentTaskList};return value.tasks&&Array.isArray(value.tasks.items)?value.tasks:undefined}catch{return undefined}
}

function taskToolOperation(entry:ChatEntry):TaskToolEntryGroup['tool']|''{
	return entry.kind==='tool'&&(entry.tool==='TaskCreate'||entry.tool==='TaskUpdate')?entry.tool:''
}

export function groupedTaskToolEntries(entries:ChatEntry[]):ChatRenderItem[]{
	let turn=0
	const groups=new Map<string,ChatEntry[]>()
	for(const entry of entries){
		if(entry.kind==='user')turn++
		const tool=taskToolOperation(entry)
		if(!tool)continue
		const key=`${turn}:${tool}`
		const group=groups.get(key)
		if(group)group.push(entry)
		else groups.set(key,[entry])
	}
	const hidden=new Set<string>()
	const groupedAt=new Map<string,TaskToolEntryGroup>()
	for(const items of groups.values()){
		if(items.length<2)continue
		for(const entry of items.slice(0,-1))hidden.add(entry.id)
		const last=items.at(-1)!
		groupedAt.set(last.id,{kind:'task_tool_group',id:`task_group_${items[0].id}`,tool:taskToolOperation(last) as TaskToolEntryGroup['tool'],entries:items})
	}
	const result:ChatRenderItem[]=[]
	for(const entry of entries){
		if(hidden.has(entry.id))continue
		const group=groupedAt.get(entry.id)
		result.push(group||{kind:'entry',entry})
	}
	return result
}

const liveSSHTaskTargetCache=new WeakMap<ChatEntry,LiveSSHTaskTarget|null>()
function liveSSHTaskTarget(entry:ChatEntry){
	if(liveSSHTaskTargetCache.has(entry))return liveSSHTaskTargetCache.get(entry)||undefined
	let target:LiveSSHTaskTarget|undefined
	if(entry.kind==='tool'&&entry.tool==='ssh_task'){
		const payload=parseRecord(entry.content),result=jsonRecord(payload.result),display=jsonRecord(payload._display),argumentsValue=jsonRecord(display?.arguments)
		if(textValue(argumentsValue?.action)==='status'){
			const taskID=textValue(payload.task_id)||textValue(result?.task_id)||textValue(argumentsValue?.task_id)
			if(taskID)target={entryID:entry.id,taskID,status:textValue(payload.status)||textValue(result?.status)}
		}
	}
	liveSSHTaskTargetCache.set(entry,target||null)
	return target
}
export function latestLiveSSHTaskTargets(entries:ChatEntry[]){
	const latest=new Map<string,LiveSSHTaskTarget>()
	for(const entry of entries){const target=liveSSHTaskTarget(entry);if(target)latest.set(target.taskID,target)}
	return[...latest.values()]
}
