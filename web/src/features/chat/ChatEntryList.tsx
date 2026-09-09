import { Suspense, lazy, memo, useMemo } from 'react'
import { useTranslation } from 'react-i18next'
import { Bot, BrainCircuit, ChevronRight, ImagePlus, UserRound } from 'lucide-react'
import { CopyButton } from '../../components/CopyButton'
import { streamTextTail, type StreamText } from '../../api/streamText'
import { useDocumentVisible } from '../../lib/hooks'
import i18n, { localeFor } from '../../lib/i18n'
import { useLiveSSHTasks, type LiveSSHTaskSnapshot, type LiveSSHTaskTarget } from '../../lib/liveTasks'
import { compactTokenCount, formatFileSize } from '../../lib/utils'
import type { ChatTokenUsage, Host } from '../../types'
import { ToolEventCard } from '../tools/components/ToolEventCard'
import { TaskToolGroupCard } from './TaskToolGroupCard'
import type { ChatEntry, ChatRenderItem } from './types'
import { useChatCardDisclosure, type ChatDisclosurePositionHandler } from './useChatCardDisclosure'

const MarkdownMessage=lazy(()=>import('../../components/MarkdownMessage').then(module=>({default:module.MarkdownMessage})))

const StreamingTextNodes=memo(function StreamingTextNodes({value}:{value:StreamText}){
	return <>{value.blocks.map((block,index)=><span key={index}>{block}</span>)}{value.tail&&<span key="tail">{value.tail}</span>}</>
})

export const ChatEntryList=memo(function ChatEntryList({items,sessionID,visible,targets,actionEntryID,hosts,onDisclosure}:{items:ChatRenderItem[];sessionID:string;visible:boolean;targets:readonly LiveSSHTaskTarget[];actionEntryID:string;hosts:Host[];onDisclosure:ChatDisclosurePositionHandler}){
	const documentVisible=useDocumentVisible()
	const liveSSHTasks=useLiveSSHTasks(visible&&documentVisible,sessionID,targets)
	const owners=useMemo(()=>new Map(targets.map(target=>[target.entryID,target.taskID])),[targets])
	return <>{items.map(item=>{
		if(item.kind==='task_tool_group')return <TaskToolGroupCard key={item.id} group={item} onDisclosure={onDisclosure}/>
		const taskID=owners.get(item.entry.id)
		return <ChatBubble key={item.entry.id} sessionID={sessionID} entry={item.entry} showActions={item.entry.id===actionEntryID} hosts={hosts} liveSSHTaskOwner={!!taskID} liveSSHTask={taskID?liveSSHTasks.get(taskID):undefined} onDisclosure={onDisclosure}/>
	})}</>
})

