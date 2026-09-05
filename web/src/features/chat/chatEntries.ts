import { chatAttachmentURL } from '../../api/api'
import { appendStreamText, streamTextFrom, streamTextValue } from '../../api/streamText'
import type { AgentEvent, ChatMessage, ChatTokenUsage, QueuedChatMessage } from '../../types'
import { type ChatEntry } from './types'
import { jsonRecord, parseRecord, textValue } from '../tools/payload'
import { clientId } from '../../lib/utils'

const liveToolOutputBufferChars=128<<10
export function insertQueuedMessage(current:QueuedChatMessage[],item:QueuedChatMessage,position=0){
	const existingIndex=current.findIndex(existing=>existing.id===item.id)
	if(existingIndex>=0){
		const existing=current[existingIndex]
		const merged={...existing,...item,attachments:item.attachments||existing.attachments,attachment_count:item.attachment_count??existing.attachment_count}
		if(merged.message===existing.message&&merged.mode===existing.mode&&merged.created_at===existing.created_at&&merged.attachments===existing.attachments&&merged.attachment_count===existing.attachment_count)return current
		return current.map((candidate,index)=>index===existingIndex?merged:candidate)
	}
	const next=[...current]
	const index=position>0?Math.min(position-1,next.length):next.length
	next.splice(index,0,item)
	return next
}

export function queuedMessageEntries(messages:QueuedChatMessage[],imageFallback:(count:number)=>string):ChatEntry[]{
	return messages.map(item=>{
		const attachmentCount=item.attachments?.length||item.attachment_count||0
		return{
			id:`queued_${item.id}`,kind:'user',content:item.message||(attachmentCount?imageFallback(attachmentCount):''),
			images:item.attachments?.map(image=>({id:image.id,name:image.name,mimeType:image.mime_type,sizeBytes:image.size_bytes})),
		}
	})
}

export function historyEntries(messages:ChatMessage[],sessionID:string):ChatEntry[]{
	return messages.map((item,index)=>{
		const kind=item.role==='assistant_progress'?'assistant':item.role
		const toolStatus=item.tool_status||(kind==='tool'?toolContentStatus(item.content):'')
		return{id:item.tool_call_id?`tool_${item.tool_call_id}`:item.id||`history_${index}_${item.created_at}`,sourceMessageId:item.id,persisted:true,kind,content:item.content,contentTruncated:item.content_truncated,contentChars:item.content_chars,tool:item.tool_name,toolCallId:item.tool_call_id,runId:item.run_id,transient:kind==='tool'&&!settledToolStatus(toolStatus),progress:item.role==='assistant_progress',startedAt:kind==='tool'?Date.parse(item.created_at):undefined,status:item.status,lifecycle:item.role==='assistant'||item.role==='assistant_progress'?'committed':undefined,images:item.attachments?.map(image=>({id:image.id,name:image.name,mimeType:image.mime_type,sizeBytes:image.size_bytes,url:chatAttachmentURL(sessionID,image.id)})),tokenUsage:item.token_usage}
	})
}

function materializeStreamText(entry:ChatEntry):ChatEntry{
	if(!entry.streamText)return entry
	const{streamText,...rest}=entry
	return{...rest,content:streamTextValue(streamText)}
}

export function deactivateReasoning(entry:ChatEntry):ChatEntry{
	if(entry.kind!=='reasoning'||(!entry.active&&!entry.streamText))return entry
	return{...materializeStreamText(entry),active:false}
}

function startAssistantLifecycle(entries:ChatEntry[],messageID:string):ChatEntry[]{
	if(!messageID)return entries
	const existing=entries.find(item=>item.id===messageID)
	if(existing)return entries.map(item=>item.id===messageID&&item.lifecycle!=='committed'?{...item,lifecycle:'streaming' as const}:deactivateReasoning(item))
	return[...entries.map(deactivateReasoning),{id:messageID,kind:'assistant' as const,content:'',lifecycle:'streaming' as const}]
}

