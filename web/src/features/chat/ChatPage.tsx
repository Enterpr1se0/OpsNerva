import { memo, useCallback, useEffect, useMemo, useRef, useState, type FormEvent } from 'react'
import { createPortal, flushSync } from 'react-dom'
import { useTranslation } from 'react-i18next'
import { Activity, History, ImagePlus, ListPlus, LoaderCircle, PanelLeftOpen, Send, Square, X, Zap } from 'lucide-react'
import { api, reconnectChatStream, streamChat } from '../../api/api'
import { subscribeApplicationEvents } from '../../api/appEvents'
import { DestructiveConfirmDialog } from '../../components/DestructiveConfirmDialog'
import type { LiveSSHTaskTarget } from '../../lib/liveTasks'
import { useNotifier } from '../../lib/notifications'
import { clientId, compactTokenCount, errorStatus, errorText, keepEquivalent } from '../../lib/utils'
import type { AgentEvent, Approval, ChatQueueMode, ChatSession, ChatSessionDelta, ChatState, Host, ModelProvider, QueuedChatMessage, SSHShell, SystemSettings, ToolCapabilities } from '../../types'
import { ApprovalDialog } from '../approval/ApprovalDialog'
import { jsonRecord, parseRecord, textValue } from '../tools/payload'
import { ChatWorkspacePanel } from '../workspace'
import { ChatActivityStatus } from './ChatActivityStatus'
import { ChatEntryList } from './ChatEntryList'
import { ChatSessionSidebar, SessionRenameDialog } from './ChatSessionSidebar'
import { ChatVisibilityContext } from './ChatVisibilityContext'
import { ComposerControls } from './ComposerControls'
import { SessionPlan } from './SessionPlan'
import { useSessionPlan } from './sessionPlanState'
import './sessionPlan.css'
import { agentFrameAffectsEntries, deactivateReasoning, historyEntries, insertQueuedMessage, mergePersistedToolEntries, prependHistoryEntries, queuedMessageEntries, reduceAgentEntryFrames, settledTurnEntries, updateToolRunStatus } from './chatEntries'
import { applyChatSessionDelta, contextWindowForSession, newChatSessionID, newSessionMarker, recalledSession, recalledWorkspace, recalledWorkspacePanelCollapsed, rememberSession, rememberWorkspace, rememberWorkspacePanelCollapsed } from './sessionState'
import { latestLiveSSHTaskTargets } from './taskEntries'
import type { ChatEntry, ConnectionRetryState, ContextUsage, ModelRetryState, PendingChatImage } from './types'
import { useChatScroll } from './useChatScroll'

type ChatPageProps={
	visible:boolean
	onActivate:()=>void
	hosts:Host[]
	providers:ModelProvider[]
	approvals:Approval[]
	workspaceShells:SSHShell[]
	capabilities:ToolCapabilities
	settings:SystemSettings|null
	imageTypes:string[]
	agentAvailable:boolean
	modelName?:string
	contextWindow:number
	refreshConnections:()=>Promise<void>
	dismissApproval:(approvalID:string)=>void
	onCreateWorkspaceShell:(workspaceID:string)=>Promise<void>
	onOpenWorkspaceShell:(shell:SSHShell)=>void
	onWorkspaceShellStarted:(shell:SSHShell)=>void
	onSettingsChanged:(settings:SystemSettings)=>void
	onHostChanged:(host:Host)=>void
	onModelChanged:(provider:ModelProvider)=>void
	sidebarTarget:HTMLDivElement|null
	onSessionDeleted:(sessionID:string)=>void
	onError:(message:string)=>void
}

const emptyChatEntries:ChatEntry[]=[]
const emptyLiveSSHTaskTargets:readonly LiveSSHTaskTarget[]=[]
type ActiveChatStream = { id: string; sessionId: string; controller: AbortController }
type ChatHistoryCursor = {createdAt:string;id:string}

function reconnectDelay(attempt:number){return attempt<=1?0:Math.min(10_000,500*2**Math.min(attempt-2,5))}
function waitForReconnect(delay:number,signal:AbortSignal){
	if(delay<=0)return Promise.resolve()
	return new Promise<void>((resolve,reject)=>{
		const timer=window.setTimeout(done,delay)
		function done(){signal.removeEventListener('abort',cancel);resolve()}
		function cancel(){window.clearTimeout(timer);signal.removeEventListener('abort',cancel);reject(new DOMException('Aborted','AbortError'))}
		signal.addEventListener('abort',cancel,{once:true})
	})
}

function workspaceShellStartedByTool(content:string):SSHShell|null{
	const payload=parseRecord(content)
	const shell=jsonRecord(payload.shell)||jsonRecord(jsonRecord(payload.result)?.shell)
	const display=jsonRecord(payload._display),argumentsValue=jsonRecord(display?.arguments)
	if(!shell||shell.kind!=='workspace'||(!jsonRecord(payload.shell_usage)&&textValue(argumentsValue?.action)!=='start'))return null
	return shell as unknown as SSHShell
}

const PersistentPageBoundary=memo(function PersistentPageBoundary({visible,children}:{visible:boolean;children:(visible:boolean)=>React.ReactNode}){
	return children(visible)
},(previous,next)=>!previous.visible&&!next.visible)