const ChatBubble=memo(function ChatBubble({ sessionID, entry, showActions, hosts, liveSSHTaskOwner, liveSSHTask, onDisclosure }: {sessionID:string;entry:ChatEntry;showActions:boolean;hosts:Host[];liveSSHTaskOwner:boolean;liveSSHTask?:LiveSSHTaskSnapshot;onDisclosure:ChatDisclosurePositionHandler}) {
	const {t}=useTranslation()
  if (entry.kind === 'tool') return <ToolEventCard sessionID={sessionID} entry={entry} hosts={hosts} liveSSHTaskOwner={liveSSHTaskOwner} currentLiveSSHTask={liveSSHTask} onDisclosure={onDisclosure}/>
  if (entry.kind === 'reasoning') return <ReasoningCard content={entry.content} streamText={entry.streamText} active={!!entry.active} onDisclosure={onDisclosure}/>
	const hasContent=!!entry.content||!!entry.streamText?.length
  if (entry.kind === 'assistant' && !hasContent) return null
	return <div className={`bubble ${entry.kind} ${entry.status||''} ${entry.progress?'progress':''}`}><div className="avatar">{entry.kind === 'user' ? <UserRound size={17}/> : entry.kind === 'error' ? '!' : <Bot size={17}/>}</div><div><span className="bubble-label">{entry.kind === 'user' ? <>{t('chat.operator')}{entry.status==='failed'&&<em>{t('chat.turnIncomplete')}</em>}{entry.status==='pending'&&<em>{t('chat.processing')}</em>}{entry.status==='waiting_for_approval'&&<em>{t('statusLabels.approval_required')}</em>}</> : entry.kind === 'error' ? t('common.error') : 'OpsNerva'}</span>{entry.images&&entry.images.length>0&&<div className="message-images">{entry.images.map(image=>image.url?<a href={image.url} target="_blank" rel="noopener noreferrer" title={`${image.name} · ${formatFileSize(image.sizeBytes)}`} key={image.id}><img src={image.url} alt={image.name}/><span>{image.name}</span></a>:<span className="message-image-placeholder" title={`${image.name} · ${formatFileSize(image.sizeBytes)}`} key={image.id}><ImagePlus size={18}/><span>{image.name}</span></span>)}</div>}{hasContent&&<div className={`bubble-copy ${entry.kind==='assistant'&&entry.lifecycle!=='streaming'?'markdown-body':''}`}>{entry.kind==='assistant'&&entry.lifecycle!=='streaming'?<Suspense fallback={entry.content}><MarkdownMessage content={entry.content}/></Suspense>:entry.streamText?<StreamingTextNodes value={entry.streamText}/>:entry.content}</div>}{showActions&&<div className="assistant-message-footer"><CopyButton value={entry.content} className="message-copy-button"/>{entry.tokenUsage&&<TokenUsageLine usage={entry.tokenUsage}/>}</div>}</div></div>
})

function TokenUsageLine({usage}:{usage:ChatTokenUsage}){
	const {t,i18n:instance}=useTranslation()
	const locale=localeFor(instance.language)
	const item=(label:string,value:number,known=true)=><span title={known?value.toLocaleString(locale):undefined}><b>{label}</b>{known?compactTokenCount(value):'--'}</span>
	return <div className="token-usage-line"><em>Tokens</em>{item(t('chat.tokenInput'),usage.input_tokens,usage.input_tokens>0)}{item(t('chat.tokenOutput'),usage.output_tokens,usage.output_tokens>0)}{item(t('chat.tokenTotal'),usage.total_tokens)}</div>
}

function latestReasoningLine(content:string){
	const tail=content.slice(-2048).trimEnd()
	const line=tail.slice(Math.max(tail.lastIndexOf('\n'),tail.lastIndexOf('\r'))+1).trim()||i18n.t('chat.reasoningFallback')
	const characters=Array.from(line.slice(-144))
	return characters.length>72?`…${characters.slice(-72).join('')}`:line
}

function ReasoningCard({content,streamText,active,onDisclosure}:{content:string;streamText?:StreamText;active:boolean;onDisclosure:ChatDisclosurePositionHandler}){
	const {t}=useTranslation()
	const disclosure=useChatCardDisclosure(onDisclosure,'reasoning-chevron')
	const latest=latestReasoningLine(streamText?streamTextTail(streamText,2048):content)
	return <details className={`reasoning-card ${active?'active':''}`} open={disclosure.expanded} onTransitionEnd={disclosure.finishTransition}>
	  <summary onClick={event=>{event.preventDefault();disclosure.toggle(event.currentTarget)}}><span className="reasoning-icon"><BrainCircuit size={15}/></span><span className="reasoning-title">{active?t('chat.reasoningActive'):t('chat.reasoning')}</span><span className="reasoning-latest" title={latest}>{latest}</span><ChevronRight className="reasoning-chevron" size={14}/></summary>
	  {disclosure.renderBody&&<div className="reasoning-content"><pre>{streamText?<StreamingTextNodes value={streamText}/>:content}</pre></div>}
  </details>
}