function appendAssistantDelta(entries:ChatEntry[],messageID:string,content:string):ChatEntry[]{
	if(!messageID||!content)return entries
	const existing=entries.find(item=>item.id===messageID)
	if(existing?.lifecycle==='committed')return entries
	if(existing)return entries.map(item=>item.id===messageID?{...item,content:'',streamText:appendStreamText(item.streamText||streamTextFrom(item.content),content),lifecycle:'streaming' as const}:deactivateReasoning(item))
	return[...entries.map(deactivateReasoning),{id:messageID,kind:'assistant' as const,content:'',streamText:streamTextFrom(content),lifecycle:'streaming' as const}]
}

function commitAssistantLifecycle(entries:ChatEntry[],messageID:string,progress=false){
	if(!messageID)return entries
	return entries.flatMap(item=>{
		if(item.id!==messageID)return[item]
		const committed=materializeStreamText(item)
		return committed.content.trim()?[{...committed,progress,lifecycle:'committed' as const}]:[]
	})
}

function bindTurnUser(entries:ChatEntry[],userMessageID:string,clientUserEntryID:string){
	if(!userMessageID||entries.some(item=>item.kind==='user'&&item.sourceMessageId===userMessageID))return entries
	let index=clientUserEntryID?entries.findIndex(item=>item.kind==='user'&&item.id===clientUserEntryID&&(item.status==='pending'||item.status==='waiting_for_approval')):-1
	if(index<0)for(let current=entries.length-1;current>=0;current--){if(entries[current].kind==='user'&&(entries[current].status==='pending'||entries[current].status==='waiting_for_approval')){index=current;break}}
	return index<0?entries:entries.map((item,current)=>current===index?{...item,sourceMessageId:userMessageID}:item)
}

function updateTurnUserStatus(entries:ChatEntry[],status:'pending'|'waiting_for_approval'|'completed'|'failed',userMessageID:string|undefined,clientUserEntryID:string){
	let index=userMessageID?entries.findIndex(item=>item.kind==='user'&&item.sourceMessageId===userMessageID):-1
	if(index<0&&!userMessageID&&clientUserEntryID)index=entries.findIndex(item=>item.kind==='user'&&item.id===clientUserEntryID&&(item.status==='pending'||item.status==='waiting_for_approval'))
	if(index<0&&!userMessageID)for(let current=entries.length-1;current>=0;current--){if(entries[current].kind==='user'&&(entries[current].status==='pending'||entries[current].status==='waiting_for_approval')){index=current;break}}
	return index<0||entries[index].status===status?entries:entries.map((item,current)=>current===index?{...item,status}:item)
}

function resetAssistantLifecycle(entries:ChatEntry[],messageID:string){
	if(!messageID)return entries
	return entries.filter(item=>item.id!==messageID)
}

function toolContentWithStatus(content:string,status:string,runID?:string){
	try{
		const payload=JSON.parse(content)
		if(payload&&typeof payload==='object'&&!Array.isArray(payload))return JSON.stringify({...payload,status,...(runID?{run_id:runID}:{})})
	}catch{/* keep malformed tool output visible */}
	return JSON.stringify({status,...(runID?{run_id:runID}:{}),value:content})
}

function toolContentRunID(content:string){
	const payload=parseRecord(content),result=jsonRecord(payload.result),task=jsonRecord(payload.task)
	return textValue(payload.run_id)||textValue(result?.run_id)||textValue(task?.run_id)
}

export function toolContentStatus(content:string){
	const payload=parseRecord(content)
	return textValue(payload.status)||textValue(jsonRecord(payload.result)?.status)||textValue(jsonRecord(payload.task)?.status)
}

function settledToolStatus(status:string){return['completed','partial','failed','interrupted','rejected','denied','expired','unknown'].includes(status)}

function toolEntryRunID(item:ChatEntry){
	return item.kind==='tool'?item.runId||toolContentRunID(item.content):''
}