export const ChatPage=memo(function ChatPage({ visible, onActivate, hosts, providers, approvals, workspaceShells, capabilities, settings, imageTypes, agentAvailable, modelName, contextWindow, refreshConnections, dismissApproval, onCreateWorkspaceShell, onOpenWorkspaceShell, onWorkspaceShellStarted, onSettingsChanged, onHostChanged, onModelChanged, sidebarTarget, onSessionDeleted, onError }:ChatPageProps) {
		const {t}=useTranslation()
		const notify=useNotifier()
		const activeContextWindow=contextWindow
  const [entries, setEntries] = useState<ChatEntry[]>([])
	  const [message, setMessage] = useState('')
	  const [pendingImages,setPendingImages]=useState<PendingChatImage[]>([])
	  const [imageNotice,setImageNotice]=useState('')
	  const [imageInputKey,setImageInputKey]=useState(0)
  const [sessionId, setSessionId] = useState('')
  const [sessions, setSessions] = useState<ChatSession[]>([])
  const [historyError, setHistoryError] = useState('')
  const [sessionDeleteCandidate,setSessionDeleteCandidate]=useState<ChatSession|null>(null)
  const [deletingSession,setDeletingSession]=useState(false)
	const [sessionRenameCandidate,setSessionRenameCandidate]=useState<ChatSession|null>(null)
	const [renamingSession,setRenamingSession]=useState(false)
	const [sessionRenameError,setSessionRenameError]=useState('')
  const [loadingSession, setLoadingSession] = useState('')
	const [historyCursor,setHistoryCursor]=useState<ChatHistoryCursor|null>(null)
	const [historyHasMore,setHistoryHasMore]=useState(false)
	const [loadingOlderMessages,setLoadingOlderMessages]=useState(false)
  const [workspacePanelCollapsed,setWorkspacePanelCollapsed]=useState(recalledWorkspacePanelCollapsed)
  const [running, setRunning] = useState(false)
  const [detachedRunning,setDetachedRunning]=useState(false)
	const [queuedMessages,setQueuedMessages]=useState<QueuedChatMessage[]>([])
	const [queueingMode,setQueueingMode]=useState<ChatQueueMode|null>(null)
	const [stopping,setStopping]=useState(false)
	const [compressingContext,setCompressingContext]=useState(false)
	const [modelRetry,setModelRetry]=useState<ModelRetryState|null>(null)
	const [connectionRetry,setConnectionRetry]=useState<ConnectionRetryState|null>(null)
		const [contextUsage,setContextUsage]=useState<ContextUsage>({tokens:0,window:activeContextWindow})
		useEffect(()=>setContextUsage(current=>current.tokens===0?{...current,window:activeContextWindow}:current),[activeContextWindow])
	const plan=useSessionPlan(visible,sessionId)
	const [workspaceID,setWorkspaceID]=useState(recalledWorkspace)
	const [fileBrowserMode,setFileBrowserMode]=useState<'workspace'|'sftp'>('workspace')
	const [sftpHostID,setSFTPHostID]=useState('')
	const [boundWorkspaceID,setBoundWorkspaceID]=useState('')
	const [workspaceSwitching,setWorkspaceSwitching]=useState(false)
	const sessionIDRef=useRef('')
	  const activeStreamRef=useRef<ActiveChatStream|null>(null)
	const lastAgentEventIDRef=useRef(0)
	const lastAgentEventSessionRef=useRef('')
	const startedQueueMessageIDsRef=useRef(new Set<string>())
	  const imageURLsRef=useRef(new Set<string>())
	const reconnectErrorRef=useRef('')
  const sessionLoadRef=useRef('')
	const initialSessionRestoredRef=useRef(false)
  const currentApprovals=useMemo(()=>sessionId?approvals.filter(item=>item.session_id===sessionId).sort((left,right)=>left.created_at.localeCompare(right.created_at)||left.id.localeCompare(right.id)):[],[approvals,sessionId])
	const approvalCountsBySession=useMemo(()=>{const counts=new Map<string,number>();for(const approval of approvals){if(!approval.session_id)continue;counts.set(approval.session_id,(counts.get(approval.session_id)||0)+1)}return counts},[approvals])
	const sessionBusy=running||detachedRunning
	const queueingMessage=queueingMode!==null
	const toolsRunning=useMemo(()=>running?false:entries.some(item=>item.kind==='tool'&&item.transient),[entries,running])
	const liveSSHTaskTargets=useMemo(()=>visible?latestLiveSSHTaskTargets(entries):emptyLiveSSHTaskTargets,[entries,visible])
	const conversationEntries=useMemo(()=>visible?[...entries,...queuedMessageEntries(queuedMessages,count=>t('chat.queuedImages',{count}))]:emptyChatEntries,[entries,queuedMessages,t,visible])
	const latestConversationEntryID=conversationEntries.at(-1)?.id||''
	const {messagesRef,trackUserScroll,pauseLatestOnWheel,pauseLatest,preserveChatDisclosurePosition,followLatest,captureHistoryAnchor}=useChatScroll({visible,latestConversationEntryID,loadingSession,sessionId})
	const latestCompletedAssistantEntryID=useMemo(()=>{
		if(!visible||sessionBusy)return ''
		for(let index=entries.length-1;index>=0;index--){
			const entry=entries[index]
			if(entry.kind==='assistant'&&!entry.progress&&entry.lifecycle==='committed'&&entry.content)return entry.id
		}
		return ''
	},[entries,sessionBusy,visible])
	const selectedWorkspace=capabilities.workspaces.find(workspace=>workspace.id===workspaceID)||capabilities.workspaces[0]
	useEffect(()=>{sessionIDRef.current=sessionId},[sessionId])
	useEffect(()=>{if(!sessionId)setContextUsage({tokens:0,window:activeContextWindow})},[activeContextWindow,sessionId])
	useEffect(()=>{if(!selectedWorkspace)return;if(workspaceID!==selectedWorkspace.id)setWorkspaceID(selectedWorkspace.id);rememberWorkspace(selectedWorkspace.id)},[selectedWorkspace,workspaceID])
	useEffect(()=>()=>{sessionLoadRef.current='';const stream=activeStreamRef.current;activeStreamRef.current=null;stream?.controller.abort()},[])
	useEffect(()=>()=>{for(const url of imageURLsRef.current)URL.revokeObjectURL(url);imageURLsRef.current.clear()},[])
	const addImages=(files:File[])=>{const accepted=files.filter(file=>imageTypes.includes(file.type));if(accepted.length!==files.length)setImageNotice(t('chat.imageTypeRejected'));if(!accepted.length)return;const next=accepted.map(file=>{const url=URL.createObjectURL(file);imageURLsRef.current.add(url);return{id:clientId(),file,url}});setPendingImages(current=>[...current,...next])}
	const removePendingImage=(id:string)=>{setPendingImages(current=>{const target=current.find(image=>image.id===id);if(target){URL.revokeObjectURL(target.url);imageURLsRef.current.delete(target.url)}return current.filter(image=>image.id!==id)});setImageInputKey(value=>value+1)}
	const clearPendingImages=useCallback(()=>{for(const image of pendingImages){URL.revokeObjectURL(image.url);imageURLsRef.current.delete(image.url)}setPendingImages([]);setImageInputKey(value=>value+1);setImageNotice('')},[pendingImages])

  const detachActiveStream = useCallback(() => {
    const stream=activeStreamRef.current
    if(!stream)return
    activeStreamRef.current=null
    stream.controller.abort()
    setRunning(false)
    setModelRetry(null)
		setConnectionRetry(null)
  }, [])

  const loadSession = useCallback(async (id: string) => {
    const requestID=clientId()
    sessionLoadRef.current=requestID
    setLoadingSession(id)
    followLatest(true)
    try {
      const state = await api.chatState(id)
      if(sessionLoadRef.current!==requestID)return
		lastAgentEventSessionRef.current=id;lastAgentEventIDRef.current=0
	      setEntries(historyEntries(state.messages||[],id));setHistoryHasMore(!!state.messages_has_more);setHistoryCursor(state.messages_next_created_at&&state.messages_next_id?{createdAt:state.messages_next_created_at,id:state.messages_next_id}:null);setDetachedRunning(!!state.active);setQueuedMessages(state.queued_messages||[]);setQueueingMode(null);setStopping(false);setModelRetry(null);setConnectionRetry(null);setContextUsage({tokens:state.context_tokens||0,window:contextWindowForSession(state.context_tokens||0,state.context_window||0,activeContextWindow)});setWorkspaceID(state.workspace_id||'');setBoundWorkspaceID(state.workspace_id||'')
	      startedQueueMessageIDsRef.current.clear()
      setSessionId(id); rememberSession(id); setHistoryError('')
	} catch (err) { if(sessionLoadRef.current===requestID)setHistoryError(errorText(err)) }
	finally { if(sessionLoadRef.current===requestID)setLoadingSession('') }
	}, [activeContextWindow,followLatest])
	useEffect(()=>subscribeApplicationEvents<ChatSession[]|ChatSessionDelta>('sessions',event=>{
		if(event.type==='error'){setHistoryError(event.error||'');return}
		if(event.type!=='event'||!event.data)return
		if(event.mode==='delta'){
			setSessions(current=>applyChatSessionDelta(current,event.data as ChatSessionDelta));setHistoryError('')
			return
		}
		const items=event.data as ChatSession[]
		setSessions(current=>keepEquivalent(current,items));setHistoryError('')
		if(initialSessionRestoredRef.current)return
		initialSessionRestoredRef.current=true
		const remembered=recalledSession()
		if(remembered===newSessionMarker)return
		const target=items.some(item=>item.id===remembered)?remembered:items[0]?.id
		if(target)void loadSession(target)
	}),[loadSession])

	const loadOlderMessages=useCallback(async()=>{
		if(!sessionId||!historyHasMore||!historyCursor||loadingOlderMessages)return
		const targetSessionID=sessionId
		const restoreHistoryAnchor=captureHistoryAnchor()
		setLoadingOlderMessages(true);setHistoryError('')
		try{
			const page=await api.chatMessages(targetSessionID,historyCursor)
			if(sessionIDRef.current!==targetSessionID)return
			pauseLatest()
			flushSync(()=>setEntries(current=>prependHistoryEntries(page.messages||[],targetSessionID,current)))
			setHistoryHasMore(page.has_more)
			setHistoryCursor(page.next_created_at&&page.next_id?{createdAt:page.next_created_at,id:page.next_id}:null)
			restoreHistoryAnchor()
		}catch(err){if(sessionIDRef.current===targetSessionID)setHistoryError(errorText(err))}
		finally{if(sessionIDRef.current===targetSessionID)setLoadingOlderMessages(false)}
	},[captureHistoryAnchor,historyCursor,historyHasMore,loadingOlderMessages,pauseLatest,sessionId])

	const newChat=useCallback(()=>{
		if(workspaceSwitching)return
		onActivate()
		detachActiveStream()
		lastAgentEventSessionRef.current='';lastAgentEventIDRef.current=0
		sessionLoadRef.current=''
    setLoadingSession('')
	    followLatest(true);startedQueueMessageIDsRef.current.clear();setSessionId('');setBoundWorkspaceID('');setEntries([]);setHistoryHasMore(false);setHistoryCursor(null);setLoadingOlderMessages(false); setMessage('');clearPendingImages(); setHistoryError('');setContextUsage({tokens:0,window:activeContextWindow});setDetachedRunning(false);setQueuedMessages([]);setQueueingMode(null);setStopping(false);setCompressingContext(false);setModelRetry(null);setConnectionRetry(null);rememberSession(newSessionMarker)
	},[activeContextWindow,clearPendingImages,detachActiveStream,followLatest,onActivate,workspaceSwitching])

	const switchSession=useCallback((id:string)=>{
		if(workspaceSwitching)return
		onActivate()
    if(id===sessionId){
      if(loadingSession){sessionLoadRef.current='';setLoadingSession('')}
      return
    }
		detachActiveStream()
		startedQueueMessageIDsRef.current.clear()
		lastAgentEventSessionRef.current=id;lastAgentEventIDRef.current=0
		setDetachedRunning(false);setQueuedMessages([]);setQueueingMode(null);setStopping(false);setConnectionRetry(null);setHistoryHasMore(false);setHistoryCursor(null);setLoadingOlderMessages(false)
    void loadSession(id)
	},[detachActiveStream,loadSession,loadingSession,onActivate,sessionId,workspaceSwitching])
	const openSessionRename=useCallback((session:ChatSession)=>{setSessionRenameError('');setSessionRenameCandidate(session)},[])
	const openSessionDelete=useCallback((session:ChatSession)=>setSessionDeleteCandidate(session),[])

	const switchWorkspace=useCallback(async(id:string)=>{
		if(id===selectedWorkspace?.id||sessionBusy||loadingSession||workspaceSwitching)return
		if(!sessionId){setWorkspaceID(id);return}
		setWorkspaceSwitching(true);setHistoryError('')
		try{
			const session=await api.setChatSessionWorkspace(sessionId,id)
			setWorkspaceID(session.workspace_id);setBoundWorkspaceID(session.workspace_id)
			setSessions(current=>current.map(item=>item.id===session.id?{...item,workspace_id:session.workspace_id,updated_at:session.updated_at}:item))
		}catch(err){setHistoryError(errorText(err))}
		finally{setWorkspaceSwitching(false)}
	},[loadingSession,selectedWorkspace?.id,sessionBusy,sessionId,workspaceSwitching])

  const removeSession = async () => {
    if(!sessionDeleteCandidate)return
    const session=sessionDeleteCandidate
    setDeletingSession(true)
    try {
      await api.deleteChatSession(session.id)
	  setSessions(current=>current.filter(item=>item.id!==session.id))
	  onSessionDeleted(session.id)
      if (session.id === sessionId) newChat()
    } catch (err) { setHistoryError(errorText(err)) }
    finally { setDeletingSession(false); setSessionDeleteCandidate(null) }
  }

	const renameSession=async(title:string)=>{
		if(!sessionRenameCandidate)return
		setRenamingSession(true);setSessionRenameError('')
		try{
			const renamed=await api.renameChatSession(sessionRenameCandidate.id,title)
			setSessions(current=>current.map(item=>item.id===renamed.id?{...item,title:renamed.title}:item))
			setSessionRenameCandidate(null)
		}catch(err){setSessionRenameError(errorText(err))}
		finally{setRenamingSession(false)}
	}

	const handleAgentFrames=useCallback((frames:readonly AgentEvent[],userEntryID='',workspace='')=>{
		if(!frames.length)return
		for(const frame of frames){
			const eventSessionID=frame.session_id||sessionIDRef.current
			if(eventSessionID&&lastAgentEventSessionRef.current!==eventSessionID){lastAgentEventSessionRef.current=eventSessionID;lastAgentEventIDRef.current=0}
			if(frame.event_id&&frame.event_id>lastAgentEventIDRef.current)lastAgentEventIDRef.current=frame.event_id
			if(frame.session_id&&frame.session_id!==sessionIDRef.current){
				sessionIDRef.current=frame.session_id
				if(activeStreamRef.current)activeStreamRef.current.sessionId=frame.session_id
				setSessionId(frame.session_id)
				if(workspace)setBoundWorkspaceID(workspace)
				rememberSession(frame.session_id)
			}
			if(frame.type==='retry'){
				setModelRetry({attempt:frame.retry_attempt||1,max:frame.retry_max||1})
			}else if(['approval','approval_paused','approval_resuming','reasoning','reasoning_reset','tool','tool_output','message_start','message','message_commit','message_reset','queued','queue_started','turn_done','turn_steered','done','interrupted','model_error','error'].includes(frame.type))setModelRetry(null)
			if(frame.type==='queued'&&frame.message_id&&!startedQueueMessageIDsRef.current.has(frame.message_id)){
				setQueuedMessages(current=>insertQueuedMessage(current,{id:frame.message_id!,message:frame.content||'',mode:frame.queue_mode||'followup',attachment_count:frame.attachment_count||0,created_at:new Date().toISOString()},frame.queue_position))
			}
			if(frame.type==='queue_started'&&frame.message_id){
				startedQueueMessageIDsRef.current.add(frame.message_id)
				setQueuedMessages(current=>current.filter(item=>item.id!==frame.message_id))
			}
			if(frame.type==='context_usage'&&frame.context_tokens!==undefined)setContextUsage({tokens:frame.context_tokens,window:frame.context_window||activeContextWindow})
			if(frame.type==='context_compression'){
				setCompressingContext(frame.status==='in_progress')
				if(frame.status==='completed'&&frame.output_tokens!==undefined)setContextUsage(current=>({tokens:frame.output_tokens!,window:current.window}))
			}
			if(frame.type==='tool'&&frame.content){
				if(frame.status!=='in_progress'&&['ssh_shell','workspace_shell','ssh_tunnel'].includes(frame.tool_name||''))void refreshConnections()
				if(frame.tool_name==='workspace_shell'){
					const shell=workspaceShellStartedByTool(frame.content)
					if(shell)onWorkspaceShellStarted(shell)
				}
			}
			if(frame.type==='done'||frame.type==='interrupted'){
				startedQueueMessageIDsRef.current.clear()
				setStopping(false)
				setQueuedMessages([])
			}
			if(frame.type==='model_error'||frame.type==='error'){
				startedQueueMessageIDsRef.current.clear()
				setQueuedMessages([])
			}
		}
		if(frames.some(agentFrameAffectsEntries))setEntries(old=>reduceAgentEntryFrames(old,frames,{
			userEntryID,queuedImages:count=>t('chat.queuedImages',{count}),stopped:t('chat.stopped'),agentError:t('chat.agentError'),
		}))
	},[activeContextWindow,onWorkspaceShellStarted,refreshConnections,t])

	useEffect(()=>{
		if(!sessionId||running||!detachedRunning)return
		let active=true
		const controller=new AbortController()
		const reconnect=async()=>{
			let attempt=0
			while(active&&!controller.signal.aborted){
				attempt++
				const delay=reconnectDelay(attempt)
				const now=Date.now()
				setConnectionRetry({attempt,readyAt:now+delay})
				try{
					await waitForReconnect(delay,controller.signal)
					const state=await api.chatState(sessionId)
					if(!active)return
					setQueuedMessages(state.queued_messages||[])
					setContextUsage({tokens:state.context_tokens||0,window:contextWindowForSession(state.context_tokens||0,state.context_window||0,activeContextWindow)})
					setBoundWorkspaceID(state.workspace_id||'')
					if(!state.active){
						setEntries(old=>settledTurnEntries(state.messages||[],sessionId,old,false))
						setDetachedRunning(false);setStopping(false);setConnectionRetry(null);setHistoryError('')
						return
					}
					setEntries(old=>settledTurnEntries(state.messages||[],sessionId,old,true))
					setConnectionRetry(null)
					setHistoryError('')
					await reconnectChatStream(sessionId,lastAgentEventSessionRef.current===sessionId?lastAgentEventIDRef.current:0,frames=>{if(active)handleAgentFrames(frames)},controller.signal)
					attempt=0
				}catch(err){
					if(!active||controller.signal.aborted)return
					const status=errorStatus(err)
					if(status===400||status===401||status===403||status===404){
						setEntries(old=>[...old.filter(item=>item.lifecycle!=='streaming'),{id:clientId(),kind:'error',content:reconnectErrorRef.current||errorText(err)}])
						setDetachedRunning(false);setStopping(false);setConnectionRetry(null)
						return
					}
				}
			}
		}
		void reconnect()
		return()=>{active=false;controller.abort()}
	},[activeContextWindow,detachedRunning,handleAgentFrames,running,sessionId])

	useEffect(()=>{
		if(!visible||!sessionId||!toolsRunning||running)return
		let disposed=false
		let lastRunningToolCount=-1
		let refreshInFlight=false
		const refreshPersistedTools=()=>{
			if(refreshInFlight)return
			refreshInFlight=true
			void api.chatMessages(sessionId).then(page=>{
				if(!disposed)setEntries(old=>mergePersistedToolEntries(page.messages||[],sessionId,old))
			}).catch(()=>{/* the next state transition retries */}).finally(()=>{refreshInFlight=false})
		}
		const unsubscribe=subscribeApplicationEvents<Partial<ChatState>>('chat_state',event=>{
			if(event.type!=='event'||!event.data)return
			const state=event.data
			const has=<Key extends keyof ChatState>(key:Key)=>Object.prototype.hasOwnProperty.call(state,key)
			const messages=state.messages
			if(messages?.length)setEntries(old=>mergePersistedToolEntries(messages,sessionId,old))
			if(has('running_tool_calls')){
				const count=state.running_tool_calls||0
				if(count!==lastRunningToolCount){lastRunningToolCount=count;if(!messages?.length)refreshPersistedTools()}
			}
			if(has('queued_messages'))setQueuedMessages(current=>keepEquivalent(current,state.queued_messages||[]))
			if(has('workspace_id')&&state.workspace_id!==undefined){setWorkspaceID(state.workspace_id);setBoundWorkspaceID(state.workspace_id)}
			if(has('context_tokens')||has('context_window'))setContextUsage(current=>{
				const tokens=state.context_tokens??current.tokens
				const window=state.context_window??current.window
				return keepEquivalent(current,{tokens,window:contextWindowForSession(tokens,window,activeContextWindow)})
			})
			if(has('active')){
				setDetachedRunning(!!state.active)
				if(!state.active&&(state.running_tool_calls??lastRunningToolCount)<=0)setStopping(false)
			}
		},{sessionId})
		return()=>{disposed=true;unsubscribe()}
	},[activeContextWindow,running,sessionId,toolsRunning,visible])

	  const sendQuery = async (query:string,queryImages:PendingChatImage[]) => {
	    query=query.trim(); if((!query&&!queryImages.length)||sessionBusy||loadingSession||workspaceSwitching)return
	    let querySessionID=sessionId||newChatSessionID()
    const userEntryID=clientId()
    const streamID=clientId()
    const controller=new AbortController()
		const workspace=selectedWorkspace?.id||''
		activeStreamRef.current={id:streamID,sessionId:querySessionID,controller}
		lastAgentEventSessionRef.current=querySessionID;lastAgentEventIDRef.current=0
    const isAttached=()=>activeStreamRef.current?.id===streamID
    followLatest()
		setSessionId(querySessionID);rememberSession(querySessionID)
		reconnectErrorRef.current=''
		setStopping(false);setCompressingContext(false);setModelRetry(null);setConnectionRetry(null);setRunning(true)
	    const entryImages=queryImages.map(image=>({id:image.id,name:image.file.name,mimeType:image.file.type,sizeBytes:image.file.size,url:image.url}))
	    setEntries((old) => [...old.filter(item=>item.kind!=='error'), { id: userEntryID, kind: 'user', content: query, images:entryImages, status:'pending' }])
	    try {
			await streamChat(querySessionID,workspace,query,queryImages.map(image=>image.file),(frames:readonly AgentEvent[])=>{
				if(!isAttached())return
				for(const frame of frames)if(frame.session_id)querySessionID=frame.session_id
				handleAgentFrames(frames,userEntryID,workspace)
			},controller.signal)
		}catch(err){
			if(isAttached()&&!controller.signal.aborted){
				setModelRetry(null)
				const status=errorStatus(err)
				if(status>0&&status<500&&status!==408&&status!==429){
					setEntries(old=>[...old.map(item=>item.id===userEntryID?{...item,status:'failed' as const}:item),{id:clientId(),kind:'error',content:errorText(err)}])
				}else{
					reconnectErrorRef.current=errorText(err)
					setDetachedRunning(true)
				}
			}
		}
    finally {
      if(isAttached()){
        setModelRetry(null)
        setConnectionRetry(null)
        setEntries(old=>old.map(deactivateReasoning))
        setRunning(false)
        setStopping(false)
        setCompressingContext(false)
        if(querySessionID){
          try{
            const state=await api.chatState(querySessionID)
            if(isAttached()){
              setDetachedRunning(!!state.active)
              setQueuedMessages(state.queued_messages||[])
              setContextUsage({tokens:state.context_tokens||0,window:contextWindowForSession(state.context_tokens||0,state.context_window||0,activeContextWindow)})
              setBoundWorkspaceID(state.workspace_id||'')
              setEntries(old=>settledTurnEntries(state.messages||[],querySessionID,old,!!state.active))
              for(const image of queryImages){URL.revokeObjectURL(image.url);imageURLsRef.current.delete(image.url)}
            }
          }catch{/* the next state event or reload will recover state */}
        }
        if(isAttached())activeStreamRef.current=null
      }
    }
	  }

	const queueQuery=async(query:string,queryImages:PendingChatImage[],mode:ChatQueueMode)=>{
		if(!sessionId||(!query&&!queryImages.length)||!sessionBusy||stopping||queueingMessage)return
		setQueueingMode(mode)
		try{
			const result=await api.queueChatMessage(sessionId,query,queryImages.map(image=>image.file),mode)
			if(!startedQueueMessageIDsRef.current.has(result.item.id))setQueuedMessages(current=>insertQueuedMessage(current,result.item,result.position))
			setMessage(current=>current.trim()===query?'':current)
			const submitted=new Set(queryImages.map(image=>image.id))
			setPendingImages(current=>current.filter(image=>!submitted.has(image.id)))
			for(const image of queryImages){URL.revokeObjectURL(image.url);imageURLsRef.current.delete(image.url)}
			setImageInputKey(value=>value+1);setImageNotice('')
		}catch(err){notify(errorText(err),'error')}
		finally{setQueueingMode(null)}
	}

	const submitMessage=(mode:ChatQueueMode='steering')=>{const query=message.trim();if((!query&&!pendingImages.length)||loadingSession||workspaceSwitching||stopping||queueingMessage)return;const images=pendingImages;if(sessionBusy){void queueQuery(query,images,mode);return}setMessage('');setPendingImages([]);setImageInputKey(value=>value+1);setImageNotice('');void sendQuery(query,images)}
	const submit = (event: FormEvent) => {event.preventDefault();submitMessage()}
	const stopAgent = async () => {
		const targetSessionID=activeStreamRef.current?.sessionId||sessionId
		if(!targetSessionID||(!sessionBusy&&!toolsRunning)||stopping)return
		setStopping(true)
		let requested=false
		try{
			const result=await api.cancelChatSession(targetSessionID)
			requested=result.cancelled
			if(result.cancelled)setQueuedMessages([])
			if(!result.cancelled){const state=await api.chatState(targetSessionID);setDetachedRunning(!!state.active);setQueuedMessages(state.queued_messages||[]);setContextUsage({tokens:state.context_tokens||0,window:contextWindowForSession(state.context_tokens||0,state.context_window||0,activeContextWindow)});setEntries(old=>settledTurnEntries(state.messages||[],targetSessionID,old,!!state.active))}
		}catch(err){setEntries(old=>[...old,{id:clientId(),kind:'error',content:t('chat.stopFailed',{message:errorText(err)})}])}
		finally{if(!requested)setStopping(false)}
  }
	const compressContext=useCallback(async()=>{
		if(!sessionId||sessionBusy||loadingSession||compressingContext)return
		setCompressingContext(true)
		try{
			const result=await api.compressChatContext(sessionId)
			setContextUsage(current=>({tokens:result.after_tokens,window:current.window}))
			notify(t('chat.contextCompressed',{before:compactTokenCount(result.before_tokens),after:compactTokenCount(result.after_tokens)}))
		}catch(err){notify(errorText(err),'error')}
		finally{setCompressingContext(false)}
	},[sessionId,sessionBusy,loadingSession,compressingContext,notify,t])
	const composerEmpty=!message.trim()&&!pendingImages.length
	const showComposerStop=(sessionBusy||toolsRunning)&&composerEmpty
	const setWorkspaceCollapsed=useCallback((collapsed:boolean)=>{
		rememberWorkspacePanelCollapsed(collapsed)
		setWorkspacePanelCollapsed(collapsed)
	},[])
	const collapseWorkspacePanel=useCallback(()=>setWorkspaceCollapsed(true),[setWorkspaceCollapsed])
	const sessionSidebar=sidebarTarget&&createPortal(<ChatSessionSidebar sessions={sessions} historyError={historyError} approvalCounts={approvalCountsBySession} activeSessionID={sessionId} activeCurrentSession={sessionBusy||toolsRunning} workspaceSwitching={workspaceSwitching} loadingSession={loadingSession} onNew={newChat} onOpen={switchSession} onRename={openSessionRename} onDelete={openSessionDelete}/>,sidebarTarget)

	return <>{sessionSidebar}<PersistentPageBoundary visible={visible}>{pageVisible=><ChatVisibilityContext.Provider value={pageVisible}><div className={`chat-layout ${workspacePanelCollapsed?'workspace-panel-collapsed ':''}${pageVisible?'':'page-hidden'}`}>
		<ChatWorkspacePanel key={selectedWorkspace?.id||''} active={pageVisible&&!workspacePanelCollapsed} mode={fileBrowserMode} onModeChange={setFileBrowserMode} workspaces={capabilities.workspaces} workspaceID={selectedWorkspace?.id||''} hosts={hosts} sftpHostID={sftpHostID} onSFTPHostChange={setSFTPHostID} shells={workspaceShells} switching={workspaceSwitching} disabled={sessionBusy||!!loadingSession} bound={!!selectedWorkspace&&boundWorkspaceID===selectedWorkspace.id} onSelect={switchWorkspace} onCreateShell={onCreateWorkspaceShell} onOpenShell={onOpenWorkspaceShell} onCollapse={collapseWorkspacePanel}/>
	  {workspacePanelCollapsed&&<button type="button" className="chat-panel-open-button" onClick={()=>setWorkspaceCollapsed(false)} title={t('workspace.expandPanel')} aria-label={t('workspace.expandPanel')}><PanelLeftOpen size={15}/></button>}
    <div className="chat-main panel">
	  <div className="session-approval-slot">{currentApprovals.length>0&&<ApprovalDialog key={currentApprovals[0].id} approval={currentApprovals[0]} pendingCount={currentApprovals.length} hosts={hosts} running={sessionBusy||toolsRunning} stopping={stopping} onStop={()=>void stopAgent()} dismissApproval={dismissApproval} onApproved={result=>{if(result.status==='running')setEntries(old=>updateToolRunStatus(old,result.run_id,'in_progress'));if(result.shell?.kind==='workspace')onWorkspaceShellStarted(result.shell)}} onNotice={notify}/>}</div>
	      <div className="session-plan-slot">{plan&&<SessionPlan key={`${plan.session_id}:${plan.created_at}`} plan={plan} active={sessionBusy&&!stopping&&currentApprovals.length===0} visible={pageVisible}/>}</div>
		<div className="conversation-view">
			<div className="messages" ref={messagesRef} onScroll={trackUserScroll} onWheel={pauseLatestOnWheel} onTouchMove={pauseLatest}>
				{historyHasMore&&<button type="button" className="chat-history-more" disabled={loadingOlderMessages} onClick={()=>void loadOlderMessages()}>{loadingOlderMessages?<LoaderCircle className="spin" size={13}/>:<History size={13}/>} {t('chat.loadEarlier')}</button>}
				{conversationEntries.length === 0 && <div className="empty-chat"><div className="radar"><Activity size={35}/></div><h2>{t('chat.emptyTitle')}</h2></div>}
				<ChatEntryList items={conversationEntries} sessionID={sessionId} visible={pageVisible} targets={liveSSHTaskTargets} actionEntryID={latestCompletedAssistantEntryID} hosts={hosts} onDisclosure={preserveChatDisclosurePosition}/>
				{(sessionBusy||toolsRunning)&&<ChatActivityStatus visible={pageVisible} stopping={stopping} connectionRetry={connectionRetry} modelRetry={modelRetry}/>}
				{conversationEntries.length>0&&<div className="chat-scroll-anchor" aria-hidden="true"/>}
			</div>
		</div>
		  <form className="composer" onSubmit={submit}>
			  <ComposerControls sessionId={sessionId} sessionBusy={sessionBusy} loadingSession={!!loadingSession} workspaceSwitching={workspaceSwitching} compressingContext={compressingContext} settings={settings} hosts={hosts} providers={providers} modelName={modelName} contextUsage={contextUsage} onSettingsChanged={onSettingsChanged} onHostChanged={onHostChanged} onModelChanged={onModelChanged} onError={onError} onCompress={compressContext}/>
			  {pendingImages.length>0&&<div className="composer-images">{pendingImages.map(image=><div key={image.id}><img src={image.url} alt={image.file.name}/><span title={image.file.name}>{image.file.name}</span><button type="button" onClick={()=>removePendingImage(image.id)} title={t('chat.removeImage')}><X size={11}/></button></div>)}</div>}
			  {imageNotice&&<div className="composer-image-notice">{imageNotice}<button type="button" onClick={()=>setImageNotice('')}><X size={11}/></button></div>}
			  <div className="input-row"><label className="image-attach-button" title={t('chat.addImages')}><ImagePlus size={18}/><input key={imageInputKey} type="file" accept={imageTypes.join(',')} multiple disabled={!agentAvailable||stopping||workspaceSwitching||!!loadingSession} onChange={event=>addImages(Array.from(event.target.files||[]))}/></label><textarea value={message} onChange={(event) => setMessage(event.target.value)} onPaste={event=>{const files=Array.from(event.clipboardData.files).filter(file=>file.type.startsWith('image/'));if(files.length)addImages(files)}} placeholder={!agentAvailable?t('chat.configureModel'):loadingSession?t('chat.loadingConversation'):sessionBusy?t('chat.steerPlaceholder'):t('chat.prompt')} disabled={!agentAvailable||stopping||workspaceSwitching||!!loadingSession} onKeyDown={(event) => { if (event.key === 'Enter' && !event.shiftKey) { event.preventDefault(); event.currentTarget.form?.requestSubmit() } }}/><div className="composer-send-actions">{sessionBusy&&!composerEmpty&&<button type="button" className="composer-followup-button" onClick={()=>submitMessage('followup')} disabled={!agentAvailable||stopping||queueingMessage||workspaceSwitching||!!loadingSession} title={t('chat.queueMessage')}>{queueingMode==='followup'?<LoaderCircle className="spin" size={16}/>:<><ListPlus size={16}/><span>{t('chat.followup')}</span></>}</button>}{showComposerStop?<button type="button" className="composer-stop-button" onClick={()=>void stopAgent()} disabled={stopping||!(activeStreamRef.current?.sessionId||sessionId)} title={t('chat.stopTitle')} aria-label={t('chat.stopTitle')}><Square size={15} fill="currentColor"/></button>:<button className={sessionBusy?'composer-steer-button':''} aria-label={t(sessionBusy?'chat.steerMessage':'common.next')} title={sessionBusy?t('chat.steerMessage'):undefined} disabled={!agentAvailable || stopping || queueingMessage || workspaceSwitching || !!loadingSession || composerEmpty}>{queueingMode==='steering'?<LoaderCircle className="spin" size={18}/>:sessionBusy?<><Zap size={16}/><span>{t('chat.steering')}</span></>:<Send size={18}/>}</button>}</div></div>
		  </form>
    </div>
	{sessionDeleteCandidate&&<DestructiveConfirmDialog title={t('chat.deleteTitle',{title:sessionDeleteCandidate.title})} busy={deletingSession} onCancel={()=>setSessionDeleteCandidate(null)} onConfirm={()=>void removeSession()}/>}
	{sessionRenameCandidate&&<SessionRenameDialog key={sessionRenameCandidate.id} session={sessionRenameCandidate} busy={renamingSession} error={sessionRenameError} onCancel={()=>{if(!renamingSession)setSessionRenameCandidate(null)}} onConfirm={title=>void renameSession(title)}/>}
	</div></ChatVisibilityContext.Provider>}</PersistentPageBoundary></>
})
