import { type LiveSSHTaskTarget } from '../../lib/liveTasks'
import { type ChatEntry } from './types'
import { jsonRecord, parseRecord, textValue } from '../tools/payload'

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