function updateToolStatusByRunID(entries:ChatEntry[],status:string,runID?:string){
	if(!runID)return entries
	return entries.map(item=>item.kind==='tool'&&item.transient&&toolEntryRunID(item)===runID?{...item,content:toolContentWithStatus(item.content,status,runID),runId:runID}:item)
}


export function settledTurnEntries(messages:ChatMessage[],sessionID:string,current:ChatEntry[],active:boolean){
	const persisted=historyEntries(messages,sessionID)
	const persistedCalls=new Set(persisted.filter(item=>item.kind==='tool').map(item=>item.toolCallId).filter(Boolean))
	const persistedMessageIDs=new Set(persisted.map(item=>item.sourceMessageId).filter(Boolean))
	const older=current.filter(item=>item.persisted&&item.sourceMessageId&&!persistedMessageIDs.has(item.sourceMessageId))
	let latestUser=-1
	for(let index=messages.length-1;index>=0;index--){if(messages[index].role==='user'){latestUser=index;break}}
	const latestTurnCompleted=latestUser>=0&&messages.slice(latestUser+1).some(item=>item.role==='assistant'&&item.status==='completed')
	const keepErrors=active||!latestTurnCompleted
	return[
		...older,
		...persisted,
		...current.filter(item=>item.kind==='error'?keepErrors:item.kind==='tool'&&item.transient&&(!item.toolCallId||!persistedCalls.has(item.toolCallId))),
	]
}

export function prependHistoryEntries(messages:ChatMessage[],sessionID:string,current:ChatEntry[]){
	const older=historyEntries(messages,sessionID)
	const currentMessageIDs=new Set(current.map(item=>item.sourceMessageId).filter(Boolean))
	return[...older.filter(item=>!item.sourceMessageId||!currentMessageIDs.has(item.sourceMessageId)),...current]
}

export function mergePersistedToolEntries(messages:ChatMessage[],sessionID:string,current:ChatEntry[]){
	const persisted=historyEntries(messages,sessionID).filter(item=>item.kind==='tool'&&item.toolCallId)
	const byCallID=new Map(persisted.map(item=>[item.toolCallId!,item]))
	const matched=new Set<string>()
	let changed=false
	const merged=current.map(item=>{
		if(item.kind!=='tool'||!item.toolCallId)return item
		const next=byCallID.get(item.toolCallId)
		if(!next)return item
		matched.add(item.toolCallId)
		if(item.content===next.content&&item.runId===next.runId&&item.tool===next.tool&&item.transient===next.transient)return item
		changed=true
		return{...item,...next,startedAt:next.startedAt||item.startedAt,liveStdout:item.liveStdout,liveStderr:item.liveStderr,liveOutput:item.liveOutput,liveOutputStream:item.liveOutputStream,transferredBytes:item.transferredBytes,transferTotalBytes:item.transferTotalBytes}
	})
	const missing=persisted.filter(item=>!matched.has(item.toolCallId!))
	return changed||missing.length?[...merged,...missing]:current
}

export function updateToolRunStatus(entries:ChatEntry[],runID:string,status:string){
	if(!runID)return entries
	return entries.map(item=>{
		if(item.kind!=='tool')return item
		const itemRunID=toolEntryRunID(item),currentStatus=toolContentStatus(item.content)
		if(itemRunID===runID&&status==='in_progress'&&!item.transient&&settledToolStatus(currentStatus))return item
		return itemRunID===runID?{...item,runId:runID,content:toolContentWithStatus(item.content,status),transient:status==='in_progress'||status==='approval_required'}:item
	})
}

