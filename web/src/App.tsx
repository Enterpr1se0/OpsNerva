import { FormEvent, createContext, memo, useCallback, useContext, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import Markdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { useTranslation } from 'react-i18next'
import type { TFunction } from 'i18next'
import type { Terminal as XTermInstance } from '@xterm/xterm'
import { invoke } from '@tauri-apps/api/core'
import { getCurrentWindow } from '@tauri-apps/api/window'
import '@xterm/xterm/css/xterm.css'
import {
  Activity, BookOpen, Bot, BrainCircuit, Braces, Check, ChevronLeft, ChevronRight, CircleDot, Copy, Cpu, Edit3, Eye, EyeOff, FileText, FolderOpen, FolderOutput, FunctionSquare, History, ImagePlus, KeyRound, LockKeyhole, Maximize2, Minimize2, Minus, Monitor, Moon, PanelLeftClose, PanelLeftOpen, Sun,
  Cable, Download, ListChecks, LoaderCircle, LogOut, Plus, Power, RefreshCw, Save, Search, Send, Server, Settings2, ShieldAlert, ShieldCheck, SlidersHorizontal, Square, TerminalSquare, Trash2, UploadCloud, UserRound, X, Zap,
} from 'lucide-react'
import { api, chatAttachmentURL, reconnectChatStream, sftpDownloadURL, sshShellEventsURL, streamChat, workspaceDownloadURL, workspaceFileEventsURL } from './api'
import { CopyButton, CopyablePre } from './CopyButton'
import { AppSelect, ModelCombobox } from './Controls'
import i18n, { localeFor, type SupportedLanguage } from './i18n'
import { TextFileEditor } from './TextFileEditor'
import type { AgentEvent, AgentTask, AgentTaskList, Approval, ApprovalExecutionResult, ApprovalMode, ChatMessage, ChatSession, CommandReview, Health, Host, HostAuthType, HostInput, HostSudoMode, LLMToolCatalog, LLMToolDescriptor, LLMToolGuard, ManagedSkill, MCPServer, MCPServerInput, MCPTransport, ModelCatalog, ModelProvider, ModelProviderInput, ModelProviderKind, ModelReasoningEffort, Proxy, ProxyInput, Run, ServerLogEntry, SFTPFileEntry, SSHShell, SSHShellEvent, SSHTunnel, SystemSettings, SystemSettingsInput, ToolCapabilities, WebSearchSettings, WebSearchSettingsInput, WorkspaceCapability, WorkspaceFilePreview, WorkspaceInput, WorkspaceShellMode } from './types'

type Page = 'chat' | 'ssh' | 'config' | 'extensions' | 'audit' | 'logs'
type ChatEntryImage = {id:string;name:string;mimeType:string;sizeBytes:number;url:string}
type PendingChatImage = {id:string;file:File;url:string}
type ChatEntry = { id: string; kind: 'user' | 'assistant' | 'tool' | 'reasoning' | 'error'; content: string; tool?: string; toolCallId?:string; runId?:string; transient?:boolean; startedAt?:number; liveStdout?:string; liveStderr?:string; liveOutput?:string; liveOutputStream?:'stdout'|'stderr'; transferredBytes?:number; transferTotalBytes?:number; images?:ChatEntryImage[]; active?: boolean; lifecycle?:'streaming'|'committed'; status?: 'pending' | 'completed' | 'failed' }
type ModelRetryState = {attempt:number;max:number;readyAt:number}
type ActiveChatStream = { id: string; sessionId: string; controller: AbortController }
type ConnectionRetryState = {attempt:number;readyAt:number}
type ContextUsage = {tokens:number;window:number}

function historyEntries(messages:ChatMessage[],sessionID:string):ChatEntry[]{
	return messages.map((item,index)=>{
		const kind=item.role==='assistant_progress'?'assistant':item.role
		const toolStatus=item.tool_status||(kind==='tool'?toolContentStatus(item.content):'')
		return{id:item.tool_call_id?`tool_${item.tool_call_id}`:item.id||`history_${index}_${item.created_at}`,kind,content:item.content,tool:item.tool_name,toolCallId:item.tool_call_id,runId:item.run_id,transient:kind==='tool'&&!settledToolStatus(toolStatus),startedAt:kind==='tool'?Date.parse(item.created_at):undefined,status:item.status,lifecycle:item.role==='assistant'||item.role==='assistant_progress'?'committed':undefined,images:item.attachments?.map(image=>({id:image.id,name:image.name,mimeType:image.mime_type,sizeBytes:image.size_bytes,url:chatAttachmentURL(sessionID,image.id)}))}
	})
}

function reconnectBaseEntries(messages:ChatMessage[],sessionID:string){
	return historyEntries(messages,sessionID)
}

function newChatSessionID(){return `session_${clientId().replace(/[^A-Za-z0-9]/g,'')}`}
function reconnectDelay(attempt:number){return attempt<=1?0:Math.min(10_000,500*2**Math.min(attempt-2,5))}
function errorStatus(error:unknown){const status=(error as{status?:unknown})?.status;return typeof status==='number'?status:0}
function waitForReconnect(delay:number,signal:AbortSignal){
	if(delay<=0)return Promise.resolve()
	return new Promise<void>((resolve,reject)=>{
		const timer=window.setTimeout(done,delay)
		function done(){signal.removeEventListener('abort',cancel);resolve()}
		function cancel(){window.clearTimeout(timer);signal.removeEventListener('abort',cancel);reject(new DOMException('Aborted','AbortError'))}
		signal.addEventListener('abort',cancel,{once:true})
	})
}

function deactivateReasoning(entry:ChatEntry):ChatEntry{
	return entry.kind==='reasoning'&&entry.active?{...entry,active:false}:entry
}

function compactTokenCount(value:number){
	if(value<1000)return String(value)
	if(value<1_000_000)return `${Number((value/1000).toFixed(value<10_000?1:0))}K`
	return `${Number((value/1_000_000).toFixed(value<10_000_000?1:0))}M`
}

function contextWindowForSession(tokens:number,window:number,fallback:number){
	return tokens>0?window:(window||fallback)
}

function ContextUsageRing({usage}:{usage:ContextUsage}){
	const {t,i18n:instance}=useTranslation()
	const known=usage.window>0
	const percent=known?Math.min(100,Math.max(0,Math.round(usage.tokens/usage.window*100))):0
	const label=t(known?'chat.contextUsage':'chat.contextUsageUnknown',{used:usage.tokens.toLocaleString(localeFor(instance.language)),limit:usage.window.toLocaleString(localeFor(instance.language))})
	return <span className={`context-usage-ring ${percent>=90?'danger':percent>=70?'warn':''}`} role={known?'meter':'status'} aria-label={label} aria-valuemin={known?0:undefined} aria-valuemax={known?usage.window:undefined} aria-valuenow={known?usage.tokens:undefined} title={label}>
		<svg viewBox="0 0 36 36" aria-hidden="true"><circle className="context-usage-track" cx="18" cy="18" r="15.5"/><circle className="context-usage-value" cx="18" cy="18" r="15.5" pathLength="100" strokeDasharray={`${percent} 100`}/></svg>
	</span>
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
	if(existing)return entries.map(item=>item.id===messageID?{...item,content:item.content+content,lifecycle:'streaming' as const}:deactivateReasoning(item))
	return[...entries.map(deactivateReasoning),{id:messageID,kind:'assistant' as const,content,lifecycle:'streaming' as const}]
}

function commitAssistantLifecycle(entries:ChatEntry[],messageID:string){
	if(!messageID)return entries
	return entries.flatMap(item=>item.id===messageID?(item.content.trim()?[{...item,lifecycle:'committed' as const}]:[]):[item])
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

function toolContentStatus(content:string){
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


function settledTurnEntries(messages:ChatMessage[],sessionID:string,current:ChatEntry[],_active:boolean){
	const persisted=historyEntries(messages,sessionID)
	const persistedCalls=new Set(persisted.filter(item=>item.kind==='tool').map(item=>item.toolCallId).filter(Boolean))
	return[
		...persisted,
		...current.filter(item=>item.kind==='error'||item.kind==='tool'&&item.transient&&(!item.toolCallId||!persistedCalls.has(item.toolCallId))),
	]
}

function mergePersistedToolEntries(messages:ChatMessage[],sessionID:string,current:ChatEntry[]){
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

function updateToolRunStatus(entries:ChatEntry[],runID:string,status:string){
	if(!runID)return entries
	return entries.map(item=>{
		if(item.kind!=='tool')return item
		const itemRunID=toolEntryRunID(item),currentStatus=toolContentStatus(item.content)
		if(itemRunID===runID&&status==='in_progress'&&!item.transient&&settledToolStatus(currentStatus))return item
		return itemRunID===runID?{...item,runId:runID,content:toolContentWithStatus(item.content,status),transient:status==='in_progress'||status==='approval_required'}:item
	})
}

function workspaceShellStartedByTool(content:string):SSHShell|null{
	const payload=parseRecord(content)
	const shell=jsonRecord(payload.shell)||jsonRecord(jsonRecord(payload.result)?.shell)
	const display=jsonRecord(payload._display),argumentsValue=jsonRecord(display?.arguments)
	if(!shell||shell.kind!=='workspace'||(!jsonRecord(payload.shell_usage)&&textValue(argumentsValue?.action)!=='start'))return null
	return shell as unknown as SSHShell
}

function topbarShell(shell:SSHShell){
	return shell.kind==='workspace'?shell.surface==='workspace_agent':shell.surface!=='workspace'
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
		const content=toolContentWithStatus(item.content,status,runID)
		const chunk=frame.content||''
		const outputStream=frame.stream==='stdout'||frame.stream==='stderr'?frame.stream:undefined
		const liveOutput=outputStream&&chunk?`${item.liveOutput||''}${chunk}`.slice(-16_384):item.liveOutput
		return {
			...item,
			content,
			tool:frame.tool_name||item.tool,
			toolCallId:callID||item.toolCallId,
			runId:runID||item.runId,
			liveStdout:frame.stream==='stdout'?(item.liveStdout||'')+chunk:item.liveStdout,
			liveStderr:frame.stream==='stderr'?(item.liveStderr||'')+chunk:item.liveStderr,
			liveOutput,
			liveOutputStream:outputStream&&chunk?outputStream:item.liveOutputStream,
			transferredBytes:frame.stream==='progress'&&typeof frame.transferred_bytes==='number'?frame.transferred_bytes:item.transferredBytes,
			transferTotalBytes:frame.stream==='progress'&&typeof frame.total_bytes==='number'?frame.total_bytes:item.transferTotalBytes,
			transient:status==='in_progress'||status==='approval_required',
		}
	})
}

function tasksFromToolContent(content:string):AgentTaskList|undefined{
  try{const value=JSON.parse(content) as {tasks?:AgentTaskList};return value.tasks&&Array.isArray(value.tasks.items)?value.tasks:undefined}catch{return undefined}
}

function unresolvedTaskDependencies(task:AgentTask,tasks:AgentTask[]){
	return task.blocked_by.filter(id=>tasks.find(candidate=>candidate.id===id)?.status!=='completed')
}

let clientIdCounter = 0
function clientId() {
  try {
    if (typeof globalThis.crypto?.randomUUID === 'function') return globalThis.crypto.randomUUID()
    const random = new Uint32Array(2)
    globalThis.crypto?.getRandomValues(random)
    if (random[0] || random[1]) return `client_${random[0].toString(36)}${random[1].toString(36)}`
  } catch { /* insecure or legacy browser: rendering keys do not require cryptographic randomness */ }
  clientIdCounter += 1
  return `client_${Date.now().toString(36)}_${clientIdCounter.toString(36)}_${Math.random().toString(36).slice(2)}`
}

function errorText(error: unknown) {
  const message = error instanceof Error ? error.message : String(error)
  if (/failed to fetch|networkerror|load failed/i.test(message)) {
    return i18n.t('errors.apiUnavailable')
  }
  if (message.includes('model provider request failed')) {
    return i18n.t('errors.providerUnavailable',{message})
  }
  return message
}

const newSessionMarker = '__new__'
const defaultChatImageTypes=['image/png','image/jpeg','image/webp','image/gif']
const desktopRuntime='__TAURI_INTERNALS__' in window
const desktopWindow=desktopRuntime?getCurrentWindow():null
function rememberSession(id: string) { try { localStorage.setItem('opsnerva.activeSession', id) } catch { /* storage may be disabled */ } }
function recalledSession() { try { return localStorage.getItem('opsnerva.activeSession') || '' } catch { return '' } }
function rememberWorkspace(id:string){try{if(id)localStorage.setItem('opsnerva.activeWorkspace',id)}catch{/* storage may be disabled */}}
function recalledWorkspace(){try{return localStorage.getItem('opsnerva.activeWorkspace')||''}catch{return''}}
function rememberWorkspacePanelCollapsed(collapsed:boolean){try{localStorage.setItem('opsnerva.chatPanel.workspace',String(collapsed))}catch{/* storage may be disabled */}}
function recalledWorkspacePanelCollapsed(){try{return localStorage.getItem('opsnerva.chatPanel.workspace')==='true'}catch{return false}}
type ColorTheme='light'|'dark'
type ThemePreference='system'|ColorTheme
const themeStorageKey='opsnerva.theme'
function normalizeThemePreference(value:string|null):ThemePreference{return value==='light'||value==='dark'||value==='system'?value:'system'}
function recalledThemePreference():ThemePreference{
	try{return normalizeThemePreference(localStorage.getItem(themeStorageKey))}catch{return'system'}
}
function systemColorTheme():ColorTheme{return window.matchMedia?.('(prefers-color-scheme: dark)').matches?'dark':'light'}
function resolvedColorTheme(preference:ThemePreference,systemTheme:ColorTheme):ColorTheme{return preference==='system'?systemTheme:preference}
function rememberThemePreference(preference:ThemePreference){try{localStorage.setItem(themeStorageKey,preference)}catch{/* storage may be disabled */}}
function applyColorTheme(theme:ColorTheme){document.documentElement.dataset.theme=theme;document.documentElement.style.colorScheme=theme}
const initialThemePreference=recalledThemePreference()
const initialSystemTheme=systemColorTheme()
applyColorTheme(resolvedColorTheme(initialThemePreference,initialSystemTheme))

function DesktopTitlebar(){
	const {t}=useTranslation()
	if(!desktopWindow)return null
	return <header className="desktop-titlebar" data-tauri-drag-region onDoubleClick={event=>{if(!(event.target as Element).closest('.desktop-window-controls'))void desktopWindow.toggleMaximize().catch(()=>{})}}>
		<div className="desktop-titlebar-brand" data-tauri-drag-region><TerminalSquare size={14}/><b data-tauri-drag-region>OpsNerva</b></div>
		<div className="desktop-window-controls">
			<button type="button" onClick={()=>void desktopWindow.minimize().catch(()=>{})} title={t('shell.minimize')} aria-label={t('shell.minimize')}><Minus size={15}/></button>
			<button type="button" onClick={()=>void desktopWindow.toggleMaximize().catch(()=>{})} title={t('shell.maximize')} aria-label={t('shell.maximize')}><Maximize2 size={13}/></button>
			<button type="button" className="desktop-window-close" onClick={()=>void desktopWindow.close().catch(()=>{})} title={t('common.close')} aria-label={t('common.close')}><X size={15}/></button>
		</div>
	</header>
}

function AppFrame({children}:{children:React.ReactNode}){
	return <div className={`app-frame ${desktopRuntime?'desktop-app-frame':'web-app-frame'}`}><DesktopTitlebar/>{children}</div>
}

type NotificationTone='success'|'error'
type AppNotification={id:string;message:string;tone:NotificationTone}
type NotificationSink=(message:string,tone?:NotificationTone)=>void
const NotificationContext=createContext<NotificationSink>(()=>{})
function useNotifier(){return useContext(NotificationContext)}

function NotificationItem({notification,onDismiss}:{notification:AppNotification;onDismiss:(id:string)=>void}){
	const {t}=useTranslation()
	useEffect(()=>{
		if(notification.tone!=='success')return
		const timer=window.setTimeout(()=>onDismiss(notification.id),4000)
		return()=>window.clearTimeout(timer)
	},[notification.id,notification.tone,onDismiss])
	return <div className={`app-notification ${notification.tone}`} role={notification.tone==='error'?'alert':'status'}>
		<span className="app-notification-icon">{notification.tone==='error'?<ShieldAlert size={16}/>:<Check size={16}/>}</span>
		<span>{notification.message}</span>
		<button type="button" onClick={()=>onDismiss(notification.id)} title={t('common.dismiss')} aria-label={t('common.dismiss')}><X size={14}/></button>
	</div>
}

function NotificationCenter({notifications,onDismiss}:{notifications:AppNotification[];onDismiss:(id:string)=>void}){
	if(!notifications.length)return null
	return createPortal(<div className="notification-center" aria-live="polite">{notifications.map(notification=><NotificationItem key={notification.id} notification={notification} onDismiss={onDismiss}/>)}</div>,document.body)
}

function App() {
	const {t}=useTranslation()
  const [page, setPage] = useState<Page>('chat')
	const [themePreference,setThemePreference]=useState<ThemePreference>(initialThemePreference)
	const [systemTheme,setSystemTheme]=useState<ColorTheme>(initialSystemTheme)
	const colorTheme=resolvedColorTheme(themePreference,systemTheme)
	const [refreshing,setRefreshing]=useState(false)
	const [chatSidebarTarget,setChatSidebarTarget]=useState<HTMLDivElement|null>(null)
  const [health, setHealth] = useState<Health | null>(null)
  const [hosts, setHosts] = useState<Host[]>([])
  const [providers, setProviders] = useState<ModelProvider[]>([])
  const [proxies, setProxies] = useState<Proxy[]>([])
  const [settings, setSettings] = useState<SystemSettings | null>(null)
	const [capabilities,setCapabilities]=useState<ToolCapabilities>({workspaces:[]})
	const [toolCatalog,setToolCatalog]=useState<LLMToolCatalog|null>(null)
	const [skills,setSkills]=useState<ManagedSkill[]>([])
	const [mcpServers,setMCPServers]=useState<MCPServer[]>([])
  const [approvals, setApprovals] = useState<Approval[]>([])
  const [runs, setRuns] = useState<Run[]>([])
  const [sshTunnels,setSSHTunnels]=useState<SSHTunnel[]>([])
  const [sshShells,setSSHShells]=useState<SSHShell[]>([])
  const [selectedShell,setSelectedShell]=useState<SSHShell|null>(null)
  const [openConnectionPanel,setOpenConnectionPanel]=useState<'tunnel'|'shell'|null>(null)
	const [notifications,setNotifications]=useState<AppNotification[]>([])
	const [agentStreaming,setAgentStreaming]=useState(false)
	const connectionRefreshRef=useRef<Promise<void>|null>(null)
	const dismissNotification=useCallback((id:string)=>setNotifications(current=>current.filter(item=>item.id!==id)),[])
	const notify=useCallback<NotificationSink>((message,tone='success')=>{
		const normalized=message.trim()
		if(!normalized)return
		setNotifications(current=>{
			if(tone==='error'&&current.some(item=>item.tone===tone&&item.message===normalized))return current
			const next=[...current.filter(item=>item.message!==normalized||item.tone!==tone),{id:clientId(),message:normalized,tone}]
			const successIDs=next.filter(item=>item.tone==='success').map(item=>item.id)
			const expiredSuccessIDs=new Set(successIDs.slice(0,-4))
			return next.filter(item=>!expiredSuccessIDs.has(item.id))
		})
	},[])
	const reportError=useCallback((message:string)=>notify(message,'error'),[notify])
	const refreshConnections=useCallback(()=>{
		if(connectionRefreshRef.current)return connectionRefreshRef.current
		const task=Promise.allSettled([api.sshTunnels(),api.sshShells()]).then(([tunnels,shells])=>{
			if(tunnels.status==='fulfilled')setSSHTunnels(tunnels.value.tunnels||[])
			if(shells.status==='fulfilled')setSSHShells(shells.value.shells||[])
		})
		connectionRefreshRef.current=task
		void task.finally(()=>{if(connectionRefreshRef.current===task)connectionRefreshRef.current=null})
		return task
	},[])

  const refresh = useCallback(async () => {
    try {
	  const [nextHealth, nextHosts, nextProviders, nextProxies, nextSettings, nextCapabilities, nextToolCatalog, nextSkills, nextMCPServers, nextApprovals, nextRuns] = await Promise.all([
		api.health(), api.hosts(), api.modelProviders(), api.proxies(), api.systemSettings(), api.capabilities(), api.llmTools(), api.skills(), api.mcpServers(), api.approvals(), api.runs(), refreshConnections(),
      ])
	  setHealth(nextHealth); setHosts(nextHosts); setProviders(nextProviders); setProxies(nextProxies);setSettings(nextSettings);setCapabilities(nextCapabilities);setToolCatalog(nextToolCatalog);setSkills(nextSkills);setMCPServers(nextMCPServers); setApprovals(nextApprovals); setRuns(nextRuns)
	} catch (err) { notify(errorText(err),'error') }
  }, [notify,refreshConnections])
	const refreshApprovals=useCallback(async(decidedID?:string)=>{
		if(decidedID)setApprovals(current=>current.filter(item=>item.id!==decidedID))
		try{setApprovals(await api.approvals())}
		catch(err){notify(errorText(err),'error')}
	},[notify])
	const removeSessionState=useCallback((sessionID:string)=>{
		setApprovals(current=>current.filter(item=>item.session_id!==sessionID))
		setRuns(current=>current.filter(item=>item.session_id!==sessionID))
		setSSHShells(current=>current.filter(item=>item.session_id!==sessionID))
	},[])

	useEffect(()=>{
		if(!desktopRuntime)return
		const handleContextMenu=(event:MouseEvent)=>{
			const target=event.target instanceof Element?event.target:null
			if(target?.closest('input, textarea, [contenteditable="true"]'))return
			event.preventDefault()
		}
		document.addEventListener('contextmenu',handleContextMenu)
		return()=>document.removeEventListener('contextmenu',handleContextMenu)
	},[])
	useEffect(()=>{
		if(!desktopRuntime||!settings)return
		void invoke('set_tray_mode',{enabled:settings.mcp_http_enabled}).catch(()=>{})
	},[settings?.mcp_http_enabled])
	useLayoutEffect(()=>applyColorTheme(colorTheme),[colorTheme])
	useEffect(()=>rememberThemePreference(themePreference),[themePreference])
	useEffect(()=>{
		const media=window.matchMedia('(prefers-color-scheme: dark)')
		const sync=()=>setSystemTheme(media.matches?'dark':'light')
		sync()
		media.addEventListener('change',sync)
		return()=>media.removeEventListener('change',sync)
	},[])
	useEffect(()=>{
		const sync=(event:StorageEvent)=>{if(event.key===themeStorageKey)setThemePreference(normalizeThemePreference(event.newValue))}
		window.addEventListener('storage',sync)
		return()=>window.removeEventListener('storage',sync)
	},[])
	useEffect(()=>{
		const sync=()=>{document.documentElement.dataset.windowActive=document.hasFocus()?'true':'false'}
		sync();window.addEventListener('focus',sync);window.addEventListener('blur',sync)
		return()=>{window.removeEventListener('focus',sync);window.removeEventListener('blur',sync)}
	},[])
	useEffect(() => { void refresh() }, [refresh])
	useEffect(()=>{
		const timer=window.setInterval(()=>{if(document.visibilityState==='visible')void refreshConnections()},2000)
		return()=>window.clearInterval(timer)
	},[refreshConnections])
	useEffect(() => {
		if(agentStreaming)return
		const timer=window.setInterval(()=>{if(document.visibilityState==='visible')void refresh()},10000)
		return()=>window.clearInterval(timer)
	},[agentStreaming,refresh])
	const navigate=useCallback((next:Page)=>{
		if(next===page)return
		const reduced=window.matchMedia('(prefers-reduced-motion: reduce)').matches
		const transition=(document as Document&{startViewTransition?:(update:()=>void)=>unknown}).startViewTransition
		if(transition&&!reduced)transition.call(document,()=>setPage(next))
		else setPage(next)
	},[page])
	useEffect(()=>{
		if(!desktopRuntime)return
		const pages:Page[]=['chat','ssh','extensions','audit','logs','config']
		const shortcut=(event:KeyboardEvent)=>{
			if(!(event.ctrlKey||event.metaKey)||event.altKey||event.shiftKey)return
			const target=event.target instanceof Element?event.target:null
			if(target?.closest('input, textarea, select, [contenteditable="true"]'))return
			if(!/^[1-6]$/.test(event.key))return
			const next=pages[Number(event.key)-1]
			if(!next)return
			event.preventDefault();navigate(next)
		}
		window.addEventListener('keydown',shortcut)
		return()=>window.removeEventListener('keydown',shortcut)
	},[navigate])

  const title = t(`shell.pageTitles.${page}`)
	const manualRefresh=async()=>{
		if(refreshing)return
		setRefreshing(true)
		try{await refresh()}finally{setRefreshing(false)}
	}
	const stopSSHTunnel=async(id:string)=>{
		try{
			await api.stopSSHTunnel(id)
			setSSHTunnels(current=>current.filter(item=>item.id!==id))
		}catch(err){
			notify(errorText(err),'error')
		}
	}
	const registerSSHTunnel=(tunnel:SSHTunnel)=>{
		setSSHTunnels(current=>[...current.filter(item=>item.id!==tunnel.id),tunnel])
	}
	const rememberSSHShell=(shell:SSHShell)=>{
		setSSHShells(current=>[...current.filter(item=>item.id!==shell.id),shell])
	}
	const registerSSHShell=(shell:SSHShell)=>{
		rememberSSHShell(shell)
		setSelectedShell(shell)
	}
	const createWorkspaceShell=async(workspaceID:string)=>{
		try{registerSSHShell(await api.startSSHShell({workspace_id:workspaceID}))}
		catch(err){notify(errorText(err),'error')}
	}
	const observeAgentWorkspaceShell=(shell:SSHShell)=>{
		rememberSSHShell(shell)
	}
  return <NotificationContext.Provider value={notify}><AppFrame><div className="app-shell">
    <aside className="sidebar">
      <div className="brand"><div className="brand-mark"><TerminalSquare size={21}/></div><div className="brand-name"><strong>OpsNerva</strong></div></div>
      <nav className="sidebar-nav">
		<Nav active={page === 'config'} icon={<Settings2/>} label={t('shell.nav.configuration')} onClick={() => navigate('config')}/>
        <Nav active={page === 'chat'} icon={<Bot/>} label={t('shell.nav.agent')} onClick={() => navigate('chat')}/>
        <Nav active={page === 'ssh'} icon={<TerminalSquare/>} label={t('shell.nav.ssh')} onClick={() => navigate('ssh')}/>
		<Nav active={page === 'extensions'} icon={<Braces/>} label={t('shell.nav.extensions')} onClick={() => navigate('extensions')}/>
        <Nav active={page === 'audit'} icon={<History/>} label={t('shell.nav.audit')} onClick={() => navigate('audit')}/>
        <Nav active={page === 'logs'} icon={<FileText/>} label={t('shell.nav.logs')} onClick={() => navigate('logs')}/>
      </nav>
	  <section className="sidebar-conversations active"><div ref={setChatSidebarTarget}/></section>
      <div className="sidebar-foot">
        <div className="build">v0.1.7</div>
      </div>
    </aside>
    <main>
	      <header className="topbar"><div><h1>{title}</h1></div><div className="top-actions">
        <SSHTunnelStatus tunnels={sshTunnels} hosts={hosts} open={openConnectionPanel==='tunnel'} onOpenChange={open=>setOpenConnectionPanel(current=>open?'tunnel':current==='tunnel'?null:current)} onStop={stopSSHTunnel} onCreated={registerSSHTunnel}/>
		<SSHShellStatus shells={sshShells.filter(topbarShell)} hosts={hosts} open={openConnectionPanel==='shell'} onOpenChange={open=>setOpenConnectionPanel(current=>open?'shell':current==='shell'?null:current)} onOpen={shell=>{setOpenConnectionPanel(null);setSelectedShell(shell)}} onCreated={registerSSHShell}/>
        <LanguageSwitch/>
		<ThemeSwitch preference={themePreference} onChange={setThemePreference}/>
        <span className={`status ${health?.status === 'ok' ? 'online' : ''}`}><CircleDot size={14}/>{health?.status === 'ok' ? t('shell.online') : t('shell.disconnected')}</span>
        <button className={`icon-button ${refreshing?'refreshing':''}`} onClick={()=>void manualRefresh()} disabled={refreshing} title={t(refreshing?'common.refreshing':'shell.refresh')} aria-label={t(refreshing?'common.refreshing':'shell.refresh')}><RefreshCw size={17}/></button>
      </div></header>
      <section className={`workspace workspace-${page}`}>
			<ChatPage visible={page==='chat'} onActivate={()=>navigate('chat')}
				hosts={hosts} providers={providers} approvals={approvals} runs={runs} workspaceShells={sshShells.filter(shell=>shell.kind==='workspace')}
				capabilities={capabilities} settings={settings} imageTypes={settings?.chat_image_allowed_types||defaultChatImageTypes}
					agentAvailable={!!health?.agent_available} modelName={health?.model?.model} contextWindow={health?.model?.context_window||0} refresh={refresh} refreshConnections={refreshConnections}
				refreshApprovals={refreshApprovals} onCreateWorkspaceShell={createWorkspaceShell} onOpenWorkspaceShell={setSelectedShell} onWorkspaceShellStarted={observeAgentWorkspaceShell} onSettingsChanged={setSettings}
				onHostChanged={host=>setHosts(current=>current.map(item=>item.id===host.id?host:item))}
				onModelChanged={provider=>{setProviders(current=>current.map(item=>item.id===provider.id?provider:{...item,active:provider.active?false:item.active}));void api.health().then(setHealth).catch(err=>reportError(errorText(err)))}}
				sidebarTarget={chatSidebarTarget} onSessionDeleted={removeSessionState} onError={reportError} onStreamingChange={setAgentStreaming}
			/>
			{page === 'ssh' && <SSHWorkspacePage
				hosts={hosts} shells={sshShells.filter(shell=>shell.kind!=='workspace'&&shell.surface==='workspace')}
				onCreated={rememberSSHShell} refresh={refresh} onError={reportError}
			/>}
		{page === 'config' && <ConfigurationPage hosts={hosts} providers={providers} proxies={proxies} settings={settings} capabilities={capabilities} health={health} refresh={refresh}/>}
		{page === 'extensions' && <ExtensionsPage skills={skills} mcpServers={mcpServers} toolCatalog={toolCatalog} refresh={refresh}/>}
        {page === 'audit' && <AuditPage runs={runs} hosts={hosts}/>}
        {page === 'logs' && <LogsPage/>}
      </section>
	      {selectedShell&&<SSHShellTerminal
			key={selectedShell.id}
			initialShell={selectedShell}
			relatedShells={selectedShell.kind==='workspace'?sshShells.filter(shell=>shell.kind==='workspace'&&shell.workspace_id===selectedShell.workspace_id&&sshShellActive(shell.status)):[]}
			onSelect={setSelectedShell}
			onClose={()=>setSelectedShell(null)}
			onChanged={()=>void refresh()}
			onError={reportError}
		/>}
    </main>
	<NotificationCenter notifications={notifications} onDismiss={dismissNotification}/>
  </div></AppFrame></NotificationContext.Provider>
}

function LanguageSwitch(){
	const {t,i18n:instance}=useTranslation()
	const language:SupportedLanguage=instance.resolvedLanguage?.startsWith('zh')?'zh':'en'
	return <div className="language-switch" role="group" aria-label={t('language.label')}>
		<button type="button" className={language==='zh'?'active':''} aria-pressed={language==='zh'} onClick={()=>void instance.changeLanguage('zh')}>{t('language.chinese')}</button>
		<button type="button" className={language==='en'?'active':''} aria-pressed={language==='en'} onClick={()=>void instance.changeLanguage('en')}>{t('language.english')}</button>
	</div>
}

function ThemeSwitch({preference,onChange}:{preference:ThemePreference;onChange:(preference:ThemePreference)=>void}){
	const {t}=useTranslation()
	const options:[ThemePreference,React.ReactNode,string][]=[
		['system',<Monitor size={14}/>,t('shell.systemTheme')],
		['light',<Sun size={14}/>,t('shell.lightTheme')],
		['dark',<Moon size={14}/>,t('shell.darkTheme')],
	]
	return <div className="theme-switch" role="group" aria-label={t('shell.theme')}>
		{options.map(([value,icon,label])=><button type="button" className={preference===value?'active':''} aria-pressed={preference===value} title={label} aria-label={label} onClick={()=>onChange(value)} key={value}>{icon}</button>)}
	</div>
}

function useAutoCollapseDetails(open:boolean,onClose:()=>void){
	const detailsRef=useRef<HTMLDetailsElement>(null)
	const closeRef=useRef(onClose)
	closeRef.current=onClose
	useEffect(()=>{
		if(!open)return
		const outside=(event:Event)=>{
			const target=event.target
			if(target instanceof Node&&!detailsRef.current?.contains(target))closeRef.current()
		}
		const escape=(event:KeyboardEvent)=>{
			if(event.key!=='Escape')return
			event.preventDefault()
			closeRef.current()
			detailsRef.current?.querySelector<HTMLElement>('summary')?.focus()
		}
		document.addEventListener('pointerdown',outside,true)
		document.addEventListener('focusin',outside,true)
		document.addEventListener('keydown',escape,true)
		return()=>{
			document.removeEventListener('pointerdown',outside,true)
			document.removeEventListener('focusin',outside,true)
			document.removeEventListener('keydown',escape,true)
		}
	},[open])
	return detailsRef
}

function ApprovalModeStatus({settings,onChanged,onError}:{settings:SystemSettings|null;onChanged:(settings:SystemSettings)=>void;onError:(message:string)=>void}){
	const {t}=useTranslation()
	const mode=settings?.approval_mode??'manual'
	const [open,setOpen]=useState(false)
	const [busy,setBusy]=useState(false)
	const [confirmFullAccess,setConfirmFullAccess]=useState(false)
	const detailsRef=useAutoCollapseDetails(open,()=>setOpen(false))
	const apply=async(next:ApprovalMode)=>{
		if(!settings||next===mode){setOpen(false);return}
		setBusy(true)
		try{
			const result=await api.saveSystemSettings({agent_max_iterations:settings.agent_max_iterations,approval_mode:next})
			onChanged(result)
			setOpen(false)
		}catch(err){onError(errorText(err))}
		finally{setBusy(false)}
	}
	const select=(next:ApprovalMode)=>{
		if(next==='full_access'&&mode!=='full_access'){setOpen(false);setConfirmFullAccess(true);return}
		void apply(next)
	}
	return <>
		<details ref={detailsRef} className={`approval-mode-status ${mode}`} open={open} onToggle={event=>setOpen(event.currentTarget.open)}>
			<summary title={t('settings.approvalMode')} onClick={event=>{if(busy)event.preventDefault()}}>{busy?<LoaderCircle className="spin" size={13}/>:<ShieldCheck size={13}/>}<span>{t(`settings.approvalMode_${mode}`)}</span><ChevronRight size={12}/></summary>
			<div className="approval-mode-menu">
				{(['manual','auto','full_access'] as ApprovalMode[]).map(value=><button type="button" className={value===mode?'active':''} disabled={busy||!settings} onClick={()=>select(value)} key={value}><span>{t(`settings.approvalMode_${value}`)}</span>{value===mode&&<Check size={13}/>}</button>)}
			</div>
		</details>
		{confirmFullAccess&&<FullAccessConfirmDialog onCancel={()=>setConfirmFullAccess(false)} onConfirm={()=>{setConfirmFullAccess(false);void apply('full_access')}}/>}
	</>
}

function hostInputWithAgentState(host:Host,enabled:boolean):HostInput{
	return {
		id:host.id,name:host.name,address:host.address,port:host.port,user:host.user,agent_enabled:enabled,
		auth_type:host.auth_type,private_key:'',known_hosts_file:host.known_hosts_file||'',proxy_jump_host_id:host.proxy_jump_host_id||'',proxy_id:host.proxy_id||'',
		password:'',sudo_mode:host.sudo_mode,sudo_password:'',
	}
}

function ComposerHostSelector({hosts,disabled,onChanged,onError}:{hosts:Host[];disabled:boolean;onChanged:(host:Host)=>void;onError:(message:string)=>void}){
	const {t}=useTranslation()
	const [open,setOpen]=useState(false)
	const [busy,setBusy]=useState('')
	const detailsRef=useAutoCollapseDetails(open,()=>setOpen(false))
	const activeHosts=hosts.filter(host=>host.agent_enabled)
	const names=activeHosts.map(host=>host.name).join(', ')
	const label=activeHosts.length?t('chat.hostsCount',{count:activeHosts.length,names}):t('chat.noHosts')
	const toggle=async(host:Host)=>{
		if(disabled||busy)return
		setBusy(host.id)
		try{onChanged(await api.saveHost(hostInputWithAgentState(host,!host.agent_enabled)))}
		catch(err){onError(errorText(err))}
		finally{setBusy('')}
	}
	return <details ref={detailsRef} className="composer-selector composer-hosts" open={open} onToggle={event=>setOpen(event.currentTarget.open)}>
		<summary title={t('chat.switchHosts')} aria-label={t('chat.switchHosts')} onClick={event=>{if(disabled)event.preventDefault()}}><Server size={13}/><span>{label}</span><ChevronRight size={11}/></summary>
		<div className="composer-selector-menu composer-host-menu">
			{hosts.map(host=><button type="button" className={host.agent_enabled?'active':''} disabled={disabled||!!busy} onClick={()=>void toggle(host)} key={host.id}><span><Server size={13}/><b>{host.name}</b></span>{busy===host.id?<LoaderCircle className="spin" size={13}/>:<em>{t(host.agent_enabled?'common.disable':'common.enable')}</em>}</button>)}
			{!hosts.length&&<span className="composer-selector-empty">{t('chat.noHosts')}</span>}
		</div>
	</details>
}

function ComposerModelSelector({providers,fallbackModel,disabled,onChanged,onError}:{providers:ModelProvider[];fallbackModel?:string;disabled:boolean;onChanged:(provider:ModelProvider)=>void;onError:(message:string)=>void}){
	const {t}=useTranslation()
	const [open,setOpen]=useState(false)
	const [busy,setBusy]=useState('')
	const detailsRef=useAutoCollapseDetails(open,()=>setOpen(false))
	const active=providers.find(provider=>provider.active)
	const label=active?.model||fallbackModel||t('chat.noModel')
	const activate=async(provider:ModelProvider)=>{
		if(disabled||busy||provider.active){setOpen(false);return}
		setBusy(provider.id)
		try{const selected=await api.activateModelProvider(provider.id);onChanged(selected);setOpen(false)}
		catch(err){onError(errorText(err))}
		finally{setBusy('')}
	}
	return <details ref={detailsRef} className="composer-selector composer-model" open={open} onToggle={event=>setOpen(event.currentTarget.open)}>
		<summary title={t('chat.switchModel')} aria-label={t('chat.switchModel')}><Cpu size={13}/><span>{label}</span><ChevronRight size={11}/></summary>
		<div className="composer-selector-menu composer-model-menu">
			{providers.map(provider=><button type="button" className={provider.active?'active':''} disabled={disabled||!!busy} onClick={()=>void activate(provider)} key={provider.id}><span><b>{provider.name}</b><small>{provider.model}</small></span>{busy===provider.id?<LoaderCircle className="spin" size={13}/>:provider.active?<Check size={13}/>:null}</button>)}
			{!providers.length&&<span className="composer-selector-empty">{t('chat.noModel')}</span>}
		</div>
	</details>
}

const reasoningEfforts:ModelReasoningEffort[]=['','low','medium','high','xhigh']

function ComposerReasoningSelector({providers,disabled,onChanged,onError}:{providers:ModelProvider[];disabled:boolean;onChanged:(provider:ModelProvider)=>void;onError:(message:string)=>void}){
	const {t}=useTranslation()
	const [open,setOpen]=useState(false)
	const [busy,setBusy]=useState(false)
	const detailsRef=useAutoCollapseDetails(open,()=>setOpen(false))
	const active=providers.find(provider=>provider.active)
	const current=active?.reasoning_effort||''
	const apply=async(reasoningEffort:ModelReasoningEffort)=>{
		if(!active||disabled||busy||reasoningEffort===current){setOpen(false);return}
		setBusy(true)
		try{
			const updated=await api.saveModelProvider({
				id:active.id,name:active.name,kind:active.kind,base_url:active.base_url||'',model:active.model,context_window:active.context_window||null,
				reasoning_effort:reasoningEffort,api_key:'',proxy_id:active.proxy_id||'',user_agent:active.user_agent||'',
			})
			onChanged(updated)
			setOpen(false)
		}catch(err){onError(errorText(err))}
		finally{setBusy(false)}
	}
	return <details ref={detailsRef} className="composer-selector composer-reasoning" open={open} onToggle={event=>setOpen(event.currentTarget.open)}>
		<summary title={t('models.reasoningEffort')} aria-label={t('models.reasoningEffort')}>{busy?<LoaderCircle className="spin" size={13}/>:<BrainCircuit size={13}/>}<span>{current||'default'}</span><ChevronRight size={11}/></summary>
		<div className="composer-selector-menu composer-reasoning-menu">
			{reasoningEfforts.map(value=><button type="button" className={value===current?'active':''} disabled={!active||disabled||busy} onClick={()=>void apply(value)} key={value||'default'}><span><b>{value||'default'}</b></span>{value===current&&<Check size={13}/>}</button>)}
		</div>
	</details>
}

function SSHTunnelStatus({tunnels,hosts,open,onOpenChange,onStop,onCreated}:{tunnels:SSHTunnel[];hosts:Host[];open:boolean;onOpenChange:(open:boolean)=>void;onStop:(id:string)=>Promise<void>;onCreated:(tunnel:SSHTunnel)=>void}){
	const {t,i18n:instance}=useTranslation()
	const [stopping,setStopping]=useState('')
	const [creating,setCreating]=useState(false)
	const detailsRef=useAutoCollapseDetails(open,()=>onOpenChange(false))
	return <>
		<details ref={detailsRef} className="ssh-tunnel-status" open={open} onToggle={event=>onOpenChange(event.currentTarget.open)}>
			<summary title={t('tunnels.title')}><Cable size={14}/><span>{t('tunnels.short')}</span><em>{tunnels.length}</em></summary>
			<div className="ssh-tunnel-popover">
				<header><span><Cable size={15}/><b>{t('tunnels.title')}</b></span><button type="button" disabled={!hosts.length} onClick={()=>{onOpenChange(false);setCreating(true)}}><Plus size={13}/>{t('tunnels.create')}</button></header>
				<div>
					{tunnels.map(tunnel=><section className={tunnel.status} key={tunnel.id}>
						<div className="ssh-tunnel-route"><i/><code>{tunnel.host_name||tunnel.host_id}:{tunnel.remote_host}:{tunnel.remote_port}</code><span>→</span><code>localhost:{tunnel.local_port}</code></div>
						<dl><div><dt>{t('common.status')}</dt><dd>{t(`statusLabels.${tunnel.status}`,{defaultValue:tunnel.status})}</dd></div><div><dt>{t('tunnels.connections')}</dt><dd>{tunnel.active_connections} / {tunnel.total_connections}</dd></div><div><dt>{t('tunnels.traffic')}</dt><dd>↑ {formatFileSize(tunnel.bytes_sent)} · ↓ {formatFileSize(tunnel.bytes_received)}</dd></div><div><dt>{t('tunnels.started')}</dt><dd>{new Date(tunnel.started_at).toLocaleTimeString(localeFor(instance.language))}</dd></div></dl>
						<div className="ssh-tunnel-meta"><span>{tunnel.proxy_used?t('tunnels.viaProxy'):t('tunnels.direct')}</span><code>{tunnel.id}</code><button type="button" disabled={stopping===tunnel.id} onClick={async()=>{setStopping(tunnel.id);try{await onStop(tunnel.id)}finally{setStopping('')}}}>{stopping===tunnel.id?<LoaderCircle className="spin" size={12}/>:<Square size={10} fill="currentColor"/>}{t('tunnels.stop')}</button></div>
						{tunnel.error&&<p><ShieldAlert size={12}/>{tunnel.error}</p>}
					</section>)}
					{!tunnels.length&&<div className="ssh-tunnel-empty">{hosts.length?t('tunnels.empty'):t('connections.noHosts')}</div>}
				</div>
			</div>
		</details>
		{creating&&<SSHTunnelCreateDialog hosts={hosts} onCancel={()=>setCreating(false)} onCreated={tunnel=>{onCreated(tunnel);setCreating(false)}}/>}
	</>
}

function SSHTunnelCreateDialog({hosts,onCancel,onCreated}:{hosts:Host[];onCancel:()=>void;onCreated:(tunnel:SSHTunnel)=>void}){
	const {t}=useTranslation()
	const [hostID,setHostID]=useState(hosts[0]?.id||'')
	const [remoteHost,setRemoteHost]=useState('127.0.0.1')
	const [remotePort,setRemotePort]=useState('')
	const [localPort,setLocalPort]=useState('')
	const [busy,setBusy]=useState(false)
	const [error,setError]=useState('')
	const submit=async(event:FormEvent)=>{
		event.preventDefault()
		const remote=Number(remotePort),local=localPort===''?0:Number(localPort)
		if(!hostID||!remoteHost.trim()){setError(t('common.required'));return}
		if(!Number.isInteger(remote)||remote<1||remote>65535||local!==0&&(!Number.isInteger(local)||local<1||local>65535)){setError(t('tunnels.portRange'));return}
		setBusy(true);setError('')
		try{
			const tunnel=await api.startSSHTunnel({host_id:hostID,remote_host:remoteHost.trim(),remote_port:remote,local_port:local})
			onCreated(tunnel)
		}catch(err){setError(errorText(err))}
		finally{setBusy(false)}
	}
	return createPortal(<div className="connection-dialog-backdrop" onMouseDown={event=>{if(event.target===event.currentTarget&&!busy)onCancel()}}>
		<form className="connection-dialog panel" role="dialog" aria-modal="true" aria-labelledby="new-tunnel-title" noValidate onSubmit={submit}>
			<header><span><Cable size={20}/><span><small>{t('tunnels.title')}</small><h2 id="new-tunnel-title">{t('tunnels.create')}</h2></span></span><button type="button" disabled={busy} onClick={onCancel}><X size={16}/></button></header>
			<div className="connection-dialog-fields">
				<label><span>{t('common.host')}</span><AppSelect value={hostID} ariaLabel={t('common.host')} onChange={setHostID} options={hosts.map(host=>({value:host.id,label:`${host.name} · ${host.user}@${host.address}:${host.port}`}))}/></label>
				<label><span>{t('tunnels.remoteHost')}</span><input value={remoteHost} onChange={event=>setRemoteHost(event.target.value)}/></label>
				<label><span>{t('tunnels.remotePort')}</span><input inputMode="numeric" value={remotePort} onChange={event=>setRemotePort(event.target.value.replace(/\D/g,'').slice(0,5))} autoFocus/></label>
				<label><span>{t('tunnels.localPort')}</span><input inputMode="numeric" value={localPort} onChange={event=>setLocalPort(event.target.value.replace(/\D/g,'').slice(0,5))} placeholder={t('tunnels.automaticPort')}/></label>
			</div>
			{error&&<p className="connection-dialog-error"><ShieldAlert size={14}/>{error}</p>}
			<footer><button type="button" disabled={busy} onClick={onCancel}>{t('common.cancel')}</button><button className="primary" disabled={busy||!hostID}>{busy?<LoaderCircle className="spin" size={14}/>:<Plus size={14}/>} {busy?t('tunnels.starting'):t('tunnels.start')}</button></footer>
		</form>
	</div>,document.body)
}

function SSHShellStatus({shells,hosts,open,onOpenChange,onOpen,onCreated}:{shells:SSHShell[];hosts:Host[];open:boolean;onOpenChange:(open:boolean)=>void;onOpen:(shell:SSHShell)=>void;onCreated:(shell:SSHShell)=>void}){
	const {t}=useTranslation()
	const [creating,setCreating]=useState(false)
	const detailsRef=useAutoCollapseDetails(open,()=>onOpenChange(false))
	return <>
		<details ref={detailsRef} className="ssh-shell-status" open={open} onToggle={event=>onOpenChange(event.currentTarget.open)}>
			<summary title={t('sshShell.title')}><TerminalSquare size={14}/><span>{t('sshShell.short')}</span><em>{shells.length}</em></summary>
			<div className="ssh-shell-popover">
				<header><span><TerminalSquare size={15}/><b>{t('sshShell.title')}</b></span><button type="button" disabled={!hosts.length} onClick={()=>{onOpenChange(false);setCreating(true)}}><Plus size={13}/>{t('sshShell.create')}</button></header>
				<div>
					{shells.map(shell=><button type="button" className={shell.status} onClick={()=>onOpen(shell)} key={shell.id}>
						<span><i/><b>{shell.kind==='workspace'?`${t('common.workspace')} · ${shell.workspace_id}`:shell.host_name||shell.host_id}</b><code>{shell.kind==='workspace'?t('workspace.agent'):shell.elevated?'root':shell.user}</code></span>
						<small>{shell.cwd||(shell.kind==='workspace'?'.':'~')}</small>
						<ChevronRight size={14}/>
					</button>)}
					{!shells.length&&<div className="ssh-shell-empty">{hosts.length?t('sshShell.empty'):t('connections.noHosts')}</div>}
				</div>
			</div>
		</details>
		{creating&&<SSHShellCreateDialog hosts={hosts} onCancel={()=>setCreating(false)} onCreated={shell=>{onCreated(shell);setCreating(false)}}/>}
	</>
}

function SSHShellCreateDialog({hosts,surface,onCancel,onCreated}:{hosts:Host[];surface?:'quick'|'workspace';onCancel:()=>void;onCreated:(shell:SSHShell)=>void}){
	const {t}=useTranslation()
	const [hostID,setHostID]=useState(hosts[0]?.id||'')
	const [busy,setBusy]=useState(false)
	const [error,setError]=useState('')
	const submit=async(event:FormEvent)=>{
		event.preventDefault()
		setBusy(true);setError('')
		try{
			const shell=await api.startSSHShell({host_id:hostID,...(surface?{surface}:{})})
			onCreated(shell)
		}catch(err){setError(errorText(err))}
		finally{setBusy(false)}
	}
	return createPortal(<div className="connection-dialog-backdrop" onMouseDown={event=>{if(event.target===event.currentTarget&&!busy)onCancel()}}>
		<form className="connection-dialog compact panel" role="dialog" aria-modal="true" aria-labelledby="new-shell-title" noValidate onSubmit={submit}>
			<header><span><TerminalSquare size={20}/><span><small>{t('sshShell.title')}</small><h2 id="new-shell-title">{t('sshShell.create')}</h2></span></span><button type="button" disabled={busy} onClick={onCancel}><X size={16}/></button></header>
			<div className="connection-dialog-fields single">
				<label><span>{t('common.host')}</span><AppSelect value={hostID} ariaLabel={t('common.host')} onChange={setHostID} options={hosts.map(host=>({value:host.id,label:`${host.name} · ${host.user}@${host.address}:${host.port}`}))}/></label>
			</div>
			{error&&<p className="connection-dialog-error"><ShieldAlert size={14}/>{error}</p>}
			<footer><button type="button" disabled={busy} onClick={onCancel}>{t('common.cancel')}</button><button className="primary" disabled={busy||!hostID}>{busy?<LoaderCircle className="spin" size={14}/>:<TerminalSquare size={14}/>} {busy?t('sshShell.starting'):t('sshShell.start')}</button></footer>
		</form>
	</div>,document.body)
}

function sshShellActive(status:string){return ['starting','running','stopping'].includes(status)}

function SSHShellTerminal({initialShell,relatedShells=[],onSelect,onClose,onChanged,onError,embedded=false}:{initialShell:SSHShell;relatedShells?:SSHShell[];onSelect?:(shell:SSHShell)=>void;onClose:()=>void;onChanged:()=>void;onError:(message:string)=>void;embedded?:boolean}){
	const {t}=useTranslation()
	const [shell,setShell]=useState(initialShell)
	const [secret,setSecret]=useState('')
	const [sendingSecret,setSendingSecret]=useState(false)
	const [closing,setClosing]=useState(false)
	const [inputSource,setInputSource]=useState<'agent'|'operator'|''>('')
	const terminalElement=useRef<HTMLDivElement>(null)
	const terminalRef=useRef<XTermInstance|null>(null)
	const lastSequence=useRef(0)
	const onChangedRef=useRef(onChanged)
	const onErrorRef=useRef(onError)
	onChangedRef.current=onChanged
	onErrorRef.current=onError
	const active=sshShellActive(shell.status)

	useEffect(()=>{
		const container=terminalElement.current
		if(!container)return
		let disposed=false
		let cleanup=()=>{}
		void Promise.all([import('@xterm/xterm'),import('@xterm/addon-fit')]).then(([xtermModule,fitModule])=>{
			if(disposed)return
			const terminal=new xtermModule.Terminal({
				cursorBlink:true,
				convertEol:false,
				fontFamily:"'JetBrains Mono','Cascadia Code','SFMono-Regular',Consolas,monospace",
				fontSize:13,
				theme:{background:'#071019',foreground:'#d8e3ea',cursor:'#55d6be',selectionBackground:'#31546a'},
				scrollback:4_294_967_295,
			})
			const fit=new fitModule.FitAddon()
			terminal.loadAddon(fit)
			terminal.open(container)
			terminalRef.current=terminal
			fit.fit()
			terminal.focus()

			let inputBuffer=''
			let inputTimer:number|undefined
			let sendChain=Promise.resolve()
			const flushInput=()=>{
				if(inputTimer!==undefined)window.clearTimeout(inputTimer)
				inputTimer=undefined
				const input=inputBuffer
				inputBuffer=''
				if(!input)return
				sendChain=sendChain.then(()=>api.sshShellInput(initialShell.id,input,false)).catch(err=>onErrorRef.current(errorText(err)))
			}
			const inputDisposable=terminal.onData(data=>{
				inputBuffer+=data
				if(data.includes('\r')||data.includes('\n')||data.includes('\x03'))flushInput()
				else if(inputTimer===undefined)inputTimer=window.setTimeout(flushInput,24)
			})
			let resizeTimer:number|undefined
			const resizeDisposable=terminal.onResize(({cols,rows})=>{
				if(resizeTimer!==undefined)window.clearTimeout(resizeTimer)
				resizeTimer=window.setTimeout(()=>{void api.resizeSSHShell(initialShell.id,cols,rows).catch(()=>{/* the session may have ended */})},120)
			})
			const observer=new ResizeObserver(()=>fit.fit())
			observer.observe(container)

			const source=new EventSource(sshShellEventsURL(initialShell.id,0))
			const onShellEvent=(raw:Event)=>{
				const message=raw as MessageEvent<string>
				try{
					const event=JSON.parse(message.data) as SSHShellEvent
					if(event.sequence<=lastSequence.current)return
					lastSequence.current=event.sequence
					if(event.content&&(event.stream==='stdout'||event.stream==='stderr'))terminal.write(event.content)
					if(event.stream==='input'&&(event.source==='agent'||event.source==='operator'))setInputSource(event.source)
					if(event.status&&event.stream==='status'){
						setShell(current=>({...current,status:event.status as SSHShell['status'],last_sequence:event.sequence}))
						if(!sshShellActive(event.status)){
							source.close()
							void api.sshShell(initialShell.id,event.sequence).then(snapshot=>setShell(snapshot.shell)).catch(()=>{/* final state is already visible */})
							onChangedRef.current()
						}
					}
				}catch{/* malformed events are ignored; the next sequence remains recoverable */}
			}
			source.addEventListener('shell-event',onShellEvent)
			source.onerror=()=>{if(source.readyState===EventSource.CLOSED)onErrorRef.current(t('sshShell.streamEnded'))}
			cleanup=()=>{
				flushInput()
				source.removeEventListener('shell-event',onShellEvent)
				source.close()
				observer.disconnect()
				inputDisposable.dispose()
				resizeDisposable.dispose()
				if(inputTimer!==undefined)window.clearTimeout(inputTimer)
				if(resizeTimer!==undefined)window.clearTimeout(resizeTimer)
				terminal.dispose()
				terminalRef.current=null
			}
			if(disposed)cleanup()
		}).catch(err=>onErrorRef.current(errorText(err)))
		return()=>{disposed=true;cleanup()}
	},[initialShell.id,t])

	const sendSecret=async(event:FormEvent)=>{
		event.preventDefault()
		if(!secret||!active||sendingSecret)return
		setSendingSecret(true)
		try{await api.sshShellInput(shell.id,`${secret}\r`,true);setSecret('');terminalRef.current?.focus()}
		catch(err){onError(errorText(err))}
		finally{setSendingSecret(false)}
	}
	const interrupt=async()=>{try{setShell(await api.interruptSSHShell(shell.id))}catch(err){onError(errorText(err))}finally{terminalRef.current?.focus()}}
	const stop=async()=>{setClosing(true);try{setShell(await api.closeSSHShell(shell.id));onChanged()}catch(err){onError(errorText(err))}finally{setClosing(false)}}
	const titleID=`ssh-shell-terminal-title-${shell.id}`
	const workspaceShell=shell.kind==='workspace'
	const terminal=<section className={`ssh-shell-terminal-dialog ${embedded?'embedded':''}`} role={embedded?undefined:'dialog'} aria-modal={embedded?undefined:true} aria-labelledby={titleID}>
			<header>
				<div><TerminalSquare size={20}/><span><small>{workspaceShell?t('workspace.terminal'):t('sshShell.interactive')}</small><h2 id={titleID}>{workspaceShell?shell.workspace_id:shell.host_name||shell.host_id}</h2></span></div>
				<div className="ssh-shell-terminal-state">{workspaceShell&&relatedShells.length>1&&<AppSelect className="terminal-session-select" value={shell.id} ariaLabel={t('workspace.switchTerminal')} onChange={value=>{const selected=relatedShells.find(item=>item.id===value);if(selected)onSelect?.(selected)}} options={relatedShells.map(item=>({value:item.id,label:`${t(item.surface==='workspace_agent'?'workspace.agent':'workspace.operator')} · ${item.cwd||'.'}`}))}/>}<em className={shell.status}>{t(`statusLabels.${shell.status}`,{defaultValue:shell.status})}</em><code>{shell.elevated?'root':shell.user}</code>{!embedded&&<button type="button" onClick={onClose} title={t('common.close')}><X size={16}/></button>}</div>
			</header>
			<div className="ssh-shell-terminal-meta"><span>{workspaceShell?shell.backend:shell.host_id}</span><span>{shell.cwd||(workspaceShell?'.':'~')}</span>{workspaceShell&&<span className="workspace-shell-owner">{t(shell.surface==='workspace_agent'?'workspace.agent':'workspace.operator')}</span>}{workspaceShell&&inputSource&&<span>{t('workspace.inputBy',{source:t(inputSource==='agent'?'workspace.agent':'workspace.operator')})}</span>}{shell.termination_reason&&<span>{t(`sshShell.termination.${shell.termination_reason}`,{defaultValue:shell.termination_reason})}</span>}<code>{shell.id}</code></div>
			<div className="ssh-shell-terminal-screen" ref={terminalElement}/>
			<footer>
				<form onSubmit={sendSecret}><LockKeyhole size={14}/><PasswordInput value={secret} onChange={event=>setSecret(event.target.value)} disabled={!active||sendingSecret} placeholder={t('sshShell.sensitivePlaceholder')} autoComplete="off"/><button className="primary" disabled={!secret||!active||sendingSecret}>{sendingSecret?<LoaderCircle className="spin" size={13}/>:<Send size={13}/>} {t('sshShell.sendSensitive')}</button></form>
				<div><button type="button" disabled={!active} onClick={()=>void interrupt()}><Square size={10}/>{t('sshShell.interrupt')}</button><button type="button" className="danger" disabled={!active||closing} onClick={()=>void stop()}>{closing?<LoaderCircle className="spin" size={13}/>:<Power size={13}/>} {t('sshShell.closeSession')}</button></div>
			</footer>
			{shell.error&&<p className="ssh-shell-terminal-error"><ShieldAlert size={13}/>{shell.error}</p>}
		</section>
	return embedded?terminal:<div className="ssh-shell-terminal-backdrop">{terminal}</div>
}

function SSHWorkspacePage({hosts,shells,onCreated,refresh,onError}:{hosts:Host[];shells:SSHShell[];onCreated:(shell:SSHShell)=>void;refresh:()=>Promise<void>;onError:(message:string)=>void}){
	const {t}=useTranslation()
	const [selectedShellID,setSelectedShellID]=useState(shells[0]?.id||'')
	const [creating,setCreating]=useState(false)
	useEffect(()=>{
		if(!shells.some(shell=>shell.id===selectedShellID))setSelectedShellID(shells[0]?.id||'')
	},[selectedShellID,shells])
	const selectedShell=shells.find(shell=>shell.id===selectedShellID)
	const selectedHost=hosts.find(host=>host.id===selectedShell?.host_id)
	const created=(shell:SSHShell)=>{onCreated(shell);setSelectedShellID(shell.id);setCreating(false)}
	if(!hosts.length)return <div className="ssh-workspace-empty panel"><Server size={28}/><b>{t('connections.noHosts')}</b></div>
	return <div className="ssh-workspace">
		<SFTPBrowser key={selectedHost?.id||'disconnected'} host={selectedHost}/>
		<section className="ssh-workspace-terminal panel">
			<header className="ssh-terminal-tabs">
				<div>{shells.map(shell=><button type="button" className={shell.id===selectedShellID?'active':''} onClick={()=>setSelectedShellID(shell.id)} key={shell.id}><i className={shell.status}/><span>{shell.host_name||shell.host_id}</span><small>{shell.elevated?'root':shell.user}</small></button>)}</div>
				<button type="button" className="ssh-new-terminal" onClick={()=>setCreating(true)}><Plus size={14}/> {t('sshWorkspace.newTerminal')}</button>
			</header>
			<div className="ssh-terminal-stage">
				{selectedShell?<SSHShellTerminal key={selectedShell.id} initialShell={selectedShell} embedded onClose={()=>setSelectedShellID('')} onChanged={()=>void refresh()} onError={onError}/>:<div className="ssh-terminal-empty"><TerminalSquare size={32}/><b>{t('sshWorkspace.noTerminal')}</b><button type="button" className="primary" onClick={()=>setCreating(true)}><Plus size={14}/> {t('sshWorkspace.newTerminal')}</button></div>}
			</div>
		</section>
		{creating&&<SSHShellCreateDialog hosts={hosts} surface="workspace" onCancel={()=>setCreating(false)} onCreated={created}/>}
	</div>
}

type SFTPNameEditor={mode:'create'}|{mode:'rename';entry:SFTPFileEntry}
type SFTPDeleteCandidate={entry:SFTPFileEntry}
type SFTPOverwriteCandidate={file:File;path:string}
type SFTPTextEncoding='utf-8'|'utf-16le'|'utf-16be'|'gb18030'
type SFTPTextFile={entry:SFTPFileEntry;content:string;binary:boolean;encoding:SFTPTextEncoding}

function remoteChildPath(parent:string,name:string){return parent==='/'?`/${name}`:`${parent}/${name}`}
function remoteParentPath(value:string){if(!value||value==='/')return '/';const parts=value.split('/').filter(Boolean);parts.pop();return `/${parts.join('/')}`||'/'}
function textFileName(name:string){
	const extension=name.toLowerCase().match(/(?:^|\.)([^./]+)$/)?.[1]||''
	return new Set(['txt','md','markdown','json','jsonl','yaml','yml','toml','ini','conf','config','env','properties','xml','html','htm','css','scss','less','js','jsx','ts','tsx','mjs','cjs','go','rs','py','rb','php','java','kt','kts','c','h','cc','cpp','hpp','cs','swift','sh','bash','zsh','fish','ps1','bat','cmd','sql','csv','tsv','log','service','socket','timer']).has(extension)
}
function utf16Encoding(bytes:Uint8Array,name:string):SFTPTextEncoding|''{
	if(bytes.length>=2&&bytes[0]===0xff&&bytes[1]===0xfe)return'utf-16le'
	if(bytes.length>=2&&bytes[0]===0xfe&&bytes[1]===0xff)return'utf-16be'
	if(!textFileName(name)||bytes.length<4)return''
	const sample=Math.min(bytes.length,4096)
	let evenZeros=0,oddZeros=0,pairs=0
	for(let index=0;index+1<sample;index+=2){if(bytes[index]===0)evenZeros+=1;if(bytes[index+1]===0)oddZeros+=1;pairs+=1}
	if(!pairs)return''
	if(oddZeros/pairs>.3&&evenZeros/pairs<.1)return'utf-16le'
	if(evenZeros/pairs>.3&&oddZeros/pairs<.1)return'utf-16be'
	return''
}
function decodeTextFile(buffer:ArrayBuffer,name:string){
	const bytes=new Uint8Array(buffer)
	const utf16=utf16Encoding(bytes,name)
	if(utf16)return{content:new TextDecoder(utf16,{fatal:true}).decode(bytes),binary:false,encoding:utf16}
	if(bytes.includes(0))return{content:'',binary:true,encoding:'utf-8' as const}
	try{return{content:new TextDecoder('utf-8',{fatal:true}).decode(bytes),binary:false,encoding:'utf-8' as const}}
	catch{
		if(textFileName(name)){
			try{return{content:new TextDecoder('gb18030',{fatal:true}).decode(bytes),binary:false,encoding:'gb18030' as const}}
			catch{/* invalid text remains binary */}
		}
		return{content:'',binary:true,encoding:'utf-8' as const}
	}
}

function SFTPBrowser({host}:{host?:Host}){
	const {t,i18n:instance}=useTranslation()
	const hostID=host?.id||''
	const [path,setPath]=useState('')
	const [pathInput,setPathInput]=useState('')
	const [entries,setEntries]=useState<SFTPFileEntry[]>([])
	const [loading,setLoading]=useState(false)
	const [busy,setBusy]=useState(false)
	const [listError,setListError]=useState('')
	const [notice,setNotice]=useState('')
	const [noticeError,setNoticeError]=useState(false)
	const [dragging,setDragging]=useState(false)
	const [inputKey,setInputKey]=useState(0)
	const [nameEditor,setNameEditor]=useState<SFTPNameEditor|null>(null)
	const [deleteCandidate,setDeleteCandidate]=useState<SFTPDeleteCandidate|null>(null)
	const [overwriteCandidate,setOverwriteCandidate]=useState<SFTPOverwriteCandidate|null>(null)
	const [textFile,setTextFile]=useState<SFTPTextFile|null>(null)
	const [openingFile,setOpeningFile]=useState('')
	const loadRequest=useRef(0)
	const load=useCallback(async(target='')=>{
		if(!hostID)return
		const request=++loadRequest.current
		setLoading(true);setListError('')
		try{
			const result=await api.sftpEntries(hostID,target)
			if(request!==loadRequest.current)return
			setPath(result.path);setPathInput(result.path);setEntries(result.entries||[])
		}catch(err){
			if(request!==loadRequest.current)return
			setEntries([]);setListError(errorText(err))
		}finally{if(request===loadRequest.current)setLoading(false)}
	},[hostID])
	useEffect(()=>{void load('')},[load])
	const download=(entry:SFTPFileEntry)=>{
		const anchor=document.createElement('a')
		anchor.href=sftpDownloadURL(hostID,entry.path);anchor.download=entry.name
		document.body.appendChild(anchor);anchor.click();anchor.remove()
	}
	const openTextFile=async(entry:SFTPFileEntry)=>{
		if(openingFile)return
		setOpeningFile(entry.path);setNotice('');setNoticeError(false)
		try{
			const decoded=decodeTextFile(await api.sftpFile(hostID,entry.path),entry.name)
			setTextFile({entry,...decoded})
		}catch(err){setNotice(errorText(err));setNoticeError(true)}
		finally{setOpeningFile('')}
	}
	const uploadFiles=async(files:File[],overwrite=false)=>{
		if(!files.length||busy)return
		setBusy(true);setNotice('');setNoticeError(false)
		let uploaded=0
		for(const file of files){
			const target=remoteChildPath(path,file.name)
			try{await api.uploadSFTPFile(hostID,target,file,overwrite);uploaded+=1}
			catch(err){
				const message=errorText(err)
				if(files.length===1&&!overwrite&&message.includes('already exists'))setOverwriteCandidate({file,path:target})
				else{setNotice(message);setNoticeError(true)}
				break
			}
		}
		if(uploaded){setNotice(t('sshWorkspace.uploaded',{count:uploaded}));setNoticeError(false)}
		setInputKey(value=>value+1);setBusy(false)
		if(uploaded)await load(path)
	}
	const saveName=async(name:string)=>{
		if(!name.trim()||name==='.'||name==='..'||name.includes('/'))return
		setBusy(true);setNotice('');setNoticeError(false)
		try{
			if(nameEditor?.mode==='create'){
				await api.createSFTPDirectory(hostID,remoteChildPath(path,name))
				setNotice(t('sshWorkspace.directoryCreated'))
			}else if(nameEditor?.mode==='rename'){
				await api.renameSFTPEntry(hostID,nameEditor.entry.path,remoteChildPath(path,name))
				setNotice(t('sshWorkspace.renamed'))
			}
			setNameEditor(null);await load(path)
		}catch(err){setNotice(errorText(err));setNoticeError(true)}
		finally{setBusy(false)}
	}
	const remove=async()=>{
		if(!deleteCandidate)return
		setBusy(true);setNotice('');setNoticeError(false)
		try{
			await api.deleteSFTPEntry(hostID,deleteCandidate.entry.path,deleteCandidate.entry.type==='directory')
			setNotice(t('sshWorkspace.deleted'));setDeleteCandidate(null);await load(path)
		}catch(err){setNotice(errorText(err));setNoticeError(true)}
		finally{setBusy(false)}
	}
	const overwrite=async()=>{
		if(!overwriteCandidate)return
		const candidate=overwriteCandidate
		setBusy(true);setNotice('');setNoticeError(false)
		try{
			await api.uploadSFTPFile(hostID,candidate.path,candidate.file,true)
			setNotice(t('sshWorkspace.uploaded',{count:1}));setOverwriteCandidate(null);await load(path)
		}catch(err){setNotice(errorText(err));setNoticeError(true)}
		finally{setBusy(false)}
	}
	const saveTextFile=async(content:string)=>{
		if(!textFile)return
		const result=await api.uploadSFTPTextFile(hostID,textFile.entry.path,content,textFile.encoding)
		setTextFile({entry:result.entry,content,binary:false,encoding:textFile.encoding})
		setNotice(t('sshWorkspace.saved',{path:textFile.entry.path}));setNoticeError(false)
		await load(path)
	}
	const acceptsFiles=(event:React.DragEvent<HTMLElement>)=>Array.from(event.dataTransfer.types).includes('Files')
	return <>
		<aside className={`sftp-browser panel ${dragging?'dragging':''}`} onDragEnter={event=>{if(hostID&&acceptsFiles(event)){event.preventDefault();setDragging(true)}}} onDragOver={event=>{if(hostID&&acceptsFiles(event)){event.preventDefault();event.dataTransfer.dropEffect=busy?'none':'copy'}}} onDragLeave={event=>{event.preventDefault();if(!(event.relatedTarget instanceof Node&&event.currentTarget.contains(event.relatedTarget)))setDragging(false)}} onDrop={event=>{if(!hostID||!acceptsFiles(event))return;event.preventDefault();setDragging(false);if(!busy)void uploadFiles(Array.from(event.dataTransfer.files))}}>
			<header><div><FolderOpen size={17}/><b>SFTP</b></div><span className="sftp-host">{host?`${host.name} · ${host.user}@${host.address}`:'—'}</span></header>
			<form className="sftp-path" onSubmit={event=>{event.preventDefault();void load(pathInput)}}><button type="button" disabled={!path||path==='/'} onClick={()=>void load(remoteParentPath(path))} title={t('workspace.parent')}>‹</button><input value={pathInput} disabled={!hostID} onChange={event=>setPathInput(event.target.value)} aria-label={t('sshWorkspace.remotePath')}/><button type="submit" disabled={!hostID||loading}><ChevronRight size={13}/></button><button type="button" disabled={!hostID||loading} onClick={()=>void load(path)} title={t('common.refresh')}><RefreshCw className={loading?'spin':''} size={13}/></button></form>
			<div className="sftp-actions"><button type="button" disabled={busy||!path} onClick={()=>setNameEditor({mode:'create'})}><Plus size={13}/>{t('sshWorkspace.newDirectory')}</button><label className={busy||!path?'disabled':''}><UploadCloud size={13}/>{t('common.upload')}<input key={inputKey} type="file" multiple disabled={busy||!path} onChange={event=>void uploadFiles(Array.from(event.target.files||[]))}/></label></div>
			<div className="sftp-list">{!hostID?<span className="sftp-state">{t('sshWorkspace.noTerminal')}</span>:loading?<span className="sftp-state"><LoaderCircle className="spin" size={14}/>{t('common.loading')}</span>:listError?<span className="sftp-state error">{listError}</span>:entries.length?entries.map(entry=><div className="sftp-row" key={`${entry.type}:${entry.path}`}><button type="button" className="sftp-entry" onClick={()=>entry.type==='directory'?void load(entry.path):void openTextFile(entry)} title={entry.path}>{openingFile===entry.path?<LoaderCircle className="spin" size={14}/>:entry.type==='directory'?<FolderOpen size={14}/>:<FileText size={14}/>}<span><b>{entry.name}</b><small>{entry.mode} · {entry.type==='directory'?'—':formatFileSize(entry.size||0)} · {new Date(entry.modified_at).toLocaleString(localeFor(instance.language))}</small></span></button>{entry.type!=='directory'&&<button type="button" onClick={()=>download(entry)} title={t('common.download')}><Download size={12}/></button>}<button type="button" onClick={()=>setNameEditor({mode:'rename',entry})} title={t('sshWorkspace.rename')}><Edit3 size={12}/></button><button type="button" className="danger" onClick={()=>setDeleteCandidate({entry})} title={t('common.delete')}><Trash2 size={12}/></button></div>):<span className="sftp-state">{t('workspace.emptyDirectory')}</span>}</div>
			{notice&&<div className={`sftp-notice ${noticeError?'error':''}`}>{notice}<button onClick={()=>setNotice('')}><X size={11}/></button></div>}
			{dragging&&<div className="sftp-drop"><UploadCloud size={28}/><b>{t('workspace.dropFilesHere')}</b></div>}
		</aside>
		{nameEditor&&<SFTPNameDialog mode={nameEditor.mode} initialName={nameEditor.mode==='rename'?nameEditor.entry.name:''} busy={busy} onCancel={()=>setNameEditor(null)} onConfirm={name=>void saveName(name)}/>}
		{deleteCandidate&&<DestructiveConfirmDialog title={t('sshWorkspace.deleteTitle',{name:deleteCandidate.entry.name})} busy={busy} onCancel={()=>setDeleteCandidate(null)} onConfirm={()=>void remove()}/>}
		{overwriteCandidate&&<SFTPOverwriteDialog path={overwriteCandidate.path} busy={busy} onCancel={()=>setOverwriteCandidate(null)} onConfirm={()=>void overwrite()}/>}
		{textFile&&<TextFileEditor path={textFile.entry.path} meta={`${textFile.entry.mode} · ${formatFileSize(textFile.entry.size||0)} · ${textFile.encoding.toUpperCase()} · ${new Date(textFile.entry.modified_at).toLocaleString(localeFor(instance.language))}`} content={textFile.content} binary={textFile.binary} editable onClose={()=>setTextFile(null)} onSave={saveTextFile} onDownload={()=>download(textFile.entry)}/>}
	</>
}

function SFTPNameDialog({mode,initialName,busy,onCancel,onConfirm}:{mode:'create'|'rename';initialName:string;busy:boolean;onCancel:()=>void;onConfirm:(name:string)=>void}){
	const {t}=useTranslation()
	const [name,setName]=useState(initialName)
	const valid=!!name.trim()&&name!=='.'&&name!=='..'&&!name.includes('/')
	return createPortal(<div className="connection-dialog-backdrop" onMouseDown={event=>{if(event.target===event.currentTarget&&!busy)onCancel()}}><form className="connection-dialog compact panel" noValidate onSubmit={event=>{event.preventDefault();if(valid)onConfirm(name)}}><header><span><FolderOpen size={19}/><span><small>SFTP</small><h2>{t(mode==='create'?'sshWorkspace.newDirectory':'sshWorkspace.rename')}</h2></span></span><button type="button" disabled={busy} onClick={onCancel}><X size={15}/></button></header><div className="connection-dialog-fields single"><label><span>{t('sshWorkspace.name')}</span><input value={name} onChange={event=>setName(event.target.value)} autoFocus/></label></div><footer><button type="button" disabled={busy} onClick={onCancel}>{t('common.cancel')}</button><button className="primary" disabled={busy||!valid}>{busy?<LoaderCircle className="spin" size={13}/>:<Save size={13}/>} {t('common.save')}</button></footer></form></div>,document.body)
}

function SFTPOverwriteDialog({path,busy,onCancel,onConfirm}:{path:string;busy:boolean;onCancel:()=>void;onConfirm:()=>void}){
	const {t}=useTranslation()
	return createPortal(<div className="connection-dialog-backdrop" onMouseDown={event=>{if(event.target===event.currentTarget&&!busy)onCancel()}}><section className="sftp-overwrite-dialog panel"><header><FileText size={19}/><h2>{t('sshWorkspace.overwriteTitle')}</h2></header><code>{path}</code><footer><button type="button" disabled={busy} onClick={onCancel}>{t('common.cancel')}</button><button type="button" className="primary" disabled={busy} onClick={onConfirm}>{busy?<LoaderCircle className="spin" size={13}/>:<UploadCloud size={13}/>} {t('sshWorkspace.overwrite')}</button></footer></section></div>,document.body)
}

type PasswordInputProps=Omit<React.InputHTMLAttributes<HTMLInputElement>,'type'>

function PasswordInput(props:PasswordInputProps){
	const {t}=useTranslation()
	const [visible,setVisible]=useState(false)
	const label=t(visible?'common.hidePassword':'common.showPassword')
	return <div className="password-input"><input {...props} type={visible?'text':'password'}/><button type="button" aria-label={label} aria-pressed={visible} title={label} onClick={()=>setVisible(value=>!value)}>{visible?<EyeOff size={16}/>:<Eye size={16}/>}</button></div>
}


function DestructiveConfirmDialog({title,busy,onCancel,onConfirm}:{title:string;busy:boolean;onCancel:()=>void;onConfirm:()=>void}){
	const {t}=useTranslation()
	useEffect(()=>{const close=(event:KeyboardEvent)=>{if(event.key==='Escape'&&!busy)onCancel()};window.addEventListener('keydown',close);return()=>window.removeEventListener('keydown',close)},[busy,onCancel])
	return <div className="destructive-dialog-backdrop" onMouseDown={event=>{if(event.target===event.currentTarget&&!busy)onCancel()}}><section className="destructive-dialog panel" role="dialog" aria-modal="true" aria-labelledby="destructive-dialog-title"><header><Trash2 size={21}/><h2 id="destructive-dialog-title">{title}</h2></header><footer><button type="button" autoFocus disabled={busy} onClick={onCancel}>{t('common.cancel')}</button><button type="button" className="danger" disabled={busy} onClick={onConfirm}>{busy?<LoaderCircle className="spin" size={14}/>:<Trash2 size={14}/>} {busy?t('common.deleting'):t('common.delete')}</button></footer></section></div>
}

function FullAccessConfirmDialog({onCancel,onConfirm}:{onCancel:()=>void;onConfirm:()=>void}){
	const {t}=useTranslation()
	return createPortal(<div className="destructive-dialog-backdrop" onMouseDown={event=>{if(event.target===event.currentTarget)onCancel()}}><section className="destructive-dialog panel" role="dialog" aria-modal="true" aria-labelledby="full-access-dialog-title"><header><ShieldAlert size={21}/><h2 id="full-access-dialog-title">{t('settings.fullAccessTitle')}</h2></header><footer><button type="button" autoFocus onClick={onCancel}>{t('common.cancel')}</button><button type="button" className="danger" onClick={onConfirm}><ShieldAlert size={14}/>{t('common.enable')}</button></footer></section></div>,document.body)
}

type ConfigurationSection = 'models' | 'hosts' | 'proxies' | 'system'

function ConfigurationEditorPage({icon,title,busy,onBack,children}:{icon:React.ReactNode;title:string;busy?:boolean;onBack:()=>void;children:React.ReactNode}){
	const {t}=useTranslation()
	return <div className="configuration-editor-page">
		<button type="button" className="configuration-editor-back" disabled={busy} onClick={onBack}><ChevronLeft size={16}/>{t('config.backToList')}</button>
		<header className="configuration-editor-header panel"><div>{icon}</div><span><small>{t('config.editor')}</small><h2>{title}</h2></span></header>
		{children}
	</div>
}

function ConfigurationPage({hosts,providers,proxies,settings,capabilities,health,refresh}:{hosts:Host[];providers:ModelProvider[];proxies:Proxy[];settings:SystemSettings|null;capabilities:ToolCapabilities;health:Health|null;refresh:()=>Promise<void>}) {
  const {t}=useTranslation()
  const [section,setSection]=useState<ConfigurationSection>('models')
  const [showAddresses,setShowAddresses]=useState(false)
  const tabs:[ConfigurationSection,React.ReactNode,string,string][]=[
    ['models',<Cpu size={17}/>, t('config.tabs.models'), t('config.configured',{count:providers.length})],
    ['hosts',<Server size={17}/>, t('config.tabs.hosts'), t('config.registered',{count:hosts.length})],
    ['proxies',<Cable size={17}/>, t('config.tabs.proxies'), t('config.configured',{count:proxies.length})],
    ['system',<SlidersHorizontal size={17}/>, t('config.tabs.system'), t('config.maxIterations',{count:settings?.agent_max_iterations??50})],
	  ]
	  return <div className="configuration-center page-stack">
	    <div className="section-tabs-row"><div className="configuration-tabs" role="tablist" aria-label={t('config.sections')}>{tabs.map(([id,icon,label,meta])=><button type="button" role="tab" aria-selected={section===id} className={section===id?'active':''} onClick={()=>setSection(id)} key={id}>{icon}<span><b>{label}</b><small>{meta}</small></span><ChevronRight size={15}/></button>)}</div></div>
    <div className="configuration-content" role="tabpanel">
	  {section==='models'&&(
		<ModelsPage providers={providers} proxies={proxies} showAddresses={showAddresses} onToggleAddresses={()=>setShowAddresses(value=>!value)} refresh={refresh}/>
	  )}
	  {section==='hosts'&&(
		<HostsPage hosts={hosts} proxies={proxies} showAddresses={showAddresses} onToggleAddresses={()=>setShowAddresses(value=>!value)} refresh={refresh}/>
	  )}
	  {section==='proxies'&&(
		<ProxiesPage proxies={proxies} showAddresses={showAddresses} onToggleAddresses={()=>setShowAddresses(value=>!value)} refresh={refresh}/>
	  )}
	  {section==='system'&&<SystemSettingsPage settings={settings} providers={providers} proxies={proxies} capabilities={capabilities} modelStatus={health?.model} refresh={refresh}/>}
    </div>
  </div>
}

function AddressVisibilityButton({visible,onToggle}:{visible:boolean;onToggle:()=>void}){
	const {t}=useTranslation()
	const label=t(visible?'config.hideAddresses':'config.showAddresses')
	return <button type="button" className={`icon-button configuration-address-toggle ${visible?'active':''}`} aria-label={label} title={label} onClick={onToggle}>{visible?<EyeOff size={17}/>:<Eye size={17}/>}</button>
}

type ExtensionSection = 'overview' | 'skills' | 'mcp' | 'tools'

function ExtensionsPage({skills,mcpServers,toolCatalog,refresh}:{skills:ManagedSkill[];mcpServers:MCPServer[];toolCatalog:LLMToolCatalog|null;refresh:()=>Promise<void>}){
	const {t}=useTranslation()
		const [section,setSection]=useState<ExtensionSection>('overview')
		const enabledSkills=skills.filter(skill=>skill.enabled).length
		const readyMCP=mcpServers.filter(server=>server.status==='ready').length
		const tabs:[ExtensionSection,React.ReactNode,string,string][]=[
		['overview',<Braces size={17}/>, t('extensions.tabs.overview'), t('extensions.active',{count:enabledSkills+readyMCP})],
		['skills',<BookOpen size={17}/>, t('extensions.tabs.skills'), t('extensions.enabledRatio',{enabled:enabledSkills,total:skills.length})],
		['mcp',<Zap size={17}/>, t('extensions.tabs.mcp'), t('extensions.readyRatio',{ready:readyMCP,total:mcpServers.length})],
		['tools',<FunctionSquare size={17}/>, t('extensions.tabs.tools'), t('extensions.loaded',{count:toolCatalog?.count??0})],
		]
		return <div className="extensions-center page-stack">
			<div className="section-tabs-row"><div className="extension-tabs configuration-tabs" role="tablist" aria-label={t('extensions.sections')}>{tabs.map(([id,icon,label,meta])=><button type="button" role="tab" aria-selected={section===id} className={section===id?'active':''} onClick={()=>setSection(id)} key={id}>{icon}<span><b>{label}</b><small>{meta}</small></span><ChevronRight size={15}/></button>)}</div></div>
		<div className="configuration-content" role="tabpanel">
			{section==='overview'&&<div className="extension-overview"><button className="panel" onClick={()=>setSection('skills')}><div><BookOpen size={21}/></div><span><h3>Skills</h3></span><strong>{enabledSkills}<small>{t('extensions.enabledUnit')}</small></strong><ChevronRight size={16}/></button><button className="panel" onClick={()=>setSection('mcp')}><div><Zap size={21}/></div><span><h3>{t('extensions.tabs.mcp')}</h3></span><strong>{readyMCP}<small>{t('extensions.readyUnit')}</small></strong><ChevronRight size={16}/></button><button className="panel" onClick={()=>setSection('tools')}><div><FunctionSquare size={21}/></div><span><h3>{t('extensions.tabs.tools')}</h3></span><strong>{toolCatalog?.count??0}<small>{t('extensions.functionsUnit')}</small></strong><ChevronRight size={16}/></button></div>}
			{section==='skills'&&<SkillsPage skills={skills} refresh={refresh}/>}
			{section==='mcp'&&<MCPServersPage servers={mcpServers} refresh={refresh}/>}
			{section==='tools'&&<LLMToolsPage catalog={toolCatalog} refresh={refresh}/>}
		</div>
	</div>
}

type MCPFormState = {
	id:string;name:string;transport:MCPTransport;command:string;argsText:string;cwd:string;url:string;envText:string;headersText:string;enabled:boolean;clearEnv:boolean;clearHeaders:boolean
}

const mcpImportExample=JSON.stringify({mcpServers:{'cloudflare-api':{url:'https://mcp.cloudflare.com/mcp'}}},null,2)

function mcpStringMap(value:unknown,serverName:string,field:string){
	if(value===undefined)return undefined
	if(!value||typeof value!=='object'||Array.isArray(value))throw new Error(i18n.t('mcp.invalidField',{name:serverName,field}))
	const result:Record<string,string>={}
	for(const [name,content] of Object.entries(value)){
		if(typeof content!=='string')throw new Error(i18n.t('mcp.invalidField',{name:serverName,field}))
		result[name]=content
	}
	return result
}

function parseMCPImport(value:string):MCPServerInput[]{
	let parsed:unknown
	try{parsed=JSON.parse(value)}catch{throw new Error(i18n.t('mcp.invalidConfig'))}
	if(!parsed||typeof parsed!=='object'||Array.isArray(parsed))throw new Error(i18n.t('mcp.invalidConfig'))
	const root=(parsed as Record<string,unknown>).mcpServers
	if(!root||typeof root!=='object'||Array.isArray(root)||!Object.keys(root).length)throw new Error(i18n.t('mcp.invalidConfig'))
	return Object.entries(root).map(([rawName,value])=>{
		const name=rawName.trim()
		if(!name||!value||typeof value!=='object'||Array.isArray(value))throw new Error(i18n.t('mcp.invalidEntry',{name:rawName||'?'}))
		const entry=value as Record<string,unknown>
		const url=typeof entry.url==='string'?entry.url.trim():''
		const command=typeof entry.command==='string'?entry.command.trim():''
		if((url?1:0)+(command?1:0)!==1)throw new Error(i18n.t('mcp.invalidEntry',{name}))
		if(entry.args!==undefined&&(!Array.isArray(entry.args)||entry.args.some(item=>typeof item!=='string')))throw new Error(i18n.t('mcp.invalidField',{name,field:'args'}))
		if(entry.cwd!==undefined&&typeof entry.cwd!=='string')throw new Error(i18n.t('mcp.invalidField',{name,field:'cwd'}))
		if(entry.disabled!==undefined&&typeof entry.disabled!=='boolean')throw new Error(i18n.t('mcp.invalidField',{name,field:'disabled'}))
		if(entry.enabled!==undefined&&typeof entry.enabled!=='boolean')throw new Error(i18n.t('mcp.invalidField',{name,field:'enabled'}))
		return {
			name,
			transport:url?'streamable_http':'stdio',
			command,
			args:Array.isArray(entry.args)?entry.args as string[]:[],
			cwd:typeof entry.cwd==='string'?entry.cwd.trim():'',
			url,
			env:mcpStringMap(entry.env,name,'env'),
			headers:mcpStringMap(entry.headers,name,'headers'),
			enabled:typeof entry.disabled==='boolean'?!entry.disabled:typeof entry.enabled==='boolean'?entry.enabled:true,
		}
	})
}

function parseMCPPairs(value:string,kind:'env'|'header'){
	const result:Record<string,string>={}
	for(const raw of value.split(/\r?\n/)){
		const line=raw.trim();if(!line)continue
		const separator=kind==='env'?line.indexOf('='):line.indexOf(':')
		if(separator<1)throw new Error(i18n.t(kind==='env'?'mcp.invalidEnv':'mcp.invalidHeader',{line}))
		const name=line.slice(0,separator).trim(),content=line.slice(separator+1).trim()
		if(!name)throw new Error(i18n.t('mcp.invalidName',{kind}))
		result[name]=content
	}
	return result
}

function MCPServersPage({servers,refresh}:{servers:MCPServer[];refresh:()=>Promise<void>}){
	const {t,i18n:instance}=useTranslation()
	const notify=useNotifier()
	const [form,setForm]=useState<MCPFormState|null>(null)
	const [importConfig,setImportConfig]=useState<string|null>(null)
	const [busy,setBusy]=useState('')
	const [error,setError]=useState('')
	const [deleteCandidate,setDeleteCandidate]=useState<MCPServer|null>(null)
	const [authorizing,setAuthorizing]=useState('')
	const openCreate=()=>{setForm(null);setImportConfig('');setError('')}
	const openEdit=(server:MCPServer)=>{setImportConfig(null);setForm({id:server.id,name:server.name,transport:server.transport,command:server.command||'',argsText:(server.args||[]).join('\n'),cwd:server.cwd||'',url:server.url||'',envText:'',headersText:'',enabled:server.enabled,clearEnv:false,clearHeaders:false});setError('')}
	const importServers=async(event:FormEvent)=>{event.preventDefault();if(importConfig===null)return;setBusy('import');setError('');let imported=0;try{
		const inputs=parseMCPImport(importConfig)
		const existingNames=new Set(servers.map(server=>server.name))
		const duplicate=inputs.find(input=>existingNames.has(input.name))
		if(duplicate)throw new Error(t('mcp.nameExists',{name:duplicate.name}))
		for(const input of inputs){await api.saveMCPServer(input);imported++}
		setImportConfig(null);notify(t('mcp.imported',{count:imported}));await refresh()
	}catch(err){if(imported)await refresh();setError(imported?t('mcp.importPartial',{count:imported,message:errorText(err)}):errorText(err))}finally{setBusy('')}}
	const save=async(event:FormEvent)=>{event.preventDefault();if(!form)return;const requiredField=!form.name.trim()?t('mcp.displayName'):form.transport==='stdio'&&!form.command.trim()?t('mcp.command'):form.transport==='streamable_http'&&!form.url.trim()?t('mcp.endpoint'):'';if(requiredField){setError(`${requiredField}: ${t('common.required')}`);return}setBusy('save');setError('');try{
		const input:MCPServerInput={id:form.id,name:form.name.trim(),transport:form.transport,command:form.transport==='stdio'?form.command.trim():'',args:form.transport==='stdio'?form.argsText.split(/\r?\n/).map(item=>item.trim()).filter(Boolean):[],cwd:form.transport==='stdio'?form.cwd.trim():'',url:form.transport==='streamable_http'?form.url.trim():'',enabled:form.enabled}
		if(form.envText.trim()||form.clearEnv)input.env=form.clearEnv?{}:parseMCPPairs(form.envText,'env')
		if(form.headersText.trim()||form.clearHeaders)input.headers=form.clearHeaders?{}:parseMCPPairs(form.headersText,'header')
			const saved=await api.saveMCPServer(input);setForm(null);notify(`${t('mcp.saved',{name:saved.name,status:t(`statusLabels.${saved.status}`,{defaultValue:saved.status})})}${saved.last_error?` · ${saved.last_error}`:''}`);await refresh()
	}catch(err){setError(errorText(err))}finally{setBusy('')}}
	const test=async(server:MCPServer)=>{setBusy(`test-${server.id}`);setError('');try{const result=await api.testMCPServer(server.id);notify(t('mcp.healthy',{count:result.tool_count,latency:result.latency_ms}))}catch(err){setError(errorText(err))}finally{setBusy('')}}
	const toggle=async(server:MCPServer)=>{setBusy(`toggle-${server.id}`);setError('');try{const result=await api.setMCPServerEnabled(server.id,!server.enabled);notify(`${t('mcp.toggled',{name:result.name,state:result.enabled?t('common.enabled'):t('common.disabled'),status:t(`statusLabels.${result.status}`,{defaultValue:result.status})})}${result.last_error?` · ${result.last_error}`:''}`);await refresh()}catch(err){setError(errorText(err))}finally{setBusy('')}}
	const retry=async(server:MCPServer)=>{setBusy(`retry-${server.id}`);setError('');try{const result=await api.retryMCPServer(server.id);notify(t('mcp.reconnected',{name:result.name,count:result.tool_count}));await refresh()}catch(err){setError(errorText(err));await refresh()}finally{setBusy('')}}
	const authorize=async(server:MCPServer)=>{
		let popup:Window|null=null
		if(!desktopRuntime){popup=window.open('about:blank','_blank');if(popup)popup.opener=null}
		setBusy(`oauth-start-${server.id}`);setError('')
		try{
			const result=await api.startMCPOAuth(server.id)
			if(desktopRuntime)await invoke('open_external_url',{url:result.authorization_url})
			else if(popup)popup.location.href=result.authorization_url
			else throw new Error(t('mcp.popupBlocked'))
			setAuthorizing(server.id);setBusy('')
			for(let attempt=0;attempt<400;attempt++){
				await new Promise(resolve=>window.setTimeout(resolve,1500))
				const current=(await api.mcpServers()).find(item=>item.id===server.id)
				if(current?.status==='ready'&&current.oauth_configured){notify(t('mcp.authorized',{name:server.name}));await refresh();return}
				if(current?.status==='error'){setError(current.last_error||t('mcp.authorizationFailed'));await refresh();return}
			}
			setError(t('mcp.authorizationExpired'))
		}catch(err){popup?.close();setError(errorText(err))}finally{setBusy('');setAuthorizing('')}
	}
	const clearOAuth=async(server:MCPServer)=>{setBusy(`oauth-clear-${server.id}`);setError('');try{await api.clearMCPOAuth(server.id);notify(t('mcp.authorizationCleared',{name:server.name}));await refresh()}catch(err){setError(errorText(err))}finally{setBusy('')}}
	const remove=async()=>{if(!deleteCandidate)return;const server=deleteCandidate;setBusy(`delete-${server.id}`);setError('');try{await api.deleteMCPServer(server.id);notify(t('mcp.deleted',{name:server.name}));await refresh()}catch(err){setError(errorText(err))}finally{setBusy('');setDeleteCandidate(null)}}
		return <div className="mcp-page page-stack">
			<div className="page-actions"><div/><button className="primary" onClick={openCreate}><Plus size={15}/>{t('mcp.add')}</button></div>
		{error&&<div className="skill-error"><ShieldAlert size={15}/>{error}<button onClick={()=>setError('')}><X size={14}/></button></div>}
		{importConfig!==null&&<form className="mcp-form mcp-import-form panel" onSubmit={importServers}><header><div><Zap size={19}/><span><h3>{t('mcp.importConfig')}</h3></span></div><button type="button" onClick={()=>setImportConfig(null)} title={t('common.close')}><X size={15}/></button></header><div className="mcp-import-body"><textarea autoFocus spellCheck={false} aria-label={t('mcp.config')} value={importConfig} onChange={event=>setImportConfig(event.target.value)} placeholder={mcpImportExample}/></div><footer><span className="mcp-form-spacer"/><button type="button" onClick={()=>setImportConfig(null)}>{t('common.cancel')}</button><button className="primary" disabled={busy==='import'||!importConfig.trim()}>{busy==='import'?<LoaderCircle className="spin" size={14}/>:<Plus size={14}/>} {busy==='import'?t('mcp.importing'):t('mcp.import')}</button></footer></form>}
		{form&&<form className="mcp-form panel" noValidate onSubmit={save}><header><div><Zap size={19}/><span><h3>{form.name||t('mcp.server')}</h3></span></div><button type="button" onClick={()=>setForm(null)} title={t('common.close')}><X size={15}/></button></header><div className="mcp-form-grid"><label><span>{t('mcp.displayName')}</span><input value={form.name} onChange={event=>setForm({...form,name:event.target.value})}/></label><label><span>{t('mcp.transport')}</span><AppSelect value={form.transport} ariaLabel={t('mcp.transport')} onChange={value=>setForm({...form,transport:value as MCPTransport})} options={[{value:'stdio',label:t('mcp.localProcess')},{value:'streamable_http',label:'Streamable HTTP'}]}/></label>{form.transport==='stdio'?<><label><span>{t('mcp.command')}</span><input value={form.command} onChange={event=>setForm({...form,command:event.target.value})}/></label><label><span>{t('mcp.cwd')}</span><input value={form.cwd} onChange={event=>setForm({...form,cwd:event.target.value})}/></label><label className="mcp-wide"><span>{t('mcp.args')}</span><textarea value={form.argsText} onChange={event=>setForm({...form,argsText:event.target.value})}/></label></>:<label className="mcp-wide"><span>{t('mcp.endpoint')}</span><input value={form.url} onChange={event=>setForm({...form,url:event.target.value})}/></label>}<label className="mcp-wide"><span>{t('mcp.env')}</span><textarea value={form.envText} onChange={event=>setForm({...form,envText:event.target.value,clearEnv:false})} placeholder={t('mcp.preserve')}/><small><label><input type="checkbox" checked={form.clearEnv} onChange={event=>setForm({...form,clearEnv:event.target.checked,envText:event.target.checked?'':form.envText})}/> {t('mcp.clearEnv')}</label></small></label><label className="mcp-wide"><span>{t('mcp.headers')}</span><textarea value={form.headersText} onChange={event=>setForm({...form,headersText:event.target.value,clearHeaders:false})} placeholder={t('mcp.preserve')}/><small><label><input type="checkbox" checked={form.clearHeaders} onChange={event=>setForm({...form,clearHeaders:event.target.checked,headersText:event.target.checked?'':form.headersText})}/> {t('mcp.clearHeaders')}</label></small></label></div><footer><label className="mcp-enable-on-save"><input type="checkbox" checked={form.enabled} onChange={event=>setForm({...form,enabled:event.target.checked})}/><i/><span><b>{t('mcp.enableAfterSave')}</b></span></label><button type="button" onClick={()=>setForm(null)}>{t('common.cancel')}</button><button className="primary" disabled={busy==='save'}>{busy==='save'?<LoaderCircle className="spin" size={14}/>:<Save size={14}/>} {busy==='save'?t('common.saving'):t('mcp.saveServer')}</button></footer></form>}
		<div className="mcp-grid">{servers.map(server=><article className={`mcp-card panel ${server.status}`} key={server.id}><header><div className="mcp-card-icon"><Zap size={19}/></div><span><h3>{server.name}</h3><code>{server.transport==='stdio'?server.command:server.url}</code></span><em className={server.status}><CircleDot size={9}/>{t(`statusLabels.${server.status}`,{defaultValue:server.status})}</em></header><dl><div><dt>{t('mcp.discoveredTools')}</dt><dd>{server.tool_count}</dd></div><div><dt>{t('mcp.secrets')}</dt><dd>{server.oauth_configured?t('mcp.oauth'):t('mcp.configuredSecrets',{count:(server.env_keys?.length||0)+(server.header_keys?.length||0)})}</dd></div><div><dt>{t('mcp.lastConnected')}</dt><dd>{server.connected_at?new Date(server.connected_at).toLocaleString(localeFor(instance.language)):'—'}</dd></div></dl>{server.last_error&&<div className="mcp-card-error"><ShieldAlert size={13}/><span>{server.last_error}</span></div>}<div className="mcp-actions"><button onClick={()=>void test(server)} disabled={!!busy||authorizing===server.id}><Activity size={13}/>{busy===`test-${server.id}`?t('common.testing'):t('common.test')}</button><button onClick={()=>openEdit(server)} disabled={!!busy||authorizing===server.id}><Edit3 size={13}/>{t('common.edit')}</button>{server.transport==='streamable_http'&&(server.status==='error'||server.oauth_configured)&&<button onClick={()=>void authorize(server)} disabled={!!busy||!!authorizing}><KeyRound size={13}/>{authorizing===server.id?t('mcp.authorizing'):server.oauth_configured?t('mcp.reauthorize'):t('mcp.authorize')}</button>}{server.oauth_configured&&<button title={t('mcp.clearAuthorization')} onClick={()=>void clearOAuth(server)} disabled={!!busy||!!authorizing}><LogOut size={13}/></button>}{server.enabled&&server.status!=='ready'&&authorizing!==server.id&&<button onClick={()=>void retry(server)} disabled={!!busy}><RefreshCw className={busy===`retry-${server.id}`?'spin':''} size={13}/>{t('common.retry')}</button>}<button className={server.enabled?'disable':'enable'} onClick={()=>void toggle(server)} disabled={!!busy||authorizing===server.id}>{busy===`toggle-${server.id}`?<LoaderCircle className="spin" size={13}/>:server.enabled?<X size={13}/>:<Check size={13}/>} {server.enabled?t('common.disable'):t('common.enable')}</button><button className="danger" title={t('common.delete')} onClick={()=>setDeleteCandidate(server)} disabled={!!busy||authorizing===server.id}><Trash2 size={13}/></button></div>{server.tools?.length?<details className="mcp-tools"><summary>{t('mcp.modelTools',{count:server.tools.length})} <ChevronRight size={13}/></summary><div>{server.tools.map(item=><section key={item.exposed_name}><code>{item.exposed_name}</code><span>{t('mcp.remote')} · {item.name}</span><p>{item.description}</p></section>)}</div></details>:null}</article>)}</div>
		{!servers.length&&<Empty icon={<Zap/>} title={t('mcp.emptyTitle')}/>}
		{deleteCandidate&&<DestructiveConfirmDialog title={t('mcp.deleteTitle',{name:deleteCandidate.name})} busy={busy===`delete-${deleteCandidate.id}`} onCancel={()=>setDeleteCandidate(null)} onConfirm={()=>void remove()}/>}
	</div>
}

type ToolParameterView = {name:string;type:string;description:string;required:boolean}

function toolCategoryLabel(value:string){return i18n.t(`toolCategories.${value}`,{defaultValue:value})}
function toolGuardLabel(value:LLMToolGuard){return i18n.t(`toolGuards.${value}`,{defaultValue:value})}

function schemaRecord(value:unknown):Record<string,unknown>{return value!==null&&typeof value==='object'&&!Array.isArray(value)?value as Record<string,unknown>:{}}
function schemaType(value:unknown){if(Array.isArray(value))return value.map(String).join(' | ');return typeof value==='string'?value:'any'}
function toolParameters(tool?:LLMToolDescriptor):ToolParameterView[]{
	if(!tool)return[]
	const schema=schemaRecord(tool.input_schema)
	const properties=schemaRecord(schema.properties)
	const required=new Set(Array.isArray(schema.required)?schema.required.map(String):[])
	return Object.entries(properties).map(([name,value])=>{const field=schemaRecord(value);return{name,type:schemaType(field.type),description:typeof field.description==='string'?field.description:'',required:required.has(name)}})
}

function LLMToolsPage({catalog,refresh}:{catalog:LLMToolCatalog|null;refresh:()=>Promise<void>}){
	const {t}=useTranslation()
	const [query,setQuery]=useState('')
	const [category,setCategory]=useState('all')
	const [selectedName,setSelectedName]=useState('')
	const [refreshing,setRefreshing]=useState(false)
	const [busyName,setBusyName]=useState('')
	const [error,setError]=useState('')
	const tools=catalog?.tools||[]
	const categories=useMemo(()=>Array.from(new Set(tools.map(tool=>tool.category))),[tools])
	const filtered=useMemo(()=>{const needle=query.trim().toLowerCase();return tools.filter(tool=>(category==='all'||tool.category===category)&&(!needle||`${tool.name} ${tool.description} ${tool.category}`.toLowerCase().includes(needle)))},[tools,query,category])
	const selected=filtered.find(tool=>tool.name===selectedName)||filtered[0]
	const parameters=toolParameters(selected)
	const protectedCount=tools.filter(tool=>tool.enabled&&tool.guard==='approval_required').length
	const readOnlyCount=tools.filter(tool=>tool.enabled&&tool.guard==='read_only').length
	const refreshCatalog=async()=>{setRefreshing(true);try{await refresh()}finally{setRefreshing(false)}}
	const setEnabled=async(tool:LLMToolDescriptor)=>{setBusyName(tool.name);setError('');try{await api.setLLMToolEnabled(tool.name,!tool.enabled);await refresh()}catch(err){setError(errorText(err))}finally{setBusyName('')}}

	return <div className="llm-tools-page page-stack">
		<section className={`tool-catalog-hero panel ${catalog?.loaded?'loaded':'unloaded'}`}>
			<div className="tool-catalog-mark"><FunctionSquare size={24}/><i/></div>
				<div><h2>{catalog?.loaded?t('tools.loadedTitle'):t('tools.unloadedTitle')}</h2></div>
			<dl><div><dt>{t('common.agent')}</dt><dd>{catalog?.agent||'ops-nerva'}</dd></div><div><dt>{t('common.model')}</dt><dd>{catalog?.model||t('tools.notLoaded')}</dd></div><div><dt>{t('common.functions')}</dt><dd>{catalog?.count??0} / {catalog?.total??0}</dd></div><div><dt>{t('tools.execution')}</dt><dd>{catalog?.execution_mode||'sequential'}</dd></div></dl>
			<button className="tool-catalog-refresh" onClick={refreshCatalog} disabled={refreshing}><RefreshCw className={refreshing?'spin':''} size={14}/>{refreshing?t('common.refreshing'):t('tools.refreshSnapshot')}</button>
		</section>
			{error&&<div className="tool-function-error"><ShieldAlert size={15}/><span>{error}</span><button onClick={()=>setError('')} title={t('common.dismiss')}><X size={14}/></button></div>}
		<div className="tool-catalog-metrics"><Metric label={t('tools.enabledFunctions')} value={String(catalog?.count??0)} tone="green"/><Metric label={t('tools.availableFunctions')} value={String(catalog?.total??0)}/><Metric label={t('tools.readOnlyEnabled')} value={String(readOnlyCount)}/><Metric label={t('tools.approvalEnabled')} value={String(protectedCount)} tone="amber"/></div>
		<div className="tool-catalog-toolbar panel"><label><Search size={15}/><input value={query} onChange={event=>setQuery(event.target.value)} placeholder={t('tools.searchPlaceholder')}/></label><AppSelect value={category} ariaLabel={t('tools.category')} onChange={setCategory} options={[{value:'all',label:t('tools.allCategories',{count:tools.length})},...categories.map(value=>({value,label:`${toolCategoryLabel(value)} · ${tools.filter(tool=>tool.category===value).length}`}))]}/><span>{t('tools.visible',{count:filtered.length})}</span></div>
		{!catalog?<div className="tool-catalog-loading panel"><LoaderCircle className="spin" size={20}/>{t('tools.loadingSnapshot')}</div>:!catalog.loaded?<Empty icon={<FunctionSquare/>} title={t('tools.runtimeMissing')} text={t('tools.runtimeMissingText')}/>:<div className="tool-catalog-browser">
			<section className="tool-function-list panel">{filtered.length?filtered.map(tool=>{const count=toolParameters(tool).length;return <button className={`${selected?.name===tool.name?'active':''} ${tool.enabled?'':'disabled'}`} onClick={()=>setSelectedName(tool.name)} key={tool.name}><div className="tool-function-icon"><Braces size={16}/></div><span><code>{tool.name}</code><p>{tool.description}</p><small><em>{toolCategoryLabel(tool.category)}</em><i className={tool.guard}>{toolGuardLabel(tool.guard)}</i>{!tool.enabled&&<i className="disabled">{t('tools.disabled')}</i>}</small></span><b>{count}<small>{t('tools.argsUnit')}</small></b><ChevronRight size={14}/></button>}):<div className="tool-filter-empty"><Search size={20}/><b>{t('tools.noMatch')}</b></div>}</section>
			<aside className={`tool-function-inspector panel ${selected?.enabled?'':'disabled'}`}>{selected?<><header><div className="tool-function-icon"><FunctionSquare size={18}/></div><span><small>{t('tools.functionDetail')}</small><code>{selected.name}</code></span><div className="tool-function-controls"><em className={selected.guard}>{toolGuardLabel(selected.guard)}</em><button className={selected.enabled?'enabled':''} role="switch" aria-checked={selected.enabled} onClick={()=>void setEnabled(selected)} disabled={busyName===selected.name} title={selected.enabled?t('tools.disableFunction'):t('tools.enableFunction')}>{busyName===selected.name?<LoaderCircle className="spin" size={14}/>:<Power size={14}/>}<span>{selected.enabled?t('common.enabled'):t('common.disabled')}</span></button></div></header><p className="tool-function-description">{selected.description}</p><dl className="tool-function-meta"><div><dt>{t('tools.category')}</dt><dd>{toolCategoryLabel(selected.category)}</dd></div><div><dt>{t('common.arguments')}</dt><dd>{parameters.length}</dd></div><div><dt>{t('tools.safetyGate')}</dt><dd>{toolGuardLabel(selected.guard)}</dd></div></dl><section className="tool-parameter-list"><h3>{t('tools.inputParameters')} <span>{t('tools.requiredCount',{count:parameters.filter(item=>item.required).length})}</span></h3>{parameters.length?parameters.map(parameter=><div key={parameter.name}><code>{parameter.name}</code><em>{parameter.type}</em>{parameter.required&&<b>{t('common.required')}</b>}{parameter.description&&<p>{parameter.description}</p>}</div>):<p className="tool-no-arguments">{t('tools.noArguments')}</p>}</section><details className="tool-schema-raw"><summary>{t('tools.rawSchema')} <ChevronRight size={13}/></summary><CopyablePre>{JSON.stringify(selected.input_schema,null,2)}</CopyablePre></details></>:<div className="tool-inspector-empty"><Braces size={26}/></div>}</aside>
		</div>}
	</div>
}

function SkillsPage({skills,refresh}:{skills:ManagedSkill[];refresh:()=>Promise<void>}){
	const {t,i18n:instance}=useTranslation()
	const notify=useNotifier()
	const [query,setQuery]=useState('')
	const [selectedName,setSelectedName]=useState('')
	const [selected,setSelected]=useState<ManagedSkill|null>(null)
	const [draft,setDraft]=useState('')
	const [loading,setLoading]=useState(false)
	const [saving,setSaving]=useState(false)
	const [uploading,setUploading]=useState(false)
	const [reloading,setReloading]=useState(false)
	const [uploadOpen,setUploadOpen]=useState(false)
	const [uploadName,setUploadName]=useState('')
	const [uploadFile,setUploadFile]=useState<File|null>(null)
	const [deleteName,setDeleteName]=useState('')
	const [deleting,setDeleting]=useState(false)
	const [toggling,setToggling]=useState(false)
	const [error,setError]=useState('')
	const filtered=useMemo(()=>{const needle=query.trim().toLowerCase();return skills.filter(skill=>!needle||`${skill.name} ${skill.summary}`.toLowerCase().includes(needle))},[skills,query])
	useEffect(()=>{if(!skills.length){setSelectedName('');setSelected(null);setDraft('');return}if(!selectedName||!skills.some(skill=>skill.name===selectedName))setSelectedName(skills[0].name)},[skills,selectedName])
	useEffect(()=>{if(!selectedName)return;let cancelled=false;setLoading(true);setError('');api.skill(selectedName).then(skill=>{if(cancelled)return;setSelected(skill);setDraft(skill.content||'')}).catch(err=>{if(!cancelled)setError(errorText(err))}).finally(()=>{if(!cancelled)setLoading(false)});return()=>{cancelled=true}},[selectedName])
	const dirty=!!selected&&draft!==selected.content
	const markdownUpload=!!uploadFile&&/\.(?:md|markdown)$/i.test(uploadFile.name)
	const selectFile=(file:File|null)=>{setUploadFile(file);if(file&&/\.(?:md|markdown)$/i.test(file.name)&&!uploadName){const base=file.name.replace(/\.(markdown|md)$/i,'').replace(/[^A-Za-z0-9_.-]+/g,'-').replace(/^-+|-+$/g,'').slice(0,64);setUploadName(base)}else if(file)setUploadName('')}
	const upload=async(event:FormEvent)=>{event.preventDefault();if(!uploadFile)return;if(markdownUpload&&!/^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$/.test(uploadName.trim())){setError(t('skills.invalidName'));return}setUploading(true);setError('');try{const results=await api.uploadSkill(uploadName.trim(),uploadFile);const result=results[0]!;await refresh();setSelectedName(result.name);setSelected(result);setDraft(result.content||'');setUploadOpen(false);setUploadName('');setUploadFile(null);notify(t(results.length===1?'skills.uploaded':'skills.uploadedMany',{name:result.name,count:results.length}))}catch(err){setError(errorText(err))}finally{setUploading(false)}}
	const save=async()=>{if(!selected)return;setSaving(true);setError('');try{const result=await api.saveSkill(selected.name,draft);setSelected(result);setDraft(result.content||'');await refresh();notify(t('skills.saved',{name:result.name}))}catch(err){setError(errorText(err))}finally{setSaving(false)}}
	const permanentlyDelete=async()=>{if(!deleteName)return;setDeleting(true);setError('');try{await api.deleteSkill(deleteName);setDeleteName('');setSelectedName('');setSelected(null);setDraft('');await refresh();notify(t('skills.deleted',{name:deleteName}))}catch(err){setError(errorText(err))}finally{setDeleting(false)}}
	const toggleEnabled=async()=>{if(!selected)return;setToggling(true);setError('');try{const result=await api.setSkillEnabled(selected.name,!selected.enabled);setSelected(result);setDraft(result.content||draft);await refresh();notify(t(result.enabled?'skills.toggledEnabled':'skills.toggledDisabled',{name:result.name}))}catch(err){setError(errorText(err))}finally{setToggling(false)}}
	const reload=async()=>{setReloading(true);setError('');try{await api.reloadSkills();await refresh();notify(t('skills.reloaded'))}catch(err){setError(errorText(err))}finally{setReloading(false)}}

	return <div className="skills-page page-stack">
			<div className="page-actions"><div/><div className="skill-page-actions"><button onClick={()=>void reload()} disabled={reloading}><RefreshCw className={reloading?'spin':''} size={15}/>{reloading?t('common.refreshing'):t('skills.reload')}</button><button className="primary" onClick={()=>{setUploadOpen(value=>!value);setError('')}}><UploadCloud size={15}/>{uploadOpen?t('skills.closeUpload'):t('skills.uploadSkill')}</button></div></div>
		{error&&<div className="skill-error"><ShieldAlert size={15}/>{error}<button onClick={()=>setError('')}><X size={14}/></button></div>}
		{uploadOpen&&<form className="skill-upload-panel panel" noValidate onSubmit={upload}><div><div className="skill-upload-icon"><UploadCloud size={20}/></div><span><b>{t('skills.uploadPackage')}</b><small>{t('skills.packageHelp')}</small></span></div>{markdownUpload&&<label><span>{t('skills.skillName')}</span><input value={uploadName} onChange={event=>setUploadName(event.target.value)}/></label>}<label className="skill-file-picker"><FileText size={15}/><span><b>{uploadFile?.name||t('skills.choosePackage')}</b><small>{uploadFile?formatFileSize(uploadFile.size):t('skills.maxPackage')}</small></span><input type="file" accept=".md,.markdown,.zip,.7z,text/markdown,application/zip,application/x-7z-compressed" onChange={event=>selectFile(event.target.files?.[0]||null)}/></label><button className="primary" disabled={uploading||!uploadFile||(markdownUpload&&!uploadName.trim())}>{uploading?<LoaderCircle className="spin" size={14}/>:<UploadCloud size={14}/>} {uploading?t('common.uploading'):t('skills.uploadActivate')}</button></form>}
		<section className="skill-registry-summary panel"><div><BookOpen size={19}/><span><b>{t('skills.summary',{enabled:skills.filter(skill=>skill.enabled).length,total:skills.length})}</b></span></div><label><Search size={14}/><input value={query} onChange={event=>setQuery(event.target.value)} placeholder={t('skills.search')}/></label></section>
		<div className="skill-manager-layout">
			<section className="skill-list panel">{filtered.length?filtered.map(skill=><button className={`${selectedName===skill.name?'active':''} ${skill.enabled?'':'disabled'}`} onClick={()=>setSelectedName(skill.name)} key={skill.name}><div className="skill-card-icon"><BookOpen size={16}/></div><span><code>{skill.name}</code>{skill.summary&&<p>{skill.summary}</p>}<small><em className={skill.enabled?'enabled':'disabled'}>{skill.enabled?t('common.enabled'):t('common.disabled')}</em>{skill.file_count||1} {t('common.files')} · {formatFileSize(skill.size_bytes||0)}{skill.updated_at?` · ${new Date(skill.updated_at).toLocaleDateString(localeFor(instance.language))}`:''}</small></span><ChevronRight size={14}/></button>):<div className="skill-list-empty"><BookOpen size={23}/><b>{skills.length?t('skills.noMatch'):t('skills.noneInstalled')}</b></div>}</section>
				<section className="skill-editor panel">{loading?<div className="skill-editor-state"><LoaderCircle className="spin" size={21}/>{t('skills.loading')}</div>:selected?<><header><div><BookOpen size={17}/><span><small>{t('skills.managed')} · {selected.enabled?t('common.enabled'):t('common.disabled')}</small><code>{selected.name}</code></span></div><section><button className={selected.enabled?'skill-disable':'skill-enable'} disabled={toggling} onClick={toggleEnabled}>{toggling?<LoaderCircle className="spin" size={13}/>:selected.enabled?<X size={13}/>:<Check size={13}/>} {selected.enabled?t('common.disable'):t('common.enable')}</button><button disabled={!dirty||saving} onClick={save}>{saving?<LoaderCircle className="spin" size={13}/>:<Save size={13}/>} {saving?t('common.saving'):t('skills.saveChanges')}</button><button className="danger" onClick={()=>setDeleteName(selected.name)}><Trash2 size={13}/>{t('common.delete')}</button></section></header><div className="skill-editor-meta"><span><b>SHA256</b><code title={selected.content_sha256}>{selected.content_sha256?.slice(0,16)||'—'}</code></span><span><b>{t('common.files')}</b><code>{selected.file_count||1}</code></span><span><b>{t('common.size')}</b><code>{formatFileSize(selected.size_bytes||0)}</code></span><span><b>{t('common.updated')}</b><code>{selected.updated_at?new Date(selected.updated_at).toLocaleString(localeFor(instance.language)):'—'}</code></span></div><div className="skill-editor-split"><label><span>SKILL.md</span><textarea value={draft} spellCheck={false} onChange={event=>setDraft(event.target.value)}/></label><section><span>{t('skills.livePreview')}</span><div className="markdown-body"><Markdown skipHtml remarkPlugins={[remarkGfm]} components={{a:({href,children})=><a href={href} target="_blank" rel="noopener noreferrer">{children}</a>,img:({alt})=><span className="markdown-image-blocked">{t('skills.blockedImage',{alt:alt||t('common.image')})}</span>,pre:({children})=><CopyablePre>{children}</CopyablePre>}}>{draft||t('skills.emptySkill')}</Markdown></div></section></div></>:<div className="skill-editor-state"><BookOpen size={25}/><b>{t('skills.select')}</b></div>}</section>
		</div>
		{deleteName&&<DestructiveConfirmDialog title={t('skills.deleteTitle',{name:deleteName})} busy={deleting} onCancel={()=>setDeleteName('')} onConfirm={()=>void permanentlyDelete()}/>}
	</div>
}

function SettingsDisclosure({icon,title,meta,children,className=''}:{icon:React.ReactNode;title:string;meta?:React.ReactNode;children:React.ReactNode;className?:string}){
	return <details className={`settings-disclosure panel ${className}`.trim()}><summary><span className="settings-disclosure-icon">{icon}</span><b>{title}</b>{meta&&<em>{meta}</em>}<ChevronRight size={16}/></summary><div className="settings-disclosure-body">{children}</div></details>
}

function SettingsSectionFooter({dirty,busy,saving,onDiscard}:{dirty:boolean;busy:boolean;saving:boolean;onDiscard:()=>void}){
	const {t}=useTranslation()
	return <footer className="settings-section-footer"><button type="button" disabled={!dirty||busy} onClick={onDiscard}>{t('settings.discard')}</button><button type="submit" className="primary" disabled={!dirty||busy}>{saving?t('settings.applying'):t('settings.apply')}</button></footer>
}

type SystemSettingsSection='iterations'|'prompt'|'explanation'|'images'|'shell'

function SystemSettingsPage({settings,providers,proxies,capabilities,modelStatus,refresh}:{settings:SystemSettings|null;providers:ModelProvider[];proxies:Proxy[];capabilities:ToolCapabilities;modelStatus?:Health['model'];refresh:()=>Promise<void>}) {
  const {t}=useTranslation()
	const notify=useNotifier()
  const savedValue=settings?.agent_max_iterations??50
  const savedPrompt=settings?.system_prompt??''
	const defaultPrompt=settings?.default_system_prompt??''
  const savedExplanation=settings?.approval_explanations_enabled??true
	  const savedSubagentProvider=settings?.subagent_model_provider_id??''
	  const savedAutomaticApprovalProvider=settings?.automatic_approval_model_provider_id??''
	  const savedSubagentTimeout=settings?.subagent_timeout_seconds??30
	  const savedImageTypes=settings?.chat_image_allowed_types??defaultChatImageTypes
  const savedShellMode=settings?.workspace_shell_mode??(settings?.workspace_shell_platform==='windows'?'host':'sandbox')
  const [maxIterations,setMaxIterations]=useState(savedValue)
  const [systemPrompt,setSystemPrompt]=useState(savedPrompt)
  const [explanationEnabled,setExplanationEnabled]=useState(savedExplanation)
  const [subagentProvider,setSubagentProvider]=useState(savedSubagentProvider)
	const [automaticApprovalProvider,setAutomaticApprovalProvider]=useState(savedAutomaticApprovalProvider)
	  const [subagentTimeout,setSubagentTimeout]=useState(savedSubagentTimeout)
	  const [imageTypes,setImageTypes]=useState(savedImageTypes)
  const [shellMode,setShellMode]=useState<WorkspaceShellMode>(savedShellMode)
	const [iterationsDirty,setIterationsDirty]=useState(false)
	const [promptDirty,setPromptDirty]=useState(false)
	const [explanationDirty,setExplanationDirty]=useState(false)
	const [imagesDirty,setImagesDirty]=useState(false)
	const [shellDirty,setShellDirty]=useState(false)
	const [savingSection,setSavingSection]=useState<SystemSettingsSection|''>('')
	useEffect(()=>{if(!iterationsDirty)setMaxIterations(savedValue)},[savedValue,iterationsDirty])
	useEffect(()=>{if(!promptDirty)setSystemPrompt(savedPrompt)},[savedPrompt,promptDirty])
	useEffect(()=>{if(!explanationDirty){setExplanationEnabled(savedExplanation);setSubagentProvider(savedSubagentProvider);setAutomaticApprovalProvider(savedAutomaticApprovalProvider);setSubagentTimeout(savedSubagentTimeout)}},[savedExplanation,savedSubagentProvider,savedAutomaticApprovalProvider,savedSubagentTimeout,explanationDirty])
	useEffect(()=>{if(!imagesDirty)setImageTypes(savedImageTypes)},[savedImageTypes,imagesDirty])
	useEffect(()=>{if(!shellDirty)setShellMode(savedShellMode)},[savedShellMode,shellDirty])
	const update=(value:number)=>{setMaxIterations(Math.max(5,Math.min(100,value||5)));setIterationsDirty(true)}
	const updateSystemPrompt=(value:string)=>{setSystemPrompt(value);setPromptDirty(true)}
	const restoreDefaultPrompt=()=>{setSystemPrompt(defaultPrompt);setPromptDirty(true)}
	const toggleExplanation=(value:boolean)=>{setExplanationEnabled(value);setExplanationDirty(true)}
	const selectSubagentProvider=(value:string)=>{setSubagentProvider(value);setExplanationDirty(true)}
	const selectAutomaticApprovalProvider=(value:string)=>{setAutomaticApprovalProvider(value);setExplanationDirty(true)}
	const updateSubagentTimeout=(value:number)=>{setSubagentTimeout(Math.max(5,Math.min(120,value||5)));setExplanationDirty(true)}
	const toggleImageType=(value:string)=>{setImageTypes(current=>current.includes(value)?current.length===1?current:current.filter(item=>item!==value):[...current,value]);setImagesDirty(true)}
	const selectShellMode=(value:WorkspaceShellMode)=>{setShellMode(value);setShellDirty(true)}
	const discard=(section:SystemSettingsSection)=>{
		switch(section){
		case 'iterations':setMaxIterations(savedValue);setIterationsDirty(false);break
		case 'prompt':setSystemPrompt(savedPrompt);setPromptDirty(false);break
		case 'explanation':setExplanationEnabled(savedExplanation);setSubagentProvider(savedSubagentProvider);setAutomaticApprovalProvider(savedAutomaticApprovalProvider);setSubagentTimeout(savedSubagentTimeout);setExplanationDirty(false);break
		case 'images':setImageTypes(savedImageTypes);setImagesDirty(false);break
		case 'shell':setShellMode(savedShellMode);setShellDirty(false);break
		}
	}
	const save=async(section:SystemSettingsSection)=>{
		const input:SystemSettingsInput={agent_max_iterations:section==='iterations'?maxIterations:savedValue}
		switch(section){
		case 'prompt':input.system_prompt=systemPrompt;break
		case 'explanation':input.approval_explanations_enabled=explanationEnabled;input.subagent_model_provider_id=subagentProvider;input.automatic_approval_model_provider_id=automaticApprovalProvider;input.subagent_timeout_seconds=subagentTimeout;break
		case 'images':input.chat_image_allowed_types=imageTypes;break
		case 'shell':input.workspace_shell_mode=shellMode;break
		}
		setSavingSection(section)
		try{
			const result=await api.saveSystemSettings(input)
			switch(section){
			case 'iterations':setMaxIterations(result.agent_max_iterations);setIterationsDirty(false);break
			case 'prompt':setSystemPrompt(result.system_prompt);setPromptDirty(false);break
			case 'explanation':setExplanationEnabled(result.approval_explanations_enabled);setSubagentProvider(result.subagent_model_provider_id);setAutomaticApprovalProvider(result.automatic_approval_model_provider_id);setSubagentTimeout(result.subagent_timeout_seconds);setExplanationDirty(false);break
			case 'images':setImageTypes(result.chat_image_allowed_types);setImagesDirty(false);break
			case 'shell':setShellMode(result.workspace_shell_mode);setShellDirty(false);break
			}
			notify(t('settings.saved'))
			await refresh()
		}catch(err){notify(errorText(err),'error')}finally{setSavingSection('')}
	}
	const submit=(section:SystemSettingsSection)=>(event:FormEvent)=>{event.preventDefault();void save(section)}
	const busy=!!savingSection
  return <div className="system-settings page-stack">

		<div className="settings-form">
			<SettingsDisclosure icon={<SlidersHorizontal size={18}/>} title={t('settings.maxIterations')} meta={<strong>{maxIterations}</strong>}>
				<form onSubmit={submit('iterations')}><div className="iteration-editor"><input aria-label={t('settings.maxIterations')} type="range" min="5" max="100" step="1" value={maxIterations} onChange={event=>update(Number(event.target.value))}/><label><span>{t('settings.rounds')}</span><input type="number" min="5" max="100" value={maxIterations} onChange={event=>update(Number(event.target.value))}/></label></div><div className="iteration-presets"><span>{t('settings.quickPresets')}</span>{[20,50,100].map(value=><button type="button" className={maxIterations===value?'active':''} onClick={()=>update(value)} key={value}><b>{value}</b></button>)}</div><SettingsSectionFooter dirty={iterationsDirty} busy={busy} saving={savingSection==='iterations'} onDiscard={()=>discard('iterations')}/></form>
			</SettingsDisclosure>
			<SettingsDisclosure icon={<Bot size={18}/>} title={t('settings.systemPrompt')} meta={systemPrompt.length?t('settings.systemPromptCharacters',{count:systemPrompt.length}):undefined}>
				<form onSubmit={submit('prompt')}><div className="system-prompt-actions"><button type="button" disabled={systemPrompt===defaultPrompt} onClick={restoreDefaultPrompt}><RefreshCw size={13}/>{t('settings.restoreDefaultPrompt')}</button></div><textarea className="system-prompt-input" aria-label={t('settings.systemPrompt')} spellCheck={false} value={systemPrompt} onChange={event=>updateSystemPrompt(event.target.value)}/><small className="system-prompt-count">{systemPrompt.length?t('settings.systemPromptCharacters',{count:systemPrompt.length}):t('settings.emptySystemPrompt')}</small><SettingsSectionFooter dirty={promptDirty} busy={busy} saving={savingSection==='prompt'} onDiscard={()=>discard('prompt')}/></form>
			</SettingsDisclosure>
			<SettingsDisclosure icon={<BrainCircuit size={18}/>} title={t('settings.explanationSection')} meta={<><span className={modelStatus?.approval_agent_available?'ready':'offline'}><CircleDot size={9}/>{t('settings.explanationAgent')} · {modelStatus?.approval_agent_available?t('settings.runnerReady'):t('settings.modelUnavailable')}</span><span className={modelStatus?.automatic_approval_agent_available?'ready':'offline'}><CircleDot size={9}/>Auto · {modelStatus?.automatic_approval_agent_available?t('settings.runnerReady'):t('settings.modelUnavailable')}</span></>}>
				<form onSubmit={submit('explanation')}><div className="subagent-settings"><label className="subagent-toggle"><span><b>{t('settings.commandAgent')}</b></span><input type="checkbox" checked={explanationEnabled} onChange={event=>toggleExplanation(event.target.checked)}/><i/></label><div className="subagent-config-grid"><label><span><b>{t('settings.commandAgent')}</b></span><AppSelect value={subagentProvider} ariaLabel={t('settings.commandAgent')} onChange={selectSubagentProvider} options={[{value:'',label:t('settings.followMain')},...providers.map(provider=>({value:provider.id,label:`${provider.name} · ${provider.model}`}))]}/></label><label><span><b>{t('settings.automaticApprovalAgent')}</b></span><AppSelect value={automaticApprovalProvider} ariaLabel={t('settings.automaticApprovalAgent')} onChange={selectAutomaticApprovalProvider} options={[{value:'',label:t('settings.followMain')},...providers.map(provider=>({value:provider.id,label:`${provider.name} · ${provider.model}`}))]}/></label><label><span><b>{t('settings.requestTimeout')}</b></span><div className="subagent-timeout-input"><input aria-label={t('settings.timeout')} type="number" min="5" max="120" step="1" value={subagentTimeout} onChange={event=>updateSubagentTimeout(Number(event.target.value))}/><em>{t('settings.seconds',{count:subagentTimeout})}</em></div></label></div>{modelStatus?.approval_error&&<div className="subagent-runtime-error"><ShieldAlert size={14}/><span>{modelStatus.approval_error}</span></div>}{modelStatus?.automatic_approval_error&&<div className="subagent-runtime-error"><ShieldAlert size={14}/><span>{modelStatus.automatic_approval_error}</span></div>}</div><SettingsSectionFooter dirty={explanationDirty} busy={busy} saving={savingSection==='explanation'} onDiscard={()=>discard('explanation')}/></form>
			</SettingsDisclosure>
			<SettingsDisclosure icon={<ImagePlus size={18}/>} title={t('settings.chatImages')} meta={imageTypes.map(value=>value.replace('image/','').toUpperCase()).join(' · ')}>
				<form onSubmit={submit('images')}><div className="chat-image-formats">{[['image/png','PNG'],['image/jpeg','JPEG'],['image/webp','WebP'],['image/gif','GIF']].map(([value,label])=><label className={imageTypes.includes(value)?'active':''} key={value}><input type="checkbox" checked={imageTypes.includes(value)} disabled={imageTypes.length===1&&imageTypes.includes(value)} onChange={()=>toggleImageType(value)}/><span>{label}</span></label>)}</div><SettingsSectionFooter dirty={imagesDirty} busy={busy} saving={savingSection==='images'} onDiscard={()=>discard('images')}/></form>
			</SettingsDisclosure>
			<SettingsDisclosure icon={<TerminalSquare size={18}/>} title={t('settings.shellBackend')} meta={settings?.workspace_shell_platform||t('settings.detecting')}>
				<form onSubmit={submit('shell')}><div className="workspace-shell-modes" role="group" aria-label={t('settings.shellBackend')}><button type="button" className={shellMode==='sandbox'?'active':''} disabled={!settings?.workspace_sandbox_available} onClick={()=>selectShellMode('sandbox')}><ShieldCheck size={16}/><span><b>{t('settings.sandbox')}</b><small>{settings?.workspace_sandbox_available?t('settings.sandboxAvailable'):t('settings.unavailableHost')}</small></span></button><button type="button" className={`${shellMode==='host'?'active ':''}host`} disabled={!settings?.workspace_host_shell_available} onClick={()=>selectShellMode('host')}><TerminalSquare size={16}/><span><b>{t('settings.hostShell')}</b><small>{settings?.workspace_host_shell_available?`${settings.workspace_shell_name||t('settings.systemShell')} · ${t('settings.fullAuthority')}`:t('settings.noShell')}</small></span></button><button type="button" className={shellMode==='disabled'?'active':''} onClick={()=>selectShellMode('disabled')}><Power size={16}/><span><b>{t('settings.shellDisabled')}</b></span></button></div>{shellMode==='host'&&<div className="workspace-shell-warning"><ShieldAlert size={15}/><b>{t('settings.hostWarning')}</b></div>}{shellMode==='sandbox'&&!settings?.workspace_sandbox_available&&<div className="workspace-shell-warning"><ShieldAlert size={15}/><b>{t('settings.sandboxWarning')}</b></div>}<SettingsSectionFooter dirty={shellDirty} busy={busy} saving={savingSection==='shell'} onDiscard={()=>discard('shell')}/></form>
			</SettingsDisclosure>
		</div>
	<WorkspaceSettingsPanel workspaces={capabilities.workspaces} refresh={refresh} onNotify={notify}/>
	<MCPServerModePanel settings={settings} refresh={refresh}/>
	<WebSearchSettingsPanel proxies={proxies} refresh={refresh}/>
  </div>
}

function WorkspaceSettingsPanel({workspaces,refresh,onNotify}:{workspaces:WorkspaceCapability[];refresh:()=>Promise<void>;onNotify:NotificationSink}){
	const {t}=useTranslation()
	const empty:WorkspaceInput={id:'',access:'read_only'}
	const [open,setOpen]=useState(false),[editing,setEditing]=useState(''),[input,setInput]=useState<WorkspaceInput>(empty),[busy,setBusy]=useState(''),[deleteCandidate,setDeleteCandidate]=useState<WorkspaceCapability|null>(null)
	const beginCreate=()=>{setEditing('');setInput(empty);setOpen(true)}
	const beginEdit=(workspace:WorkspaceCapability)=>{setEditing(workspace.id);setInput({id:workspace.id,access:workspace.access});setOpen(true)}
	const close=()=>{setOpen(false);setEditing('');setInput(empty)}
	const save=async()=>{if(!input.id.trim())return;setBusy('save');try{if(editing)await api.updateWorkspace(editing,{...input,id:editing});else await api.createWorkspace({...input,id:input.id.trim()});await refresh();onNotify(editing?t('workspace.settingsUpdated',{id:editing}):t('workspace.settingsCreated',{id:input.id.trim()}));close()}catch(err){onNotify(errorText(err),'error')}finally{setBusy('')}}
	const remove=async()=>{if(!deleteCandidate)return;const workspace=deleteCandidate;setBusy(`delete-${workspace.id}`);try{await api.deleteWorkspace(workspace.id);await refresh();onNotify(t('workspace.settingsRemoved',{id:workspace.id}));if(editing===workspace.id)close()}catch(err){onNotify(errorText(err),'error')}finally{setBusy('');setDeleteCandidate(null)}}
	return <SettingsDisclosure className="workspace-settings" icon={<FolderOpen size={18}/>} title={t('settings.capabilities')} meta={t('workspace.registeredCount',{count:workspaces.length})}><div className="workspace-settings-actions"><button type="button" onClick={beginCreate}><Plus size={13}/>{t('workspace.add')}</button></div>{open&&<div className="workspace-settings-editor"><label><span>{t('workspace.id')}</span><input value={input.id} disabled={!!editing} maxLength={64} onChange={event=>setInput(current=>({...current,id:event.target.value}))}/></label><label><span>{t('workspace.permission')}</span><AppSelect value={input.access} ariaLabel={t('workspace.permission')} onChange={value=>setInput(current=>({...current,access:value as WorkspaceInput['access']}))} options={[{value:'read_only',label:t('workspace.readOnly')},{value:'read_write',label:t('workspace.readWrite')}]}/></label><div><button type="button" onClick={close}>{t('common.cancel')}</button><button type="button" className="primary" disabled={busy==='save'||!input.id.trim()} onClick={()=>void save()}>{busy==='save'?<LoaderCircle className="spin" size={13}/>:<Save size={13}/>} {t('common.save')}</button></div></div>}<div className="workspace-settings-list">{workspaces.map(workspace=><div className="workspace-settings-row" key={workspace.id}><code>{workspace.id}</code><em className={workspace.access}>{workspace.access==='read_write'?t('workspace.readWrite'):t('workspace.readOnly')}</em><button type="button" title={t('common.edit')} onClick={()=>beginEdit(workspace)}><Edit3 size={13}/></button><button type="button" className="danger" disabled={busy===`delete-${workspace.id}`} title={t('workspace.remove')} onClick={()=>setDeleteCandidate(workspace)}>{busy===`delete-${workspace.id}`?<LoaderCircle className="spin" size={13}/>:<Trash2 size={13}/>}</button></div>)}{!workspaces.length&&<div className="workspace-settings-empty">{t('settings.noWorkspace')}</div>}</div>{deleteCandidate&&<DestructiveConfirmDialog title={t('workspace.removeTitle',{id:deleteCandidate.id})} busy={busy===`delete-${deleteCandidate.id}`} onCancel={()=>setDeleteCandidate(null)} onConfirm={()=>void remove()}/>}</SettingsDisclosure>
}

const defaultWebSearchInput:WebSearchSettingsInput={enabled:false,base_url:'https://api.tavily.com',api_key:'',proxy_id:'',timeout_seconds:20,max_results:10}

function MCPServerModePanel({settings,refresh}:{settings:SystemSettings|null;refresh:()=>Promise<void>}){
	const {t}=useTranslation()
	const notify=useNotifier()
	const [busy,setBusy]=useState<'start'|'stop'|'rotate'|''>(''),[token,setToken]=useState('')
	const enabled=!!settings?.mcp_http_enabled
	const endpoint=`${window.location.origin}/mcp`
	useEffect(()=>{if(!enabled)setToken('')},[enabled])
	const update=async(nextEnabled:boolean,rotate=false)=>{
		if(!settings)return
		setBusy(rotate?'rotate':nextEnabled?'start':'stop')
		try{
			const result=await api.saveSystemSettings({
				agent_max_iterations:settings.agent_max_iterations,
				mcp_http_enabled:nextEnabled,
				rotate_mcp_http_token:rotate,
			})
			setToken(result.mcp_http_token||'')
			notify(t(nextEnabled?'mcpServerMode.started':'mcpServerMode.stopped'))
			if(desktopRuntime)await invoke('set_tray_mode',{enabled:result.mcp_http_enabled}).catch(()=>{})
			await refresh()
		}catch(err){notify(errorText(err),'error')}finally{setBusy('')}
	}
	const copy=async(value:string,message:string)=>{
		try{await navigator.clipboard.writeText(value);notify(message)}
		catch(err){notify(errorText(err),'error')}
	}
	const enterLightweightMode=async()=>{
		try{await invoke('enter_lightweight_mode')}
		catch(err){notify(errorText(err),'error')}
	}
	return <SettingsDisclosure className="mcp-server-mode" icon={<Braces size={18}/>} title={t('mcpServerMode.title')} meta={enabled?t('common.enabled'):t('common.disabled')}>
		<div className="mcp-server-mode-fields">
			<label><span>{t('mcpServerMode.endpoint')}</span><div><input readOnly value={endpoint}/><button type="button" title={t('common.copy')} onClick={()=>void copy(endpoint,t('mcpServerMode.endpointCopied'))}><Copy size={13}/></button></div></label>
			{enabled&&<label><span>{t('mcpServerMode.token')}</span><div><input readOnly type={token?'text':'password'} value={token||'••••••••••••••••'} /><button type="button" disabled={!token} title={t('common.copy')} onClick={()=>void copy(token,t('mcpServerMode.tokenCopied'))}><Copy size={13}/></button></div>{!token&&settings?.mcp_http_token_configured&&<small>{t('mcpServerMode.tokenStored')}</small>}</label>}
		</div>
		<footer>
			{enabled&&desktopRuntime&&<button type="button" disabled={!!busy} onClick={()=>void enterLightweightMode()}><Minimize2 size={13}/>{t('mcpServerMode.lightweightMode')}</button>}
			{enabled&&<button type="button" disabled={!!busy} onClick={()=>void update(true,true)}>{busy==='rotate'?<LoaderCircle className="spin" size={13}/>:<RefreshCw size={13}/>} {t('mcpServerMode.rotate')}</button>}
			<button type="button" className={enabled?'danger':'primary'} disabled={!!busy||!settings} onClick={()=>void update(!enabled)}>{busy?<LoaderCircle className="spin" size={13}/>:<Power size={13}/>} {t(enabled?'mcpServerMode.stop':'mcpServerMode.start')}</button>
		</footer>
	</SettingsDisclosure>
}

function WebSearchSettingsPanel({proxies,refresh}:{proxies:Proxy[];refresh:()=>Promise<void>}){
	const {t}=useTranslation()
	const notify=useNotifier()
	const [stored,setStored]=useState<WebSearchSettings|null>(null),[input,setInput]=useState<WebSearchSettingsInput>(defaultWebSearchInput)
	const [loading,setLoading]=useState(true),[busy,setBusy]=useState(''),[dirty,setDirty]=useState(false)
	const hasEffectiveAPIKey=!!input.api_key?.trim()||!!stored?.has_api_key&&!input.clear_api_key
	const applyStored=(value:WebSearchSettings)=>{setStored(value);setInput({enabled:value.enabled,base_url:value.base_url,api_key:'',proxy_id:value.proxy_id||'',timeout_seconds:value.timeout_seconds,max_results:value.max_results});setDirty(false)}
	useEffect(()=>{let active=true;api.webSearchSettings().then(value=>{if(active)applyStored(value)}).catch(err=>{if(active)notify(errorText(err),'error')}).finally(()=>{if(active)setLoading(false)});return()=>{active=false}},[])
	const update=<K extends keyof WebSearchSettingsInput>(key:K,value:WebSearchSettingsInput[K])=>{setInput(current=>({...current,[key]:value}));setDirty(true)}
	const save=async()=>{setBusy('save');try{const value=await api.saveWebSearchSettings(input);applyStored(value);notify(t('webSearch.saved'));await refresh()}catch(err){notify(errorText(err),'error')}finally{setBusy('')}}
	const test=async()=>{setBusy('test');try{const result=await api.testWebSearch();notify(t('webSearch.testPassed',{count:result.results.length}))}catch(err){notify(errorText(err),'error')}finally{setBusy('')}}
	const clearKey=()=>{setInput(current=>({...current,enabled:false,api_key:'',clear_api_key:true}));setDirty(true)}
	if(loading)return <SettingsDisclosure className="web-search-settings" icon={<Search size={18}/>} title={t('webSearch.title')} meta={t('common.loading')}><div className="settings-loading"><LoaderCircle className="spin" size={16}/>{t('common.loading')}</div></SettingsDisclosure>
	return <SettingsDisclosure className="web-search-settings" icon={<Search size={18}/>} title={t('webSearch.title')} meta={input.enabled?t('common.enabled'):t('common.disabled')}><label className="web-search-toggle"><span>{t('webSearch.title')}</span><input type="checkbox" checked={input.enabled} onChange={event=>update('enabled',event.target.checked)}/><i/><b>{input.enabled?t('common.enabled'):t('common.disabled')}</b></label><div className="web-search-grid"><label><span>{t('webSearch.baseURL')}</span><input value={input.base_url} onChange={event=>update('base_url',event.target.value)} placeholder="https://api.tavily.com"/></label><label><span>{t('webSearch.apiKey')}</span><PasswordInput value={input.api_key||''} onChange={event=>update('api_key',event.target.value)} placeholder={stored?.has_api_key?t('webSearch.savedSecret'):''}/></label><label><span>{t('common.proxy')}</span><AppSelect value={input.proxy_id||''} ariaLabel={t('common.proxy')} onChange={value=>update('proxy_id',value)} options={[{value:'',label:t('common.direct')},...proxies.map(proxy=>({value:proxy.id,label:`${proxy.name} · ${proxy.url}`}))]}/></label><label><span>{t('webSearch.timeout')}</span><input type="number" min="5" max="120" value={input.timeout_seconds} onChange={event=>update('timeout_seconds',Number(event.target.value))}/></label><label><span>{t('webSearch.maxResults')}</span><input type="number" min="1" max="20" value={input.max_results} onChange={event=>update('max_results',Number(event.target.value))}/></label></div><footer><div>{stored?.has_api_key&&<button type="button" className="danger" onClick={clearKey}>{t('webSearch.clearKey')}</button>}</div><button type="button" disabled={busy!==''||dirty||!stored?.enabled||!stored.has_api_key} onClick={()=>void test()}>{busy==='test'?<LoaderCircle className="spin" size={13}/>:<Search size={13}/>} {t('common.test')}</button><button type="button" className="primary" disabled={busy!==''||!dirty||input.enabled&&!hasEffectiveAPIKey} onClick={()=>void save()}>{busy==='save'?<LoaderCircle className="spin" size={13}/>:<Save size={13}/>} {t('common.save')}</button></footer></SettingsDisclosure>
}

function Nav({ active, icon, label, count, warn, onClick }: {active:boolean;icon:React.ReactNode;label:string;count?:number;warn?:boolean;onClick:()=>void}) {
  return <button className={`nav-item ${active ? 'active' : ''}`} onClick={onClick} title={label} aria-label={label} aria-current={active?'page':undefined}>{icon}<span>{label}</span>{count !== undefined && <em className={warn ? 'warn' : ''}>{count}</em>}</button>
}

function ChatPage({ visible, onActivate, hosts, providers, approvals, runs, workspaceShells, capabilities, settings, imageTypes, agentAvailable, modelName, contextWindow, refresh, refreshConnections, refreshApprovals, onCreateWorkspaceShell, onOpenWorkspaceShell, onWorkspaceShellStarted, onSettingsChanged, onHostChanged, onModelChanged, sidebarTarget, onSessionDeleted, onError, onStreamingChange }: {visible:boolean;onActivate:()=>void;hosts:Host[];providers:ModelProvider[];approvals:Approval[];runs:Run[];workspaceShells:SSHShell[];capabilities:ToolCapabilities;settings:SystemSettings|null;imageTypes:string[];agentAvailable:boolean;modelName?:string;contextWindow:number;refresh:()=>Promise<void>;refreshConnections:()=>Promise<void>;refreshApprovals:(decidedID?:string)=>Promise<void>;onCreateWorkspaceShell:(workspaceID:string)=>Promise<void>;onOpenWorkspaceShell:(shell:SSHShell)=>void;onWorkspaceShellStarted:(shell:SSHShell)=>void;onSettingsChanged:(settings:SystemSettings)=>void;onHostChanged:(host:Host)=>void;onModelChanged:(provider:ModelProvider)=>void;sidebarTarget:HTMLDivElement|null;onSessionDeleted:(sessionID:string)=>void;onError:(message:string)=>void;onStreamingChange:(streaming:boolean)=>void}) {
		const {t,i18n:instance}=useTranslation()
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
  const [loadingSession, setLoadingSession] = useState('')
  const [workspacePanelCollapsed,setWorkspacePanelCollapsed]=useState(recalledWorkspacePanelCollapsed)
  const [running, setRunning] = useState(false)
  const [detachedRunning,setDetachedRunning]=useState(false)
	const [stopping,setStopping]=useState(false)
	const [modelRetry,setModelRetry]=useState<ModelRetryState|null>(null)
	const [connectionRetry,setConnectionRetry]=useState<ConnectionRetryState|null>(null)
	const [retryClock,setRetryClock]=useState(0)
  const [reasoningSeen, setReasoningSeen] = useState(false)
		const [contextUsage,setContextUsage]=useState<ContextUsage>({tokens:0,window:activeContextWindow})
		useEffect(()=>setContextUsage(current=>current.tokens===0?{...current,window:activeContextWindow}:current),[activeContextWindow])
  const [tasks,setTasks]=useState<AgentTaskList|null>(null)
	const [tasksExpanded,setTasksExpanded]=useState(false)
	const [approvalNotice,setApprovalNotice]=useState('')
	const [workspaceID,setWorkspaceID]=useState(recalledWorkspace)
	const [boundWorkspaceID,setBoundWorkspaceID]=useState('')
	const [workspaceSwitching,setWorkspaceSwitching]=useState(false)
  const messagesRef=useRef<HTMLDivElement>(null)
  const stickToLatest=useRef(true)
	const userScrollFrame=useRef(0)
	const disclosureScrollFrame=useRef(0)
	  const activeStreamRef=useRef<ActiveChatStream|null>(null)
	  const imageURLsRef=useRef(new Set<string>())
	const reconnectErrorRef=useRef('')
  const sessionLoadRef=useRef('')
  const currentApprovals=useMemo(()=>sessionId?approvals.filter(item=>item.session_id===sessionId):[],[approvals,sessionId])
	const pendingExplanationID=currentApprovals.find(item=>item.ai_review?.status==='pending')?.id||''
	const sessionBusy=running||detachedRunning
	const toolsRunning=entries.some(item=>item.kind==='tool'&&item.transient)
	const selectedWorkspace=capabilities.workspaces.find(workspace=>workspace.id===workspaceID)||capabilities.workspaces[0]
	useEffect(()=>{if(!sessionId)setContextUsage({tokens:0,window:activeContextWindow})},[activeContextWindow,sessionId])
	useEffect(()=>{if(!selectedWorkspace)return;if(workspaceID!==selectedWorkspace.id)setWorkspaceID(selectedWorkspace.id);rememberWorkspace(selectedWorkspace.id)},[selectedWorkspace,workspaceID])
	useEffect(()=>{if(!tasks)setTasksExpanded(false);else setTasksExpanded(true)},[tasks?.session_id,!!tasks])
	useEffect(()=>{
		if(!modelRetry&&!connectionRetry)return
		setRetryClock(Date.now())
		const timer=window.setInterval(()=>setRetryClock(Date.now()),1000)
		return()=>window.clearInterval(timer)
	},[modelRetry,connectionRetry])
	useEffect(()=>{onStreamingChange(running)},[running,onStreamingChange])
	useEffect(()=>()=>onStreamingChange(false),[onStreamingChange])
	useEffect(()=>()=>{sessionLoadRef.current='';const stream=activeStreamRef.current;activeStreamRef.current=null;stream?.controller.abort();window.cancelAnimationFrame(userScrollFrame.current);window.cancelAnimationFrame(disclosureScrollFrame.current)},[])
	useEffect(()=>()=>{for(const url of imageURLsRef.current)URL.revokeObjectURL(url);imageURLsRef.current.clear()},[])
	const addImages=(files:File[])=>{const accepted=files.filter(file=>imageTypes.includes(file.type));if(accepted.length!==files.length)setImageNotice(t('chat.imageTypeRejected'));if(!accepted.length)return;const next=accepted.map(file=>{const url=URL.createObjectURL(file);imageURLsRef.current.add(url);return{id:clientId(),file,url}});setPendingImages(current=>[...current,...next])}
	const removePendingImage=(id:string)=>{setPendingImages(current=>{const target=current.find(image=>image.id===id);if(target){URL.revokeObjectURL(target.url);imageURLsRef.current.delete(target.url)}return current.filter(image=>image.id!==id)});setImageInputKey(value=>value+1)}
	const clearPendingImages=()=>{for(const image of pendingImages){URL.revokeObjectURL(image.url);imageURLsRef.current.delete(image.url)}setPendingImages([]);setImageInputKey(value=>value+1);setImageNotice('')}
	useEffect(()=>{
		if(!pendingExplanationID)return
		void refreshApprovals()
		const timer=window.setInterval(()=>{if(document.visibilityState==='visible')void refreshApprovals()},1500)
		return()=>window.clearInterval(timer)
	},[pendingExplanationID,refreshApprovals])

  const refreshSessions = useCallback(async () => {
    try {
      const items = await api.chatSessions(); setSessions(items); setHistoryError(''); return items
    } catch (err) { setHistoryError(errorText(err)); return [] }
  }, [])

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
    stickToLatest.current=true
    try {
      const state = await api.chatState(id)
      if(sessionLoadRef.current!==requestID)return
	      setEntries(historyEntries(state.messages||[],id));setDetachedRunning(!!state.active);setStopping(false);setModelRetry(null);setConnectionRetry(null);setTasks(state.tasks?.items?.length?state.tasks:null);setContextUsage({tokens:state.context_tokens||0,window:contextWindowForSession(state.context_tokens||0,state.context_window||0,activeContextWindow)});setWorkspaceID(state.workspace_id||'');setBoundWorkspaceID(state.workspace_id||'')
      setSessionId(id); rememberSession(id); setHistoryError('')
      void refresh()
    } catch (err) { if(sessionLoadRef.current===requestID)setHistoryError(errorText(err)) }
    finally { if(sessionLoadRef.current===requestID)setLoadingSession('') }
  }, [activeContextWindow,refresh])

  const activeSessionCount=useMemo(()=>sessions.filter(item=>item.active).length,[sessions])
  useEffect(()=>{
    if(!activeSessionCount)return
    const timer=window.setInterval(()=>{if(document.visibilityState==='visible')void refreshSessions()},2500)
    return()=>window.clearInterval(timer)
  },[activeSessionCount,refreshSessions])

  useLayoutEffect(()=>{
    if(!stickToLatest.current)return
    const container=messagesRef.current
    if(container)container.scrollTop=container.scrollHeight
  },[entries,loadingSession])

	const trackUserScroll=()=>{
		window.cancelAnimationFrame(userScrollFrame.current)
		userScrollFrame.current=window.requestAnimationFrame(()=>{
			const container=messagesRef.current
			if(container)stickToLatest.current=container.scrollHeight-container.scrollTop-container.clientHeight<90
		})
	}

	const preserveToolDisclosurePosition=useCallback((summary:HTMLElement)=>{
		const container=messagesRef.current
		if(!container||!container.contains(summary))return
		window.cancelAnimationFrame(userScrollFrame.current)
		stickToLatest.current=false
		const top=summary.getBoundingClientRect().top
		window.cancelAnimationFrame(disclosureScrollFrame.current)
		disclosureScrollFrame.current=window.requestAnimationFrame(()=>{
			if(!container.isConnected||!summary.isConnected)return
			container.scrollTop+=summary.getBoundingClientRect().top-top
		})
	},[])

  useEffect(() => {
    let active = true
    void (async () => {
      const items = await api.chatSessions().catch((err) => { if (active) setHistoryError(errorText(err)); return [] })
      if (!active) return
      setSessions(items)
      const remembered = recalledSession()
      if (remembered === newSessionMarker) return
      const target = items.some((item) => item.id === remembered) ? remembered : items[0]?.id
      if (target) await loadSession(target)
    })()
    return () => { active = false }
  }, [loadSession])

  const newChat = () => {
		if(workspaceSwitching)return
		onActivate()
    detachActiveStream()
    sessionLoadRef.current=''
    setLoadingSession('')
	    stickToLatest.current=true;setSessionId('');setBoundWorkspaceID('');setEntries([]); setMessage('');clearPendingImages(); setHistoryError(''); setReasoningSeen(false);setContextUsage({tokens:0,window:activeContextWindow});setDetachedRunning(false);setStopping(false);setModelRetry(null);setConnectionRetry(null);setTasks(null); rememberSession(newSessionMarker)
    void refreshSessions()
  }

  const switchSession = (id:string) => {
		if(workspaceSwitching)return
		onActivate()
    if(id===sessionId){
      if(loadingSession){sessionLoadRef.current='';setLoadingSession('')}
      return
    }
    detachActiveStream()
		setDetachedRunning(false);setStopping(false);setConnectionRetry(null)
    void loadSession(id)
    void refreshSessions()
  }

	const switchWorkspace=async(id:string)=>{
		if(id===selectedWorkspace?.id||sessionBusy||loadingSession||workspaceSwitching)return
		if(!sessionId){setWorkspaceID(id);return}
		setWorkspaceSwitching(true);setHistoryError('')
		try{
			const session=await api.setChatSessionWorkspace(sessionId,id)
			setWorkspaceID(session.workspace_id);setBoundWorkspaceID(session.workspace_id)
			setSessions(current=>current.map(item=>item.id===session.id?{...item,workspace_id:session.workspace_id,updated_at:session.updated_at}:item))
			void refreshSessions()
		}catch(err){setHistoryError(errorText(err))}
		finally{setWorkspaceSwitching(false)}
	}

  const removeSession = async () => {
    if(!sessionDeleteCandidate)return
    const session=sessionDeleteCandidate
    setDeletingSession(true)
    try {
      await api.deleteChatSession(session.id)
	  onSessionDeleted(session.id)
      if (session.id === sessionId) newChat()
      await refreshSessions()
    } catch (err) { setHistoryError(errorText(err)) }
    finally { setDeletingSession(false); setSessionDeleteCandidate(null) }
  }

	const handleAgentFrame=useCallback((frame:AgentEvent,userEntryID='',workspace='')=>{
		const updateUser=(item:ChatEntry,status:'completed'|'failed')=>item.kind==='user'&&(userEntryID?item.id===userEntryID:item.status==='pending')?{...item,status}:item
		if(frame.session_id){
			if(activeStreamRef.current)activeStreamRef.current.sessionId=frame.session_id
			setSessionId(frame.session_id)
			if(workspace)setBoundWorkspaceID(workspace)
			rememberSession(frame.session_id)
		}
		if(frame.type==='retry'){
			const now=Date.now()
			setRetryClock(now)
			setModelRetry({attempt:frame.retry_attempt||1,max:frame.retry_max||1,readyAt:now+(frame.retry_delay_ms||0)})
		}else if(['approval','reasoning','tool','tool_output','message_start','message','message_commit','message_reset','done','interrupted','model_error','error'].includes(frame.type))setModelRetry(null)
		if(frame.type==='approval'){
			setEntries(old=>updateToolStatusByRunID(old.map(item=>updateUser(item,'completed')),'approval_required',frame.run_id))
			setApprovalNotice('')
			void refreshApprovals()
		}
		if(frame.type==='tool_output')setEntries(old=>appendToolOutput(old,frame))
		if(frame.type==='context_usage'&&frame.context_tokens!==undefined)setContextUsage({tokens:frame.context_tokens,window:frame.context_window||activeContextWindow})
		if(frame.type==='reasoning'&&frame.content){
			setReasoningSeen(true)
			const reasoningID=`reasoning_${frame.segment_id||'current'}`
			setEntries(old=>{
				const existing=old.find(item=>item.id===reasoningID)
				if(existing)return old.map(item=>item.id===reasoningID?{...item,content:item.content+frame.content,active:true}:item)
				return[...old.map(deactivateReasoning),{id:reasoningID,kind:'reasoning',content:frame.content!,active:true}]
			})
		}
		if(frame.type==='tool'&&frame.content){
			if(frame.status!=='in_progress'&&['ssh_shell','workspace_shell','ssh_tunnel'].includes(frame.tool_name||''))void refreshConnections()
			if(frame.tool_name==='workspace_shell'){
				const shell=workspaceShellStartedByTool(frame.content)
				if(shell)onWorkspaceShellStarted(shell)
			}
			setEntries(old=>{
				const callID=frame.tool_call_id||''
				const runID=frame.run_id||toolContentRunID(frame.content!)
				let index=callID?old.findIndex(item=>item.kind==='tool'&&item.toolCallId===callID):-1
				if(index<0&&runID)index=old.findIndex(item=>item.kind==='tool'&&toolEntryRunID(item)===runID)
				const transient=frame.status==='in_progress'
				if(index>=0){
					const current=old[index]
					if(transient&&!current.transient&&settledToolStatus(toolContentStatus(current.content)))return old
					return old.map((item,itemIndex)=>itemIndex===index?{...item,content:frame.content!,tool:frame.tool_name||item.tool,toolCallId:callID||item.toolCallId,runId:runID||item.runId,liveStdout:transient?item.liveStdout:undefined,liveStderr:transient?item.liveStderr:undefined,transient}:item)
				}
				const entry:ChatEntry={id:callID?`tool_${callID}`:clientId(),kind:'tool',content:frame.content!,tool:frame.tool_name,toolCallId:callID||undefined,runId:runID||undefined,transient,startedAt:Date.now()}
				return[...old.map(deactivateReasoning),entry]
			})
			if(/^Task(Create|Get|Update|List)$/.test(frame.tool_name||'')){const nextTasks=tasksFromToolContent(frame.content);if(nextTasks)setTasks(nextTasks.items.length?nextTasks:null)}
			if(/approval_id|approval_required/.test(frame.content))void refresh()
		}
		if(frame.type==='message_start'&&frame.message_id)setEntries(old=>startAssistantLifecycle(old,frame.message_id!))
		if(frame.type==='message'&&frame.message_id&&frame.content)setEntries(old=>appendAssistantDelta(old,frame.message_id!,frame.content!))
		if(frame.type==='message_commit'&&frame.message_id)setEntries(old=>commitAssistantLifecycle(old,frame.message_id!))
		if(frame.type==='message_reset'&&frame.message_id)setEntries(old=>resetAssistantLifecycle(old,frame.message_id!))
		if(frame.type==='done'){
			setStopping(false)
			setEntries(old=>old.map(item=>updateUser(item,'completed')))
		}
		if(frame.type==='interrupted'){
			setStopping(false)
			setEntries(old=>[...old.map(item=>updateUser(deactivateReasoning(item),'failed')),{id:clientId(),kind:'assistant',content:frame.content||t('chat.stopped'),lifecycle:'committed'}])
		}
		if(frame.type==='model_error'||frame.type==='error')setEntries(old=>[...old.map(item=>updateUser(item,'failed')),{id:clientId(),kind:'error',content:frame.error||t('chat.agentError')}])
	},[activeContextWindow,onWorkspaceShellStarted,refresh,refreshApprovals,refreshConnections,t])

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
				setRetryClock(now)
				setConnectionRetry({attempt,readyAt:now+delay})
				try{
					await waitForReconnect(delay,controller.signal)
					const state=await api.chatState(sessionId)
					if(!active)return
					setTasks(state.tasks?.items?.length?state.tasks:null)
					setContextUsage({tokens:state.context_tokens||0,window:contextWindowForSession(state.context_tokens||0,state.context_window||0,activeContextWindow)})
					setBoundWorkspaceID(state.workspace_id||'')
					if(!state.active){
						setEntries(old=>settledTurnEntries(state.messages||[],sessionId,old,false))
						setDetachedRunning(false);setStopping(false);setConnectionRetry(null);setHistoryError('')
						void refreshSessions();void refresh()
						return
					}
					setEntries(reconnectBaseEntries(state.messages||[],sessionId))
					setReasoningSeen(false)
					setConnectionRetry(null)
					setHistoryError('')
					await reconnectChatStream(sessionId,0,frame=>{if(active)handleAgentFrame(frame)},controller.signal)
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
	},[activeContextWindow,detachedRunning,handleAgentFrame,refresh,refreshSessions,running,sessionId])

	useEffect(()=>{
		if(!sessionId||!toolsRunning)return
		let active=true
		const poll=async()=>{
			try{
				const state=await api.chatState(sessionId)
				if(!active)return
				setEntries(old=>mergePersistedToolEntries(state.messages||[],sessionId,old))
				setContextUsage({tokens:state.context_tokens||0,window:contextWindowForSession(state.context_tokens||0,state.context_window||0,activeContextWindow)})
				if(state.active&&!running)setDetachedRunning(true)
				if(!state.active&&!(state.tool_calls||[]).some(call=>call.status==='running'))setStopping(false)
			}catch{/* keep the last confirmed tool state and retry */}
		}
		void poll()
		const timer=window.setInterval(()=>{if(document.visibilityState==='visible')void poll()},1000)
		return()=>{active=false;window.clearInterval(timer)}
	},[activeContextWindow,running,sessionId,toolsRunning])

	  const sendQuery = async (query:string,queryImages:PendingChatImage[]) => {
	    query=query.trim(); if((!query&&!queryImages.length)||sessionBusy||loadingSession||workspaceSwitching)return
	    let querySessionID=sessionId||newChatSessionID()
    const userEntryID=clientId()
    const streamID=clientId()
    const controller=new AbortController()
		const workspace=selectedWorkspace?.id||''
		activeStreamRef.current={id:streamID,sessionId:querySessionID,controller}
    const isAttached=()=>activeStreamRef.current?.id===streamID
    stickToLatest.current=true
		setSessionId(querySessionID);rememberSession(querySessionID)
		reconnectErrorRef.current=''
		setApprovalNotice('');setReasoningSeen(false);setStopping(false);setModelRetry(null);setConnectionRetry(null);setRunning(true)
	    const entryImages=queryImages.map(image=>({id:image.id,name:image.file.name,mimeType:image.file.type,sizeBytes:image.file.size,url:image.url}))
	    setEntries((old) => [...old, { id: userEntryID, kind: 'user', content: query, images:entryImages, status:'pending' }])
	    try {
			await streamChat(querySessionID,workspace,query,queryImages.map(image=>image.file),(frame:AgentEvent)=>{
				if(!isAttached())return
				if(frame.session_id)querySessionID=frame.session_id
				handleAgentFrame(frame,userEntryID,workspace)
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
      if(!isAttached())return
      setModelRetry(null)
			setConnectionRetry(null)
			setEntries((old) => old.map(deactivateReasoning))
      setRunning(false)
		setStopping(false)
		if(querySessionID){try{const state=await api.chatState(querySessionID);if(!isAttached())return;setDetachedRunning(!!state.active);setTasks(state.tasks?.items?.length?state.tasks:null);setContextUsage({tokens:state.context_tokens||0,window:contextWindowForSession(state.context_tokens||0,state.context_window||0,activeContextWindow)});setBoundWorkspaceID(state.workspace_id||'');setEntries(old=>settledTurnEntries(state.messages||[],querySessionID,old,!!state.active));for(const image of queryImages){URL.revokeObjectURL(image.url);imageURLsRef.current.delete(image.url)}}catch{/* polling or the next reload will recover state */}}
      if(!isAttached())return
      activeStreamRef.current=null
      void refreshSessions();void refresh()
    }
  }

	  const submit = (event: FormEvent) => {event.preventDefault();const query=message.trim();if((!query&&!pendingImages.length)||sessionBusy||loadingSession||workspaceSwitching)return;const images=pendingImages;setMessage('');setPendingImages([]);setImageInputKey(value=>value+1);setImageNotice('');void sendQuery(query,images)}
	const stopAgent = async () => {
		const targetSessionID=activeStreamRef.current?.sessionId||sessionId
		if(!targetSessionID||(!sessionBusy&&!toolsRunning)||stopping)return
		setStopping(true)
		let requested=false
		try{
			const result=await api.cancelChatSession(targetSessionID)
			requested=result.cancelled
			if(!result.cancelled){const state=await api.chatState(targetSessionID);setDetachedRunning(!!state.active);setTasks(state.tasks?.items?.length?state.tasks:null);setContextUsage({tokens:state.context_tokens||0,window:contextWindowForSession(state.context_tokens||0,state.context_window||0,activeContextWindow)});setEntries(historyEntries(state.messages||[],targetSessionID));void refreshSessions();void refresh()}
		}catch(err){setEntries(old=>[...old,{id:clientId(),kind:'error',content:t('chat.stopFailed',{message:errorText(err)})}])}
		finally{if(!requested)setStopping(false)}
  }
	const streamingResponseStarted=entries.some((item)=>item.kind==='assistant'&&item.lifecycle==='streaming'&&item.content!=='')
  const retryDelay=modelRetry?Math.max(0,Math.ceil((modelRetry.readyAt-retryClock)/1000)):0
  const modelRetryLabel=modelRetry?t(retryDelay>0?'chat.retryWaiting':'chat.retryingModel',{
	  attempt:modelRetry.attempt,
	  max:modelRetry.max,
	  delay:retryDelay,
  }):''
	const connectionRetryDelay=connectionRetry?Math.max(0,Math.ceil((connectionRetry.readyAt-retryClock)/1000)):0
	const connectionRetryLabel=connectionRetry?t('chat.reconnecting',{attempt:connectionRetry.attempt,delay:connectionRetryDelay}):''
	const setWorkspaceCollapsed=(collapsed:boolean)=>{
		rememberWorkspacePanelCollapsed(collapsed)
		setWorkspacePanelCollapsed(collapsed)
	}
	const sessionSidebar=sidebarTarget&&createPortal(<>
		<header className="sidebar-conversation-head"><span><History size={15}/>{t('chat.conversations')}</span><button className="new-chat-button" onClick={newChat} disabled={workspaceSwitching} title={t('chat.newConversation')} aria-label={t('chat.newConversation')}><Plus size={14}/><span>{t('common.new')}</span></button></header>
		<div className="session-list">
			{historyError&&<div className="history-error">{historyError}</div>}
			{!sessions.length&&!historyError&&<div className="history-empty">{t('chat.noSaved')}</div>}
			{sessions.map(session=>{const pending=approvals.filter(item=>item.session_id===session.id).length;const active=session.active||(session.id===sessionId&&(sessionBusy||toolsRunning));return <div className={`session-item ${session.id===sessionId?'active':''}`} key={session.id}><button className="session-open" onClick={()=>switchSession(session.id)} disabled={workspaceSwitching||loadingSession===session.id}><b>{session.title}{pending>0&&<em className="session-approval-count">{t('chat.approvalCount',{count:pending})}</em>}{active&&<em className="session-running-count">{t('chat.runningBadge')}</em>}</b><span>{new Date(session.updated_at).toLocaleString(localeFor(instance.language))} · {t('chat.messageCount',{count:session.message_count})}</span></button><button className="session-delete" onClick={()=>{if(!active)setSessionDeleteCandidate(session)}} disabled={active||workspaceSwitching} title={active?t('chat.cannotDelete'):t('chat.deleteConversation')}><Trash2 size={13}/></button></div>})}
		</div>
	</>,sidebarTarget)

  return <>{sessionSidebar}<div className={`chat-layout ${workspacePanelCollapsed?'workspace-panel-collapsed ':''}${visible?'':'page-hidden'}`}>
		<ChatWorkspacePanel key={selectedWorkspace?.id||''} workspaces={capabilities.workspaces} workspaceID={selectedWorkspace?.id||''} shells={workspaceShells} switching={workspaceSwitching} disabled={sessionBusy||!!loadingSession} bound={!!selectedWorkspace&&boundWorkspaceID===selectedWorkspace.id} onSelect={id=>void switchWorkspace(id)} onCreateShell={onCreateWorkspaceShell} onOpenShell={onOpenWorkspaceShell} onCollapse={()=>setWorkspaceCollapsed(true)}/>
    <div className="chat-main panel">
	  <div className="panel-header"><div><Bot size={18}/><span>{t('chat.session')}</span>{workspacePanelCollapsed&&<button className="chat-panel-open-button" onClick={()=>setWorkspaceCollapsed(false)} title={t('workspace.expandPanel')} aria-label={t('workspace.expandPanel')}><PanelLeftOpen size={15}/></button>}</div><div className="chat-header-actions"><span className="session-id">{sessionId ? sessionId.slice(0, 20) : t('chat.newSession')}</span></div></div>
      <div className="session-approval-slot">{currentApprovals.length>0&&<ApprovalDialog key={currentApprovals[0].id} approval={currentApprovals[0]} pendingCount={currentApprovals.length} hosts={hosts} running={sessionBusy} stopping={stopping} onStop={()=>void stopAgent()} refresh={refresh} refreshApprovals={refreshApprovals} onApproved={result=>{setEntries(old=>updateToolRunStatus(old,result.run_id,result.status==='running'?'in_progress':result.status));if(result.shell?.kind==='workspace')onWorkspaceShellStarted(result.shell)}} onNotice={setApprovalNotice}/>} {approvalNotice&&currentApprovals.length===0&&<div className="approval-toast"><ShieldCheck size={14}/><span>{approvalNotice}</span><button onClick={()=>setApprovalNotice('')}><X size={13}/></button></div>}</div>
	      <div className="session-task-slot">{tasks&&<SessionTasks tasks={tasks} expanded={tasksExpanded} onExpanded={setTasksExpanded}/>}</div>
		<div className="conversation-view">
			<div className="messages" ref={messagesRef} onWheel={trackUserScroll} onTouchMove={trackUserScroll} onPointerUp={trackUserScroll}>
				{entries.length === 0 && <div className="empty-chat"><div className="radar"><Activity size={35}/></div><h2>{t('chat.emptyTitle')}</h2></div>}
				{entries.map((entry) => <ChatBubble key={entry.id} entry={entry} runs={runs} hosts={hosts} onToolDisclosure={preserveToolDisclosurePosition}/>) }
				{running&&modelRetry&&<div className="thinking"><span/><span/><span/> {modelRetryLabel}</div>}
				{running&&!modelRetry&&!reasoningSeen&&!streamingResponseStarted&&<div className="thinking"><span/><span/><span/> {t('chat.waitingModel')}</div>}
				{detachedRunning&&!running&&connectionRetry&&<div className="thinking background-agent"><span/><span/><span/> {connectionRetryLabel}</div>}
			</div>
			{tasks&&tasksExpanded&&<SessionTaskItems tasks={tasks}/>}
		</div>
		  <form className="composer" onSubmit={submit}>
			  {(sessionBusy||toolsRunning)&&<div className="llm-work-status" role="status" aria-live="polite"><LoaderCircle className="spin" size={13}/><b>{stopping?t('chat.stopping'):connectionRetryLabel||modelRetryLabel||t(sessionBusy?'chat.running':'chat.toolsRunning')}</b><button type="button" className="agent-stop-button" onClick={()=>void stopAgent()} disabled={stopping||!(activeStreamRef.current?.sessionId||sessionId)} title={t('chat.stopTitle')}><Square size={11} fill="currentColor"/>{t('chat.stop')}</button></div>}
			  <div className="context-line"><ApprovalModeStatus settings={settings} onChanged={onSettingsChanged} onError={onError}/><ComposerHostSelector hosts={hosts} disabled={sessionBusy} onChanged={onHostChanged} onError={onError}/><div className="composer-model-controls"><ContextUsageRing usage={contextUsage}/><ComposerReasoningSelector providers={providers} disabled={sessionBusy} onChanged={onModelChanged} onError={onError}/><ComposerModelSelector providers={providers} fallbackModel={modelName} disabled={sessionBusy} onChanged={onModelChanged} onError={onError}/></div></div>
			  {pendingImages.length>0&&<div className="composer-images">{pendingImages.map(image=><div key={image.id}><img src={image.url} alt={image.file.name}/><span title={image.file.name}>{image.file.name}</span><button type="button" onClick={()=>removePendingImage(image.id)} title={t('chat.removeImage')}><X size={11}/></button></div>)}</div>}
			  {imageNotice&&<div className="composer-image-notice">{imageNotice}<button type="button" onClick={()=>setImageNotice('')}><X size={11}/></button></div>}
			  <div className="input-row"><label className="image-attach-button" title={t('chat.addImages')}><ImagePlus size={18}/><input key={imageInputKey} type="file" accept={imageTypes.join(',')} multiple disabled={!agentAvailable||sessionBusy||workspaceSwitching||!!loadingSession} onChange={event=>addImages(Array.from(event.target.files||[]))}/></label><textarea value={message} onChange={(event) => setMessage(event.target.value)} onPaste={event=>{const files=Array.from(event.clipboardData.files).filter(file=>file.type.startsWith('image/'));if(files.length)addImages(files)}} placeholder={!agentAvailable?t('chat.configureModel'):loadingSession?t('chat.loadingConversation'):sessionBusy?t('chat.busyPlaceholder'):t('chat.prompt')} disabled={!agentAvailable||sessionBusy||workspaceSwitching||!!loadingSession} onKeyDown={(event) => { if (event.key === 'Enter' && !event.shiftKey) { event.preventDefault(); event.currentTarget.form?.requestSubmit() } }}/><button aria-label={t('common.next')} disabled={!agentAvailable || sessionBusy || workspaceSwitching || !!loadingSession || (!message.trim()&&!pendingImages.length)}><Send size={18}/></button></div>
		  </form>
    </div>
	{sessionDeleteCandidate&&<DestructiveConfirmDialog title={t('chat.deleteTitle',{title:sessionDeleteCandidate.title})} busy={deletingSession} onCancel={()=>setSessionDeleteCandidate(null)} onConfirm={()=>void removeSession()}/>}
  </div></>
}

function formatFileSize(size:number){if(size<1024)return `${size} B`;if(size<1024*1024)return `${(size/1024).toFixed(1)} KiB`;return `${(size/1024/1024).toFixed(1)} MiB`}

type WorkspaceNotice={kind:'success'|'error';text:string}
type WorkspaceDeleteCandidate={workspaceID:string;path:string;type:'file'|'directory'}

function workspaceChildPath(path:string,name:string){return path==='.'?name:`${path}/${name}`}

function ChatWorkspacePanel({workspaces,workspaceID,shells,switching,disabled,bound,onSelect,onCreateShell,onOpenShell,onCollapse}:{workspaces:WorkspaceCapability[];workspaceID:string;shells:SSHShell[];switching:boolean;disabled:boolean;bound:boolean;onSelect:(id:string)=>void;onCreateShell:(workspaceID:string)=>Promise<void>;onOpenShell:(shell:SSHShell)=>void;onCollapse:()=>void}){
	const {t}=useTranslation()
	const workspace=workspaces.find(item=>item.id===workspaceID)||workspaces[0]
	const activeWorkspaceID=workspace?.id||''
	const [path,setPath]=useState('.')
	const [entries,setEntries]=useState<{name:string;type:'file'|'directory';size?:number}[]>([])
	const [loading,setLoading]=useState(false),[error,setError]=useState('')
	const [file,setFile]=useState<File|null>(null),[target,setTarget]=useState(''),[uploading,setUploading]=useState(false),[inputKey,setInputKey]=useState(0)
	const [notice,setNotice]=useState<WorkspaceNotice|null>(null),[dragging,setDragging]=useState(false)
	const [preview,setPreview]=useState<WorkspaceFilePreview|null>(null),[previewLoading,setPreviewLoading]=useState(''),[deleting,setDeleting]=useState('')
	const [deleteCandidate,setDeleteCandidate]=useState<WorkspaceDeleteCandidate|null>(null)
	const [startingShell,setStartingShell]=useState(false)
	const loadRequestRef=useRef(0),previewPathRef=useRef('')
	const activeShells=shells.filter(shell=>shell.workspace_id===activeWorkspaceID&&sshShellActive(shell.status)).sort((left,right)=>left.started_at.localeCompare(right.started_at))

	const load=useCallback(async(showLoading=true)=>{
		if(!activeWorkspaceID)return
		const requestID=++loadRequestRef.current
		if(showLoading)setLoading(true)
		try{
			const result=await api.workspaceFiles(activeWorkspaceID,path)
			if(loadRequestRef.current!==requestID)return
			setEntries(result.entries||[]);setError('')
		}catch(err){
			if(loadRequestRef.current!==requestID)return
			setEntries([]);setError(errorText(err))
		}finally{
			if(loadRequestRef.current===requestID)setLoading(false)
		}
	},[activeWorkspaceID,path])
	const previewPath=preview?.path||''
	useEffect(()=>{previewPathRef.current=previewPath},[previewPath])
	const refreshPreview=useCallback(async()=>{
		if(!activeWorkspaceID||!previewPath)return
		try{const result=await api.previewWorkspaceFile(activeWorkspaceID,previewPath);if(previewPathRef.current===previewPath)setPreview(result)}catch{/* keep the last successful preview; the listing still reports the error */}
	},[activeWorkspaceID,previewPath])
	const synchronize=useCallback((showLoading=false)=>{void load(showLoading);void refreshPreview()},[load,refreshPreview])

	useEffect(()=>{void load()},[load])
	useEffect(()=>{
		if(!activeWorkspaceID)return
		const source=new EventSource(workspaceFileEventsURL(activeWorkspaceID,path))
		const changed=()=>synchronize(false)
		source.addEventListener('workspace-change',changed)
		source.onopen=changed
		return()=>{source.removeEventListener('workspace-change',changed);source.close()}
	},[activeWorkspaceID,path,synchronize])

	const choose=(event:React.ChangeEvent<HTMLInputElement>)=>{
		const selected=event.target.files?.[0]||null
		setFile(selected);setTarget(selected?workspaceChildPath(path,selected.name):'');setNotice(null)
	}
	const upload=async()=>{
		if(!workspace||!file||!target.trim())return
		setUploading(true);setNotice(null)
		try{
			const result=await api.uploadWorkspaceFile(workspace.id,file,target.trim())
			setNotice({kind:'success',text:t('workspace.uploaded',{path:result.path})});setFile(null);setTarget('');setInputKey(value=>value+1)
		}catch(err){setNotice({kind:'error',text:errorText(err)})}
		finally{setUploading(false)}
	}
	const uploadDropped=async(files:File[])=>{
		if(!workspace||workspace.access!=='read_write'||uploading||!files.length)return
		setUploading(true);setNotice({kind:'success',text:t('workspace.uploadingFiles',{count:files.length})})
		let uploaded=0
		const failures:string[]=[]
		for(const dropped of files){
			try{await api.uploadWorkspaceFile(workspace.id,dropped,workspaceChildPath(path,dropped.name));uploaded+=1}
			catch(err){failures.push(`${dropped.name}: ${errorText(err)}`)}
		}
		if(failures.length){setNotice({kind:'error',text:t('workspace.uploadPartial',{uploaded,failed:failures.length,message:failures[0]})})}
		else{setNotice({kind:'success',text:t('workspace.uploadedFiles',{count:uploaded})})}
		setUploading(false)
	}
	const acceptsFiles=(event:React.DragEvent<HTMLElement>)=>workspace?.access==='read_write'&&Array.from(event.dataTransfer.types).includes('Files')
	const dragEnter=(event:React.DragEvent<HTMLElement>)=>{if(!acceptsFiles(event))return;event.preventDefault();event.stopPropagation();setDragging(true)}
	const dragOver=(event:React.DragEvent<HTMLElement>)=>{if(!acceptsFiles(event))return;event.preventDefault();event.stopPropagation();event.dataTransfer.dropEffect=uploading?'none':'copy'}
	const dragLeave=(event:React.DragEvent<HTMLElement>)=>{if(workspace?.access!=='read_write')return;event.preventDefault();event.stopPropagation();if(event.relatedTarget instanceof Node&&event.currentTarget.contains(event.relatedTarget))return;setDragging(false)}
	const drop=(event:React.DragEvent<HTMLElement>)=>{if(!acceptsFiles(event))return;event.preventDefault();event.stopPropagation();setDragging(false);if(!uploading)void uploadDropped(Array.from(event.dataTransfer.files))}
	const openEntry=async(name:string,type:'file'|'directory')=>{
		const next=workspaceChildPath(path,name)
		if(type==='directory'){setPath(next);return}
		if(!workspace)return
		setPreviewLoading(next);setNotice(null)
		try{setPreview(await api.previewWorkspaceFile(workspace.id,next))}catch(err){setNotice({kind:'error',text:errorText(err)})}finally{setPreviewLoading('')}
	}
	const download=(relativePath:string,name:string)=>{
		if(!workspace)return
		const anchor=document.createElement('a')
		anchor.href=workspaceDownloadURL(workspace.id,relativePath);anchor.download=name
		document.body.appendChild(anchor);anchor.click();anchor.remove()
	}
	const requestEntryRemoval=(name:string,type:'file'|'directory')=>{
		if(workspace)setDeleteCandidate({workspaceID:workspace.id,path:workspaceChildPath(path,name),type})
	}
	const revealDirectory=async(relativePath:string)=>{
		if(!workspace||!desktopRuntime)return
		setNotice(null)
		try{await invoke('open_workspace_directory',{workspaceId:workspace.id,relativePath})}
		catch(err){setNotice({kind:'error',text:errorText(err)})}
	}
	const removeEntry=async()=>{
		if(!deleteCandidate)return
		const candidate=deleteCandidate
		setDeleting(candidate.path);setNotice(null)
		try{
			const result=await api.deleteWorkspaceEntry(candidate.workspaceID,candidate.path)
			if(candidate.workspaceID===workspace?.id&&preview?.path===candidate.path)setPreview(null)
			setNotice({kind:'success',text:t('workspace.deleted',{type:t(`workspace.${result.type}`,{defaultValue:result.type})})})
		}catch(err){setNotice({kind:'error',text:errorText(err)})}finally{setDeleting('');setDeleteCandidate(null)}
	}
	const savePreview=async(content:string)=>{
		if(!workspace||!preview)return
		const saved=await api.saveWorkspaceTextFile(workspace.id,preview.path,content)
		setPreview({...preview,content,binary:false,size:saved.size,sha256:saved.sha256})
		setNotice({kind:'success',text:t('workspace.saved',{path:saved.path})})
	}
	const up=()=>{if(path==='.')return;const parts=path.split('/');parts.pop();setPath(parts.join('/')||'.')}
	const createShell=async()=>{
		if(!workspace||startingShell)return
		setStartingShell(true)
		try{await onCreateShell(workspace.id)}finally{setStartingShell(false)}
	}

	if(!workspace)return <aside className="workspace-browser-panel panel empty"><div className="panel-header"><div><FolderOpen size={17}/><span>{t('common.workspace')}</span></div><div className="workspace-panel-actions"><button type="button" onClick={onCollapse} title={t('workspace.collapsePanel')} aria-label={t('workspace.collapsePanel')}><PanelLeftClose size={14}/></button></div></div><div className="workspace-empty"><FolderOpen size={23}/><span>{t('workspace.noConfigured')}</span></div></aside>
	return <>
		<aside className={`workspace-browser-panel panel ${dragging?'dragging':''}`} onDragEnter={dragEnter} onDragOver={dragOver} onDragLeave={dragLeave} onDrop={drop}>
			<div className="panel-header"><div><FolderOpen size={17}/><span>{t('common.workspace')}</span></div><div className="workspace-panel-actions"><button type="button" disabled={!workspace.shell||startingShell} onClick={()=>void createShell()} title={t('workspace.newTerminal')}>{startingShell?<LoaderCircle className="spin" size={14}/>:<TerminalSquare size={14}/>}</button><AppSelect className="workspace-switch-select" value={workspace.id} disabled={workspaces.length<2||disabled||switching} ariaLabel={t('workspace.switchWorkspace')} onChange={onSelect} options={workspaces.map(item=>({value:item.id,label:item.id}))}/><button type="button" onClick={onCollapse} title={t('workspace.collapsePanel')} aria-label={t('workspace.collapsePanel')}><PanelLeftClose size={14}/></button></div></div>
			<div className="workspace-summary"><div className="chat-workspace-head"><span><b>{workspace.id}</b>{(switching||bound)&&<small>{switching?t('workspace.switching'):t('workspace.boundToConversation')}</small>}</span><em className={workspace.access}>{workspace.access==='read_write'?t('workspace.readWrite'):t('workspace.readOnly')}</em></div>{activeShells.length>0&&<div className="workspace-shell-sessions">{activeShells.map(shell=><button type="button" onClick={()=>onOpenShell(shell)} title={shell.id} key={shell.id}><i className={shell.status}/><b>{t(shell.surface==='workspace_agent'?'workspace.agent':'workspace.operator')}</b><code>{shell.cwd||'.'}</code></button>)}</div>}</div>
			<div className="workspace-path-row"><button onClick={up} disabled={path==='.'} title={t('workspace.parent')}>‹</button><code title={path}>{path}</code>{workspace.access==='read_write'&&<label title={t('workspace.uploadFile')}><UploadCloud size={14}/><input key={inputKey} type="file" onChange={choose}/></label>}<button onClick={()=>synchronize(true)} title={t('workspace.refreshFiles')}><RefreshCw size={12}/></button></div>
			{file&&<div className="chat-upload-row"><input value={target} onChange={event=>setTarget(event.target.value)} aria-label={t('workspace.relativePath')}/><button onClick={()=>void upload()} disabled={uploading||!target.trim()}>{uploading?'...':t('common.upload')}</button><button onClick={()=>{setFile(null);setTarget('');setInputKey(value=>value+1)}} title={t('workspace.cancelUpload')}><X size={11}/></button></div>}
			<div className="workspace-file-list">{loading?<span className="workspace-files-state"><LoaderCircle className="spin" size={13}/>{t('common.loading')}</span>:error?<span className="workspace-files-state error">{error}</span>:entries.length?entries.map(entry=>{const fullPath=workspaceChildPath(path,entry.name);return <div className="workspace-file-row" key={`${entry.type}:${entry.name}`}><button className="workspace-file-open" onClick={()=>void openEntry(entry.name,entry.type)} title={entry.type==='file'?t('workspace.previewFile'):t('workspace.openDirectory')}>{previewLoading===fullPath?<LoaderCircle className="spin" size={13}/>:entry.type==='directory'?<FolderOpen size={13}/>:<FileText size={13}/>}<span>{entry.name}</span>{entry.type==='file'&&<small>{formatFileSize(entry.size??0)}</small>}</button>{(entry.type==='file'||desktopRuntime&&entry.type==='directory'||workspace.access==='read_write')&&<div className="workspace-file-actions">{entry.type==='file'&&<button className="workspace-file-download" onClick={()=>download(fullPath,entry.name)} title={t('common.download')}><Download size={12}/></button>}{desktopRuntime&&entry.type==='directory'&&<button className="workspace-file-reveal" onClick={()=>void revealDirectory(fullPath)} title={t('workspace.revealDirectory')}><FolderOutput size={12}/></button>}{workspace.access==='read_write'&&<button className="workspace-file-delete" onClick={()=>requestEntryRemoval(entry.name,entry.type)} disabled={deleting===fullPath} title={t('workspace.deleteEntry',{type:t(`workspace.${entry.type}`)})}><Trash2 size={12}/></button>}</div>}</div>}):<span className="workspace-files-state">{t('workspace.emptyDirectory')}</span>}</div>
			{notice&&<div className={`chat-workspace-notice ${notice.kind}`}>{notice.text}</div>}
			{dragging&&<div className="workspace-drop-overlay"><UploadCloud size={27}/><b>{t('workspace.dropFilesHere')}</b><span>{path}</span></div>}
		</aside>
		{preview&&<TextFileEditor path={preview.path} meta={`${formatFileSize(preview.size)} · SHA-256 ${preview.sha256}`} content={preview.content||''} binary={preview.binary} editable={workspace.access==='read_write'} onClose={()=>setPreview(null)} onSave={savePreview} onDownload={()=>download(preview.path,preview.path.split('/').at(-1)||'download')}/>}
		{deleteCandidate&&<DestructiveConfirmDialog title={t('workspace.deleteTitle',{path:`${deleteCandidate.workspaceID}:${deleteCandidate.path}`})} busy={deleting===deleteCandidate.path} onCancel={()=>setDeleteCandidate(null)} onConfirm={()=>void removeEntry()}/>}
	</>
}

function SessionTasks({tasks,expanded,onExpanded}:{tasks:AgentTaskList;expanded:boolean;onExpanded:(expanded:boolean)=>void}){
	const {t}=useTranslation()
	const completed=tasks.items.filter(task=>task.status==='completed').length
	const current=tasks.items.find(task=>task.status==='in_progress')||tasks.items.find(task=>task.status==='pending'&&!unresolvedTaskDependencies(task,tasks.items).length)
	const blocked=tasks.items.filter(task=>task.status==='pending'&&unresolvedTaskDependencies(task,tasks.items).length>0).length
	const state=current?'active':blocked?'blocked':'completed'
  const progress=tasks.items.length?Math.round(completed/tasks.items.length*100):0
	return <details className={`session-tasks ${state}`} open={expanded} onToggle={event=>onExpanded(event.currentTarget.open)}><summary><span className="task-list-icon"><ListChecks size={16}/></span><span className="task-list-summary"><b>{t('agentTasks.title')}</b><small>{current?`${current.active_form||current.subject} · #${current.id}`:blocked?t('agentTasks.blocked',{count:blocked}):`${completed}/${tasks.items.length}`}</small></span><span className="task-list-progress"><i><em style={{width:`${progress}%`}}/></i><b>{progress}%</b></span><span className={`task-list-state ${state}`} key={state}>{t(`statusLabels.${state}`,{defaultValue:state})}</span><ChevronRight size={14}/></summary></details>
}

function SessionTaskItems({tasks}:{tasks:AgentTaskList}){
	const {t}=useTranslation()
	const blocked=tasks.items.some(task=>task.status==='pending'&&unresolvedTaskDependencies(task,tasks.items).length>0)&&!tasks.items.some(task=>task.status==='in_progress'||task.status==='pending'&&!unresolvedTaskDependencies(task,tasks.items).length)
	return <section className={`session-task-view ${blocked?'blocked':'active'}`}><ol className="session-task-items">{tasks.items.map(task=>{const blockers=unresolvedTaskDependencies(task,tasks.items);const taskBlocked=task.status==='pending'&&blockers.length>0;const status=taskBlocked?'blocked':task.status;return <li className={status} key={task.id}><span className="task-item-marker">{task.status==='completed'?<Check size={12}/>:task.status==='in_progress'?<LoaderCircle size={12}/>:taskBlocked?<ShieldAlert size={12}/>:task.id}</span><div title={task.description}><b>{task.subject}</b>{taskBlocked&&<small>#{blockers.join(', #')}</small>}</div><em>{task.owner||t(`statusLabels.${status}`,{defaultValue:status.replace('_',' ')})}</em></li>})}</ol></section>
}

const ChatBubble=memo(function ChatBubble({ entry, runs, hosts, onToolDisclosure }: {entry: ChatEntry;runs:Run[];hosts:Host[];onToolDisclosure:(summary:HTMLElement)=>void}) {
	const {t}=useTranslation()
  if (entry.kind === 'tool') return <ToolEventCard entry={entry} runs={runs} hosts={hosts} onDisclosure={onToolDisclosure}/>
  if (entry.kind === 'reasoning') return <ReasoningCard content={entry.content} active={!!entry.active}/>
  if (entry.kind === 'assistant' && !entry.content) return null
	return <div className={`bubble ${entry.kind} ${entry.status||''}`}><div className="avatar">{entry.kind === 'user' ? <UserRound size={17}/> : entry.kind === 'error' ? '!' : <Bot size={17}/>}</div><div><span className="bubble-label">{entry.kind === 'user' ? <>{t('chat.operator')}{entry.status==='failed'&&<em>{t('chat.responseFailed')}</em>}{entry.status==='pending'&&<em>{t('chat.processing')}</em>}</> : entry.kind === 'error' ? t('common.error') : 'OpsNerva'}</span>{entry.images&&entry.images.length>0&&<div className="message-images">{entry.images.map(image=><a href={image.url} target="_blank" rel="noopener noreferrer" title={`${image.name} · ${formatFileSize(image.sizeBytes)}`} key={image.id}><img src={image.url} alt={image.name}/><span>{image.name}</span></a>)}</div>}{entry.content&&<div className={`bubble-copy ${entry.kind==='assistant'&&entry.lifecycle!=='streaming'?'markdown-body':''}`}>{entry.kind==='assistant'&&entry.lifecycle!=='streaming'?<Markdown skipHtml remarkPlugins={[remarkGfm]} components={{a:({href,children})=><a href={href} target="_blank" rel="noopener noreferrer">{children}</a>,img:({alt})=><span className="markdown-image-blocked">{t('chat.blockedImage',{alt:alt||t('common.image')})}</span>,pre:({children})=><CopyablePre>{children}</CopyablePre>}}>{entry.content}</Markdown>:entry.content}</div>}</div></div>
})

function latestReasoningLine(content:string){
  const lines=content.split(/\r?\n/).map((line)=>line.trim()).filter(Boolean)
	const line=lines.at(-1)||i18n.t('chat.reasoningFallback')
  const characters=Array.from(line)
  return characters.length>72?`…${characters.slice(-72).join('')}`:line
}

function ReasoningCard({content,active}:{content:string;active:boolean}){
	const {t}=useTranslation()
  const latest=latestReasoningLine(content)
  return <details className={`reasoning-card ${active?'active':''}`}>
	  <summary><span className="reasoning-icon"><BrainCircuit size={15}/></span><span className="reasoning-title">{active?t('chat.reasoningActive'):t('chat.reasoning')}</span><span className="reasoning-latest" title={latest}>{latest}</span><ChevronRight className="reasoning-chevron" size={14}/></summary>
    <div className="reasoning-content"><pre>{content}</pre></div>
  </details>
}

type JsonRecord = Record<string,unknown>
function toolLabel(value:string){return i18n.t(`toolNames.${value}`,{defaultValue:value})}
function toolSummaryIcon(name:string|undefined){
	if(name?.startsWith('Task'))return <ListChecks size={15}/>
	if(name?.startsWith('workspace_'))return <FolderOpen size={15}/>
	if(name?.startsWith('web_'))return <Search size={15}/>
	if(name==='skill')return <BookOpen size={15}/>
	if(name?.startsWith('mcp__'))return <Braces size={15}/>
	if(name?.includes('file_'))return <FileText size={15}/>
	if(name?.startsWith('ssh_'))return name==='ssh_exec'||name==='ssh_run_script'||name==='ssh_shell'?<TerminalSquare size={15}/>:<Server size={15}/>
	return <FunctionSquare size={15}/>
}
function jsonRecord(value:unknown):JsonRecord|undefined{return value!==null&&typeof value==='object'&&!Array.isArray(value)?value as JsonRecord:undefined}
function parseRecord(value:string):JsonRecord{try{return jsonRecord(JSON.parse(value))||{value:JSON.parse(value)}}catch{return{value}}}
function requestFromRun(run?:Run):JsonRecord|undefined{if(!run)return;try{return jsonRecord(JSON.parse(run.request_json))}catch{return}}
function runAutoApproved(run?:Run){return run?.ai_review?.kind==='automatic_approval'&&run.ai_review.status==='completed'&&run.ai_review.decision==='allow'}
function textValue(value:unknown){return typeof value==='string'?value:''}
function shellArg(value:string){return /^[A-Za-z0-9_@%+=:,./-]+$/.test(value)?value:JSON.stringify(value)}
function fullProgram(request:JsonRecord){const program=textValue(request.program);const args=Array.isArray(request.args)?request.args.map(value=>String(value)):[];return [program,...args].filter(Boolean).map(shellArg).join(' ')}
function compactScript(script:string){const lines=script.split(/\r?\n/).map(line=>line.trim()).filter(Boolean);if(!lines.length)return i18n.t('tool.bashScript');return lines.length===1?lines[0]:i18n.t('tool.moreLines',{line:lines[0],count:lines.length-1})}
function latestOutput(value:string,limit=3){return value.trimEnd().split(/\r?\n/).filter(line=>line.trim()!=='').slice(-limit).map(line=>Array.from(line).length>180?`${Array.from(line).slice(0,180).join('')}…`:line).join('\n')}
function formatDuration(value:unknown,run?:Run){if(typeof value==='number'&&Number.isFinite(value))return value>=1e9?`${(value/1e9).toFixed(2)} s`:`${(value/1e6).toFixed(1)} ms`;if(run?.completed_at){const ms=Date.parse(run.completed_at)-Date.parse(run.started_at);if(Number.isFinite(ms))return ms>=1000?`${(ms/1000).toFixed(2)} s`:`${ms} ms`}return'—'}
function numberValue(value:unknown){return typeof value==='number'&&Number.isFinite(value)?value:0}
function sshTunnelRoute(host:string,remoteHost:string,remotePort:number,localPort:number,automatic='auto'){
	return `${host}:${remoteHost}:${remotePort} → localhost:${localPort||automatic}`
}
function cleanFileChangeOutput(value:string){const lines=value.split(/\r?\n/),result:string[]=[];for(let index=0;index<lines.length;index++){if(lines[index]==='__OPS_FILE_VALIDATION_OK__')continue;if(lines[index]==='__OPS_FILE_AFTER__'){index++;continue}result.push(lines[index])}return result.join('\n').trim()}

type ToolTarget={kind:'host'|'workspace'|'scope';label:string;name:string;id?:string}
function hostIdentity(hosts:Host[],hostID:string){
	const host=hosts.find(item=>item.id===hostID||item.name===hostID)
	return {name:host?.name||'',id:host?.id||hostID}
}
function recordArray(value:unknown){return Array.isArray(value)?value.map(jsonRecord).filter((item):item is JsonRecord=>!!item):[]}

type DiffRow={kind:'header'|'hunk'|'add'|'delete'|'context'|'meta';oldLine?:number;newLine?:number;text:string}
function parseDiffRows(diff:string):DiffRow[]{
	let oldLine=0,newLine=0
	return diff.replace(/\n$/, '').split('\n').map(line=>{
		const hunk=line.match(/^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/)
		if(hunk){oldLine=Number(hunk[1]);newLine=Number(hunk[2]);return{kind:'hunk',text:line}}
		if(line.startsWith('--- ')||line.startsWith('+++ '))return{kind:'header',text:line}
		if(line.startsWith('+'))return{kind:'add',newLine:newLine++,text:line}
		if(line.startsWith('-'))return{kind:'delete',oldLine:oldLine++,text:line}
		if(line.startsWith(' ')){const row={kind:'context' as const,oldLine,newLine,text:line};oldLine++;newLine++;return row}
		return{kind:'meta',text:line}
	})
}

function DiffViewer({change}:{change:JsonRecord}){
	const {t}=useTranslation(),diff=textValue(change.diff),rows=parseDiffRows(diff)
	return <section className="diff-viewer"><header><span><FileText size={14}/>{t('tool.fileEdit')}</span><div><em className="add">+{numberValue(change.additions)}</em><em className="delete">-{numberValue(change.deletions)}</em><CopyButton value={diff}/></div></header><div className="diff-scroll" role="table" aria-label={t('tool.diff')}><div className="diff-lines">{rows.map((row,index)=><div className={`diff-line ${row.kind}`} role="row" key={index}><span className="old-line">{row.oldLine??''}</span><span className="new-line">{row.newLine??''}</span><code>{row.text||' '}</code></div>)}</div></div></section>
}

function ToolEventCard({entry,runs,hosts,onDisclosure}:{entry:ChatEntry;runs:Run[];hosts:Host[];onDisclosure:(summary:HTMLElement)=>void}){
	const {t}=useTranslation()
  const payload=parseRecord(entry.content)
	const taskPayload=jsonRecord(payload.task)
	const resultPayload=jsonRecord(payload.result)
  const runID=entry.runId||textValue(payload.run_id)||textValue(taskPayload?.run_id)||textValue(resultPayload?.run_id)
	const run=runs.find(item=>item.id===runID)
	const display=jsonRecord(payload._display)
	const toolArguments=jsonRecord(display?.arguments)
	const displayRequest=jsonRecord(display?.request)||requestFromRun(run)
	const executionTool=!!entry.tool&&['ssh_exec','ssh_run_script','ssh_tunnel','ssh_shell','ssh_file_read','ssh_file_list','ssh_file_edit','ssh_file_transfer','workspace_file_list','workspace_file_read','workspace_file_edit','workspace_file_delete','workspace_file_upload','workspace_file_download','workspace_shell'].includes(entry.tool)
	const request=executionTool?displayRequest:undefined
	const shellPayload=jsonRecord(payload.shell)||jsonRecord(resultPayload?.shell)
	const destinationHostID=textValue(display?.host_id)||run?.host_id||textValue(request?.host_id)||textValue(toolArguments?.host_id)||textValue(toolArguments?.destination_host_id)||textValue(shellPayload?.host_id)||textValue(payload.host_id)||textValue(resultPayload?.host_id)
	const destinationHost=hostIdentity(hosts,destinationHostID)
  const hostID=destinationHost.id
  const hostName=destinationHost.name||hostID||'—'
  const payloadStatus=textValue(payload.status)||textValue(taskPayload?.status)||textValue(resultPayload?.status)
  const runStatus=run?.status==='running'?'in_progress':run?.status
  const status=payloadStatus==='approval_required'&&runStatus&&runStatus!=='approval_required'?runStatus:payloadStatus||runStatus||'completed'
	const program=request?fullProgram(request):''
	const script=request?textValue(request.script):''
	const change=jsonRecord(request?.change)||jsonRecord(payload.change)||jsonRecord(resultPayload?.change)
  const remotePath=request?(textValue(request.remote_path)||(!entry.tool?.startsWith('workspace_')?textValue(request.path):'')):''
	const workspaceID=textValue(display?.workspace_id)||(request?textValue(request.workspace_id):'')||textValue(shellPayload?.workspace_id)||textValue(payload.workspace_id)||textValue(resultPayload?.workspace_id)
	const relativePath=request?(textValue(request.relative_path)||(entry.tool?.startsWith('workspace_')?textValue(request.path):'')):''
	const unifiedFileRead=entry.tool==='ssh_file_read'||entry.tool==='workspace_file_read'
	const requestMode=(request?textValue(request.mode):'')||(unifiedFileRead?(request&&textValue(request.pattern)?`${entry.tool==='workspace_file_read'?'workspace':'remote'}_search`:`${entry.tool==='workspace_file_read'?'workspace':'remote'}_read`):'')
	const tunnelOperation=entry.tool==='ssh_tunnel'
	const tunnelAction=textValue(toolArguments?.action)||(requestMode==='ssh_tunnel_start'?'start':'')
	const tunnel=jsonRecord(payload.tunnel)||jsonRecord(resultPayload?.tunnel)
	const tunnelRemoteHost=(request?textValue(request.remote_host):'')||textValue(tunnel?.remote_host)||textValue(toolArguments?.remote_host)
	const tunnelRemotePort=(request?numberValue(request.remote_port):0)||numberValue(tunnel?.remote_port)||numberValue(toolArguments?.remote_port)
	const tunnelLocalPort=(request?numberValue(request.local_port):0)||numberValue(tunnel?.local_port)||numberValue(toolArguments?.local_port)
	const tunnelRoute=tunnelAction==='start'?sshTunnelRoute(hostName,tunnelRemoteHost,tunnelRemotePort,tunnelLocalPort,t('tunnels.automaticPort')):tunnelAction==='stop'?textValue(toolArguments?.tunnel_id):''
	const shellTool=entry.tool==='ssh_shell'||entry.tool==='workspace_shell'
	const shellAction=textValue(toolArguments?.action)||(requestMode==='ssh_shell_start'||requestMode==='workspace_shell_start'?'start':requestMode==='workspace_shell'?'run':'')
	const shellOperation=shellTool&&shellAction!=='run'
	const shellID=textValue(toolArguments?.shell_id)||textValue(shellPayload?.id)
	const shellEvents=[...recordArray(payload.events),...recordArray(resultPayload?.events)]
	const shellChunks=[...recordArray(payload.chunks),...recordArray(resultPayload?.chunks)]
	const shellChunkStdout=shellChunks.filter(chunk=>textValue(chunk.stream)==='stdout').map(chunk=>textValue(chunk.content)).join('')
	const shellChunkStderr=shellChunks.filter(chunk=>textValue(chunk.stream)==='stderr').map(chunk=>textValue(chunk.content)).join('')
	const shellChunkOutput=shellChunks.map(chunk=>textValue(chunk.content)).join('')
	const shellHasMore=payload.has_more===true||resultPayload?.has_more===true
	const shellOutput=shellChunkOutput||textValue(payload.output)||textValue(resultPayload?.output)||textValue(payload.recent_output)||textValue(resultPayload?.recent_output)||shellEvents
		.filter(event=>['stdout','stderr'].includes(textValue(event.stream)))
		.map(event=>textValue(event.content))
		.join('')||entry.liveStdout||''
	const shellInput=textValue(toolArguments?.input)
	const shellInputDisplay=`${shellInput}${toolArguments?.submit===true&&!/[\r\n]$/.test(shellInput)?' ↵':''}`
	const shellActionName=shellAction?t(`sshShell.toolActions.${shellAction}`,{defaultValue:t('sshShell.short')}):t('sshShell.short')
	const shellActionLabel=entry.tool==='ssh_shell'?`SSH Shell · ${shellActionName}`:shellActionName
	const shellSummary=shellOperation?(shellAction==='start'
		?`${workspaceID?`${workspaceID}:${request?textValue(request.cwd)||'.':textValue(toolArguments?.cwd)||'.'}`:`${hostName}:${request?textValue(request.cwd)||'~':textValue(toolArguments?.cwd)||'~'}`} · PTY`
		:shellAction==='input'
			?shellInputDisplay
			:shellAction==='output'
				?`${latestOutput(shellOutput,1)||shellID}${shellHasMore?` · ${t('tool.moreOutput')}`:''}`
				:shellAction==='list'
					?String(numberValue(payload.count)||numberValue(resultPayload?.count))
					:shellID):''
	const shellPrimaryContent=shellAction==='input'?shellInputDisplay:shellAction==='output'?shellOutput:shellSummary
	const shellPrimaryAction=shellOperation&&shellAction==='input'
	const shellOutputAction=shellOperation&&shellAction==='output'
	const fileSearchMode=unifiedFileRead&&(requestMode==='remote_search'||requestMode==='workspace_search')
	const fileReadMode=unifiedFileRead&&(requestMode==='remote_read'||requestMode==='workspace_read')
	const structuredFileOperation=fileReadMode||fileSearchMode
	const searchPattern=request?textValue(request.search_pattern):''
	const searchResult=jsonRecord(payload.search)||jsonRecord(resultPayload?.search)
	const searchMatchMode=(request?textValue(request.search_match_mode):'')||textValue(searchResult?.match_mode)
	const searchMatchModeLabel=searchMatchMode==='literal'?t('tool.matchModeLiteral'):searchMatchMode==='regex'?t('tool.matchModeRegex'):searchMatchMode||'—'
	const searchFound=searchResult?.found===true
	const workspaceShellBackend=request?textValue(request.workspace_shell_backend):''
	const workspaceUpload=requestMode==='workspace_upload'||entry.tool==='workspace_file_upload'
	const workspaceDownload=requestMode==='workspace_download'||entry.tool==='workspace_file_download'
	const sshTransfer=requestMode==='ssh_file_transfer'||entry.tool==='ssh_file_transfer'
	const workspaceTool=!!entry.tool?.startsWith('workspace_')
	const sourceHostID=(request?textValue(request.source_host_id):'')||textValue(toolArguments?.source_host_id)
	const sourcePath=(request?textValue(request.source_path):'')||textValue(toolArguments?.source_path)
	const sourceHost=hostIdentity(hosts,sourceHostID)
	const sourceHostName=sourceHost.name||sourceHost.id
	const file=jsonRecord(payload.file)||jsonRecord(resultPayload?.file)
	const filePath=textValue(file?.path)||remotePath||relativePath
	const fileTarget=`${workspaceID?`${workspaceID}:`:''}${filePath}`
	const eventToolLabel=shellOperation?shellActionLabel:structuredFileOperation?t(fileSearchMode?(workspaceID?'toolNames.workspace_file_search_mode':'toolNames.ssh_file_search_mode'):(workspaceID?'toolNames.workspace_file_read':'toolNames.ssh_file_read')):toolLabel(entry.tool||'')
	const transferSummary=tunnelRoute||shellSummary||(workspaceUpload?`${workspaceID}:${relativePath} → ${hostName}:${remotePath}`:workspaceDownload?`${hostName}:${remotePath} → ${workspaceID}:${relativePath}`:sshTransfer?`${sourceHostName}:${sourcePath} → ${hostName}:${remotePath}`:'')
  const planSteps=Array.isArray(payload.steps)?payload.steps.map(jsonRecord).filter((step):step is JsonRecord=>!!step):[]
  const planSummary=textValue(payload.goal)||textValue(planSteps.find(step=>textValue(step.status)==='in_progress'||textValue(step.status)==='blocked')?.title)
	const genericArgumentSummary=executionTool?'':toolArgumentSummary(entry.tool,toolArguments)
	const operation=filePath||(script?t('tool.bashScript'):program||genericArgumentSummary||eventToolLabel||t('tool.result'))
  const env=request?jsonRecord(request.env):undefined
	const rawStdout=shellOperation&&(shellAction==='input'||shellAction==='output')?(shellChunks.length?shellChunkStdout:shellOutput):textValue(payload.stdout)||textValue(resultPayload?.stdout)||entry.liveStdout||run?.stdout_redacted||''
	const stdout=change?cleanFileChangeOutput(rawStdout):rawStdout
	  const stderr=(shellOperation&&shellChunks.length?shellChunkStderr:'')||textValue(payload.stderr)||textValue(resultPayload?.stderr)||entry.liveStderr||run?.stderr_redacted||run?.error||''
	const outputView=textValue(payload.output_view)||textValue(resultPayload?.output_view)
	const stdoutOmitted=numberValue(payload.stdout_omitted_bytes)||numberValue(resultPayload?.stdout_omitted_bytes)
	const stderrOmitted=numberValue(payload.stderr_omitted_bytes)||numberValue(resultPayload?.stderr_omitted_bytes)
	const waitDeadlineReached=payload.wait_deadline_reached===true||resultPayload?.wait_deadline_reached===true
	const transferTotal=entry.transferTotalBytes||0
	const transferred=Math.min(entry.transferredBytes||0,transferTotal)
	const transferPercent=transferTotal>0?Math.min(100,Math.round(transferred/transferTotal*100)):0
	const outputLabel=(label:string,omitted:number)=>omitted>0?`${label} · ${outputView.toUpperCase()} · ${t('tool.outputOmitted',{count:omitted})}`:label
		const previewStream=status==='in_progress'&&entry.liveOutput?entry.liveOutputStream:stdout?'stdout':stderr?'stderr':undefined
		const previewContent=status==='in_progress'&&entry.liveOutput?entry.liveOutput:previewStream==='stderr'?stderr:stdout
	  const outputPreview=status==='in_progress'?latestOutput(previewContent,1):''
		const commandSummary=transferSummary||(fileSearchMode?`${fileTarget} · ${searchMatchModeLabel} pattern=${JSON.stringify(searchPattern)}`:filePath)||program||(script?compactScript(script):'')||planSummary||genericArgumentSummary||operation
	const summaryLabel=eventToolLabel||entry.tool||t('common.functions')
	const historyRuns=[...recordArray(payload.runs),...recordArray(resultPayload?.runs)]
	const historyHostIDs=[...new Set(historyRuns.map(item=>textValue(item.host_id)).filter(Boolean))]
	const listedHosts=[...recordArray(payload.hosts),...recordArray(resultPayload?.hosts)]
	const targets:ToolTarget[]=[]
	if(sshTransfer){
		if(sourceHost.id)targets.push({kind:'host',label:t('tool.sourceHost'),name:sourceHost.name,id:sourceHost.id})
		if(hostID)targets.push({kind:'host',label:t('tool.targetHost'),name:destinationHost.name,id:hostID})
	}else if(workspaceDownload){
		if(hostID)targets.push({kind:'host',label:t('tool.sourceHost'),name:destinationHost.name,id:hostID})
		if(workspaceID)targets.push({kind:'workspace',label:t('common.workspace'),name:workspaceID})
	}else if(workspaceUpload){
		if(workspaceID)targets.push({kind:'workspace',label:t('common.workspace'),name:workspaceID})
		if(hostID)targets.push({kind:'host',label:t('tool.targetHost'),name:destinationHost.name,id:hostID})
	}else if(workspaceTool&&workspaceID){
		targets.push({kind:'workspace',label:t('common.workspace'),name:workspaceID})
	}else if(hostID){
		targets.push({kind:'host',label:t('tool.targetHost'),name:destinationHost.name,id:hostID})
	}else if(workspaceID){
		targets.push({kind:'workspace',label:t('common.workspace'),name:workspaceID})
	}else if(entry.tool==='ssh_history'&&historyHostIDs.length>0){
		for(const historyHostID of historyHostIDs.slice(0,3)){const historyHost=hostIdentity(hosts,historyHostID);targets.push({kind:'host',label:t('tool.historyHost'),name:historyHost.name,id:historyHost.id})}
		if(historyHostIDs.length>3)targets.push({kind:'scope',label:t('tool.historyHost'),name:t('tool.moreHosts',{count:historyHostIDs.length-3})})
	}else if(entry.tool==='ssh_host_list'){
		targets.push({kind:'scope',label:t('tool.scope'),name:t('tool.allHosts',{count:listedHosts.length||hosts.length})})
	}
  const instruction=textValue(payload.operator_instruction)||textValue(taskPayload?.operator_instruction)||textValue(resultPayload?.operator_instruction)
  const rawPayload={...payload};delete rawPayload._display
	  const [expanded,setExpanded]=useState(false)
		const firstSeenAt=useRef(Date.now())
		const persistedStartedAt=run?.started_at?Date.parse(run.started_at):entry.startedAt
		const startedAt=Number.isFinite(persistedStartedAt)?persistedStartedAt!:firstSeenAt.current
		const [now,setNow]=useState(Date.now())
		useEffect(()=>{
			if(status!=='in_progress')return
			setNow(Date.now())
			const timer=window.setInterval(()=>setNow(Date.now()),1000)
			return()=>window.clearInterval(timer)
		},[status])
		const elapsed=formatLiveDuration(Math.max(0,Math.floor((now-startedAt)/1000)))
		const resultExitCode=resultPayload?.exit_code
	const exitCode=typeof payload.exit_code==='number'?payload.exit_code:typeof resultExitCode==='number'?resultExitCode:run?.exit_code??'—'
	const duration=formatDuration(payload.duration??resultPayload?.duration,run)
	const autoApproved=payload.auto_approved===true||resultPayload?.auto_approved===true||runAutoApproved(run)
		  return <details className={`tool-event tool-event-rich ${status}`} open={expanded}>
			<summary onClick={event=>{event.preventDefault();onDisclosure(event.currentTarget);setExpanded(value=>!value)}}><div className="tool-summary-icon">{toolSummaryIcon(entry.tool)}</div><div className="tool-summary-copy"><div className="tool-summary-heading"><b>{summaryLabel}</b>{targets.length>0&&<div className="tool-summary-targets">{targets.map((target,index)=><span className={`tool-target-chip tool-target-${target.kind}`} title={`${target.label}: ${[target.name,target.id].filter(Boolean).join(' · ')}`} key={`${target.kind}_${target.id||target.name}_${index}`}>{target.kind==='host'?<Server size={11}/>:target.kind==='workspace'?<FolderOpen size={11}/>:<ListChecks size={11}/>} {(targets.length>1||target.kind==='scope')&&<em>{target.label}</em>}<b>{target.name||target.id}</b></span>)}</div>}</div>{commandSummary!==summaryLabel&&<code title={commandSummary}>{commandSummary}</code>}</div><div className="tool-summary-statuses">{autoApproved&&<span className="auto-approved"><ShieldCheck size={11}/>{t('approval.autoApproved')}</span>}<span className={`tool-status ${status}`} key={status}>{t(`statusLabels.${status}`,{defaultValue:status.replaceAll('_',' ')})}</span></div><ChevronRight size={14}/>{status==='in_progress'&&<div className={`tool-live-progress ${transferTotal>0?'determinate':''}`} role="progressbar" aria-valuemin={transferTotal>0?0:undefined} aria-valuemax={transferTotal>0?transferTotal:undefined} aria-valuenow={transferTotal>0?transferred:undefined}><i><em style={transferTotal>0?{width:`${transferPercent}%`}:undefined}/></i><span>{transferTotal>0?`${formatFileSize(transferred)} / ${formatFileSize(transferTotal)}`:entry.liveOutputStream?.toUpperCase()||''}</span><time>{elapsed}</time></div>}{outputPreview&&<div className={`tool-summary-preview ${previewStream==='stderr'?'stderr':''}`}><span>{shellAction==='output'?shellActionLabel:(previewStream||'stdout').toUpperCase()}</span><pre>{outputPreview}</pre></div>}</summary>
    <div className="tool-event-body">
		  {shellPrimaryAction&&<section className="tool-command-pane"><div className="tool-command-head"><span>{shellActionLabel}</span></div><div className="tool-command-block"><CopyButton value={shellPrimaryContent||'—'}/><pre>{shellPrimaryContent||'—'}</pre></div></section>}
		  {shellOutputAction&&!shellChunks.length&&<section className="tool-command-pane"><div className="tool-command-head"><span>{shellActionLabel}</span></div><div className="tool-command-block"><CopyButton value={shellOutput||'—'}/><pre>{shellOutput||'—'}</pre></div></section>}
		  {!executionTool&&toolArguments&&Object.keys(toolArguments).length>0&&<CompactTable title={t('tool.actualParameters')} columns={[t('tool.parameter'),t('tool.value')]} rows={Object.entries(toolArguments).map(([key,value])=>[key,safeToolArgument(value,key)])}/>}
      {request?<div className="tool-execution-layout">
        <section className="tool-command-pane">
		  <div className="tool-command-head"><span>{shellOperation?t('sshShell.interactive'):tunnelOperation?t('tunnels.forwarding'):structuredFileOperation?t(fileSearchMode?'tool.searchOperation':'tool.readOperation'):filePath?t('tool.fileOperation'):script?t('tool.fullScript'):t('tool.fullCommand')}</span>{workspaceShellBackend&&<em><TerminalSquare size={12}/>{workspaceShellBackend==='host'?t('approval.hostShell'):'Bubblewrap'}</em>}{request.elevated===true&&<em><ShieldAlert size={12}/>sudo / root</em>}</div>
			  <div className="tool-command-block"><CopyButton value={script||program||commandSummary}/>{shellOperation?<pre>{shellSummary}</pre>:tunnelOperation?<pre>{tunnelRoute||requestMode}</pre>:workspaceUpload?<pre>workspace_upload {workspaceID}:{relativePath} → {hostName}:{remotePath}</pre>:workspaceDownload?<pre>workspace_download {hostName}:{remotePath} → {workspaceID}:{relativePath}</pre>:sshTransfer?<pre>{sourceHostName}:{sourcePath} → {hostName}:{remotePath}</pre>:structuredFileOperation?<pre>{fileSearchMode?'search':'read'} {fileTarget}</pre>:filePath?<pre>{requestMode} {workspaceID?`${workspaceID}:`:''}{filePath}</pre>:script?<pre>{script}</pre>:program?<pre><span className="prompt-sign">$</span> {program}</pre>:<pre>{requestMode} {remotePath}</pre>}</div>
		  {change&&textValue(change.diff)&&<DiffViewer change={change}/>}
		  {env&&Object.keys(env).length>0&&<CompactTable title={t('tool.environment')} columns={[t('tool.key'),t('tool.value')]} rows={Object.entries(env).map(([key,value])=>[key,String(value)])}/>}
        </section>
        <aside className="tool-context-pane">
		  <dl className="tool-context-grid"><div><dt>{t('tool.permission')}</dt><dd>{workspaceShellBackend==='host'?t('tool.hostAuthority'):workspaceShellBackend==='sandbox'?t('tool.sandbox'):request.elevated===true?t('tool.managedSudo'):t('tool.normalUser')}</dd></div>{exitCode!=='—'&&<div><dt>{t('tool.exitCode')}</dt><dd>{exitCode}</dd></div>}{duration!=='—'&&<div><dt>{t('tool.duration')}</dt><dd>{duration}</dd></div>}{(waitDeadlineReached||shellHasMore)&&<div><dt>{t('common.status')}</dt><dd>{waitDeadlineReached?t('tool.waitDeadline'):t('tool.moreOutput')}</dd></div>}</dl>
		  {textValue(request.reason)&&<div className="tool-reason"><span>{t('tool.reason')}</span><p>{textValue(request.reason)}</p></div>}
        </aside>
      </div>:!shellPrimaryAction&&!shellOutputAction&&<GenericToolResult payload={payload}/>}
	  {file&&<FileMetadataPanel file={file}/>}
	  {fileSearchMode&&searchResult&&<div className={`file-search-result ${searchFound?'found':'empty'}`}><Search size={15}/><div><b>{t(searchFound?'tool.searchMatched':'tool.searchNoMatches')}</b><span>{searchMatchModeLabel} · {searchPattern}</span></div></div>}
	  {(textValue(payload.message)||textValue(payload.next_action))&&<div className={`tool-guidance ${payload.ok===false||['failed','denied','interrupted'].includes(status)?'error':''}`}><ShieldAlert size={15}/><div><b>{textValue(payload.code)||t('tool.result')}</b>{textValue(payload.message)&&<p>{textValue(payload.message)}</p>}{textValue(payload.next_action)&&<small>{t('common.next')} · {textValue(payload.next_action)}</small>}</div></div>}
	  {instruction&&<div className="tool-instruction"><ShieldAlert size={15}/><div><b>{t('tool.operatorInstruction')}</b><p>{instruction}</p></div></div>}
	  {sshTransfer&&transferTotal>0&&<div className="file-transfer-progress" role="progressbar" aria-valuemin={0} aria-valuemax={transferTotal} aria-valuenow={transferred}><div><span>{t('tool.transferProgress')}</span><b>{formatFileSize(transferred)} / {formatFileSize(transferTotal)}</b></div><i><em style={{width:`${transferPercent}%`}}/></i></div>}
	  {shellOperation&&shellChunks.length>0?<ShellOutputChunks chunks={shellChunks} live={status==='in_progress'}/>:((stdout&&(!shellOutputAction||shellChunks.length>0))||stderr)&&<div className="tool-output-grid">{stdout&&(!shellOutputAction||shellChunks.length>0)&&<ToolOutputPanel kind="stdout" label={outputLabel('STDOUT',stdoutOmitted)} content={stdout} live={status==='in_progress'}/>} {stderr&&<ToolOutputPanel kind="stderr" label={outputLabel(t('tool.stderrResult'),stderrOmitted)} content={stderr} live={status==='in_progress'}/>}</div>}
	  <details className="tool-raw"><summary>{t('tool.rawJson')}</summary><CopyablePre>{JSON.stringify(rawPayload,null,2)}</CopyablePre></details>
    </div>
  </details>
}

function ToolOutputPanel({kind,label,content,live}:{kind:'stdout'|'stderr';label:string;content:string;live:boolean}){
	const outputRef=useRef<HTMLPreElement>(null)
	const stickToBottom=useRef(true)
	useEffect(()=>{
		const output=outputRef.current
		if(live&&output&&stickToBottom.current)output.scrollTop=output.scrollHeight
	},[content,live])
	return <div className={`tool-output ${kind} ${live?'live':''}`}><span>{label}</span><CopyButton value={content}/><pre ref={outputRef} onScroll={event=>{const output=event.currentTarget;stickToBottom.current=output.scrollHeight-output.scrollTop-output.clientHeight<32}}>{content}</pre></div>
}

function ShellOutputChunks({chunks,live}:{chunks:JsonRecord[];live:boolean}){
	const {t}=useTranslation()
	return <div className="shell-output-chunks">{chunks.map((chunk,index)=>{
		const stream=textValue(chunk.stream)==='stderr'?'stderr':'stdout'
		return <ToolOutputPanel key={`${numberValue(chunk.first_sequence)||numberValue(chunk.sequence)}_${index}`} kind={stream} label={stream==='stderr'?t('tool.stderrResult'):'STDOUT'} content={textValue(chunk.content)} live={live}/>
	})}</div>
}

function FileMetadataPanel({file}:{file:JsonRecord}){
	const {t}=useTranslation()
	const after=textValue(file.sha256),validator=textValue(file.validator)
	return <section className="file-metadata-panel"><div className="file-metadata-head"><FileText size={16}/><div><b>{t('tool.fileDetails')}</b><span>{textValue(file.path)}</span></div>{file.validation_ok===true&&<em><Check size={12}/>{t('tool.validated')}</em>}</div><dl><div><dt>{t('tool.bytesRead')}</dt><dd>{typeof file.returned_bytes==='number'?`${file.returned_bytes} B`:'—'}</dd></div>{file.has_more===true&&<div><dt>{t('tool.nextOffset')}</dt><dd>{numberValue(file.next_offset)}</dd></div>}<div><dt>{t('tool.mode')}</dt><dd>{textValue(file.mode)||'—'}</dd></div><div><dt>{t('tool.owner')}</dt><dd>{[textValue(file.owner),textValue(file.group)].filter(Boolean).join(':')||'—'}</dd></div><div><dt>{t('tool.validator')}</dt><dd>{validator||'—'}</dd></div></dl>{after&&<div className="hash-row"><span>{t('tool.after')}</span><code>{after}</code></div>}{file.sensitive===true&&<div className="file-sensitive"><ShieldAlert size={13}/>{t('tool.sensitive')}</div>}</section>
}

function CompactTable({title,columns,rows}:{title:string;columns:string[];rows:Array<Array<unknown>>}){
  return <div className="tool-compact-table"><span>{title}</span><div className="tool-table-scroll"><table><thead><tr>{columns.map(column=><th key={column}>{column}</th>)}</tr></thead><tbody>{rows.map((row,index)=><tr key={index}>{row.map((value,column)=><td key={column}>{displayValue(value)}</td>)}</tr>)}</tbody></table></div></div>
}

function displayValue(value:unknown):string{
  if(value===null||value===undefined||value==='')return'—'
  if(Array.isArray(value))return value.map(item=>displayValue(item)).join(', ')
  const record=jsonRecord(value)
  if(record)return Object.entries(record).map(([key,item])=>`${key}=${displayValue(item)}`).join(' · ')
  return String(value)
}

function safeToolArgument(value:unknown,key=''):unknown{
	if(/(?:api[_-]?key|private[_-]?key|authorization|cookie|credential|passphrase|password|secret|token)/i.test(key))return'********'
	if(Array.isArray(value))return value.map(item=>safeToolArgument(item))
	const record=jsonRecord(value)
	if(record)return Object.fromEntries(Object.entries(record).map(([childKey,item])=>[childKey,safeToolArgument(item,childKey)]))
	return value
}

function toolArgumentSummary(toolName:string|undefined,argumentsValue:JsonRecord|undefined){
	if(!argumentsValue)return''
	if(toolName==='TaskCreate')return argumentsValue.subject?displayValue(argumentsValue.subject):''
	if(toolName==='TaskGet')return argumentsValue.taskId?`#${displayValue(argumentsValue.taskId)}`:''
	if(toolName==='TaskUpdate'){
		const taskID=argumentsValue.taskId||argumentsValue.task_id
		const nextStatus=argumentsValue.status
		return [taskID?`#${displayValue(taskID)}`:'',nextStatus?i18n.t(`statusLabels.${displayValue(nextStatus)}`,{defaultValue:displayValue(nextStatus)}):''].filter(Boolean).join(' · ')
	}
	const preferred=toolName==='web_extract'?['urls']:toolName==='skill'?['name','path']:toolName==='ssh_history'?['run_id','query']:['query','action','url','uri','path','name','run_id','task_id']
	for(const key of preferred){
		const value=safeToolArgument(argumentsValue[key],key)
		if(value===undefined||value===null||value==='')continue
		const displayed=displayValue(value)
		return Array.from(displayed).length>180?`${Array.from(displayed).slice(0,180).join('')}…`:displayed
	}
	return''
}

function formatLiveDuration(seconds:number){
	if(seconds<60)return`${seconds}s`
	const minutes=Math.floor(seconds/60)
	return`${minutes}m ${String(seconds%60).padStart(2,'0')}s`
}

function GenericToolResult({payload}:{payload:JsonRecord}){
  const hidden=new Set(['_display','stdout','stderr','operator_instruction','ok','status','code','message','next_action','run_id','duration','exit_code','auto_approved','tasks'])
  const entries=Object.entries(payload).filter(([key])=>!hidden.has(key))
	if(!entries.length)return null
  const scalars=entries.filter(([,value])=>value===null||typeof value==='string'||typeof value==='number'||typeof value==='boolean')
  const arrays=entries.filter(([,value])=>Array.isArray(value))
  const objects=entries.filter(([,value])=>!!jsonRecord(value))
  return <div className="tool-structured-result">
    {scalars.length>0&&<dl className="tool-generic-grid">{scalars.map(([key,value])=><div key={key}><dt>{key.replaceAll('_',' ')}</dt><dd>{displayValue(value)}</dd></div>)}</dl>}
    {arrays.map(([key,value])=><StructuredArray key={key} label={key} values={value as unknown[]}/>)}
    {objects.map(([key,value])=><StructuredObject key={key} label={key} value={value as JsonRecord}/>)}
  </div>
}

function StructuredArray({label,values}:{label:string;values:unknown[]}){
  const records=values.map(jsonRecord).filter((item):item is JsonRecord=>!!item)
  if(records.length===values.length&&records.length>0){const columns=[...new Set(records.flatMap(record=>Object.keys(record)))].slice(0,10);return <CompactTable title={`${label.replaceAll('_',' ')} · ${records.length} ITEMS`} columns={columns.map(column=>column.replaceAll('_',' '))} rows={records.map(record=>columns.map(column=>record[column]))}/>} 
  return <div className="tool-array-section"><span>{label.replaceAll('_',' ')}</span><div>{values.map((value,index)=><code key={index}>{displayValue(value)}</code>)}</div></div>
}

function StructuredObject({label,value}:{label:string;value:JsonRecord}){
  return <section className="tool-object-section"><h4>{label.replaceAll('_',' ')}</h4><dl className="tool-generic-grid">{Object.entries(value).map(([key,item])=><div key={key}><dt>{key.replaceAll('_',' ')}</dt><dd>{displayValue(item)}</dd></div>)}</dl></section>
}

function ReviewList({title,items,tone}:{title:string;items?:string[];tone?:string}){
  if(!items?.length)return null
  return <div className={`review-list ${tone||''}`}><b>{title}</b><ul>{items.map((item,index)=><li key={`${title}_${index}`}>{item}</li>)}</ul></div>
}

function CommandExplanationPanel({review}:{review?:CommandReview}){
	const {t,i18n:instance}=useTranslation()
  if(!review)return null
			if(review.status==='pending')return <div className="command-review-panel pending" role="status" aria-live="polite"><div className="command-review-pending"><span className="review-agent-icon"><LoaderCircle className="spin" size={17}/></span><b>{t('approval.reviewWorking')}</b></div></div>
  const explanation=review.explanation
	  return <details className={`command-review-panel ${review.status}`}><summary><span className="review-agent-icon"><BrainCircuit size={17}/></span><span><b>{t('approval.explanationAgent')}</b><small>{review.status==='completed'?t('approval.explanationCompleted'):review.status==='degraded'?t('approval.explanationPartial'):t('approval.explanationUnavailable')}</small></span><ChevronRight size={14}/></summary><div className="command-review-body">{review.decision&&<div className={`review-decision ${review.decision}`}><b>{t(`approval.review_${review.decision}`)}</b><span>{review.reason}</span></div>}{explanation&&<section className="review-explanation"><div className="review-section-title"><span>AI</span><div><b>{t('approval.plainExplanation')}</b><small>{explanation.summary}</small></div></div><p>{explanation.mechanism}</p><div className="review-list-grid"><ReviewList title={t('approval.risks')} items={explanation.risks} tone="warn"/></div></section>}{review.errors&&review.errors.length>0&&<div className="review-errors"><b>{t('approval.degradedInfo')}</b>{review.errors.map((item,index)=><code key={index}>{item}</code>)}</div>}<div className="review-meta">{t('common.model')} {review.model||t('common.unavailable')} · {review.reviewed_at?new Date(review.reviewed_at).toLocaleString(localeFor(instance.language)):'—'}</div></div></details>
}

function ApprovalDialog({
  approval,
  pendingCount,
  hosts,
  running,
  stopping,
  onStop,
  refresh,
  refreshApprovals,
  onApproved,
  onNotice,
}: {
  approval: Approval;
  pendingCount: number;
  hosts: Host[];
  running: boolean;
  stopping: boolean;
  onStop: () => void;
  refresh: () => Promise<void>;
  refreshApprovals: (decidedID?: string) => Promise<void>;
  onApproved: (result: ApprovalExecutionResult) => void;
  onNotice: (message: string) => void;
}) {
  const { t, i18n: instance } = useTranslation();
  const [note, setNote] = useState("");
  const [decisionBusy, setDecisionBusy] = useState<
    "" | "once" | "reject"
  >("");
  const [explanationBusy, setExplanationBusy] = useState(false);
  const [error, setError] = useState("");
  let request: Record<string, unknown> = {};
  try {
    request = JSON.parse(approval.request_json);
  } catch {
    request = { request: approval.request_json };
  }
  const script = textValue(request.script);
  const change = jsonRecord(request.change);
  const workspaceID = textValue(request.workspace_id);
  const filePath =
    textValue(request.remote_path) || textValue(request.relative_path);
  const requestMode = textValue(request.mode),
    relativePath = textValue(request.relative_path),
    remotePath = textValue(request.remote_path);
  const searchMatchMode = textValue(request.search_match_mode);
  const searchMatchModeLabel = searchMatchMode === "literal"
    ? t("tool.matchModeLiteral")
    : searchMatchMode === "regex"
      ? t("tool.matchModeRegex")
      : searchMatchMode || "—";
  const workspaceShellBackend = textValue(request.workspace_shell_backend);
  const workspaceShellApproval = requestMode === "workspace_shell_start";
  const hostWorkspaceShell =
    (requestMode === "workspace_shell" || workspaceShellApproval) && workspaceShellBackend === "host";
  const tunnelApproval = requestMode === "ssh_tunnel_start";
  const sshShellApproval = requestMode === "ssh_shell_start";
  const interactiveShellApproval = sshShellApproval || workspaceShellApproval;
  const fileReadApproval = [
    "remote_read",
    "remote_search",
    "workspace_read",
    "workspace_search",
  ].includes(requestMode);
  const fileSearchApproval = ["remote_search", "workspace_search"].includes(
    requestMode,
  );
  const workspaceUpload = requestMode === "workspace_upload";
  const workspaceDownload = requestMode === "workspace_download";
  const workspaceTransfer = workspaceUpload || workspaceDownload;
  const sshTransfer = requestMode === "ssh_file_transfer";
  const sourceHostID = textValue(request.source_host_id);
  const sourcePath = textValue(request.source_path);
  const elevated = request.elevated === true;
  const actionKind = script
    ? t("approval.actionScript")
    : t("approval.actionCommand");
  const approvalTitle = fileReadApproval
    ? elevated
      ? t(fileSearchApproval ? "approval.sudoSearchTitle" : "approval.sudoReadTitle")
      : t(fileSearchApproval ? "approval.searchTitle" : "approval.readTitle")
    : tunnelApproval
      ? t("approval.tunnelTitle")
    : interactiveShellApproval
      ? t("approval.sshShellTitle")
    : elevated
    ? filePath
      ? t("approval.sudoFileTitle")
      : t("approval.sudoTitle", { kind: actionKind })
    : tunnelApproval
    ? t("approval.tunnelLabel")
    : sshTransfer
      ? t("approval.transferTitle")
      : workspaceUpload
        ? t("approval.uploadTitle")
      : workspaceDownload
        ? t("approval.downloadTitle")
      : hostWorkspaceShell
        ? t("approval.hostShellTitle")
        : filePath
          ? t("approval.fileTitle")
          : t("approval.executeTitle", { kind: actionKind });
  const commandLabel = fileReadApproval
    ? elevated
      ? t(fileSearchApproval ? "approval.rootSearchLabel" : "approval.rootReadLabel")
      : t(fileSearchApproval ? "approval.searchLabel" : "approval.readLabel")
    : interactiveShellApproval
    ? t("approval.sshShellLabel")
    : sshTransfer
    ? t("approval.transferLabel")
    : workspaceUpload
      ? t("approval.uploadLabel")
    : workspaceDownload
      ? t("approval.downloadLabel")
    : elevated
      ? filePath
        ? t("approval.rootFileLabel")
        : t("approval.rootCommandLabel", { kind: actionKind })
      : filePath
        ? t("approval.fileLabel")
        : t("approval.commandLabel", { kind: actionKind });
  const target = hosts.find((host) => host.id === approval.host_id);
  const targetHost = target?.name || approval.host_id;
  const source = hosts.find((host) => host.id === sourceHostID);
  const sourceHost = source?.name || sourceHostID;
  const tunnelLocalPort = numberValue(request.local_port);
  const tunnelRemoteHost = textValue(request.remote_host);
  const tunnelRemotePort = numberValue(request.remote_port);
  const tunnelOperation = sshTunnelRoute(targetHost,tunnelRemoteHost,tunnelRemotePort,tunnelLocalPort,t('tunnels.automaticPort'));
  const sshShellOperation = `${targetHost}:${textValue(request.cwd)||'~'} · PTY`;
	const workspaceShellOperation = `${workspaceID}:${textValue(request.cwd)||'.'} · PTY`;
  const operation = tunnelApproval
    ? tunnelOperation
    : interactiveShellApproval
    ? workspaceShellApproval ? workspaceShellOperation : sshShellOperation
    : sshTransfer
    ? `${sourceHost}:${sourcePath} → ${targetHost}:${remotePath}`
    : workspaceUpload
      ? `${workspaceID}:${relativePath} → ${targetHost}:${remotePath}`
    : workspaceDownload
      ? `${targetHost}:${remotePath} → ${workspaceID}:${relativePath}`
    : fullProgram(request) ||
      script ||
      `${requestMode} ${filePath}${fileSearchApproval ? ` · ${searchMatchModeLabel} pattern=${JSON.stringify(textValue(request.search_pattern))}` : ""}`.trim() ||
      t("approval.pendingOperation");
  const targetHostIdentity = [targetHost, target?.id && target.id !== targetHost ? target.id : approval.host_id !== targetHost ? approval.host_id : ''].filter(Boolean).join(' · ')
  const sourceHostIdentity = [sourceHost, source?.id && source.id !== sourceHost ? source.id : sourceHostID !== sourceHost ? sourceHostID : ''].filter(Boolean).join(' · ')
  const hostName = workspaceUpload || sshTransfer
    ? targetHostIdentity
    : workspaceID
      ? `Workspace / ${workspaceID}`
      : targetHostIdentity;
  const executionIdentity = elevated
    ? t("approval.rootViaSudo")
    : target?.user || t("approval.serviceUser");
  const expectedSHA = textValue(request.expected_sha256),
    expectedDestinationSHA = textValue(request.expected_destination_sha256),
    validator = textValue(request.validator);
  const fileApprovalParameters: Array<Array<unknown>> = fileReadApproval
    ? [
        ...(workspaceID
          ? [["workspace_id", workspaceID]]
          : [["host_id", approval.host_id]]),
        ["path", filePath],
        ...(fileSearchApproval
          ? [
              ["match_mode", searchMatchModeLabel],
              ["pattern", textValue(request.search_pattern)],
              ["context_lines", numberValue(request.context_lines)],
            ]
          : [
              ...(workspaceID
                ? []
                : [["metadata_only", request.metadata_only === true]]),
              ["max_bytes", numberValue(request.max_bytes)],
              ["offset_bytes", numberValue(request.offset_bytes)],
              ["tail_lines", numberValue(request.tail_lines)],
            ]),
        ...(workspaceID ? [] : [["elevated", elevated]]),
      ]
    : [];
  const explanationPending = approval.ai_review?.status === "pending";
  const decide = async () => {
    setDecisionBusy("once");
    setError("");
    try {
      const result = await api.approve(
        approval.id,
        note.trim() || "Reviewed and approved.",
      );
      onApproved(result);
      onNotice(
        t("approval.approved", {
          status: t(`statusLabels.${result.status}`, {
            defaultValue: result.status,
          }),
          run: result.run_id,
        }),
      );
      void refreshApprovals(approval.id);
      void refresh();
    } catch (err) {
      setError(errorText(err));
    } finally {
      setDecisionBusy("");
    }
  };
  const reject = async () => {
    const instruction = note.trim();
    if (!instruction) {
      setError(t("approval.replacementRequired"));
      return;
    }
    setDecisionBusy("reject");
    setError("");
    try {
      await api.reject(approval.id, instruction);
      onNotice(t("approval.rejected"));
      void refreshApprovals(approval.id);
      void refresh();
    } catch (err) {
      setError(errorText(err));
    } finally {
      setDecisionBusy("");
    }
  };
  const retryExplanation = async () => {
    setExplanationBusy(true);
    setError("");
    try {
      const updated = await api.retryApprovalExplanation(approval.id);
      const status = updated.ai_review?.status;
      onNotice(
        status === "completed"
          ? t("approval.explanationReady")
          : t("approval.explanationDegraded"),
      );
      await refresh();
    } catch (err) {
      setError(errorText(err));
    } finally {
      setExplanationBusy(false);
    }
  };
  const decisionDisabled = !!decisionBusy;
  return (
    <div className="approval-modal-backdrop">
      <section
        className={`approval-dialog ${elevated ? "elevated" : ""}`}
        role="dialog"
        aria-modal="true"
        aria-labelledby="approval-dialog-title"
      >
        <div className="approval-dialog-head">
          <div className="approval-dialog-icon">
            <ShieldAlert size={20} />
          </div>
          <div>
            <span>
              {t("approval.confirmation", {
                queue:
                  pendingCount > 1
                    ? t("approval.queue", { count: pendingCount })
                    : t("approval.currentSession"),
              })}
            </span>
            <h2 id="approval-dialog-title">{approvalTitle}</h2>
          </div>
        </div>
        <div className="approval-operation">
          <span className="approval-command-label">
            {commandLabel}
            {elevated && (
              <em>
                <ShieldAlert size={12} />
                sudo / root
              </em>
            )}
          </span>
          {elevated && (
            <div className="approval-root-warning">
              <ShieldAlert size={18} />
              <div>
                <b>{t("approval.rootWarning")}</b>
              </div>
            </div>
          )}
          {filePath && (
            <div className="approval-file-target">
              <FileText size={15} />
              <div>
                <b>
                  {workspaceUpload
                    ? `${workspaceID}:${relativePath} -> ${targetHost}:${remotePath}`
                    : workspaceDownload
                      ? `${targetHost}:${remotePath} -> ${workspaceID}:${relativePath}`
                    : sshTransfer
                      ? `${sourceHost}:${sourcePath} -> ${targetHost}:${remotePath}`
                      : filePath}
                </b>
                <span>
                  {change
                    ? `${t('tool.fileEdit')} · +${numberValue(change.additions)} / -${numberValue(change.deletions)}`
                    : sshTransfer && expectedSHA
                    ? `${t("approval.sourceSHA")} · ${expectedSHA}${expectedDestinationSHA ? ` · ${t("approval.destinationSHA")} · ${expectedDestinationSHA}` : ""}`
                    : (workspaceTransfer && expectedSHA)
                      ? `Expected SHA256 · ${expectedSHA}`
                      : ''}
                  {validator ? ` · Validator ${validator}` : ""}
                </span>
              </div>
            </div>
          )}
          {fileApprovalParameters.length > 0 && (
            <CompactTable
              title={t("tool.actualParameters")}
              columns={[t("tool.parameter"), t("tool.value")]}
              rows={fileApprovalParameters}
            />
          )}
          {change&&textValue(change.diff)?<DiffViewer change={change}/>:<CopyablePre value={script||operation} preClassName="approval-command-preview">{script || `${tunnelApproval||interactiveShellApproval?'':'$ '}${operation}`}</CopyablePre>}
          <dl>
            <div>
              <dt>
                {workspaceUpload || sshTransfer
                  ? t("approval.targetHost")
                  : workspaceID
                    ? t("common.workspace")
                    : t("approval.targetHost")}
              </dt>
              <dd>{hostName}</dd>
            </div>
            {sshTransfer && (
              <div>
                <dt>{t("approval.sourceHost")}</dt>
                <dd>{sourceHostIdentity}</dd>
              </div>
            )}
            {workspaceDownload && (
              <div>
                <dt>{t("approval.sourceHost")}</dt>
                <dd>{targetHostIdentity}</dd>
              </div>
            )}
            <div>
              <dt>{t("approval.identity")}</dt>
              <dd>{executionIdentity}</dd>
            </div>
            {workspaceShellBackend && (
              <div>
                <dt>{t("approval.environment")}</dt>
                <dd>
                  {hostWorkspaceShell
                    ? t("approval.hostShell")
                    : t("tool.sandbox")}
                </dd>
              </div>
            )}
            <div>
              <dt>{t("approval.digest")}</dt>
              <dd>{approval.request_digest.slice(0, 12)}</dd>
            </div>
          </dl>
          {(hostWorkspaceShell || sshShellApproval) && (
            <div className="approval-host-shell-warning">
              <ShieldAlert size={14} />
              <span>{t(sshShellApproval?"approval.sshShellWarning":"approval.hostShellWarning")}</span>
            </div>
          )}
          {typeof request.reason === "string" && <p>{request.reason}</p>}
        </div>
        <CommandExplanationPanel review={approval.ai_review} />
        <div className="review-retry-row">
          <button
            disabled={decisionDisabled || explanationPending || explanationBusy}
            onClick={retryExplanation}
          >
            <RefreshCw
              className={explanationBusy || explanationPending ? "spin" : ""}
              size={13}
            />
            {explanationPending
              ? t("approval.reviewWorking")
              : explanationBusy
                ? t("approval.retrying")
                : t("approval.retryExplanation")}
          </button>
        </div>
        <label className="approval-guidance">
          <span>{t("approval.guidance")}</span>
          <textarea
            value={note}
            maxLength={2000}
            onChange={(event) => setNote(event.target.value)}
            autoFocus
          />
        </label>
        {error && (
          <div className="approval-dialog-error">
            <ShieldAlert size={14} />
            {error}
          </div>
        )}
        <details className="approval-request-detail">
          <summary>{t("approval.requestDetails")}</summary>
          <CopyablePre>{JSON.stringify(request, null, 2)}</CopyablePre>
        </details>
        <div className="approval-choice-grid">
          <button
            className="allow-once"
            disabled={decisionDisabled || stopping}
            onClick={() => decide()}
          >
            <Check size={16} />
            <span>
              <b>
                {decisionBusy === "once"
                  ? t("approval.executing")
                  : elevated
                    ? t("approval.allowSudo")
                    : t("approval.allowOnce")}
              </b>
            </span>
          </button>
          <button
            className="reject-guidance"
            disabled={decisionDisabled || stopping || !note.trim()}
            onClick={reject}
          >
            <X size={16} />
            <span>
              <b>
                {decisionBusy === "reject"
                  ? t("approval.rejecting")
                  : t("approval.reject")}
              </b>
            </span>
          </button>
          <button
            className="stop-agent-run"
            disabled={decisionDisabled || stopping || !running}
            onClick={onStop}
          >
            <Square size={14} fill="currentColor" />
            <span>
              <b>
                {stopping ? t("approval.stopping") : t("approval.stopAgent")}
              </b>
            </span>
          </button>
        </div>
      </section>
    </div>
  );
}

const maxPrivateKeyBytes = 1 << 20;
const emptyHostForm: HostInput = {
  name: "",
  address: "",
  port: 22,
  user: "",
  agent_enabled: true,
  auth_type: "agent",
  private_key: "",
  known_hosts_file: "",
  proxy_jump_host_id: "",
  proxy_id: "",
  password: "",
  sudo_mode: "none",
  sudo_password: "",
};
function authLabel(value:HostAuthType){return i18n.t(value==='agent'?'hosts.authAgent':value==='key'?'hosts.authKey':'hosts.authPassword')}
function sudoLabel(value:HostSudoMode){return i18n.t(value==='none'?'hosts.sudoNone':value==='nopasswd'?'hosts.sudoNopasswd':'hosts.sudoPassword')}

function HostsPage({ hosts, proxies, showAddresses, onToggleAddresses, refresh }: {hosts:Host[];proxies:Proxy[];showAddresses:boolean;onToggleAddresses:()=>void;refresh:()=>Promise<void>}) {
	const {t}=useTranslation()
	const notify=useNotifier()
  const [showForm, setShowForm] = useState(false); const [saving,setSaving]=useState(false);const [deletingHost,setDeletingHost]=useState('')
	const [deleteCandidate,setDeleteCandidate]=useState<Host|null>(null)
  const [form, setForm] = useState<HostInput>(emptyHostForm)
	const [formErrors,setFormErrors]=useState<Partial<Record<'name'|'address'|'port'|'user'|'password'|'sudo_password',string>>>({})
	const [privateKeyName,setPrivateKeyName]=useState(''),[privateKeyError,setPrivateKeyError]=useState(''),[existingPrivateKey,setExistingPrivateKey]=useState(false),[privateKeyInputKey,setPrivateKeyInputKey]=useState(0)
	const [hostKeys,setHostKeys]=useState<Record<string,{fingerprint:string;algorithm?:string;trusted:boolean}>>({}),[hostKeyErrors,setHostKeyErrors]=useState<Record<string,string>>({}),[hostKeyBusy,setHostKeyBusy]=useState('')
  const editing=!!form.id
	const resetPrivateKey=()=>{setPrivateKeyName('');setPrivateKeyError('');setExistingPrivateKey(false);setPrivateKeyInputKey(value=>value+1)}
	const updateHostForm=<K extends keyof HostInput>(field:K,value:HostInput[K])=>{setForm(current=>({...current,[field]:value}));setFormErrors(current=>{if(!current[field as keyof typeof current])return current;const next={...current};delete next[field as keyof typeof next];return next})}
	const openCreate=()=>{setForm(emptyHostForm);setFormErrors({});resetPrivateKey();setShowForm(true)}
	const openEdit=(host:Host)=>{setForm({id:host.id,name:host.name,address:host.address,port:host.port,user:host.user,agent_enabled:host.agent_enabled,auth_type:host.auth_type||'agent',private_key:'',known_hosts_file:host.known_hosts_file||'',proxy_jump_host_id:host.proxy_jump_host_id||'',proxy_id:host.proxy_id||'',password:'',sudo_mode:host.sudo_mode||'none',sudo_password:''});setFormErrors({});setPrivateKeyName('');setPrivateKeyError('');setExistingPrivateKey(host.auth_type==='key'&&host.has_private_key);setPrivateKeyInputKey(value=>value+1);setShowForm(true)}
	const setAuthType=(auth_type:HostAuthType)=>{setFormErrors(current=>({...current,password:''}));setForm(current=>({...current,auth_type,password:'',private_key:auth_type==='key'?current.private_key:''}));if(auth_type!=='key'){setPrivateKeyName('');setPrivateKeyError('');setPrivateKeyInputKey(value=>value+1)}}
	const choosePrivateKey=async(event:React.ChangeEvent<HTMLInputElement>)=>{const selected=event.target.files?.[0];setPrivateKeyError('');if(!selected){setPrivateKeyName('');setForm(current=>({...current,private_key:''}));return}if(selected.size<=0||selected.size>maxPrivateKeyBytes){setPrivateKeyName('');setForm(current=>({...current,private_key:''}));setPrivateKeyError(t('hosts.keySizeError'));return}try{const content=await selected.text();setPrivateKeyName(selected.name);setForm(current=>({...current,private_key:content}))}catch(err){setPrivateKeyName('');setForm(current=>({...current,private_key:''}));setPrivateKeyError(errorText(err))}}
	const missingPrivateKey=form.auth_type==='key'&&!form.private_key&&!existingPrivateKey
	const scan = async (host:Host) => {setHostKeyBusy(`scan-${host.id}`);setHostKeyErrors(current=>({...current,[host.id]:''}));try{const key=await api.scanKey(host.id);setHostKeys(current=>({...current,[host.id]:key}))}catch(err){setHostKeyErrors(current=>({...current,[host.id]:errorText(err)}))}finally{setHostKeyBusy('')}}
	const trust = async (host:Host) => {const key=hostKeys[host.id];if(!key||key.trusted)return;setHostKeyBusy(`trust-${host.id}`);setHostKeyErrors(current=>({...current,[host.id]:''}));try{const trusted=await api.trustKey(host.id,key.fingerprint);setHostKeys(current=>({...current,[host.id]:{...trusted,trusted:true}}));notify(t('hosts.trusted',{fingerprint:trusted.fingerprint}))}catch(err){setHostKeyErrors(current=>({...current,[host.id]:errorText(err)}))}finally{setHostKeyBusy('')}}
	const validateHost=()=>{const errors:typeof formErrors={};if(!form.name.trim())errors.name=t('common.required');if(!form.address.trim())errors.address=t('common.required');if(!Number.isInteger(form.port)||form.port<1||form.port>65535)errors.port=t('hosts.portRange');if(!form.user.trim())errors.user=t('common.required');if(!editing&&form.auth_type==='password'&&!form.password)errors.password=t('common.required');if(!editing&&form.sudo_mode==='password'&&!form.sudo_password)errors.sudo_password=t('common.required');setFormErrors(errors);return !Object.keys(errors).length&&!missingPrivateKey&&!privateKeyError}
	const save = async (event:FormEvent) => { event.preventDefault(); if(!validateHost())return;setSaving(true); try { const saved=await api.saveHost({...form,name:form.name.trim(),address:form.address.trim(),user:form.user.trim()}); setShowForm(false); setForm(emptyHostForm);setFormErrors({});resetPrivateKey();setHostKeys(current=>{const next={...current};delete next[saved.id];return next});setHostKeyErrors(current=>{const next={...current};delete next[saved.id];return next}); notify(t('hosts.saved',{name:saved.name,action:editing?t('hosts.updated'):t('hosts.registered')})); await refresh();void scan(saved) } catch(err){notify(errorText(err),'error')} finally{setSaving(false)} }
  const probe = async (host:Host) => { try { const info = await api.probe(host.id); notify(`${host.name}: ${Object.values(info).join(' · ')}`) } catch(err){notify(errorText(err),'error')} }
	const remove=async()=>{const host=deleteCandidate;if(!host)return;setDeletingHost(host.id);try{await api.deleteHost(host.id);notify(t('hosts.deleted',{name:host.name}));await refresh()}catch(err){notify(errorText(err),'error')}finally{setDeletingHost('');setDeleteCandidate(null)}}
		return <div className="page-stack">{!showForm&&<div className="page-actions"><div/><div className="page-action-buttons"><AddressVisibilityButton visible={showAddresses} onToggle={onToggleAddresses}/><button className="primary" onClick={openCreate}><Plus size={16}/>{t('hosts.add')}</button></div></div>}
		{showForm && <ConfigurationEditorPage icon={<Server size={22}/>} title={editing?t('hosts.editTitle'):t('hosts.createTitle')} busy={saving} onBack={()=>setShowForm(false)}><form className="host-form configuration-editor-form panel" noValidate onSubmit={save}><div className="form-grid host-fields">
	  <label className={formErrors.name?'invalid':''}><span>{t('hosts.name')}</span><input value={form.name} aria-invalid={!!formErrors.name} onChange={event=>updateHostForm('name',event.target.value)}/>{formErrors.name&&<small className="form-field-error">{formErrors.name}</small>}</label>
	  <label className={formErrors.address?'invalid':''}><span>{t('hosts.address')}</span><input value={form.address} aria-invalid={!!formErrors.address} onChange={event=>updateHostForm('address',event.target.value)}/>{formErrors.address&&<small className="form-field-error">{formErrors.address}</small>}</label>
	  <label className={formErrors.port?'invalid':''}><span>{t('hosts.port')}</span><input inputMode="numeric" value={form.port||''} aria-invalid={!!formErrors.port} onChange={event=>{const value=event.target.value.replace(/\D/g,'').slice(0,5);updateHostForm('port',value?Number(value):0)}}/>{formErrors.port&&<small className="form-field-error">{formErrors.port}</small>}</label>
	  <label className={formErrors.user?'invalid':''}><span>{t('hosts.user')}</span><input value={form.user} aria-invalid={!!formErrors.user} onChange={event=>updateHostForm('user',event.target.value)}/>{formErrors.user&&<small className="form-field-error">{formErrors.user}</small>}</label>
	  <label className="host-agent-toggle"><span>Agent</span><input type="checkbox" checked={form.agent_enabled} onChange={event=>setForm({...form,agent_enabled:event.target.checked})}/><i/></label>
	  <label><span>{t('hosts.authentication')}</span><AppSelect value={form.auth_type} ariaLabel={t('hosts.authentication')} onChange={value=>setAuthType(value as HostAuthType)} options={(['agent','key','password'] as HostAuthType[]).map(mode=>({value:mode,label:authLabel(mode)}))}/></label>
	  {form.auth_type==='password'&&<label className={formErrors.password?'invalid':''}><span>{t('hosts.sshPassword')}</span><PasswordInput autoComplete="new-password" value={form.password} aria-invalid={!!formErrors.password} onChange={event=>updateHostForm('password',event.target.value)} placeholder={editing?t('hosts.keepPassword'):t('common.required')}/>{formErrors.password&&<small className="form-field-error">{formErrors.password}</small>}</label>}
	  {form.auth_type==='key'&&<div className="private-key-field"><span>{t('hosts.privateKey')}</span><label className={`private-key-picker ${privateKeyError||missingPrivateKey?'invalid':''}`} title={privateKeyName||t('hosts.chooseKey')}><UploadCloud size={15}/><span><b>{privateKeyName||(existingPrivateKey?t('hosts.storedKey'):t('hosts.choosePrivateKey'))}</b>{!privateKeyName&&!existingPrivateKey&&<small>{t('hosts.keyLimit')}</small>}</span><input key={privateKeyInputKey} type="file" onChange={event=>void choosePrivateKey(event)}/></label>{(privateKeyError||missingPrivateKey)&&<small className="private-key-error">{privateKeyError||t('hosts.keyRequired')}</small>}</div>}
	  <label><span>{t('hosts.proxyJump')}</span><AppSelect value={form.proxy_jump_host_id} ariaLabel={t('hosts.proxyJump')} onChange={value=>updateHostForm('proxy_jump_host_id',value)} options={[{value:'',label:t('hosts.direct')},...hosts.filter(host=>host.id!==form.id).map(host=>({value:host.id,label:`${host.name} · ${host.user}@${host.address}:${host.port}`}))]}/></label>
	  <label><span>{t('common.proxy')}</span><AppSelect value={form.proxy_id} ariaLabel={t('common.proxy')} onChange={value=>updateHostForm('proxy_id',value)} options={[{value:'',label:t('hosts.direct')},...proxies.filter(proxy=>proxy.ssh_compatible).map(proxy=>({value:proxy.id,label:`${proxy.name} · ${proxy.url}`}))]}/></label>
	  <label><span>{t('hosts.knownHosts')}</span><input value={form.known_hosts_file} onChange={event=>setForm({...form,known_hosts_file:event.target.value})} placeholder={t('hosts.useDefault')}/></label>
	  <label><span>{t('hosts.sudoPolicy')}</span><AppSelect value={form.sudo_mode} ariaLabel={t('hosts.sudoPolicy')} onChange={value=>{setFormErrors(current=>({...current,sudo_password:''}));setForm({...form,sudo_mode:value as HostSudoMode,sudo_password:''})}} options={(['none','nopasswd','password'] as HostSudoMode[]).map(mode=>({value:mode,label:sudoLabel(mode)}))}/></label>
	  {form.sudo_mode==='password'&&<label className={formErrors.sudo_password?'invalid':''}><span>{t('hosts.sudoPasswordLabel')}</span><PasswordInput autoComplete="new-password" value={form.sudo_password} aria-invalid={!!formErrors.sudo_password} onChange={event=>updateHostForm('sudo_password',event.target.value)} placeholder={editing?t('hosts.keepPassword'):t('common.required')}/>{formErrors.sudo_password&&<small className="form-field-error">{formErrors.sudo_password}</small>}</label>}
		</div><div className="form-actions"><button type="button" onClick={()=>setShowForm(false)}>{t('common.cancel')}</button><button className="primary" disabled={saving||!!privateKeyError||missingPrivateKey}>{saving?t('common.saving'):editing?t('hosts.update'):t('hosts.save')}</button></div></form></ConfigurationEditorPage>}
		{!showForm&&<div className="host-grid">{hosts.map(host=>{const key=hostKeys[host.id]||host.host_key,keyError=hostKeyErrors[host.id],scanning=hostKeyBusy===`scan-${host.id}`,trusting=hostKeyBusy===`trust-${host.id}`,proxy=proxies.find(item=>item.id===host.proxy_id);return <article className="host-card panel" key={host.id}><div className="host-top"><div className="server-glyph"><Server size={22}/></div><div><h3>{host.name}</h3><span>{`${host.user}@${showAddresses?host.address:'••••••'}:${host.port}`}</span></div><div className="host-top-states"><span className={`host-agent-state ${host.agent_enabled?'active':''}`} title="Agent"><Bot size={13}/></span><span className={`host-key-state ${key?.trusted?'trusted':key?'untrusted':'unchecked'}`}>{scanning?t('hosts.checkingKey'):key?.trusted?t('hosts.trustedKey'):key?t('hosts.untrustedKey'):t('hosts.uncheckedKey')}</span></div></div><dl><div><dt>{t('hosts.authentication')}</dt><dd>{authLabel(host.auth_type||'agent')}</dd></div>{proxy&&<div><dt>{t('hosts.proxy')}</dt><dd title={showAddresses?proxy.url:undefined}>{proxy.name}</dd></div>}<div><dt>Sudo</dt><dd>{sudoLabel(host.sudo_mode||'none')}</dd></div><div><dt>{t('hosts.hostId')}</dt><dd>{host.id}</dd></div></dl>{(key||keyError)&&<div className={`host-key-review ${key?.trusted?'trusted':'untrusted'}`}>{key&&<><div><KeyRound size={14}/><span><b>{key.algorithm||t('hosts.hostKey')}</b><code title={key.fingerprint}>{key.fingerprint}</code></span></div>{!key.trusted&&<button className="trust" disabled={trusting} onClick={()=>void trust(host)}>{trusting?<LoaderCircle className="spin" size={13}/>:<ShieldCheck size={13}/>} {trusting?t('hosts.trustingKey'):t('hosts.trustKey')}</button>}</>}{keyError&&<span className="host-key-error">{keyError}</span>}</div>}<div className="card-actions"><button onClick={()=>void probe(host)}><Activity size={15}/>{t('hosts.probe')}</button><button disabled={scanning||trusting} onClick={()=>void scan(host)}>{scanning?<LoaderCircle className="spin" size={15}/>:<KeyRound size={15}/>} {t('hosts.checkKey')}</button><button onClick={()=>openEdit(host)}><Edit3 size={15}/>{t('common.edit')}</button><button className="danger" disabled={deletingHost===host.id} title={t('common.delete')} onClick={()=>setDeleteCandidate(host)}>{deletingHost===host.id?<LoaderCircle className="spin" size={15}/>:<Trash2 size={15}/>}</button></div></article>})}</div>}
	{!showForm&&!hosts.length && <Empty icon={<Server/>} title={t('hosts.emptyTitle')}/>}
	{deleteCandidate&&<DestructiveConfirmDialog title={t('hosts.deleteTitle',{name:deleteCandidate.name})} busy={deletingHost===deleteCandidate.id} onCancel={()=>setDeleteCandidate(null)} onConfirm={()=>void remove()}/>}
  </div>
}

const emptyProxyForm:ProxyInput={name:'',url:'',username:'',password:''}

function ProxiesPage({proxies,showAddresses,onToggleAddresses,refresh}:{proxies:Proxy[];showAddresses:boolean;onToggleAddresses:()=>void;refresh:()=>Promise<void>}){
	const {t,i18n:instance}=useTranslation()
	const notify=useNotifier()
	const [form,setForm]=useState<ProxyInput>(emptyProxyForm)
	const [showForm,setShowForm]=useState(false)
	const [busy,setBusy]=useState('')
	const [formErrors,setFormErrors]=useState<Partial<Record<'name'|'url',string>>>({})
	const [deleteCandidate,setDeleteCandidate]=useState<Proxy|null>(null)
	const editing=!!form.id
	const editingProxy=proxies.find(proxy=>proxy.id===form.id)
	const preservesPassword=!!editingProxy?.has_password&&form.username===(editingProxy.username||'')&&!form.clear_password
	const updateProxyForm=<K extends keyof ProxyInput>(field:K,value:ProxyInput[K])=>{setForm(current=>({...current,[field]:value}));setFormErrors(current=>{if(!current[field as keyof typeof current])return current;const next={...current};delete next[field as keyof typeof next];return next})}
	const openCreate=()=>{setForm(emptyProxyForm);setFormErrors({});setShowForm(true)}
	const openEdit=(proxy:Proxy)=>{setForm({id:proxy.id,name:proxy.name,url:proxy.url,username:proxy.username||'',password:''});setFormErrors({});setShowForm(true)}
	const validateProxy=()=>{const errors:typeof formErrors={};if(!form.name.trim())errors.name=t('common.required');if(!form.url.trim())errors.url=t('common.required');else try{const url=new URL(form.url);if(!url.hostname)errors.url=t('proxies.invalidUrl')}catch{errors.url=t('proxies.invalidUrl')}setFormErrors(errors);return !Object.keys(errors).length}
	const save=async(event:FormEvent)=>{event.preventDefault();if(!validateProxy())return;setBusy('save');try{const saved=await api.saveProxy({...form,name:form.name.trim(),url:form.url.trim()});notify(t('proxies.saved',{name:saved.name}));setForm(emptyProxyForm);setFormErrors({});setShowForm(false);await refresh()}catch(err){notify(errorText(err),'error')}finally{setBusy('')}}
	const test=async(proxy:Proxy)=>{setBusy(`test-${proxy.id}`);try{const result=await api.testProxy(proxy.id);notify(t('proxies.testPassed',{name:proxy.name,status:result.status_code||0,latency:result.latency_ms}))}catch(err){notify(errorText(err),'error')}finally{setBusy('')}}
	const remove=async()=>{const proxy=deleteCandidate;if(!proxy)return;setBusy(`delete-${proxy.id}`);try{await api.deleteProxy(proxy.id);notify(t('proxies.deleted',{name:proxy.name}));if(form.id===proxy.id){setForm(emptyProxyForm);setShowForm(false)}await refresh()}catch(err){notify(errorText(err),'error')}finally{setBusy('');setDeleteCandidate(null)}}
	return <div className="page-stack">
		{!showForm&&<div className="page-actions"><div><p>{t('proxies.title')}</p><span>{t('proxies.description')}</span></div><div className="page-action-buttons"><AddressVisibilityButton visible={showAddresses} onToggle={onToggleAddresses}/><button className="primary" onClick={openCreate}><Plus size={16}/>{t('proxies.add')}</button></div></div>}
		{showForm&&<ConfigurationEditorPage icon={<Cable size={22}/>} title={editing?t('proxies.editTitle'):t('proxies.createTitle')} busy={busy==='save'} onBack={()=>setShowForm(false)}><form className="proxy-form configuration-editor-form panel" noValidate onSubmit={save}><div className="form-grid proxy-fields"><label className={formErrors.name?'invalid':''}><span>{t('proxies.name')}</span><input value={form.name} maxLength={128} aria-invalid={!!formErrors.name} onChange={event=>updateProxyForm('name',event.target.value)}/>{formErrors.name&&<small className="form-field-error">{formErrors.name}</small>}</label><label className={`proxy-address-field ${formErrors.url?'invalid':''}`}><span>{t('proxies.url')}</span><input value={form.url} aria-invalid={!!formErrors.url} onChange={event=>updateProxyForm('url',event.target.value)} placeholder="socks5://127.0.0.1:1080"/>{formErrors.url&&<small className="form-field-error">{formErrors.url}</small>}</label><label><span>{t('proxies.username')}</span><input autoComplete="off" value={form.username} onChange={event=>setForm({...form,username:event.target.value,password:event.target.value?form.password:'',clear_password:false})}/></label><label><span>{t('proxies.password')}</span><PasswordInput autoComplete="new-password" value={form.password} disabled={!form.username} onChange={event=>setForm({...form,password:event.target.value,clear_password:false})} placeholder={preservesPassword?t('proxies.keepPassword'):''}/>{preservesPassword&&<small><button type="button" onClick={()=>setForm({...form,password:'',clear_password:true})}>{t('proxies.clearPassword')}</button></small>}</label></div><div className="form-actions"><button type="button" onClick={()=>setShowForm(false)}>{t('common.cancel')}</button><button className="primary" disabled={busy==='save'}>{busy==='save'?t('common.saving'):t('common.save')}</button></div></form></ConfigurationEditorPage>}
		{!showForm&&<div className="proxy-grid">{proxies.map(proxy=><article className="proxy-card panel" key={proxy.id}><div className="proxy-card-head"><div><Cable size={20}/></div><span><h3>{proxy.name}</h3><code>{showAddresses?proxy.url:'••••••'}</code></span>{proxy.ssh_compatible&&<em>SSH</em>}</div><dl><div><dt>{t('proxies.authentication')}</dt><dd>{proxy.username?`${proxy.username}${proxy.has_password?` · ${t('proxies.passwordSaved')}`:''}`:t('proxies.noAuthentication')}</dd></div><div><dt>{t('common.updated')}</dt><dd>{new Date(proxy.updated_at).toLocaleString(localeFor(instance.language))}</dd></div></dl><div className="card-actions"><button disabled={!!busy} onClick={()=>void test(proxy)}>{busy===`test-${proxy.id}`?<LoaderCircle className="spin" size={14}/>:<Activity size={14}/>} {t('common.test')}</button><button disabled={!!busy} onClick={()=>openEdit(proxy)}><Edit3 size={14}/>{t('common.edit')}</button><button className="danger" disabled={!!busy} title={t('common.delete')} onClick={()=>setDeleteCandidate(proxy)}><Trash2 size={14}/></button></div></article>)}</div>}
		{!showForm&&!proxies.length&&<Empty icon={<Cable/>} title={t('proxies.emptyTitle')}/>}
		{deleteCandidate&&<DestructiveConfirmDialog title={t('proxies.deleteTitle',{name:deleteCandidate.name})} busy={busy===`delete-${deleteCandidate.id}`} onCancel={()=>setDeleteCandidate(null)} onConfirm={()=>void remove()}/>}
	</div>
}

const emptyProviderForm: ModelProviderInput = {name:'',kind:'openai',base_url:'',model:'gpt-4o-mini',context_window:null,reasoning_effort:'',api_key:'',proxy_id:'',user_agent:''}
const providerLabels: Record<ModelProviderKind,string> = {
  openai: 'OpenAI', deepseek: 'DeepSeek', anthropic: 'Anthropic', openai_compatible: 'OpenAI-compatible', ollama: 'Ollama',
}
const providerDefaults: Record<ModelProviderKind,Pick<ModelProviderInput,'base_url'|'model'>> = {
  openai: {base_url:'',model:'gpt-4o-mini'},
  deepseek: {base_url:'https://api.deepseek.com',model:'deepseek-v4-flash'},
  anthropic: {base_url:'https://api.anthropic.com',model:'claude-opus-4-8'},
  openai_compatible: {base_url:'',model:''},
  ollama: {base_url:'http://127.0.0.1:11434/v1',model:''},
}

function ModelsPage({providers,proxies,showAddresses,onToggleAddresses,refresh}:{providers:ModelProvider[];proxies:Proxy[];showAddresses:boolean;onToggleAddresses:()=>void;refresh:()=>Promise<void>}) {
	const {t,i18n:instance}=useTranslation()
	const notify=useNotifier()
  const [showForm,setShowForm]=useState(false)
	  const [form,setForm]=useState<ModelProviderInput>(emptyProviderForm)
	  const [busy,setBusy]=useState('')
	  const [testing,setTesting]=useState<Set<string>>(()=>new Set())
		const [deleteCandidate,setDeleteCandidate]=useState<ModelProvider|null>(null)
  const [catalog,setCatalog]=useState<ModelCatalog|null>(null)
  const [discovering,setDiscovering]=useState(false)
	const [formErrors,setFormErrors]=useState<Partial<Record<'name'|'model'|'context_window',string>>>({})
  const editing=!!form.id

	const updateForm=<K extends keyof ModelProviderInput>(field:K,value:ModelProviderInput[K])=>{setForm(current=>({...current,[field]:value}));setFormErrors(current=>{if(!current[field as keyof typeof current])return current;const next={...current};delete next[field as keyof typeof next];return next})}
  const openCreate=()=>{setForm(emptyProviderForm);setCatalog(null);setFormErrors({});setShowForm(true)}
	const openEdit=(provider:ModelProvider)=>{setForm({id:provider.id,name:provider.name,kind:provider.kind,base_url:provider.base_url||'',model:provider.model,context_window:provider.context_window||null,reasoning_effort:provider.reasoning_effort||'',api_key:'',proxy_id:provider.proxy_id||'',user_agent:provider.user_agent||''});setCatalog(null);setFormErrors({});setShowForm(true)}
	const changeKind=(kind:ModelProviderKind)=>{setCatalog(null);setFormErrors({});setForm({...form,kind,context_window:null,...providerDefaults[kind]})}
	const discover=async()=>{setDiscovering(true);try{const {name:_name,model:_model,context_window:_contextWindow,reasoning_effort:_reasoningEffort,...payload}=form;const result=await api.discoverModels(payload);setCatalog(result);setForm(current=>({...current,model:result.models.includes(current.model)?current.model:''}));notify(t('models.found',{count:result.count}))}catch(err){setCatalog(null);notify(errorText(err),'error')}finally{setDiscovering(false)}}
	const selectedMetadata=catalog?.metadata?.[form.model]
		const setTestRunning=(key:string,running:boolean)=>setTesting(current=>{const next=new Set(current);if(running)next.add(key);else next.delete(key);return next})
		const testForm=async()=>{const key='form';setTestRunning(key,true);try{const {name:_name,context_window:_contextWindow,...payload}=form;const result=await api.testModelConfiguration(payload);notify(t('models.healthy',{name:result.model,response:result.response,latency:result.latency_ms}))}catch(err){notify(errorText(err),'error')}finally{setTestRunning(key,false)}}
	const validate=()=>{const errors:typeof formErrors={};if(!form.name.trim())errors.name=t('common.required');if(!form.model.trim())errors.model=t('common.required');if(form.context_window!==null&&(form.context_window<1024||form.context_window>10_000_000))errors.context_window=t('models.contextRange');setFormErrors(errors);return !Object.keys(errors).length}
	const save=async(event:FormEvent)=>{event.preventDefault();if(!validate())return;setBusy('save');try{const saved=await api.saveModelProvider({...form,name:form.name.trim(),model:form.model.trim(),context_window:form.context_window??0});notify(t('models.saved',{name:saved.name}));setShowForm(false);setForm(emptyProviderForm);setFormErrors({});await refresh()}catch(err){notify(errorText(err),'error')}finally{setBusy('')}}
	const activate=async(provider:ModelProvider)=>{setBusy(provider.id);try{await api.activateModelProvider(provider.id);notify(t('models.activated',{name:provider.name}));await refresh()}catch(err){notify(errorText(err),'error')}finally{setBusy('')}}
		const test=async(provider:ModelProvider)=>{setTestRunning(provider.id,true);try{const result=await api.testModelProvider(provider.id);notify(t('models.healthy',{name:provider.name,response:result.response,latency:result.latency_ms}))}catch(err){notify(errorText(err),'error')}finally{setTestRunning(provider.id,false)}}
	const remove=async()=>{const provider=deleteCandidate;if(!provider)return;setBusy(`delete-${provider.id}`);try{await api.deleteModelProvider(provider.id);notify(t('models.deleted',{name:provider.name}));await refresh()}catch(err){notify(errorText(err),'error')}finally{setBusy('');setDeleteCandidate(null)}}

  return <div className="page-stack">
	{!showForm&&<div className="page-actions"><div/><div className="page-action-buttons"><AddressVisibilityButton visible={showAddresses} onToggle={onToggleAddresses}/><button className="primary" onClick={openCreate}><Plus size={16}/>{t('models.add')}</button></div></div>}
    {showForm&&<ConfigurationEditorPage icon={<Cpu size={22}/>} title={editing?t('models.editTitle'):t('models.newTitle')} busy={!!busy} onBack={()=>setShowForm(false)}><form className="model-form configuration-editor-form panel" noValidate onSubmit={save}>
      <div className="form-grid model-fields">
		<label className={formErrors.name?'invalid':''}><span>{t('models.displayName')}</span><input value={form.name} aria-invalid={!!formErrors.name} onChange={event=>updateForm('name',event.target.value)}/>{formErrors.name&&<small className="form-field-error">{formErrors.name}</small>}</label>
		<label><span>{t('models.providerType')}</span><AppSelect value={form.kind} ariaLabel={t('models.providerType')} onChange={value=>changeKind(value as ModelProviderKind)} options={(Object.keys(providerLabels) as ModelProviderKind[]).map(kind=>({value:kind,label:providerLabels[kind]}))}/></label>
		<label className={`model-id-field ${formErrors.model?'invalid':''}`}><span className="field-title"><span>{t('models.modelId')}</span><button type="button" onClick={discover} disabled={discovering}><RefreshCw size={12}/>{discovering?t('models.fetching'):t('models.fetchModels')}</button></span><ModelCombobox value={form.model} models={catalog?.models||[]} metadata={catalog?.metadata} onChange={value=>updateForm('model',value)} placeholder={t('models.modelPlaceholder')} ariaLabel={t('models.modelId')} invalid={!!formErrors.model}/>{formErrors.model&&<small className="form-field-error">{formErrors.model}</small>}</label>
			<label><span>{t('models.reasoningEffort')}</span><AppSelect value={form.reasoning_effort} ariaLabel={t('models.reasoningEffort')} onChange={value=>updateForm('reasoning_effort',value as ModelProviderInput['reasoning_effort'])} options={[{value:'',label:t('models.default')},...(['low','medium','high','xhigh'] as const).map(value=>({value,label:value}))]}/></label>
			<label className={formErrors.context_window?'invalid':''}><span>{t('models.contextWindow')}</span><input inputMode="numeric" value={form.context_window??''} aria-invalid={!!formErrors.context_window} onChange={event=>{const value=event.target.value.replace(/\D/g,'').slice(0,8);updateForm('context_window',value?Number(value):null)}} placeholder={selectedMetadata?.context_window?compactTokenCount(selectedMetadata.context_window):t('models.contextAuto')}/>{formErrors.context_window&&<small className="form-field-error">{formErrors.context_window}</small>}</label>
			<label><span>{t('models.apiKey')}</span><PasswordInput autoComplete="new-password" value={form.api_key} onChange={event=>{setCatalog(null);setForm({...form,api_key:event.target.value})}} placeholder={editing?t('models.keepKey'):''}/></label>
			<label className="base-url-field"><span>{t('models.baseUrl')}</span><input value={form.base_url} onChange={event=>{setCatalog(null);setForm({...form,base_url:event.target.value})}} placeholder={form.kind==='openai'?t('models.officialEndpoint'):''}/></label>
			<label><span>{t('models.userAgent')}</span><input value={form.user_agent} onChange={event=>{setCatalog(null);setForm({...form,user_agent:event.target.value})}} placeholder={t('models.userAgentHint')}/></label>
			<label className="proxy-select-field"><span>{t('common.proxy')}</span><AppSelect value={form.proxy_id} ariaLabel={t('common.proxy')} onChange={value=>{setCatalog(null);updateForm('proxy_id',value)}} options={[{value:'',label:t('common.direct')},...proxies.map(proxy=>({value:proxy.id,label:`${proxy.name} · ${proxy.url}`}))]}/></label>
      </div>
		  <div className="form-actions"><button type="button" onClick={()=>setShowForm(false)}>{t('common.cancel')}</button><button type="button" className="test-config" onClick={testForm} disabled={!!busy||testing.has('form')||!form.model}><Activity size={14}/>{testing.has('form')?t('models.sendingHello'):t('models.testModel')}</button><button className="primary" disabled={!!busy}>{busy==='save'?t('common.saving'):t('models.saveProvider')}</button></div>
    </form></ConfigurationEditorPage>}
    {!showForm&&<div className="model-grid">{providers.map(provider=>{const proxy=proxies.find(item=>item.id===provider.proxy_id);const contextWindow=provider.context_window||provider.resolved_context_window||0;return <article className={`model-card panel ${provider.active?'active':''}`} key={provider.id}>
	  <div className="model-card-head"><div className="provider-glyph"><Cpu size={21}/></div><div><h3>{provider.name}</h3><span>{providerLabels[provider.kind]}</span></div>{provider.active&&<em><Zap size={12}/>{t('models.active')}</em>}</div>
      <div className="model-name">{provider.model}</div>
	  <dl><div><dt>{t('models.endpoint')}</dt><dd>{provider.base_url?(showAddresses?provider.base_url:'••••••'):t('models.providerDefault')}</dd></div><div><dt>{t('models.contextWindow')}</dt><dd>{contextWindow?compactTokenCount(contextWindow):t('models.contextAuto')}</dd></div><div><dt>{t('models.proxy')}</dt><dd title={showAddresses?proxy?.url:undefined}>{proxy?.name||t('models.noProxy')}</dd></div>{provider.user_agent&&<div><dt>{t('models.userAgent')}</dt><dd>{provider.user_agent}</dd></div>}<div><dt>{t('models.credential')}</dt><dd>{provider.has_api_key?t('models.encryptedKey'):t('models.noApiKey')}</dd></div><div><dt>{t('common.updated')}</dt><dd>{new Date(provider.updated_at).toLocaleString(localeFor(instance.language))}</dd></div></dl>
		  <div className="model-actions"><button onClick={()=>test(provider)} disabled={!!busy||testing.has(provider.id)}><Activity size={14}/>{testing.has(provider.id)?t('common.testing'):t('common.test')}</button><button onClick={()=>openEdit(provider)} disabled={!!busy}><Edit3 size={14}/>{t('common.edit')}</button>{!provider.active&&<button className="use-model" onClick={()=>activate(provider)} disabled={!!busy}><Zap size={14}/>{busy===provider.id?t('models.switching'):t('models.useModel')}</button>}<button className="danger" title={t('common.delete')} onClick={()=>setDeleteCandidate(provider)} disabled={!!busy}><Trash2 size={14}/></button></div>
    </article>})}</div>}
		{!showForm&&!providers.length&&<Empty icon={<Cpu/>} title={t('models.emptyTitle')}/>}
		{deleteCandidate&&<DestructiveConfirmDialog
			title={t('models.deleteTitle',{name:deleteCandidate.name})}
			busy={busy===`delete-${deleteCandidate.id}`}
			onCancel={()=>setDeleteCandidate(null)}
			onConfirm={()=>void remove()}
		/>}
  </div>
}

function AuditRunDetail({run,req,hosts}:{run:Run;req:JsonRecord;hosts:Host[]}){
	const {t}=useTranslation()
	const script=textValue(req.script)
	const program=textValue(req.program)?fullProgram(req):''
	const mode=textValue(req.mode)
	const workspaceID=textValue(req.workspace_id)
	const remotePath=textValue(req.remote_path)
	const relativePath=textValue(req.relative_path)
	const filePath=remotePath||relativePath
	const destinationHost=hostIdentity(hosts,run.host_id)
	const sourceHost=hostIdentity(hosts,textValue(req.source_host_id))
	const sourcePath=textValue(req.source_path)
	const change=jsonRecord(req.change)
	const env=jsonRecord(req.env)
	const workspaceShellBackend=textValue(req.workspace_shell_backend)
	const searchMode=mode==='remote_search'||mode==='workspace_search'
	const readMode=mode==='remote_read'||mode==='workspace_read'
	const workspaceUpload=mode==='workspace_upload'
	const workspaceDownload=mode==='workspace_download'
	const workspaceTransfer=workspaceUpload||workspaceDownload
	const sshTransfer=mode==='ssh_file_transfer'
	const tunnelMode=mode==='ssh_tunnel_start'
	const shellMode=mode==='ssh_shell_start'||mode==='workspace_shell_start'
	const shellModeLabel=mode==='ssh_shell_start'?`SSH Shell · ${t('sshShell.toolActions.start')}`:`Workspace Shell · ${t('sshShell.toolActions.start')}`
	const tunnelRoute=tunnelMode?sshTunnelRoute(destinationHost.name||destinationHost.id,textValue(req.remote_host),numberValue(req.remote_port),numberValue(req.local_port),t('tunnels.automaticPort')):''
	const shellTarget=`${mode==='workspace_shell_start'?`${workspaceID}:${textValue(req.cwd)||'.'}`:destinationHost.name||destinationHost.id} · PTY`
	const fileTarget=`${workspaceID?`${workspaceID}:`:''}${filePath}`
	const commandText=shellMode?shellTarget:tunnelMode?tunnelRoute:workspaceUpload?`workspace_upload ${workspaceID}:${relativePath} → ${destinationHost.name||destinationHost.id}:${remotePath}`:workspaceDownload?`workspace_download ${destinationHost.name||destinationHost.id}:${remotePath} → ${workspaceID}:${relativePath}`:sshTransfer?`${[sourceHost.name||sourceHost.id,sourcePath].filter(Boolean).join(':')} → ${destinationHost.name||destinationHost.id}:${remotePath}`:searchMode||readMode?`${searchMode?'search':'read'} ${fileTarget}`:script?script:program?program:filePath?`${mode} ${fileTarget}`:JSON.stringify(req,null,2)
	return <div className="audit-run-detail">
		<div className="audit-run-primary">
			<section className="audit-operation-pane">
				<div className="tool-command-head"><span>{shellMode?shellModeLabel:tunnelMode?t('tunnels.forwarding'):searchMode?t('tool.searchOperation'):readMode?t('tool.readOperation'):workspaceTransfer||sshTransfer||filePath?t('tool.fileOperation'):script?t('tool.fullScript'):t('tool.fullCommand')}</span>{(workspaceShellBackend||req.elevated===true)&&<div className="audit-operation-badges">{workspaceShellBackend&&<em><TerminalSquare size={12}/>{workspaceShellBackend==='host'?t('approval.hostShell'):'Bubblewrap'}</em>}{req.elevated===true&&<em><ShieldAlert size={12}/>sudo / root</em>}</div>}</div>
				<div className="tool-command-block"><CopyButton value={commandText}/><pre>{program&&commandText===program?<><span className="prompt-sign">$</span> {program}</>:commandText}</pre></div>
				{change&&textValue(change.diff)&&<DiffViewer change={change}/>}
			</section>
			<aside className="audit-run-context">
				<dl className="audit-run-facts">
					<div><dt>{workspaceID&&!sshTransfer?t('common.workspace'):t('tool.targetHost')}</dt><dd>{workspaceID&&!sshTransfer?workspaceID:[destinationHost.name,destinationHost.id].filter(Boolean).join(' · ')||'—'}</dd></div>
					{sshTransfer&&<div><dt>{t('tool.sourceHost')}</dt><dd>{[sourceHost.name,sourceHost.id].filter(Boolean).join(' · ')||'—'}</dd></div>}
					<div><dt>{tunnelMode?t('tunnels.remoteEndpoint'):filePath?t('tool.filePath'):t('tool.workingDirectory')}</dt><dd>{tunnelMode?`${textValue(req.remote_host)}:${numberValue(req.remote_port)}`:filePath||textValue(req.cwd)||t('tool.defaultDirectory')}</dd></div>
					<div><dt>{t('tool.permission')}</dt><dd>{workspaceShellBackend==='host'?t('tool.hostAuthority'):workspaceShellBackend==='sandbox'?t('tool.sandbox'):req.elevated===true?t('tool.managedSudo'):t('tool.normalUser')}</dd></div>
					<div><dt>{t('tool.duration')}</dt><dd>{formatDuration(undefined,run)}</dd></div>
				</dl>
				{textValue(req.reason)&&<div className="audit-run-purpose"><span>{t('tool.reason')}</span><p>{textValue(req.reason)}</p></div>}
			</aside>
		</div>
		{(run.stdout_redacted||run.stderr_redacted||run.error)&&<div className="tool-output-grid">{run.stdout_redacted&&<ToolOutputPanel kind="stdout" label="STDOUT · REDACTED" content={run.stdout_redacted} live={false}/>} {run.stderr_redacted&&<ToolOutputPanel kind="stderr" label="STDERR · REDACTED" content={run.stderr_redacted} live={false}/>} {run.error&&!run.stderr_redacted&&<ToolOutputPanel kind="stderr" label={t('common.error')} content={run.error} live={false}/>}</div>}
		<details className="audit-request-detail">
			<summary><Braces size={14}/><span>{t('tool.normalizedRequest')}</span><ChevronRight size={14}/></summary>
			<div className="audit-request-detail-body">
				<dl className="audit-request-meta"><div><dt>{t('common.operation')}</dt><dd>{toolLabel(run.tool_name||'')}</dd></div><div><dt>{t('tool.runId')}</dt><dd>{run.id}</dd></div></dl>
				{env&&Object.keys(env).length>0&&<CompactTable title={t('tool.environment')} columns={[t('tool.key'),t('tool.value')]} rows={Object.entries(env).map(([key,value])=>[key,String(value)])}/>}
				<CopyablePre>{JSON.stringify(req,null,2)}</CopyablePre>
			</div>
		</details>
	</div>
}

function auditOperationSummary(req:JsonRecord,run:Run,hosts:Host[],t:TFunction){
	const mode=textValue(req.mode)
	const destinationHost=hostIdentity(hosts,run.host_id)
	const destinationName=destinationHost.name||destinationHost.id
	const workspaceID=textValue(req.workspace_id)
	const relativePath=textValue(req.relative_path)
	const remotePath=textValue(req.remote_path)
	const sourceHost=hostIdentity(hosts,textValue(req.source_host_id))
	switch(mode){
		case'program':return fullProgram(req)
		case'script':return `${t('toolNames.ssh_run_script')} · ${compactScript(textValue(req.script))}`
		case'ssh_shell_start':return `SSH Shell · ${t('sshShell.toolActions.start')}`
		case'workspace_shell_start':return `Workspace Shell · ${t('sshShell.toolActions.start')} · ${workspaceID}:${textValue(req.cwd)||'.'}`
		case'ssh_tunnel_start':return sshTunnelRoute(destinationName,textValue(req.remote_host),numberValue(req.remote_port),numberValue(req.local_port),t('tunnels.automaticPort'))
		case'remote_read':return `${t('toolNames.ssh_file_read')} · ${remotePath}`
		case'remote_search':return `${t('toolNames.ssh_file_search_mode')} · ${remotePath} · ${textValue(req.search_pattern)}`
		case'remote_edit':return `${t('toolNames.ssh_file_edit')} · ${remotePath}`
		case'workspace_read':return `${t('toolNames.workspace_file_read')} · ${workspaceID}:${relativePath}`
		case'workspace_search':return `${t('toolNames.workspace_file_search_mode')} · ${workspaceID}:${relativePath} · ${textValue(req.search_pattern)}`
		case'workspace_edit':return `${t('toolNames.workspace_file_edit')} · ${workspaceID}:${relativePath}`
		case'workspace_delete':return `${t('toolNames.workspace_file_delete')} · ${workspaceID}:${relativePath}`
		case'workspace_directory_list':return `${t('toolNames.workspace_file_list')} · ${workspaceID}:${relativePath}`
		case'workspace_shell':return `${t('toolNames.workspace_shell')} · ${compactScript(textValue(req.script))}`
		case'workspace_upload':return `${t('toolNames.workspace_file_upload')} · ${workspaceID}:${relativePath} → ${destinationName}:${remotePath}`
		case'workspace_download':return `${t('toolNames.workspace_file_download')} · ${destinationName}:${remotePath} → ${workspaceID}:${relativePath}`
		case'ssh_file_transfer':return `${t('toolNames.ssh_file_transfer')} · ${sourceHost.name||sourceHost.id}:${textValue(req.source_path)} → ${destinationName}:${remotePath}`
		default:return mode||t('audit.unknownOperation')
	}
}

function AuditPage({runs,hosts}:{runs:Run[];hosts:Host[]}) {
	const {t,i18n:instance}=useTranslation()
  const [query,setQuery]=useState('')
  const [sessions,setSessions]=useState<ChatSession[]>([])
  useEffect(()=>{let active=true;void api.chatSessions().then(items=>{if(active)setSessions(items)}).catch(()=>{});return()=>{active=false}},[])
  const filtered=useMemo(()=>{const needle=query.toLowerCase();return runs.filter(run=>{const req=requestFromRun(run);const requestText=req?Object.values(req).flat().filter(value=>typeof value==='string').join('\n'):run.request_json;return(requestText+run.stdout_redacted+run.stderr_redacted).toLowerCase().includes(needle)})},[query,runs])
  const groups=useMemo(()=>{
    const titles=new Map(sessions.map(session=>[session.id,session.title]))
    const grouped=new Map<string,Run[]>()
    for(const run of filtered){const key=run.session_id||'__direct__';grouped.set(key,[...(grouped.get(key)||[]),run])}
	return [...grouped.entries()].map(([id,items])=>{items.sort((a,b)=>Date.parse(b.started_at)-Date.parse(a.started_at));return{id,title:id==='__direct__'?t('audit.direct'):titles.get(id)||t('audit.missingConversation'),runs:items,latest:items[0]?.started_at,pending:items.filter(run=>run.status==='approval_required').length}}).sort((a,b)=>Date.parse(b.latest||'')-Date.parse(a.latest||''))
	},[filtered,sessions,t,instance.language])
	return <div className="page-stack"><div className="audit-toolbar"><div className="search-box"><Search size={16}/><input aria-label={t('common.search')} value={query} onChange={event=>setQuery(event.target.value)}/></div><span>{t('audit.counts',{sessions:groups.length,runs:filtered.length})}</span></div><div className="audit-groups">{groups.map(group=><details className="audit-session panel" key={group.id}><summary className="audit-session-summary"><div className="audit-session-glyph"><History size={17}/></div><div className="audit-session-name"><b>{group.title}</b><span>{group.id==='__direct__'?t('audit.noSession'):group.id} · {t('audit.lastRun',{date:new Date(group.latest).toLocaleString(localeFor(instance.language))})}</span></div><div className="audit-session-stats"><span><b>{group.runs.length}</b> {t('audit.runs')}</span>{group.pending>0&&<span className="pending-count"><b>{group.pending}</b> {t('audit.pending')}</span>}</div><ChevronRight className="audit-session-chevron" size={17}/></summary><div className="audit-table"><div className="audit-row audit-head"><span>{t('audit.columns.time')}</span><span>{t('audit.columns.operation')}</span><span>{t('audit.columns.status')}</span><span>{t('audit.columns.host')}</span><span>{t('audit.columns.exit')}</span><span aria-hidden="true"/></div>{group.runs.map(run=>{let req:Record<string,unknown>={};try{req=JSON.parse(run.request_json)}catch{req={request:run.request_json}};const auditHost=hostIdentity(hosts,run.host_id);const workspaceID=textValue(req.workspace_id);const target=auditHost.name||(run.host_id.startsWith('workspace_')?workspaceID:run.host_id)||'—';const operation=auditOperationSummary(req,run,hosts,t);return <details key={run.id}><summary className="audit-row"><span>{new Date(run.started_at).toLocaleString(localeFor(instance.language))}</span><span className="command">{operation}</span><span className="audit-run-status"><span className={`run-status ${run.status}`}>{t(`statusLabels.${run.status}`,{defaultValue:run.status})}</span>{runAutoApproved(run)&&<span className="auto-approved"><ShieldCheck size={11}/>{t('approval.autoApproved')}</span>}</span><span title={run.host_id}>{target}</span><span>{run.exit_code}</span><ChevronRight className="audit-run-chevron" size={15}/></summary><div className="run-detail"><AuditRunDetail run={run} req={req} hosts={hosts}/></div></details>})}</div></details>)}</div>{!runs.length&&<Empty icon={<History/>} title={t('audit.emptyTitle')}/>} {runs.length>0&&!groups.length&&<Empty icon={<Search/>} title={t('audit.noMatch')}/>}</div>
}

function logFieldValue(value:unknown){
  if(value===null||value===undefined)return'—'
  if(typeof value==='object')return JSON.stringify(value)
  return String(value)
}

function LogsPage(){
	const {t,i18n:instance}=useTranslation()
  const [entries,setEntries]=useState<ServerLogEntry[]>([])
  const [components,setComponents]=useState<string[]>([])
  const [minimumLevel,setMinimumLevel]=useState('debug')
  const [logFile,setLogFile]=useState('')
  const [level,setLevel]=useState('debug')
  const [component,setComponent]=useState('')
  const [query,setQuery]=useState('')
  const [live,setLive]=useState(true)
  const [loading,setLoading]=useState(false)
  const [logError,setLogError]=useState('')
  const refreshLogs=useCallback(async(silent=false)=>{
    if(!silent)setLoading(true)
    try{const result=await api.logs({level,component,q:query,limit:500});setEntries(result.entries||[]);setComponents(result.components||[]);setMinimumLevel(result.minimum_level||'debug');setLogFile(result.file||'');setLogError('')}
    catch(err){setLogError(errorText(err))}
    finally{if(!silent)setLoading(false)}
  },[level,component,query])
  useEffect(()=>{void refreshLogs();if(!live)return;const timer=window.setInterval(()=>void refreshLogs(true),3000);return()=>window.clearInterval(timer)},[refreshLogs,live])
  return <div className="logs-page page-stack">
    <div className="logs-toolbar panel">
	  <div className="search-box"><Search size={16}/><input value={query} onChange={event=>setQuery(event.target.value)} placeholder={t('logs.search')}/></div>
	  <label><span>{t('logs.minimumLevel')}</span><AppSelect value={level} ariaLabel={t('logs.minimumLevel')} onChange={setLevel} options={['debug','info','warn','error'].map(value=>({value,label:value==='error'?'Error':`${value[0].toUpperCase()}${value.slice(1)}+`}))}/></label>
	  <label><span>{t('logs.component')}</span><AppSelect value={component} ariaLabel={t('logs.component')} onChange={setComponent} options={[{value:'',label:t('logs.allComponents')},...components.map(value=>({value,label:value}))]}/></label>
	  <button className={`live-toggle ${live?'active':''}`} onClick={()=>setLive(value=>!value)}><CircleDot size={13}/>{live?t('logs.live'):t('logs.paused')}</button>
	  <button className="log-refresh" onClick={()=>void refreshLogs()} disabled={loading}><RefreshCw size={14} className={loading?'spin':''}/>{loading?t('common.loading'):t('common.refresh')}</button>
	  <a className="log-export" href="/api/v1/logs/export" download><Download size={14}/>{t('logs.export')}</a>
    </div>
	<div className="logs-meta"><span>{t('logs.entries',{count:entries.length})}</span><span>{logFile?t('logs.file',{file:logFile}):t('logs.fileDisabled')}</span></div>
	{minimumLevel!=='debug'&&level==='debug'&&<div className="log-hint"><ShieldAlert size={15}/><span>{t('logs.debugHint')}</span></div>}
    {logError&&<div className="history-error panel">{logError}</div>}
    <div className="log-stream panel">
	  <div className="log-row log-head"><span>{t('logs.columns.time')}</span><span>{t('logs.columns.level')}</span><span>{t('logs.columns.component')}</span><span>{t('logs.columns.event')}</span></div>
	  {entries.map((entry,index)=><div className={`log-row log-entry ${entry.level}`} key={`${entry.time}_${index}`}><time>{new Date(entry.time).toLocaleTimeString(localeFor(instance.language),{hour12:false,fractionalSecondDigits:3})}</time><span><i className={`log-level ${entry.level}`}>{entry.level}</i></span><code className="log-component">{entry.component||t('logs.general')}</code><div className="log-event"><b>{entry.message}</b>{entry.fields&&Object.keys(entry.fields).length>0&&<div className="log-fields">{Object.entries(entry.fields).map(([key,value])=><span key={key}><em>{key}</em><code title={logFieldValue(value)}>{logFieldValue(value)}</code></span>)}</div>}</div></div>)}
		  {!entries.length&&!logError&&<Empty icon={<FileText/>} title={t('logs.emptyTitle')}/>}
    </div>
  </div>
}

function Metric({label,value,tone}:{label:string;value:string;tone?:string}){return <div className={`metric ${tone||''}`}><span>{label}</span><b>{value}</b></div>}
function Empty({icon,title,text}:{icon:React.ReactNode;title:string;text?:string}){return <div className="empty-state"><div>{icon}</div><h2>{title}</h2>{text&&<p>{text}</p>}</div>}
function pretty(value:string){try{return JSON.stringify(JSON.parse(value),null,2)}catch{return value}}

export default App