function appendToolOutput(entries:ChatEntry[],frame:AgentEvent){
	const runID=frame.run_id||''
	const callID=frame.tool_call_id||''
	let index=callID?entries.findIndex(item=>item.kind==='tool'&&item.toolCallId===callID):-1
	if(index<0&&runID){
		index=entries.findIndex(item=>item.kind==='tool'&&toolEntryRunID(item)===runID)
	}
	if(index<0)return entries
	const status=frame.status==='running'?'in_progress':frame.status||'in_progress'
	return entries.map((item,itemIndex)=>{
		if(itemIndex!==index)return item
		const currentStatus=toolContentStatus(item.content)
		if(!item.transient&&status==='in_progress'&&settledToolStatus(currentStatus))return item
		const content=currentStatus===status&&(!runID||item.runId===runID)?item.content:toolContentWithStatus(item.content,status,runID)
		const chunk=frame.content||''
		const outputStream=frame.stream==='stdout'||frame.stream==='stderr'?frame.stream:undefined
		const liveOutput=outputStream&&chunk?`${item.liveOutput||''}${chunk}`.slice(-16_384):item.liveOutput
		return {
			...item,
			content,
			tool:frame.tool_name||item.tool,
			toolCallId:callID||item.toolCallId,
			runId:runID||item.runId,
			liveStdout:frame.stream==='stdout'?appendBoundedOutput(item.liveStdout,chunk):item.liveStdout,
			liveStderr:frame.stream==='stderr'?appendBoundedOutput(item.liveStderr,chunk):item.liveStderr,
			liveOutput,
			liveOutputStream:outputStream&&chunk?outputStream:item.liveOutputStream,
			transferredBytes:frame.stream==='progress'&&typeof frame.transferred_bytes==='number'?frame.transferred_bytes:item.transferredBytes,
			transferTotalBytes:frame.stream==='progress'&&typeof frame.total_bytes==='number'?frame.total_bytes:item.transferTotalBytes,
			transient:status==='in_progress'||status==='approval_required',
		}
	})
}

function appendBoundedOutput(current:string|undefined,chunk:string){
	if(!chunk)return current||''
	if(chunk.length>=liveToolOutputBufferChars)return chunk.slice(-liveToolOutputBufferChars)
	const existing=current||''
	const keep=Math.max(0,liveToolOutputBufferChars-chunk.length)
	return existing.slice(-keep)+chunk
}

function appendReasoningDelta(entries:ChatEntry[],frame:AgentEvent){
	if(!frame.content)return entries
	const reasoningID=`reasoning_${frame.segment_id||'current'}`
	const existing=entries.find(item=>item.id===reasoningID)
	if(existing)return entries.map(item=>item.id===reasoningID?{
		...item,content:'',streamText:appendStreamText(item.streamText||streamTextFrom(item.content),frame.content!),active:true,
	}:item)
	const entry:ChatEntry={id:reasoningID,kind:'reasoning',content:'',streamText:streamTextFrom(frame.content),active:true}
	return[...entries.map(deactivateReasoning),entry]
}

function upsertToolEntry(entries:ChatEntry[],frame:AgentEvent){
	if(!frame.content)return entries
	const callID=frame.tool_call_id||''
	const runID=frame.run_id||toolContentRunID(frame.content)
	let index=callID?entries.findIndex(item=>item.kind==='tool'&&item.toolCallId===callID):-1
	if(index<0&&runID)index=entries.findIndex(item=>item.kind==='tool'&&toolEntryRunID(item)===runID)
	const transient=frame.status==='in_progress'||frame.status==='approval_required'
	if(index>=0){
		const current=entries[index]
		if(transient&&!current.transient&&settledToolStatus(toolContentStatus(current.content)))return entries
		return entries.map((item,itemIndex)=>itemIndex===index?{
			...item,content:frame.content!,tool:frame.tool_name||item.tool,toolCallId:callID||item.toolCallId,runId:runID||item.runId,
			liveStdout:transient?item.liveStdout:undefined,liveStderr:transient?item.liveStderr:undefined,transient,
		}:item)
	}
	const entry:ChatEntry={
		id:callID?`tool_${callID}`:clientId(),kind:'tool',content:frame.content,tool:frame.tool_name,toolCallId:callID||undefined,
		runId:runID||undefined,transient,startedAt:Date.now(),
	}
	return[...entries.map(deactivateReasoning),entry]
}

type AgentEntryFrameOptions={
	userEntryID:string
	queuedImages:(count:number)=>string
	stopped:string
	agentError:string
}

const agentEntryFrameTypes=new Set([
		'queue_started','turn_done','turn_steered','approval','approval_paused','approval_resuming','tool_output','token_usage','reasoning',
		'reasoning_reset','tool','message_start','message','message_commit','message_reset','done','interrupted','model_error','error',
	])

export function agentFrameAffectsEntries(frame:AgentEvent){
	return!!frame.user_message_id||agentEntryFrameTypes.has(frame.type)
}

export function reduceAgentEntryFrames(entries:ChatEntry[],frames:readonly AgentEvent[],options:AgentEntryFrameOptions){
	let next=entries
	for(const frame of frames){
		if(frame.user_message_id)next=bindTurnUser(next,frame.user_message_id,options.userEntryID)
		switch(frame.type){
			case'queue_started':
				if(frame.message_id){
					const entry:ChatEntry={id:`queued_${frame.message_id}`,kind:'user',content:frame.content||(frame.attachment_count?options.queuedImages(frame.attachment_count):''),status:'pending'}
					next=[...next.map(deactivateReasoning),entry]
				}
				break
			case'turn_done':
			case'turn_steered':
				next=updateTurnUserStatus(next.map(deactivateReasoning),'completed',frame.user_message_id,options.userEntryID)
				break
			case'approval':
			case'approval_paused':
				next=updateToolStatusByRunID(updateTurnUserStatus(next,'waiting_for_approval',frame.user_message_id,options.userEntryID),'approval_required',frame.run_id)
				break
			case'approval_resuming':
				next=updateToolStatusByRunID(updateTurnUserStatus(next,'pending',frame.user_message_id,options.userEntryID),'in_progress',frame.run_id)
				break
			case'tool_output':
				next=appendToolOutput(next,frame)
				break
			case'token_usage':
				if(frame.message_id&&frame.total_tokens){
					const usage:ChatTokenUsage={input_tokens:frame.input_tokens||0,output_tokens:frame.output_tokens||0,total_tokens:frame.total_tokens}
					next=next.map(item=>item.id===frame.message_id?{...item,tokenUsage:usage}:item)
				}
				break
			case'reasoning':
				next=appendReasoningDelta(next,frame)
				break
			case'reasoning_reset':
				if(frame.segment_id){const reasoningID=`reasoning_${frame.segment_id}`;next=next.filter(item=>item.id!==reasoningID)}
				break
			case'tool':
				next=upsertToolEntry(next,frame)
				break
			case'message_start':
				if(frame.message_id)next=startAssistantLifecycle(next,frame.message_id)
				break
			case'message':
				if(frame.message_id&&frame.content)next=appendAssistantDelta(next,frame.message_id,frame.content)
				break
			case'message_commit':
				if(frame.message_id)next=commitAssistantLifecycle(next,frame.message_id,frame.status==='progress')
				break
			case'message_reset':
				if(frame.message_id)next=resetAssistantLifecycle(next,frame.message_id)
				break
			case'done':
				next=updateTurnUserStatus(next,'completed',frame.user_message_id,options.userEntryID)
				break
			case'interrupted':
				next=[...updateTurnUserStatus(next.map(deactivateReasoning),'failed',frame.user_message_id,options.userEntryID),{id:clientId(),kind:'assistant',content:frame.content||options.stopped,lifecycle:'committed'}]
				break
			case'model_error':
			case'error':
				next=[...updateTurnUserStatus(next,'failed',frame.user_message_id,options.userEntryID),{id:clientId(),kind:'error',content:frame.error||options.agentError}]
				break
		}
	}
	return next
}
