import { FormEvent, Suspense, createContext, lazy, memo, useCallback, useContext, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { createPortal, flushSync } from 'react-dom'
import { useTranslation } from 'react-i18next'
import type { TFunction } from 'i18next'
import { getCurrentWindow } from '@tauri-apps/api/window'
import { Activity, BookOpen, Bot, BrainCircuit, Braces, Check, ChevronRight, CircleDot,  Edit3, ExternalLink, FileText, FolderOpen, FunctionSquare, History, ImagePlus, Maximize2,  Minus, Monitor, Moon, PanelLeftOpen, Sun, ListChecks, ListPlus, LoaderCircle, LogOut, Plus, RefreshCw, Save, Search, Send, Server, Settings2, ShieldAlert, ShieldCheck, Square, TerminalSquare, Trash2, UserRound, X, Zap } from 'lucide-react'
import { api, reconnectChatStream, streamChat } from './api/api'
import { subscribeApplicationEvents } from './api/appEvents'
import { CopyButton, CopyablePre } from './components/CopyButton'
import { HighlightedCode, inferScriptLanguage, languageFromPath } from './components/HighlightedCode'
import i18n, { localeFor, type SupportedLanguage } from './lib/i18n'
import { activeLiveTaskStatus, useLiveSSHTasks, type LiveSSHTaskSnapshot, type LiveSSHTaskTarget } from './lib/liveTasks'
import { streamTextTail, type StreamText } from './api/streamText'
import { PasswordInput } from './components/PasswordInput'
import { SSHShellStatus, SSHShellTerminal, SSHTunnelStatus, sshShellActive } from './features/ssh'
import { ChatWorkspacePanel, SSHWorkspacePage } from './features/workspace'
import { useNotifier, NotificationContext, type NotificationSink, type AppNotification } from './lib/notifications'
import { DestructiveConfirmDialog } from './components/DestructiveConfirmDialog'
import { FileTransferProvider } from './features/sftp'
import { auditSessionID, directAuditSessionID, useAuditData, useAuditGroupDisclosure, type AuditView } from './features/audit'
import { useChatCardDisclosure, type ChatDisclosurePositionHandler } from './features/chat'
import {  useDocumentVisible } from './lib/hooks'
import { desktopRuntime, errorStatus, errorText, formatFileSize, sshTunnelRoute, clientId, compactTokenCount } from './lib/utils'
import type { AgentEvent, AgentTask, AgentTaskList, Approval, ApprovalExecutionResult,  AuditRunDeleteResult, AuthStatus, ChatQueueMode, ChatSession, ChatSessionDelta, ChatState, ChatTokenUsage, CommandReview, Health, Host, LLMToolCatalog, ManagedSkill, MCPActivityEvent, MCPActivitySnapshot, MCPClientSession, MCPServer, MCPToolCall, ModelProvider,  Proxy, QueuedChatMessage, Run, SSHShell, SSHTunnel, SystemSettings, ToolCapabilities } from './types'
import { ChatActivityStatus } from './features/chat/ChatActivityStatus'
import { ComposerControls } from './features/chat/ComposerControls'
import type { ContextUsage } from './features/chat/types'
import { insertQueuedMessage, queuedMessageEntries, historyEntries, deactivateReasoning, toolContentStatus, settledTurnEntries, prependHistoryEntries, mergePersistedToolEntries, updateToolRunStatus, agentFrameAffectsEntries, reduceAgentEntryFrames } from './features/chat/chatEntries'
import { tasksFromToolContent, groupedTaskToolEntries, latestLiveSSHTaskTargets } from './features/chat/taskEntries'
import { type JsonRecord, toolOutputPreviewChars, toolDiffPreviewChars, toolCollectionPreviewItems, previewText, jsonRecord, limitedRecordEntries, hasRecordEntries, previewStructuredValue, parseRecord, textValue } from './features/tools/payload'
import { Empty } from './components/PageLayout'
import { defaultChatImageTypes } from './features/settings/defaults'
import type { PendingChatImage, ChatEntry, TaskToolEntryGroup, ChatRenderItem, ModelRetryState, ConnectionRetryState } from './features/chat/types'
import '@xterm/xterm/css/xterm.css'
const ConfigurationPage=lazy(()=>import('./features/settings/ConfigurationPage').then(module=>({default:module.ConfigurationPage})))
const ExtensionsPage=lazy(()=>import('./features/extensions/ExtensionsPage').then(module=>({default:module.ExtensionsPage})))
const LogsPage=lazy(()=>import('./features/logs/LogsPage').then(module=>({default:module.LogsPage})))

type Page = 'chat' | 'ssh' | 'config' | 'extensions' | 'audit' | 'logs'
const pageVisualOrder:Page[]=['chat','ssh','extensions','audit','logs','config']
const emptyChatEntries:ChatEntry[]=[]
const emptyLiveSSHTaskTargets:readonly LiveSSHTaskTarget[]=[]
const emptyChatRenderItems:ChatRenderItem[]=[]
type ActiveChatStream = { id: string; sessionId: string; controller: AbortController }
type ChatHistoryCursor = {createdAt:string;id:string}
const MarkdownMessage=lazy(()=>import('./components/MarkdownMessage').then(module=>({default:module.MarkdownMessage})))

function newChatSessionID(){return `session_${clientId().replace(/[^A-Za-z0-9]/g,'')}`}
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

function contextWindowForSession(tokens:number,window:number,fallback:number){
	return tokens>0?window:(window||fallback)
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

function keepEquivalent<T>(current:T,next:T){return JSON.stringify(current)===JSON.stringify(next)?current:next}
function applyLifecycleDelta<T extends{id:string}>(current:T[],value:T,removed=false){
	const index=current.findIndex(item=>item.id===value.id)
	if(removed)return index<0?current:current.filter(item=>item.id!==value.id)
	if(index<0)return[...current,value]
	const next=[...current];next[index]=value
	return keepEquivalent(current,next)
}
function keepEquivalentHealth(current:Health|null,next:Health){
	if(!current)return next
	const currentState=[current.status,current.agent_available,current.model]
	const nextState=[next.status,next.agent_available,next.model]
	return JSON.stringify(currentState)===JSON.stringify(nextState)?current:next
}

const newSessionMarker = '__new__'
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

const ChatVisibilityContext=createContext(true)

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
	const [auth,setAuth]=useState<AuthStatus|null>(null)
	const [loading,setLoading]=useState(true)
	const [error,setError]=useState('')
	const refresh=useCallback(async()=>{setLoading(true);setError('');try{setAuth(await api.authStatus())}catch(err){setError(errorText(err))}finally{setLoading(false)}},[])
	useEffect(()=>{void refresh()},[refresh])
	useEffect(()=>{const unauthorized=()=>setAuth(current=>current?.enabled?{enabled:true,authenticated:false}:current);window.addEventListener('opsnerva:unauthorized',unauthorized);return()=>window.removeEventListener('opsnerva:unauthorized',unauthorized)},[])
	if(loading)return <AppFrame><div className="auth-screen"><LoaderCircle className="spin" size={24}/></div></AppFrame>
	if(error&&!auth)return <AppFrame><div className="auth-screen"><section className="auth-card panel"><ShieldAlert size={24}/><div className="auth-error" role="alert">{error}</div><button className="primary" onClick={()=>void refresh()}>{t('common.retry')}</button></section></div></AppFrame>
	if(auth?.enabled&&!auth.authenticated)return <LoginPage onAuthenticated={setAuth}/>
	return <Application auth={auth||{enabled:false,authenticated:true}} onLogout={()=>setAuth({enabled:true,authenticated:false})}/>
}

function LoginPage({onAuthenticated}:{onAuthenticated:(status:AuthStatus)=>void}){
	const {t}=useTranslation()
	const [username,setUsername]=useState('')
	const [password,setPassword]=useState('')
	const [busy,setBusy]=useState(false)
	const [error,setError]=useState('')
	const login=async(event:FormEvent)=>{event.preventDefault();if(!username.trim()||!password)return;setBusy(true);setError('');try{onAuthenticated(await api.login(username,password));setPassword('')}catch(err){setError(errorText(err))}finally{setBusy(false)}}
	return <AppFrame><div className="auth-screen"><div className="auth-language"><LanguageSwitch/></div><form className="auth-card panel" onSubmit={login}><header><div className="brand-mark"><TerminalSquare size={22}/></div><h1>OpsNerva</h1></header><label><span>{t('auth.username')}</span><input autoFocus autoComplete="username" value={username} onChange={event=>setUsername(event.target.value)}/></label><label><span>{t('auth.password')}</span><PasswordInput autoComplete="current-password" value={password} onChange={event=>setPassword(event.target.value)}/></label>{error&&<div className="auth-error" role="alert"><ShieldAlert size={14}/>{error}</div>}<button className="primary" disabled={busy||!username.trim()||!password}>{busy?<LoaderCircle className="spin" size={15}/>:<LogOut className="auth-login-icon" size={15}/>} {busy?t('auth.signingIn'):t('auth.signIn')}</button></form></div></AppFrame>
}

function Application({auth,onLogout}:{auth:AuthStatus;onLogout:()=>void}) {
	const {t}=useTranslation()
  const [page, setPage] = useState<Page>('chat')
	const workspaceRef=useRef<HTMLElement>(null)
	const pageTitleRef=useRef<HTMLHeadingElement>(null)
	const previousPageRef=useRef<Page>(page)
	const pageTransitionRef=useRef(false)
	const pageDirectionRef=useRef(1)
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
	const [auditView,setAuditView]=useState<AuditView>('runs')
	const [mcpActivityRefresh,setMCPActivityRefresh]=useState(0)
  const [sshTunnels,setSSHTunnels]=useState<SSHTunnel[]>([])
  const [sshShells,setSSHShells]=useState<SSHShell[]>([])
  const [selectedShell,setSelectedShell]=useState<SSHShell|null>(null)
  const [openConnectionPanel,setOpenConnectionPanel]=useState<'tunnel'|'shell'|null>(null)
	const [notifications,setNotifications]=useState<AppNotification[]>([])
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
			if(tunnels.status==='fulfilled')setSSHTunnels(current=>keepEquivalent(current,tunnels.value.tunnels||[]))
			if(shells.status==='fulfilled')setSSHShells(current=>keepEquivalent(current,shells.value.shells||[]))
		})
		connectionRefreshRef.current=task
		void task.finally(()=>{if(connectionRefreshRef.current===task)connectionRefreshRef.current=null})
		return task
	},[])

	const refreshToolCatalog=useCallback(async()=>{
		try{setToolCatalog(await api.llmTools())}
		catch(err){notify(errorText(err),'error')}
	},[notify])
	const refreshSkills=useCallback(async()=>{
		try{setSkills(await api.skills())}
		catch(err){notify(errorText(err),'error')}
	},[notify])
	const refreshMCPServers=useCallback(async()=>{
		try{setMCPServers(await api.mcpServers())}
		catch(err){notify(errorText(err),'error')}
	},[notify])
	const refreshExtensions=useCallback(async()=>{await Promise.all([refreshToolCatalog(),refreshSkills(),refreshMCPServers()])},[refreshMCPServers,refreshSkills,refreshToolCatalog])
	const refreshHealth=useCallback(async()=>{
		try{const next=await api.health();setHealth(current=>keepEquivalentHealth(current,next))}
		catch(err){notify(errorText(err),'error')}
	},[notify])
	const refreshHosts=useCallback(async()=>{
		try{setHosts(await api.hosts())}
		catch(err){notify(errorText(err),'error')}
	},[notify])
	const refreshProviders=useCallback(async()=>{
		try{setProviders(await api.modelProviders())}
		catch(err){notify(errorText(err),'error')}
	},[notify])
	const refreshModels=useCallback(async()=>{await Promise.all([refreshProviders(),refreshHealth()])},[refreshHealth,refreshProviders])
	const refreshProxies=useCallback(async()=>{
		try{setProxies(await api.proxies())}
		catch(err){notify(errorText(err),'error')}
	},[notify])
	const refreshSettings=useCallback(async()=>{
		try{setSettings(await api.systemSettings())}
		catch(err){notify(errorText(err),'error')}
	},[notify])
	const refreshCapabilities=useCallback(async()=>{
		try{setCapabilities(await api.capabilities())}
		catch(err){notify(errorText(err),'error')}
	},[notify])
	const audit=useAuditData({active:page==='audit'&&auditView==='runs',refreshHosts,notify})
	const dismissApproval=useCallback((approvalID:string)=>{
		setApprovals(current=>current.filter(item=>item.id!==approvalID))
	},[])
	const refreshBootstrap=useCallback(async()=>{
		await Promise.all([refreshHealth(),refreshHosts(),refreshProviders(),refreshSettings(),refreshCapabilities()])
	},[refreshCapabilities,refreshHealth,refreshHosts,refreshProviders,refreshSettings])
	const refreshConfiguration=useCallback(async()=>{
		await Promise.all([refreshHealth(),refreshHosts(),refreshProviders(),refreshProxies(),refreshSettings(),refreshCapabilities()])
	},[refreshCapabilities,refreshHealth,refreshHosts,refreshProviders,refreshProxies,refreshSettings])
	const refreshChat=useCallback(async()=>{
		await Promise.all([refreshHealth(),refreshHosts(),refreshProviders(),refreshSettings(),refreshCapabilities()])
	},[refreshCapabilities,refreshHealth,refreshHosts,refreshProviders,refreshSettings])
	const removeSessionState=useCallback((sessionID:string)=>{
		setApprovals(current=>current.filter(item=>item.session_id!==sessionID))
		setSSHShells(current=>current.filter(item=>item.session_id!==sessionID))
	},[])

	useEffect(()=>{
		if(!desktopRuntime)return
		const handleContextMenu=(event:MouseEvent)=>{
			const target=event.target instanceof Element?event.target:null
			if(target?.closest('input, textarea, [contenteditable="true"], .xterm'))return
			event.preventDefault()
		}
		document.addEventListener('contextmenu',handleContextMenu)
		return()=>document.removeEventListener('contextmenu',handleContextMenu)
	},[])
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
		const sync=()=>{document.documentElement.dataset.windowActive=document.hasFocus()&&document.visibilityState==='visible'?'true':'false'}
		sync();window.addEventListener('focus',sync);window.addEventListener('blur',sync);document.addEventListener('visibilitychange',sync)
		return()=>{window.removeEventListener('focus',sync);window.removeEventListener('blur',sync);document.removeEventListener('visibilitychange',sync)}
	},[])
	useEffect(() => { void refreshBootstrap();void refreshConnections() }, [refreshBootstrap,refreshConnections])
	useEffect(()=>subscribeApplicationEvents<{tunnels?:SSHTunnel[];shells?:SSHShell[];tunnel?:SSHTunnel;shell?:SSHShell;removed?:boolean}>('connections',event=>{
		if(event.type!=='event'||!event.data)return
		if(event.mode==='delta'){
			if(event.data.tunnel)setSSHTunnels(current=>applyLifecycleDelta(current,event.data!.tunnel!,event.data!.removed))
			if(event.data.shell)setSSHShells(current=>applyLifecycleDelta(current,event.data!.shell!,event.data!.removed))
			return
		}
		setSSHTunnels(current=>keepEquivalent(current,event.data!.tunnels||[]))
		setSSHShells(current=>keepEquivalent(current,event.data!.shells||[]))
	}),[])
	useEffect(()=>subscribeApplicationEvents<Approval[]>('approvals',event=>{
		if(event.type==='error'&&event.error){notify(event.error,'error');return}
		if(event.type==='event'&&event.data)setApprovals(current=>keepEquivalent(current,event.data!))
	}),[notify])
	useEffect(()=>subscribeApplicationEvents<Health>('health',event=>{
		if(event.type==='event'&&event.data)setHealth(current=>keepEquivalentHealth(current,event.data!))
	}),[])
	useEffect(()=>{
		if(page==='extensions')void refreshExtensions()
		else if(page==='ssh')void Promise.all([refreshHosts(),refreshConnections()])
		else if(page==='config')void refreshConfiguration()
	},[page,refreshConfiguration,refreshConnections,refreshExtensions,refreshHosts])
	useLayoutEffect(()=>{
		const previous=previousPageRef.current
		previousPageRef.current=page
		if(previous===page)return
		if(pageTransitionRef.current){pageTransitionRef.current=false;return}
		if(window.matchMedia('(prefers-reduced-motion: reduce)').matches)return
		workspaceRef.current?.animate([
			{opacity:.28,transform:`translate3d(${pageDirectionRef.current*12}px,4px,0)`,filter:'blur(2px)'},
			{opacity:1,transform:'translate3d(0,0,0)',filter:'blur(0)'},
		],{duration:240,easing:'cubic-bezier(.2,.8,.2,1)'})
		pageTitleRef.current?.animate([
			{opacity:0,transform:`translateX(${pageDirectionRef.current*5}px)`},
			{opacity:1,transform:'translateX(0)'},
		],{duration:180,easing:'cubic-bezier(.2,.8,.2,1)'})
	},[page])
	const navigate=useCallback((next:Page)=>{
		if(next===page)return
		const reduced=window.matchMedia('(prefers-reduced-motion: reduce)').matches
		const transition=(document as Document&{startViewTransition?:(update:()=>void)=>unknown}).startViewTransition
		const extensionEdge=page==='extensions'||next==='extensions'
		pageDirectionRef.current=Math.sign(pageVisualOrder.indexOf(next)-pageVisualOrder.indexOf(page))||1
		if(transition&&!reduced&&!extensionEdge){
			pageTransitionRef.current=true
			transition.call(document,()=>flushSync(()=>setPage(next)))
		}else{
			if(extensionEdge)pageTransitionRef.current=true
			setPage(next)
		}
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
		try{
			if(page==='extensions')await refreshExtensions()
			else if(page==='audit'&&auditView==='runs')await audit.refresh()
			else if(page==='audit')setMCPActivityRefresh(value=>value+1)
			else if(page==='ssh')await Promise.all([refreshHosts(),refreshConnections()])
			else if(page==='config')await refreshConfiguration()
			else if(page==='chat')await refreshChat()
			else await refreshHealth()
		}finally{setRefreshing(false)}
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
	const replaceSSHTunnel=(previousID:string,tunnel:SSHTunnel)=>{
		setSSHTunnels(current=>[...current.filter(item=>item.id!==previousID&&item.id!==tunnel.id),tunnel])
	}
	const rememberSSHShell=useCallback((shell:SSHShell)=>{
		setSSHShells(current=>[...current.filter(item=>item.id!==shell.id),shell])
	},[])
	const registerSSHShell=useCallback((shell:SSHShell)=>{
		rememberSSHShell(shell)
		setSelectedShell(shell)
	},[rememberSSHShell])
	const closeSSHShell=async(id:string)=>{
		const dismiss=()=>{
			setSSHShells(current=>current.filter(item=>item.id!==id))
			setSelectedShell(current=>current?.id===id?null:current)
		}
		try{
			await api.closeSSHShell(id)
			dismiss()
		}catch(err){
			if(errorStatus(err)===404)dismiss()
			else{
				notify(errorText(err),'error')
				void refreshConnections()
			}
		}
	}
	const createWorkspaceShell=useCallback(async(workspaceID:string)=>{
		try{registerSSHShell(await api.startSSHShell({workspace_id:workspaceID}))}
		catch(err){notify(errorText(err),'error')}
	},[notify,registerSSHShell])
	const observeAgentWorkspaceShell=useCallback((shell:SSHShell)=>{
		rememberSSHShell(shell)
	},[rememberSSHShell])
	const activateChat=useCallback(()=>navigate('chat'),[navigate])
	const hostChanged=useCallback((host:Host)=>setHosts(current=>current.map(item=>item.id===host.id?host:item)),[])
	const modelChanged=useCallback((provider:ModelProvider)=>{
		setProviders(current=>current.map(item=>item.id===provider.id?provider:{...item,active:provider.active?false:item.active}))
		void api.health().then(next=>setHealth(current=>keepEquivalentHealth(current,next))).catch(err=>reportError(errorText(err)))
	},[reportError])
	const workspaceShells=useMemo(()=>sshShells.filter(shell=>shell.kind==='workspace'),[sshShells])
	const topbarShells=useMemo(()=>sshShells.filter(topbarShell),[sshShells])
	const logout=async()=>{try{await api.logout()}finally{onLogout()}}
  return <NotificationContext.Provider value={notify}><FileTransferProvider><AppFrame><div className="app-shell">
    <aside className="sidebar">
      <div className="brand"><div className="brand-mark"><TerminalSquare size={21}/></div><div className="brand-name"><strong>OpsNerva</strong></div></div>
      <nav className="sidebar-nav">
        <Nav active={page === 'chat'} icon={<Bot/>} label={t('shell.nav.agent')} onClick={() => navigate('chat')}/>
        <Nav active={page === 'ssh'} icon={<TerminalSquare/>} label={t('shell.nav.ssh')} onClick={() => navigate('ssh')}/>
		<Nav active={page === 'extensions'} icon={<Braces/>} label={t('shell.nav.extensions')} onClick={() => navigate('extensions')}/>
        <Nav active={page === 'audit'} icon={<History/>} label={t('shell.nav.audit')} onClick={() => navigate('audit')}/>
        <Nav active={page === 'logs'} icon={<FileText/>} label={t('shell.nav.logs')} onClick={() => navigate('logs')}/>
		<Nav active={page === 'config'} icon={<Settings2/>} label={t('shell.nav.configuration')} onClick={() => navigate('config')}/>
      </nav>
	  <section className="sidebar-conversations active"><div ref={setChatSidebarTarget}/></section>
      <div className="sidebar-foot">
		{auth.enabled&&<button type="button" className="auth-logout" title={t('auth.signOut')} aria-label={t('auth.signOut')} onClick={()=>void logout()}><LogOut size={14}/><span>{auth.username||t('auth.signOut')}</span></button>}
        <div className="build">v0.3.1</div>
      </div>
    </aside>
    <main>
	      <header className="topbar"><div><h1 ref={pageTitleRef}>{title}</h1></div><div className="top-actions">
		<SSHTunnelStatus tunnels={sshTunnels} hosts={hosts} open={openConnectionPanel==='tunnel'} onOpenChange={open=>setOpenConnectionPanel(current=>open?'tunnel':current==='tunnel'?null:current)} onStop={stopSSHTunnel} onCreated={registerSSHTunnel} onUpdated={replaceSSHTunnel} onRefresh={()=>void refreshConnections()}/>
		<SSHShellStatus shells={topbarShells} hosts={hosts} open={openConnectionPanel==='shell'} onOpenChange={open=>setOpenConnectionPanel(current=>open?'shell':current==='shell'?null:current)} onOpen={shell=>{setOpenConnectionPanel(null);setSelectedShell(shell)}} onClose={closeSSHShell} onCreated={registerSSHShell}/>
        <LanguageSwitch/>
		<ThemeSwitch preference={themePreference} onChange={setThemePreference}/>
        <span className={`status ${health?.status === 'ok' ? 'online' : ''}`}><CircleDot size={14}/>{health?.status === 'ok' ? t('shell.online') : t('shell.disconnected')}</span>
        <button className={`icon-button ${refreshing?'refreshing':''}`} onClick={()=>void manualRefresh()} disabled={refreshing} title={t(refreshing?'common.refreshing':'shell.refresh')} aria-label={t(refreshing?'common.refreshing':'shell.refresh')}><RefreshCw size={17}/></button>
      </div></header>
      <section ref={workspaceRef} className={`workspace workspace-${page}`}>
			<MemoChatPage visible={page==='chat'} onActivate={activateChat}
				hosts={hosts} providers={providers} approvals={approvals} runs={audit.runs} workspaceShells={workspaceShells}
				capabilities={capabilities} settings={settings} imageTypes={settings?.chat_image_allowed_types||defaultChatImageTypes}
					agentAvailable={!!health?.agent_available} modelName={health?.model?.model} contextWindow={health?.model?.context_window||0} refreshConnections={refreshConnections}
				dismissApproval={dismissApproval} onCreateWorkspaceShell={createWorkspaceShell} onOpenWorkspaceShell={setSelectedShell} onWorkspaceShellStarted={observeAgentWorkspaceShell} onSettingsChanged={setSettings}
				onHostChanged={hostChanged}
				onModelChanged={modelChanged}
				sidebarTarget={chatSidebarTarget} onSessionDeleted={removeSessionState} onError={reportError}
			/>
			{page === 'ssh' && <SSHWorkspacePage
				hosts={hosts} shells={sshShells.filter(shell=>shell.kind!=='workspace'&&shell.surface==='workspace')}
				onCreated={rememberSSHShell} refresh={refreshConnections} onError={reportError}
			/>}
		<Suspense fallback={<div className="panel" role="status">{t('common.loading')}</div>}>
		{page === 'config' && <ConfigurationPage hosts={hosts} providers={providers} proxies={proxies} settings={settings} capabilities={capabilities} health={health} refreshModels={refreshModels} refreshHosts={refreshHosts} refreshProxies={refreshProxies} refreshCapabilities={refreshCapabilities} refreshHealth={refreshHealth} onSettingsChanged={setSettings} onOpenMCPActivity={()=>{setAuditView('mcp');navigate('audit')}}/>}
		{page === 'extensions' && <ExtensionsPage skills={skills} mcpServers={mcpServers} toolCatalog={toolCatalog} refreshSkills={refreshSkills} refreshMCPServers={refreshMCPServers} refreshToolCatalog={refreshToolCatalog} onToolCatalogChanged={setToolCatalog}/>}
		{page === 'audit' && <AuditPage view={auditView} onViewChange={setAuditView} mcpRefreshKey={mcpActivityRefresh} runs={audit.runs} hosts={hosts} sessions={audit.sessions} ready={audit.ready} error={audit.error} runsHasMore={audit.runsHasMore} loadingMore={audit.loadingMore} onLoadMoreRuns={audit.loadMore} onDeleteRuns={audit.deleteRuns}/>}
        {page === 'logs' && <LogsPage/>}
		</Suspense>
      </section>
	      {selectedShell&&<SSHShellTerminal
			key={selectedShell.id}
			initialShell={selectedShell}
			relatedShells={selectedShell.kind==='workspace'?sshShells.filter(shell=>shell.kind==='workspace'&&shell.workspace_id===selectedShell.workspace_id&&sshShellActive(shell.status)):[]}
			onSelect={setSelectedShell}
			onClose={()=>setSelectedShell(null)}
			onChanged={()=>void refreshConnections()}
			onError={reportError}
		/>}
    </main>
	<NotificationCenter notifications={notifications} onDismiss={dismissNotification}/>
  </div></AppFrame></FileTransferProvider></NotificationContext.Provider>
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



function applyChatSessionDelta(current:ChatSession[],delta:ChatSessionDelta){
	const removed=new Set(delta.removed_ids||[])
	const sessions=new Map(current.filter(session=>!removed.has(session.id)).map(session=>[session.id,session]))
	for(const session of delta.sessions||[])sessions.set(session.id,session)
	const next=[...sessions.values()].sort((left,right)=>right.updated_at.localeCompare(left.updated_at)||right.id.localeCompare(left.id)).slice(0,50)
	return keepEquivalent(current,next)
}

function SessionRenameDialog({session,busy,error,onCancel,onConfirm}:{session:ChatSession;busy:boolean;error:string;onCancel:()=>void;onConfirm:(title:string)=>void}){
	const {t}=useTranslation()
	const [title,setTitle]=useState(session.title)
	const normalized=title.trim()
	useEffect(()=>{const close=(event:KeyboardEvent)=>{if(event.key==='Escape'&&!busy)onCancel()};window.addEventListener('keydown',close);return()=>window.removeEventListener('keydown',close)},[busy,onCancel])
	return createPortal(<div className="connection-dialog-backdrop" onMouseDown={event=>{if(event.target===event.currentTarget&&!busy)onCancel()}}><form className="connection-dialog compact panel session-rename-dialog" noValidate onSubmit={event=>{event.preventDefault();if(normalized&&normalized!==session.title)onConfirm(normalized)}}><header><span><Edit3 size={19}/><span><h2>{t('chat.renameConversation')}</h2></span></span><button type="button" disabled={busy} onClick={onCancel}><X size={15}/></button></header><div className="connection-dialog-fields single"><label><span>{t('chat.sessionTitle')}</span><input value={title} maxLength={80} onChange={event=>setTitle(event.target.value)} autoFocus/></label></div>{error&&<div className="connection-dialog-error"><ShieldAlert size={14}/><span>{error}</span></div>}<footer><button type="button" disabled={busy} onClick={onCancel}>{t('common.cancel')}</button><button className="primary" disabled={busy||!normalized||normalized===session.title}>{busy?<LoaderCircle className="spin" size={13}/>:<Save size={13}/>} {t('common.save')}</button></footer></form></div>,document.body)
}






function Nav({ active, icon, label, count, warn, onClick }: {active:boolean;icon:React.ReactNode;label:string;count?:number;warn?:boolean;onClick:()=>void}) {
  return <button className={`nav-item ${active ? 'active' : ''}`} onClick={onClick} title={label} aria-label={label} aria-current={active?'page':undefined}>{icon}<span>{label}</span>{count !== undefined && <em className={warn ? 'warn' : ''}>{count}</em>}</button>
}

const ChatSessionSidebar=memo(function ChatSessionSidebar({sessions,historyError,approvalCounts,activeSessionID,activeCurrentSession,workspaceSwitching,loadingSession,onNew,onOpen,onRename,onDelete}:{sessions:ChatSession[];historyError:string;approvalCounts:Map<string,number>;activeSessionID:string;activeCurrentSession:boolean;workspaceSwitching:boolean;loadingSession:string;onNew:()=>void;onOpen:(id:string)=>void;onRename:(session:ChatSession)=>void;onDelete:(session:ChatSession)=>void}){
	const {t,i18n:instance}=useTranslation()
	return <>
		<header className="sidebar-conversation-head"><span><History size={15}/>{t('chat.conversations')}</span><button className="new-chat-button" onClick={onNew} disabled={workspaceSwitching} title={t('chat.newConversation')} aria-label={t('chat.newConversation')}><Plus size={14}/><span>{t('common.new')}</span></button></header>
		<div className="session-list">
			{historyError&&<div className="history-error">{historyError}</div>}
			{!sessions.length&&!historyError&&<div className="history-empty">{t('chat.noSaved')}</div>}
			{sessions.map(session=>{const pending=approvalCounts.get(session.id)||0;const active=session.active||(session.id===activeSessionID&&activeCurrentSession);return <div className={`session-item ${session.id===activeSessionID?'active':''}`} key={session.id}><button className="session-open" onClick={()=>onOpen(session.id)} disabled={workspaceSwitching||loadingSession===session.id}><b>{session.title}{pending>0&&<em className="session-approval-count">{t('chat.approvalCount',{count:pending})}</em>}{active&&<em className="session-running-count">{t('chat.runningBadge')}</em>}</b><span>{new Date(session.updated_at).toLocaleString(localeFor(instance.language))} · {t('chat.messageCount',{count:session.message_count})}</span></button><div className="session-actions"><button className="session-edit" onClick={()=>onRename(session)} disabled={workspaceSwitching} title={t('chat.renameConversation')} aria-label={t('chat.renameConversation')}><Edit3 size={13}/></button><button className="session-delete" onClick={()=>onDelete(session)} disabled={active||workspaceSwitching} title={active?t('chat.cannotDelete'):t('chat.deleteConversation')}><Trash2 size={13}/></button></div></div>})}
		</div>
	</>
})

const PersistentPageBoundary=memo(function PersistentPageBoundary({visible,children}:{visible:boolean;children:(visible:boolean)=>React.ReactNode}){
	return children(visible)
},(previous,next)=>!previous.visible&&!next.visible)

function ChatPage({ visible, onActivate, hosts, providers, approvals, runs, workspaceShells, capabilities, settings, imageTypes, agentAvailable, modelName, contextWindow, refreshConnections, dismissApproval, onCreateWorkspaceShell, onOpenWorkspaceShell, onWorkspaceShellStarted, onSettingsChanged, onHostChanged, onModelChanged, sidebarTarget, onSessionDeleted, onError }: {visible:boolean;onActivate:()=>void;hosts:Host[];providers:ModelProvider[];approvals:Approval[];runs:Run[];workspaceShells:SSHShell[];capabilities:ToolCapabilities;settings:SystemSettings|null;imageTypes:string[];agentAvailable:boolean;modelName?:string;contextWindow:number;refreshConnections:()=>Promise<void>;dismissApproval:(approvalID:string)=>void;onCreateWorkspaceShell:(workspaceID:string)=>Promise<void>;onOpenWorkspaceShell:(shell:SSHShell)=>void;onWorkspaceShellStarted:(shell:SSHShell)=>void;onSettingsChanged:(settings:SystemSettings)=>void;onHostChanged:(host:Host)=>void;onModelChanged:(provider:ModelProvider)=>void;sidebarTarget:HTMLDivElement|null;onSessionDeleted:(sessionID:string)=>void;onError:(message:string)=>void}) {
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
  const [tasks,setTasks]=useState<AgentTaskList|null>(null)
	const [tasksExpanded,setTasksExpanded]=useState(false)
	const [workspaceID,setWorkspaceID]=useState(recalledWorkspace)
	const [fileBrowserMode,setFileBrowserMode]=useState<'workspace'|'sftp'>('workspace')
	const [sftpHostID,setSFTPHostID]=useState('')
	const [boundWorkspaceID,setBoundWorkspaceID]=useState('')
	const [workspaceSwitching,setWorkspaceSwitching]=useState(false)
  const messagesRef=useRef<HTMLDivElement>(null)
	const sessionIDRef=useRef('')
  const stickToLatest=useRef(true)
	const lastMessagesScrollTop=useRef(0)
	const disclosureScrollFrame=useRef(0)
	const activeChatDisclosures=useRef(new Set<symbol>())
	const autoScrollFrame=useRef(0)
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
	const renderEntries=useMemo(()=>visible?groupedTaskToolEntries(conversationEntries):emptyChatRenderItems,[conversationEntries,visible])
	const latestConversationEntryID=conversationEntries.at(-1)?.id||''
	const taskRows=useMemo(()=>tasks?buildSessionTaskRows(tasks):[],[tasks])
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
	useEffect(()=>{if(!tasks)setTasksExpanded(false);else setTasksExpanded(true)},[tasks?.session_id,!!tasks])
	useEffect(()=>()=>{sessionLoadRef.current='';const stream=activeStreamRef.current;activeStreamRef.current=null;stream?.controller.abort();window.cancelAnimationFrame(disclosureScrollFrame.current);window.cancelAnimationFrame(autoScrollFrame.current);activeChatDisclosures.current.clear();messagesRef.current?.classList.remove('chat-disclosure-active')},[])
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
    stickToLatest.current=true
		lastMessagesScrollTop.current=0
    try {
      const state = await api.chatState(id)
      if(sessionLoadRef.current!==requestID)return
		lastAgentEventSessionRef.current=id;lastAgentEventIDRef.current=0
	      setEntries(historyEntries(state.messages||[],id));setHistoryHasMore(!!state.messages_has_more);setHistoryCursor(state.messages_next_created_at&&state.messages_next_id?{createdAt:state.messages_next_created_at,id:state.messages_next_id}:null);setDetachedRunning(!!state.active);setQueuedMessages(state.queued_messages||[]);setQueueingMode(null);setStopping(false);setModelRetry(null);setConnectionRetry(null);setTasks(state.tasks?.items?.length?state.tasks:null);setContextUsage({tokens:state.context_tokens||0,window:contextWindowForSession(state.context_tokens||0,state.context_window||0,activeContextWindow)});setWorkspaceID(state.workspace_id||'');setBoundWorkspaceID(state.workspace_id||'')
	      startedQueueMessageIDsRef.current.clear()
      setSessionId(id); rememberSession(id); setHistoryError('')
	} catch (err) { if(sessionLoadRef.current===requestID)setHistoryError(errorText(err)) }
	finally { if(sessionLoadRef.current===requestID)setLoadingSession('') }
	}, [activeContextWindow])
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
		const container=messagesRef.current
		const previousHeight=container?.scrollHeight||0
		setLoadingOlderMessages(true);setHistoryError('')
		try{
			const page=await api.chatMessages(targetSessionID,historyCursor)
			if(sessionIDRef.current!==targetSessionID)return
			stickToLatest.current=false
			flushSync(()=>setEntries(current=>prependHistoryEntries(page.messages||[],targetSessionID,current)))
			setHistoryHasMore(page.has_more)
			setHistoryCursor(page.next_created_at&&page.next_id?{createdAt:page.next_created_at,id:page.next_id}:null)
			if(container){container.scrollTop+=container.scrollHeight-previousHeight;lastMessagesScrollTop.current=container.scrollTop}
		}catch(err){if(sessionIDRef.current===targetSessionID)setHistoryError(errorText(err))}
		finally{if(sessionIDRef.current===targetSessionID)setLoadingOlderMessages(false)}
	},[historyCursor,historyHasMore,loadingOlderMessages,sessionId])

	useEffect(()=>{
		window.cancelAnimationFrame(autoScrollFrame.current)
		if(!visible||!stickToLatest.current)return
		autoScrollFrame.current=window.requestAnimationFrame(()=>{
			const container=messagesRef.current
			if(container&&stickToLatest.current){container.scrollTop=container.scrollHeight;lastMessagesScrollTop.current=container.scrollTop}
		})
		return()=>window.cancelAnimationFrame(autoScrollFrame.current)
	},[latestConversationEntryID,loadingSession,sessionId,visible])

	const trackUserScroll=useCallback(()=>{
		const container=messagesRef.current
		if(!container)return
		const nextScrollTop=container.scrollTop
		const movingUp=nextScrollTop<lastMessagesScrollTop.current-1
		const atLatest=container.scrollHeight-nextScrollTop-container.clientHeight<24
		if(movingUp)stickToLatest.current=false
		else if(atLatest)stickToLatest.current=true
		lastMessagesScrollTop.current=nextScrollTop
	},[])
	const pauseLatestOnWheel=useCallback((event:React.WheelEvent<HTMLDivElement>)=>{if(event.deltaY<0)stickToLatest.current=false},[])
	const pauseLatestOnTouch=useCallback(()=>{stickToLatest.current=false},[])

	const preserveChatDisclosurePosition=useCallback<ChatDisclosurePositionHandler>((disclosure,summary,holdAnchor)=>{
		const container=messagesRef.current
		if(!container)return
		if(holdAnchor)activeChatDisclosures.current.add(disclosure)
		else activeChatDisclosures.current.delete(disclosure)
		if(!summary||!container.contains(summary)){
			if(activeChatDisclosures.current.size===0)container.classList.remove('chat-disclosure-active')
			return
		}
		stickToLatest.current=false
		const top=summary.getBoundingClientRect().top
		window.cancelAnimationFrame(disclosureScrollFrame.current)
		container.classList.add('chat-disclosure-active')
		disclosureScrollFrame.current=window.requestAnimationFrame(()=>{
			if(container.isConnected&&summary.isConnected){container.scrollTop+=summary.getBoundingClientRect().top-top;lastMessagesScrollTop.current=container.scrollTop}
			if(activeChatDisclosures.current.size===0)container.classList.remove('chat-disclosure-active')
		})
	},[])

	const newChat=useCallback(()=>{
		if(workspaceSwitching)return
		onActivate()
		detachActiveStream()
		lastAgentEventSessionRef.current='';lastAgentEventIDRef.current=0
		sessionLoadRef.current=''
    setLoadingSession('')
	    stickToLatest.current=true;lastMessagesScrollTop.current=0;startedQueueMessageIDsRef.current.clear();setSessionId('');setBoundWorkspaceID('');setEntries([]);setHistoryHasMore(false);setHistoryCursor(null);setLoadingOlderMessages(false); setMessage('');clearPendingImages(); setHistoryError('');setContextUsage({tokens:0,window:activeContextWindow});setDetachedRunning(false);setQueuedMessages([]);setQueueingMode(null);setStopping(false);setCompressingContext(false);setModelRetry(null);setConnectionRetry(null);setTasks(null); rememberSession(newSessionMarker)
	},[activeContextWindow,clearPendingImages,detachActiveStream,onActivate,workspaceSwitching])

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
				if(/^Task(Create|Get|Update|List)$/.test(frame.tool_name||'')){const nextTasks=tasksFromToolContent(frame.content);if(nextTasks)setTasks(nextTasks.items.length?nextTasks:null)}
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
					setTasks(state.tasks?.items?.length?state.tasks:null)
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
			if(has('tasks'))setTasks(state.tasks?.items?.length?state.tasks:null)
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
    stickToLatest.current=true
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
      if(!isAttached())return
      setModelRetry(null)
			setConnectionRetry(null)
			setEntries((old) => old.map(deactivateReasoning))
      setRunning(false)
		setStopping(false)
		setCompressingContext(false)
		if(querySessionID){try{const state=await api.chatState(querySessionID);if(!isAttached())return;setDetachedRunning(!!state.active);setQueuedMessages(state.queued_messages||[]);setTasks(state.tasks?.items?.length?state.tasks:null);setContextUsage({tokens:state.context_tokens||0,window:contextWindowForSession(state.context_tokens||0,state.context_window||0,activeContextWindow)});setBoundWorkspaceID(state.workspace_id||'');setEntries(old=>settledTurnEntries(state.messages||[],querySessionID,old,!!state.active));for(const image of queryImages){URL.revokeObjectURL(image.url);imageURLsRef.current.delete(image.url)}}catch{/* the next state event or reload will recover state */}}
      if(!isAttached())return
      activeStreamRef.current=null
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
			if(!result.cancelled){const state=await api.chatState(targetSessionID);setDetachedRunning(!!state.active);setQueuedMessages(state.queued_messages||[]);setTasks(state.tasks?.items?.length?state.tasks:null);setContextUsage({tokens:state.context_tokens||0,window:contextWindowForSession(state.context_tokens||0,state.context_window||0,activeContextWindow)});setEntries(old=>settledTurnEntries(state.messages||[],targetSessionID,old,!!state.active))}
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
		<ChatWorkspacePanel key={selectedWorkspace?.id||''} active={pageVisible} mode={fileBrowserMode} onModeChange={setFileBrowserMode} workspaces={capabilities.workspaces} workspaceID={selectedWorkspace?.id||''} hosts={hosts} sftpHostID={sftpHostID} onSFTPHostChange={setSFTPHostID} shells={workspaceShells} switching={workspaceSwitching} disabled={sessionBusy||!!loadingSession} bound={!!selectedWorkspace&&boundWorkspaceID===selectedWorkspace.id} onSelect={switchWorkspace} onCreateShell={onCreateWorkspaceShell} onOpenShell={onOpenWorkspaceShell} onCollapse={collapseWorkspacePanel}/>
	  {workspacePanelCollapsed&&<button type="button" className="chat-panel-open-button" onClick={()=>setWorkspaceCollapsed(false)} title={t('workspace.expandPanel')} aria-label={t('workspace.expandPanel')}><PanelLeftOpen size={15}/></button>}
    <div className="chat-main panel">
	  <div className="session-approval-slot">{currentApprovals.length>0&&<ApprovalDialog key={currentApprovals[0].id} approval={currentApprovals[0]} pendingCount={currentApprovals.length} hosts={hosts} running={sessionBusy||toolsRunning} stopping={stopping} onStop={()=>void stopAgent()} dismissApproval={dismissApproval} onApproved={result=>{if(result.status==='running')setEntries(old=>updateToolRunStatus(old,result.run_id,'in_progress'));if(result.shell?.kind==='workspace')onWorkspaceShellStarted(result.shell)}} onNotice={notify}/>}</div>
	      <div className="session-task-slot">{tasks&&<SessionTasks tasks={tasks} rows={taskRows} expanded={tasksExpanded} onExpanded={setTasksExpanded}/>}</div>
		<div className="conversation-view">
			<div className="messages" ref={messagesRef} onScroll={trackUserScroll} onWheel={pauseLatestOnWheel} onTouchMove={pauseLatestOnTouch}>
				{historyHasMore&&<button type="button" className="chat-history-more" disabled={loadingOlderMessages} onClick={()=>void loadOlderMessages()}>{loadingOlderMessages?<LoaderCircle className="spin" size={13}/>:<History size={13}/>} {t('chat.loadEarlier')}</button>}
				{conversationEntries.length === 0 && <div className="empty-chat"><div className="radar"><Activity size={35}/></div><h2>{t('chat.emptyTitle')}</h2></div>}
				<ChatEntryList items={renderEntries} sessionID={sessionId} visible={pageVisible} targets={liveSSHTaskTargets} actionEntryID={latestCompletedAssistantEntryID} runs={runs} hosts={hosts} onDisclosure={preserveChatDisclosurePosition}/>
				{(sessionBusy||toolsRunning)&&<ChatActivityStatus visible={pageVisible} stopping={stopping} connectionRetry={connectionRetry} modelRetry={modelRetry}/>}
				{conversationEntries.length>0&&<div className="chat-scroll-anchor" aria-hidden="true"/>}
			</div>
			{tasks&&tasksExpanded&&<SessionTaskItems rows={taskRows}/>}
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
}


const MemoChatPage=memo(ChatPage)
type SessionTaskRow={task:AgentTask;blockers:string[];status:AgentTask['status']|'blocked'}

function buildSessionTaskRows(tasks:AgentTaskList):SessionTaskRow[]{
	const completed=new Set(tasks.items.filter(task=>task.status==='completed').map(task=>task.id))
	return tasks.items.map(task=>{const blockers=task.blocked_by.filter(id=>!completed.has(id));return{task,blockers,status:task.status==='pending'&&blockers.length?'blocked':task.status}})
}

const SessionTasks=memo(function SessionTasks({tasks,rows,expanded,onExpanded}:{tasks:AgentTaskList;rows:SessionTaskRow[];expanded:boolean;onExpanded:(expanded:boolean)=>void}){
	const {t}=useTranslation()
	const completed=rows.filter(row=>row.status==='completed').length
	const current=rows.find(row=>row.status==='in_progress')?.task||rows.find(row=>row.status==='pending')?.task
	const blocked=rows.filter(row=>row.status==='blocked').length
	const state=current?'active':blocked?'blocked':'completed'
  const progress=tasks.items.length?Math.round(completed/tasks.items.length*100):0
	return <details className={`session-tasks ${state}`} open={expanded} onToggle={event=>onExpanded(event.currentTarget.open)}><summary><span className="task-list-icon"><ListChecks size={16}/></span><span className="task-list-summary"><b>{t('agentTasks.title')}</b><small>{current?current.active_form||current.subject:blocked?t('agentTasks.blocked',{count:blocked}):`${completed}/${tasks.items.length}`}</small></span><span className="task-list-progress"><i><em style={{width:`${progress}%`}}/></i><b>{progress}%</b></span><span className={`task-list-state ${state}`} key={state}>{t(`statusLabels.${state}`,{defaultValue:state})}</span><ChevronRight size={14}/></summary></details>
})

const SessionTaskItems=memo(function SessionTaskItems({rows}:{rows:SessionTaskRow[]}){
	const {t}=useTranslation()
	const blocked=rows.some(row=>row.status==='blocked')&&!rows.some(row=>row.status==='in_progress'||row.status==='pending')
	return <section className={`session-task-view ${blocked?'blocked':'active'}`}><ul className="session-task-items">{rows.map(({task,blockers,status})=><li className={status} key={task.id}><span className="task-item-marker">{status==='completed'?<Check size={12}/>:status==='in_progress'?<LoaderCircle size={12}/>:status==='blocked'?<ShieldAlert size={12}/>:<CircleDot size={10}/>}</span><div title={task.description}><b>{task.subject}</b>{status==='blocked'&&<small>{t('agentTasks.blocked',{count:blockers.length})}</small>}</div><em>{task.owner||t(`statusLabels.${status}`,{defaultValue:status.replace('_',' ')})}</em></li>)}</ul></section>
})

const StreamingTextNodes=memo(function StreamingTextNodes({value}:{value:StreamText}){
	return <>{value.blocks.map((block,index)=><span key={index}>{block}</span>)}{value.tail&&<span key="tail">{value.tail}</span>}</>
})

const ChatEntryList=memo(function ChatEntryList({items,sessionID,visible,targets,actionEntryID,runs,hosts,onDisclosure}:{items:ChatRenderItem[];sessionID:string;visible:boolean;targets:readonly LiveSSHTaskTarget[];actionEntryID:string;runs:Run[];hosts:Host[];onDisclosure:ChatDisclosurePositionHandler}){
	const documentVisible=useDocumentVisible()
	const liveSSHTasks=useLiveSSHTasks(visible&&documentVisible,sessionID,targets)
	const owners=useMemo(()=>new Map(targets.map(target=>[target.entryID,target.taskID])),[targets])
	return <>{items.map(item=>{
		if(item.kind==='task_tool_group')return <TaskToolGroupCard key={item.id} group={item} onDisclosure={onDisclosure}/>
		const taskID=owners.get(item.entry.id)
		return <ChatBubble key={item.entry.id} sessionID={sessionID} entry={item.entry} showActions={item.entry.id===actionEntryID} runs={runs} hosts={hosts} liveSSHTaskOwner={!!taskID} liveSSHTask={taskID?liveSSHTasks.get(taskID):undefined} onDisclosure={onDisclosure}/>
	})}</>
})

const ChatBubble=memo(function ChatBubble({ sessionID, entry, showActions, runs, hosts, liveSSHTaskOwner, liveSSHTask, onDisclosure }: {sessionID:string;entry:ChatEntry;showActions:boolean;runs:Run[];hosts:Host[];liveSSHTaskOwner:boolean;liveSSHTask?:LiveSSHTaskSnapshot;onDisclosure:ChatDisclosurePositionHandler}) {
	const {t}=useTranslation()
  if (entry.kind === 'tool') return <ToolEventCard sessionID={sessionID} entry={entry} runs={runs} hosts={hosts} liveSSHTaskOwner={liveSSHTaskOwner} currentLiveSSHTask={liveSSHTask} onDisclosure={onDisclosure}/>
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

function toolLabel(value:string){return i18n.t(`toolNames.${value}`,{defaultValue:value})}
function toolSummaryIcon(name:string|undefined){
	if(name?.startsWith('Task'))return <ListChecks size={15}/>
	if(name==='ssh_task')return <Activity size={15}/>
	if(name?.startsWith('workspace_'))return <FolderOpen size={15}/>
	if(name?.startsWith('web_'))return <Search size={15}/>
	if(name==='skill')return <BookOpen size={15}/>
	if(name?.startsWith('mcp__'))return <Braces size={15}/>
	if(name?.includes('file_'))return <FileText size={15}/>
	if(name?.startsWith('ssh_'))return name==='ssh_exec'||name==='ssh_run_script'||name==='ssh_shell'?<TerminalSquare size={15}/>:<Server size={15}/>
	return <FunctionSquare size={15}/>
}
function requestFromRun(run?:Run):JsonRecord|undefined{if(!run)return;try{return jsonRecord(JSON.parse(run.request_json))}catch{return}}
function runAutoApproved(run?:Run){return run?.ai_review?.kind==='automatic_approval'&&run.ai_review.status==='completed'&&run.ai_review.decision==='allow'}
function shellArg(value:string){return /^[A-Za-z0-9_@%+=:,./-]+$/.test(value)?value:JSON.stringify(value)}
function fullProgram(request:JsonRecord,full=false){
	const program=full?textValue(request.program):previewText(textValue(request.program))
	const source=Array.isArray(request.args)?request.args:[]
	const selected=full?source:source.slice(0,toolCollectionPreviewItems)
	const args=selected.map(value=>full?String(value):previewText(String(value)))
	if(!full&&source.length>selected.length)args.push(i18n.t('tool.previewItemsOmitted',{count:source.length-selected.length}))
	const command=[program,...args].filter(Boolean).map(shellArg).join(' ')
	return full?command:previewText(command,toolOutputPreviewChars)
}
function compactScript(script:string){const source=script.length>toolOutputPreviewChars?script.slice(0,toolOutputPreviewChars):script,lines=source.split(/\r?\n/).map(line=>line.trim()).filter(Boolean);if(!lines.length)return i18n.t('tool.shellScript');const first=previewText(lines[0],180);return lines.length===1&&source.length===script.length?first:i18n.t('tool.moreLines',{line:first,count:Math.max(1,lines.length-1)})}
function latestOutput(value:string,limit=3){const tail=value.length>toolOutputPreviewChars?value.slice(-toolOutputPreviewChars):value;return tail.trimEnd().split(/\r?\n/).filter(line=>line.trim()!=='').slice(-limit).map(line=>previewText(line,180)).join('\n')}
function formatDuration(value:unknown,run?:Run){if(typeof value==='number'&&Number.isFinite(value))return value>=1e9?`${(value/1e9).toFixed(2)} s`:`${(value/1e6).toFixed(1)} ms`;if(run?.completed_at){const ms=Date.parse(run.completed_at)-Date.parse(run.started_at);if(Number.isFinite(ms))return ms>=1000?`${(ms/1000).toFixed(2)} s`:`${ms} ms`}return'—'}
function numberValue(value:unknown){return typeof value==='number'&&Number.isFinite(value)?value:0}
function cleanFileChangeOutput(value:string){const lines=value.split(/\r?\n/),result:string[]=[];for(let index=0;index<lines.length;index++){if(lines[index]==='__OPS_FILE_VALIDATION_OK__')continue;if(lines[index]==='__OPS_FILE_AFTER__'){index++;continue}result.push(lines[index])}return result.join('\n').trim()}

type ToolTarget={kind:'host'|'workspace'|'scope';label:string;name:string;id?:string}
function ToolTransferRoute({sourceHost,sourcePath,destinationHost,destinationPath}:{sourceHost:string;sourcePath:string;destinationHost:string;destinationPath:string}){
	const route=`${sourceHost}:${sourcePath} → ${destinationHost}:${destinationPath}`
	return <span className="tool-transfer-route" title={route}><span><b>{sourceHost}</b><code>:{sourcePath}</code></span><i>→</i><span><b>{destinationHost}</b><code>:{destinationPath}</code></span></span>
}
type WorkspaceTransferEndpoint={kind:'host'|'workspace';name:string;path:string}
type WorkspaceTransferRouteValue={source:WorkspaceTransferEndpoint;destination:WorkspaceTransferEndpoint}
function WorkspaceTransferEndpointLabel({endpoint}:{endpoint:WorkspaceTransferEndpoint}){
	return <span data-endpoint-kind={endpoint.kind}><b>{endpoint.kind==='workspace'?<FolderOpen size={11}/>:<Server size={11}/>} {endpoint.name}</b><code>:{endpoint.path}</code></span>
}
function WorkspaceTransferRoute({source,destination}:WorkspaceTransferRouteValue){
	const route=`${source.name}:${source.path} → ${destination.name}:${destination.path}`
	return <span className="tool-transfer-route tool-workspace-transfer-route" title={route}><WorkspaceTransferEndpointLabel endpoint={source}/><i>→</i><WorkspaceTransferEndpointLabel endpoint={destination}/></span>
}
function hostIdentity(hosts:Host[],hostID:string){
	const host=hosts.find(item=>item.id===hostID||item.name===hostID)
	return {name:host?.name||'',id:host?.id||hostID,user:host?.user||''}
}
function executionPermission(request:JsonRecord|undefined,hosts:Host[],...hostIDs:string[]){
	if(request?.elevated===true)return'root'
	return hostIDs.some(hostID=>hosts.find(host=>host.id===hostID||host.name===hostID)?.user.trim().toLowerCase()==='root')?'root':'user'
}
function recordArray(value:unknown,fromEnd=false){
	if(!Array.isArray(value))return[]
	const selected=fromEnd?value.slice(-toolCollectionPreviewItems):value.slice(0,toolCollectionPreviewItems)
	return selected.map(jsonRecord).filter((item):item is JsonRecord=>!!item)
}
type ToolWorkspaceDirectoryEntry={name:string;type:'file'|'directory';size?:number}
function parseWorkspaceDirectoryOutput(value:string):ToolWorkspaceDirectoryEntry[]|undefined{
	if(!value)return
	try{
		const result=jsonRecord(JSON.parse(value))
		if(!result||!Array.isArray(result.entries))return
		const entries:ToolWorkspaceDirectoryEntry[]=[]
		for(const value of result.entries){
			const entry=jsonRecord(value),name=textValue(entry?.name),type=textValue(entry?.type)
			if(!entry||!name||(type!=='file'&&type!=='directory')||(entry.size!==undefined&&(typeof entry.size!=='number'||!Number.isFinite(entry.size))))return
			entries.push({name,type,size:typeof entry.size==='number'?entry.size:undefined})
		}
		return entries
	}catch{return}
}
function recordTableRows(value:JsonRecord){
	const {entries,truncated}=limitedRecordEntries(value,toolCollectionPreviewItems-1)
	const rows:Array<Array<unknown>>=entries.map(([key,item])=>[key,item])
	if(truncated)rows.push(['…',i18n.t('tool.moreItemsOmitted')])
	return rows
}

type DiffRow={kind:'header'|'hunk'|'add'|'delete'|'context'|'meta';oldLine?:number;newLine?:number;text:string}
function parseDiffRows(diff:string):DiffRow[]{
	let oldLine:number|undefined,newLine:number|undefined
	return diff.replace(/\n$/, '').split('\n').map(line=>{
		const hunk=line.match(/^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/)
		if(hunk){oldLine=Number(hunk[1]);newLine=Number(hunk[2]);return{kind:'hunk',text:line}}
		if(line.startsWith('@@ ')){oldLine=undefined;newLine=undefined;return{kind:'hunk',text:line}}
		if(line.startsWith('--- ')||line.startsWith('+++ '))return{kind:'header',text:line}
		if(line.startsWith('+')){const row={kind:'add' as const,newLine,text:line};if(newLine!==undefined)newLine++;return row}
		if(line.startsWith('-')){const row={kind:'delete' as const,oldLine,text:line};if(oldLine!==undefined)oldLine++;return row}
		if(line.startsWith(' ')){const row={kind:'context' as const,oldLine,newLine,text:line};if(oldLine!==undefined)oldLine++;if(newLine!==undefined)newLine++;return row}
		return{kind:'meta',text:line}
	})
}

function DiffViewer({change}:{change:JsonRecord}){
	const {t}=useTranslation(),diff=textValue(change.diff),rows=parseDiffRows(previewText(diff,toolDiffPreviewChars))
	return <section className="diff-viewer"><header><span><FileText size={14}/>{t('tool.fileEdit')}</span><div><em className="add">+{numberValue(change.additions)}</em><em className="delete">-{numberValue(change.deletions)}</em><CopyButton value={diff}/></div></header><div className="diff-scroll" role="table" aria-label={t('tool.diff')}><div className="diff-lines">{rows.map((row,index)=><div className={`diff-line ${row.kind}`} role="row" key={index}><span className="old-line">{row.oldLine??''}</span><span className="new-line">{row.newLine??''}</span><code>{row.text||' '}</code></div>)}</div></div></section>
}

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

const TaskToolGroupCard=memo(function TaskToolGroupCard({group,onDisclosure}:{group:TaskToolEntryGroup;onDisclosure:ChatDisclosurePositionHandler}){
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

function ToolEventCard({sessionID,entry:initialEntry,runs,hosts,liveSSHTaskOwner,currentLiveSSHTask,onDisclosure}:{sessionID:string;entry:ChatEntry;runs:Run[];hosts:Host[];liveSSHTaskOwner:boolean;currentLiveSSHTask?:LiveSSHTaskSnapshot;onDisclosure:ChatDisclosurePositionHandler}){
	const {t}=useTranslation()
	const chatVisible=useContext(ChatVisibilityContext)
	const disclosure=useChatCardDisclosure(onDisclosure,'tool-summary-chevron')
	const [fullContent,setFullContent]=useState('')
	const [loadingDetail,setLoadingDetail]=useState(false)
	const [detailError,setDetailError]=useState('')
	const entry=fullContent?{...initialEntry,content:fullContent,contentTruncated:false}:initialEntry
	useEffect(()=>{setFullContent('');setLoadingDetail(false);setDetailError('')},[initialEntry.sourceMessageId])
	const storedPayload=useMemo(()=>parseRecord(entry.content),[entry.content,t])
	const storedDisplay=jsonRecord(storedPayload._display)
	const storedToolArguments=jsonRecord(storedDisplay?.arguments)
	const sshTaskOperation=entry.tool==='ssh_task'
	const sshTaskAction=textValue(storedToolArguments?.action)
	const sshTaskID=sshTaskOperation?(textValue(storedPayload.task_id)||textValue(jsonRecord(storedPayload.result)?.task_id)||textValue(storedToolArguments?.task_id)):''
	const storedSSHTaskStatus=textValue(storedPayload.status)||textValue(jsonRecord(storedPayload.result)?.status)
	const liveSSHTaskUnavailable=!!currentLiveSSHTask?.error&&!textValue(currentLiveSSHTask.task?.id)
	const liveSSHTaskStatus=liveSSHTaskUnavailable?'failed':textValue(currentLiveSSHTask?.task?.status)||textValue(currentLiveSSHTask?.result?.status)
	const useLiveSSHTask=liveSSHTaskOwner&&!!currentLiveSSHTask&&activeLiveTaskStatus(storedSSHTaskStatus)
	const payload=useLiveSSHTask?{
		...storedPayload,
		...currentLiveSSHTask.result,
		task_id:sshTaskID,
		run_id:textValue(currentLiveSSHTask.result?.run_id)||textValue(currentLiveSSHTask.task?.run_id)||textValue(storedPayload.run_id),
		status:liveSSHTaskStatus||storedSSHTaskStatus,
		wait_deadline_reached:false,
		...(currentLiveSSHTask.error?{message:currentLiveSSHTask.error,code:liveSSHTaskUnavailable?'task_status_unavailable':'remote_failed'}:{}),
		_display:storedDisplay,
	}:storedPayload
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
	const destinationHostID=textValue(display?.host_id)||run?.host_id||textValue(request?.host_id)||textValue(toolArguments?.host_id)||textValue(toolArguments?.destination_host_id)||textValue(shellPayload?.host_id)||textValue(currentLiveSSHTask?.task?.host_id)||textValue(payload.host_id)||textValue(resultPayload?.host_id)
	const destinationHost=hostIdentity(hosts,destinationHostID)
  const hostID=destinationHost.id
  const hostName=destinationHost.name||hostID||'—'
  const rawPayloadStatus=textValue(payload.status)||textValue(taskPayload?.status)||textValue(resultPayload?.status)
	const payloadStatus=sshTaskOperation?(rawPayloadStatus==='running'?'in_progress':rawPayloadStatus==='waiting_for_approval'?'approval_required':rawPayloadStatus):rawPayloadStatus
  const runStatus=run?.status==='running'?'in_progress':run?.status
  const status=sshTaskOperation?payloadStatus||runStatus||'completed':payloadStatus==='approval_required'&&runStatus&&runStatus!=='approval_required'?runStatus:payloadStatus||runStatus||'completed'
	const toolLive=status==='in_progress'&&(!sshTaskOperation||liveSSHTaskOwner)
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
	const tunnelDirection=(request?textValue(request.direction):'')||textValue(tunnel?.direction)||textValue(toolArguments?.direction)||'local'
	const tunnelLocalHost=(request?textValue(request.local_host):'')||textValue(tunnel?.local_host)||textValue(toolArguments?.local_host)||'127.0.0.1'
	const tunnelRemoteHost=(request?textValue(request.remote_host):'')||textValue(tunnel?.remote_host)||textValue(toolArguments?.remote_host)
	const tunnelRemotePort=(request?numberValue(request.remote_port):0)||numberValue(tunnel?.remote_port)||numberValue(toolArguments?.remote_port)
	const tunnelLocalPort=(request?numberValue(request.local_port):0)||numberValue(tunnel?.local_port)||numberValue(toolArguments?.local_port)
	const tunnelRoute=tunnelAction==='start'?sshTunnelRoute(hostName,tunnelDirection,tunnelLocalHost,tunnelLocalPort,tunnelRemoteHost,tunnelRemotePort,t('tunnels.automaticPort')):tunnelAction==='stop'?textValue(toolArguments?.tunnel_id):''
	const shellTool=entry.tool==='ssh_shell'||entry.tool==='workspace_shell'
	const shellAction=textValue(toolArguments?.action)||(requestMode==='ssh_shell_start'||requestMode==='workspace_shell_start'?'start':requestMode==='workspace_shell'?'run':'')
	const shellOperation=shellTool&&shellAction!=='run'
	const shellID=textValue(toolArguments?.shell_id)||textValue(shellPayload?.id)
	const shellEvents=[...recordArray(payload.events,true),...recordArray(resultPayload?.events,true)]
	const shellChunks=[...recordArray(payload.chunks,true),...recordArray(resultPayload?.chunks,true)]
	const recentShellChunks=shellChunks.slice(-toolCollectionPreviewItems)
	const shellChunkStdout=recentShellChunks.filter(chunk=>textValue(chunk.stream)==='stdout').map(chunk=>previewText(textValue(chunk.content),toolOutputPreviewChars)).join('')
	const shellChunkStderr=recentShellChunks.filter(chunk=>textValue(chunk.stream)==='stderr').map(chunk=>previewText(textValue(chunk.content),toolOutputPreviewChars)).join('')
	const shellChunkOutput=recentShellChunks.map(chunk=>previewText(textValue(chunk.content),toolOutputPreviewChars)).join('')
	const shellHasMore=payload.has_more===true||resultPayload?.has_more===true
	const shellOutput=shellChunkOutput||textValue(payload.output)||textValue(resultPayload?.output)||textValue(payload.recent_output)||textValue(resultPayload?.recent_output)||shellEvents
		.filter(event=>['stdout','stderr'].includes(textValue(event.stream)))
		.slice(-toolCollectionPreviewItems)
		.map(event=>previewText(textValue(event.content),toolOutputPreviewChars))
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
	const eventToolLabel=sshTaskOperation?t(sshTaskAction==='cancel'?'tool.taskCancel':'tool.taskStatus'):shellOperation?shellActionLabel:structuredFileOperation?t(fileSearchMode?(workspaceID?'toolNames.workspace_file_search_mode':'toolNames.ssh_file_search_mode'):(workspaceID?'toolNames.workspace_file_read':'toolNames.ssh_file_read')):toolLabel(entry.tool||'')
	const workspaceTransferRoute:WorkspaceTransferRouteValue|undefined=workspaceUpload
		?{source:{kind:'workspace',name:workspaceID,path:relativePath},destination:{kind:'host',name:hostName,path:remotePath}}
		:workspaceDownload
			?{source:{kind:'host',name:hostName,path:remotePath},destination:{kind:'workspace',name:workspaceID,path:relativePath}}
			:undefined
	const workspaceTransfer=!!workspaceTransferRoute
	const fileTransfer=workspaceTransfer||sshTransfer
	const transferSummary=tunnelRoute||shellSummary||(workspaceTransferRoute?`${workspaceTransferRoute.source.name}:${workspaceTransferRoute.source.path} → ${workspaceTransferRoute.destination.name}:${workspaceTransferRoute.destination.path}`:sshTransfer?`${sourceHostName}:${sourcePath} → ${hostName}:${remotePath}`:'')
  const planSteps=Array.isArray(payload.steps)?payload.steps.slice(0,toolCollectionPreviewItems).map(jsonRecord).filter((step):step is JsonRecord=>!!step):[]
  const planSummary=textValue(payload.goal)||textValue(planSteps.find(step=>textValue(step.status)==='in_progress'||textValue(step.status)==='blocked')?.title)
	const genericArgumentSummary=executionTool||sshTaskOperation?'':toolArgumentSummary(entry.tool,toolArguments)
	const webTool=entry.tool==='web_search'||entry.tool==='web_extract'
	const webSummary=webTool?textValue(payload.query):''
	const operation=filePath||(script?t('tool.shellScript'):program||genericArgumentSummary||eventToolLabel||t('tool.result'))
  const env=request?jsonRecord(request.env):undefined
	const rawStdout=shellOperation&&(shellAction==='input'||shellAction==='output')?(shellChunks.length?shellChunkStdout:shellOutput):textValue(payload.stdout)||textValue(resultPayload?.stdout)||entry.liveStdout||run?.stdout_redacted||''
	const stdout=change?cleanFileChangeOutput(rawStdout):rawStdout
	const workspaceDirectoryEntries=useMemo(()=>entry.tool==='workspace_file_list'?parseWorkspaceDirectoryOutput(stdout):undefined,[entry.tool,stdout])
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
		const commandSummary=transferSummary||(fileSearchMode?`${fileTarget} · ${searchMatchModeLabel} pattern=${JSON.stringify(searchPattern)}`:filePath)||program||(script?compactScript(script):'')||planSummary||(sshTaskOperation?sshTaskID:'')||genericArgumentSummary||webSummary||operation
	const summaryLabel=eventToolLabel||entry.tool||t('common.functions')
	const historyRuns=[...recordArray(payload.runs),...recordArray(resultPayload?.runs)].slice(0,toolCollectionPreviewItems)
	const historyHostIDs=[...new Set(historyRuns.map(item=>textValue(item.host_id)).filter(Boolean))]
	const listedHosts=[...recordArray(payload.hosts),...recordArray(resultPayload?.hosts)].slice(0,toolCollectionPreviewItems)
	const targets:ToolTarget[]=[]
	if(!fileTransfer&&workspaceTool&&workspaceID){
		targets.push({kind:'workspace',label:t('common.workspace'),name:workspaceID})
	}else if(!fileTransfer&&hostID){
		targets.push({kind:'host',label:t('tool.targetHost'),name:destinationHost.name,id:hostID})
	}else if(!fileTransfer&&workspaceID){
		targets.push({kind:'workspace',label:t('common.workspace'),name:workspaceID})
	}else if(entry.tool==='ssh_history'&&historyHostIDs.length>0){
		for(const historyHostID of historyHostIDs.slice(0,3)){const historyHost=hostIdentity(hosts,historyHostID);targets.push({kind:'host',label:t('tool.historyHost'),name:historyHost.name,id:historyHost.id})}
		if(historyHostIDs.length>3)targets.push({kind:'scope',label:t('tool.historyHost'),name:t('tool.moreHosts',{count:historyHostIDs.length-3})})
	}else if(entry.tool==='ssh_host_list'){
		targets.push({kind:'scope',label:t('tool.scope'),name:t('tool.allHosts',{count:listedHosts.length||hosts.length})})
	}
  const instruction=textValue(payload.operator_instruction)||textValue(taskPayload?.operator_instruction)||textValue(resultPayload?.operator_instruction)
  const rawPayload={...payload};delete rawPayload._display
		const firstSeenAt=useRef(Date.now())
		const liveTaskStartedAt=textValue(currentLiveSSHTask?.task?.started_at)
		const persistedStartedAt=run?.started_at?Date.parse(run.started_at):liveTaskStartedAt?Date.parse(liveTaskStartedAt):entry.startedAt
		const startedAt=Number.isFinite(persistedStartedAt)?persistedStartedAt!:firstSeenAt.current
		const [now,setNow]=useState(Date.now())
		useEffect(()=>{
		if(!chatVisible||!toolLive)return
		setNow(Date.now())
		const timer=window.setInterval(()=>setNow(Date.now()),1000)
		return()=>window.clearInterval(timer)
		},[chatVisible,toolLive])
		const elapsed=formatLiveDuration(Math.max(0,Math.floor((now-startedAt)/1000)))
		const resultExitCode=resultPayload?.exit_code
	const exitCode=typeof payload.exit_code==='number'?payload.exit_code:typeof resultExitCode==='number'?resultExitCode:run?.exit_code??'—'
	const duration=formatDuration(payload.duration??resultPayload?.duration,run)
	const autoApproved=payload.auto_approved===true||resultPayload?.auto_approved===true||runAutoApproved(run)
	const permission=executionPermission(request,hosts,destinationHostID,...(sshTransfer?[sourceHostID]:[]))
	const purpose=request?textValue(request.reason):''
	const toggleExpanded=(summary:HTMLElement)=>{
		const opening=disclosure.toggle(summary)
		if(!opening||!initialEntry.contentTruncated||!initialEntry.sourceMessageId||loadingDetail||fullContent)return
		setLoadingDetail(true);setDetailError('')
		void api.chatMessage(sessionID,initialEntry.sourceMessageId).then(message=>setFullContent(message.content)).catch(error=>setDetailError(errorText(error))).finally(()=>setLoadingDetail(false))
	}
		  return <details className={`tool-event tool-event-rich ${sshTaskOperation?'ssh-task-tool ':''}${status}`} open={disclosure.expanded} onTransitionEnd={disclosure.finishTransition}>
			<summary onClick={event=>{event.preventDefault();toggleExpanded(event.currentTarget)}}><div className="tool-summary-icon">{toolSummaryIcon(entry.tool)}</div><div className="tool-summary-copy"><div className="tool-summary-heading"><b>{summaryLabel}</b>{sshTransfer?<ToolTransferRoute sourceHost={sourceHostName} sourcePath={sourcePath} destinationHost={hostName} destinationPath={remotePath}/>:workspaceTransferRoute?<WorkspaceTransferRoute {...workspaceTransferRoute}/>:<>{targets.length>0&&<div className="tool-summary-targets">{targets.map((target,index)=><span className={`tool-target-chip tool-target-${target.kind}`} title={`${target.label}: ${[target.name,target.id].filter(Boolean).join(' · ')}`} key={`${target.kind}_${target.id||target.name}_${index}`}>{target.kind==='host'?<Server size={11}/>:target.kind==='workspace'?<FolderOpen size={11}/>:<ListChecks size={11}/>} {(targets.length>1||target.kind==='scope')&&<em>{target.label}</em>}<b>{target.name||target.id}</b></span>)}</div>}{commandSummary!==summaryLabel&&<code className={sshTaskOperation?'tool-task-id':undefined} title={previewText(commandSummary)}>{previewText(commandSummary)}</code>}</>}</div></div><div className="tool-summary-statuses">{loadingDetail&&<LoaderCircle className="spin" size={12}/>} {autoApproved&&<span className="auto-approved"><ShieldCheck size={11}/>{t('approval.autoApproved')}</span>}<span className={`tool-status ${status}`} key={status}>{t(`statusLabels.${status}`,{defaultValue:status.replaceAll('_',' ')})}</span></div><ChevronRight className="tool-summary-chevron" size={14}/>{request&&<div className="tool-summary-meta"><span className={`tool-summary-permission ${permission}`}><em>{t('tool.permission')}</em><b>{permission}</b></span>{purpose&&<span className="tool-summary-purpose" title={purpose}><em>{t('tool.reason')}</em><b>{purpose}</b></span>}</div>}{toolLive&&<div className={`tool-live-progress ${transferTotal>0?'determinate':''}`} role="progressbar" aria-valuemin={transferTotal>0?0:undefined} aria-valuemax={transferTotal>0?transferTotal:undefined} aria-valuenow={transferTotal>0?transferred:undefined}><i><em style={transferTotal>0?{width:`${transferPercent}%`}:undefined}/></i><span>{transferTotal>0?`${formatFileSize(transferred)} / ${formatFileSize(transferTotal)}`:sshTaskOperation?t('tool.liveTask'):entry.liveOutputStream?.toUpperCase()||''}</span><time>{elapsed}</time></div>}{outputPreview&&<div className={`tool-summary-preview ${previewStream==='stderr'?'stderr':''}`}><span>{shellAction==='output'?shellActionLabel:(previewStream||'stdout').toUpperCase()}</span><pre>{outputPreview}</pre></div>}</summary>
		{disclosure.renderBody&&<div className="tool-event-body">
		  {detailError&&<div className="tool-detail-error">{detailError}</div>}
		  {shellPrimaryAction&&<section className="tool-command-pane"><div className="tool-command-head"><span>{shellActionLabel}</span></div><div className="tool-command-block"><CopyButton value={shellPrimaryContent||'—'}/><pre><HighlightedCode code={previewText(shellPrimaryContent||'—',toolOutputPreviewChars)} language={inferScriptLanguage(shellPrimaryContent||'')}/></pre></div></section>}
		  {shellOutputAction&&!shellChunks.length&&<section className="tool-command-pane"><div className="tool-command-head"><span>{shellActionLabel}</span></div><div className="tool-command-block"><CopyButton value={shellOutput||'—'}/><pre><HighlightedCode code={previewText(shellOutput||'—',toolOutputPreviewChars)} autoDetect live={toolLive}/></pre></div></section>}
		  {!executionTool&&!sshTaskOperation&&toolArguments&&hasRecordEntries(toolArguments)&&<CompactTable title={t('tool.actualParameters')} columns={[t('tool.parameter'),t('tool.value')]} rows={limitedRecordEntries(toolArguments).entries.map(([key,value])=>[key,safeToolArgument(value,key)])}/>}
      {request?<section className="tool-command-pane">
		  <div className="tool-command-head"><span>{shellOperation?t('sshShell.title'):tunnelOperation?t('tunnels.forwarding'):structuredFileOperation?t(fileSearchMode?'tool.searchOperation':'tool.readOperation'):filePath?t('tool.fileOperation'):script?t('tool.fullScript'):t('tool.fullCommand')}</span>{workspaceShellBackend&&<em><TerminalSquare size={12}/>{workspaceShellBackend==='host'?t('approval.hostShell'):'Bubblewrap'}</em>}</div>
			  <div className="tool-command-block"><CopyButton value={script||(program?()=>fullProgram(request,true):commandSummary)}/>{shellOperation?<pre>{shellSummary}</pre>:tunnelOperation?<pre>{tunnelRoute||requestMode}</pre>:workspaceUpload?<pre>workspace_upload {workspaceID}:{relativePath} → {hostName}:{remotePath}</pre>:workspaceDownload?<pre>workspace_download {hostName}:{remotePath} → {workspaceID}:{relativePath}</pre>:sshTransfer?<pre>{sourceHostName}:{sourcePath} → {hostName}:{remotePath}</pre>:structuredFileOperation?<pre>{fileSearchMode?'search':'read'} {fileTarget}</pre>:filePath?<pre>{requestMode} {workspaceID?`${workspaceID}:`:''}{filePath}</pre>:script?<pre><HighlightedCode code={previewText(script,toolOutputPreviewChars)} language={inferScriptLanguage(script)}/></pre>:program?<pre><span className="prompt-sign">$</span> <HighlightedCode code={previewText(program)} language="bash"/></pre>:<pre>{requestMode} {remotePath}</pre>}</div>
		  {change&&textValue(change.diff)&&<DiffViewer change={change}/>}
		  {env&&hasRecordEntries(env)&&<CompactTable title={t('tool.environment')} columns={[t('tool.key'),t('tool.value')]} rows={recordTableRows(env)}/>}
      </section>:!sshTaskOperation&&!shellPrimaryAction&&!shellOutputAction&&(webTool?<WebToolResult tool={entry.tool!} payload={payload}/>:<GenericToolResult payload={payload}/>)}
	  {fileSearchMode&&searchResult&&<div className={`file-search-result ${searchFound?'found':'empty'}`}><Search size={15}/><div><b>{t(searchFound?'tool.searchMatched':'tool.searchNoMatches')}</b><span>{searchMatchModeLabel} · {searchPattern}</span></div></div>}
	  {(textValue(payload.message)||textValue(payload.next_action))&&<div className={`tool-guidance ${payload.ok===false||['failed','denied','interrupted'].includes(status)?'error':''}`}><ShieldAlert size={15}/><div><b>{textValue(payload.code)||t('tool.result')}</b>{textValue(payload.message)&&<p>{textValue(payload.message)}</p>}{textValue(payload.next_action)&&<small>{t('common.next')} · {textValue(payload.next_action)}</small>}</div></div>}
	  {instruction&&<div className="tool-instruction"><ShieldAlert size={15}/><div><b>{t('tool.operatorInstruction')}</b><p>{instruction}</p></div></div>}
	  {fileTransfer&&transferTotal>0&&<div className="file-transfer-progress" role="progressbar" aria-valuemin={0} aria-valuemax={transferTotal} aria-valuenow={transferred}><div><span>{t('tool.transferProgress')}</span><b>{formatFileSize(transferred)} / {formatFileSize(transferTotal)}</b></div><i><em style={{width:`${transferPercent}%`}}/></i></div>}
	  {workspaceDirectoryEntries!==undefined?<div className="tool-output-grid"><WorkspaceDirectoryOutput entries={workspaceDirectoryEntries} content={stdout}/>{stderr&&<ToolOutputPanel kind="stderr" label={outputLabel(t('tool.stderrResult'),stderrOmitted)} content={stderr} live={toolLive}/>}</div>:shellOperation&&shellChunks.length>0?<ShellOutputChunks chunks={shellChunks} live={toolLive}/>:((stdout&&(!shellOutputAction||shellChunks.length>0))||stderr)&&<div className="tool-output-grid">{stdout&&(!shellOutputAction||shellChunks.length>0)&&<ToolOutputPanel kind="stdout" label={outputLabel('STDOUT',stdoutOmitted)} content={stdout} live={toolLive} language={fileReadMode?languageFromPath(filePath):undefined}/>} {stderr&&<ToolOutputPanel kind="stderr" label={outputLabel(t('tool.stderrResult'),stderrOmitted)} content={stderr} live={toolLive}/>}</div>}
	  {(request||sshTaskOperation)&&(exitCode!=='—'||duration!=='—'||waitDeadlineReached||shellHasMore||sshTaskOperation&&(!!runID||numberValue(payload.stdout_total_bytes)>0||numberValue(payload.stderr_total_bytes)>0))&&<aside className="tool-context-pane"><dl className="tool-context-grid">{sshTaskOperation&&runID&&<div><dt>{t('tool.runId')}</dt><dd>{runID}</dd></div>}{exitCode!=='—'&&<div><dt>{t('tool.exitCode')}</dt><dd>{exitCode}</dd></div>}{duration!=='—'&&<div><dt>{t('tool.duration')}</dt><dd>{duration}</dd></div>}{sshTaskOperation&&numberValue(payload.stdout_total_bytes)>0&&<div><dt>STDOUT</dt><dd>{formatFileSize(numberValue(payload.stdout_total_bytes))}</dd></div>}{sshTaskOperation&&numberValue(payload.stderr_total_bytes)>0&&<div><dt>STDERR</dt><dd>{formatFileSize(numberValue(payload.stderr_total_bytes))}</dd></div>}{(waitDeadlineReached||shellHasMore)&&<div><dt>{t('common.status')}</dt><dd>{waitDeadlineReached?t('tool.waitDeadline'):t('tool.moreOutput')}</dd></div>}</dl></aside>}
	  <LazyJSONDetails value={rawPayload}/>
    </div>}
  </details>
}

function ToolOutputPanel({kind,label,content,live,language}:{kind:'stdout'|'stderr';label:string;content:string;live:boolean;language?:string}){
	const outputRef=useRef<HTMLPreElement>(null)
	const stickToBottom=useRef(true)
	const preview=previewText(content,toolOutputPreviewChars)
	useEffect(()=>{
		const output=outputRef.current
		if(live&&output&&stickToBottom.current)output.scrollTop=output.scrollHeight
	},[preview,live])
	return <div className={`tool-output ${kind} ${live?'live':''}`}><span>{label}</span><div className="tool-output-frame"><CopyButton value={content}/><pre ref={outputRef} onScroll={event=>{const output=event.currentTarget;stickToBottom.current=output.scrollHeight-output.scrollTop-output.clientHeight<32}}><HighlightedCode code={preview} language={language} autoDetect live={live}/></pre></div></div>
}

function WorkspaceDirectoryOutput({entries,content}:{entries:ToolWorkspaceDirectoryEntry[];content:string}){
	const {t}=useTranslation()
	const visible=entries.slice(0,toolCollectionPreviewItems),omitted=entries.length-visible.length
	return <div className="tool-output workspace-directory-output"><span>STDOUT</span><div className="tool-output-frame"><CopyButton value={content}/><div className="tool-directory-list">{visible.length?visible.map(entry=><div className={`tool-directory-entry ${entry.type}`} key={`${entry.type}:${entry.name}`}>{entry.type==='directory'?<FolderOpen size={14}/>:<FileText size={14}/>}<b title={entry.name}>{entry.name}</b><small>{entry.type==='directory'?t('workspace.directory'):formatFileSize(entry.size||0)}</small></div>):<div className="tool-directory-empty">{t('workspace.emptyDirectory')}</div>}{omitted>0&&<div className="tool-directory-omitted">{t('tool.previewItemsOmitted',{count:omitted})}</div>}</div></div></div>
}

function ShellOutputChunks({chunks,live}:{chunks:JsonRecord[];live:boolean}){
	const {t}=useTranslation()
	const visible=chunks.slice(-toolCollectionPreviewItems)
	return <div className="shell-output-chunks">{visible.map((chunk,index)=>{
		const stream=textValue(chunk.stream)==='stderr'?'stderr':'stdout'
		return <ToolOutputPanel key={`${numberValue(chunk.first_sequence)||numberValue(chunk.sequence)}_${index}`} kind={stream} label={stream==='stderr'?t('tool.stderrResult'):'STDOUT'} content={textValue(chunk.content)} live={live}/>
	})}</div>
}

function LazyJSONDetails({value}:{value:JsonRecord}){
	const {t}=useTranslation()
	const [open,setOpen]=useState(false)
	const formatted=open?JSON.stringify(previewStructuredValue(value),null,2):''
	return <details className="tool-raw" open={open} onToggle={event=>setOpen(event.currentTarget.open)}><summary>{t('tool.rawJson')}</summary>{open&&<CopyablePre value={()=>JSON.stringify(value,null,2)}><HighlightedCode code={previewText(formatted,toolOutputPreviewChars)} language="json"/></CopyablePre>}</details>
}

function CompactTable({title,columns,rows}:{title:string;columns:string[];rows:Array<Array<unknown>>}){
  const visibleRows=rows.slice(0,toolCollectionPreviewItems)
  if(rows.length>visibleRows.length)visibleRows.push([i18n.t('tool.previewItemsOmitted',{count:rows.length-visibleRows.length})])
  return <div className="tool-compact-table"><span>{title}</span><div className="tool-table-scroll"><table><thead><tr>{columns.map(column=><th key={column}>{column}</th>)}</tr></thead><tbody>{visibleRows.map((row,index)=><tr key={index}>{row.map((value,column)=><td key={column}>{displayValue(value)}</td>)}</tr>)}</tbody></table></div></div>
}

function displayValue(value:unknown,depth=0):string{
  if(value===null||value===undefined||value==='')return'—'
	if(typeof value==='string')return previewText(value)
	if(depth>=4)return'…'
  if(Array.isArray(value)){
		const visible=value.slice(0,toolCollectionPreviewItems).map(item=>displayValue(item,depth+1))
		if(value.length>visible.length)visible.push(i18n.t('tool.previewItemsOmitted',{count:value.length-visible.length}))
		return previewText(visible.join(', '))
	}
  const record=jsonRecord(value)
  if(record){
		const {entries,truncated}=limitedRecordEntries(record),visible=entries.map(([key,item])=>`${key}=${displayValue(item,depth+1)}`)
		if(truncated)visible.push(i18n.t('tool.moreItemsOmitted'))
		return previewText(visible.join(' · '))
	}
  return String(value)
}

function safeToolArgument(value:unknown,key='',depth=0):unknown{
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

function toolArgumentSummary(toolName:string|undefined,argumentsValue:JsonRecord|undefined){
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

function formatLiveDuration(seconds:number){
	if(seconds<60)return`${seconds}s`
	const minutes=Math.floor(seconds/60)
	return`${minutes}m ${String(seconds%60).padStart(2,'0')}s`
}

function GenericToolResult({payload}:{payload:JsonRecord}){
  const hidden=new Set(['_display','stdout','stderr','operator_instruction','ok','status','code','message','next_action','run_id','duration','exit_code','auto_approved','tasks'])
	const entries=limitedRecordEntries(payload).entries.filter(([key])=>!hidden.has(key))
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

function publicWebLink(value:string){
	try{
		const parsed=new URL(value),host=parsed.hostname.toLowerCase().replace(/\.$/,'')
		if(!['http:','https:'].includes(parsed.protocol)||parsed.username||parsed.password||host==='localhost'||host.endsWith('.localhost')||host.endsWith('.local')||host.endsWith('.internal'))return
		const ipv4=host.match(/^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/)?.slice(1).map(Number)
		if(ipv4&&(ipv4.some(part=>part>255)||ipv4[0]===10||ipv4[0]===127||ipv4[0]===0||ipv4[0]===169&&ipv4[1]===254||ipv4[0]===172&&ipv4[1]>=16&&ipv4[1]<=31||ipv4[0]===192&&ipv4[1]===168))return
		if(host==='[::1]'||host==='::1'||/^\[?f[cd]/.test(host)||/^\[?fe[89ab]/.test(host))return
		return parsed
	}catch{return}
}

function WebToolResult({tool,payload}:{tool:string;payload:JsonRecord}){
	const {t}=useTranslation()
	const results=recordArray(payload.results)
	const failures=recordArray(payload.failed_results)
	const responseTime=numberValue(payload.response_time)
	const credits=numberValue(payload.credits)
	const omitted=numberValue(payload.omitted_results)
	return <div className="web-tool-result">
		{results.length>0&&<div className="web-source-list">{results.map((result,index)=>{
			const parsed=publicWebLink(textValue(result.url))
			const content=textValue(result.content)||textValue(result.raw_content)
			const title=textValue(result.title)||parsed?.hostname||textValue(result.url)
			const truncated=result.truncated===true
			return <article className="web-source-card" key={`${textValue(result.url)}_${index}`}>
				<header><div><b>{title}</b><span>{parsed?.hostname||textValue(result.url)}</span></div>{parsed&&<a href={parsed.href} target="_blank" rel="noreferrer noopener" title={t('webSearch.openSource')} aria-label={t('webSearch.openSource')}><ExternalLink size={14}/></a>}</header>
				{content&&<p>{previewText(content,tool==='web_search'?2<<10:8<<10)}</p>}
				{(textValue(result.published_date)||numberValue(result.score)>0||truncated)&&<footer>{textValue(result.published_date)&&<time>{textValue(result.published_date)}</time>}{numberValue(result.score)>0&&<span>{Math.round(numberValue(result.score)*100)}%</span>}{truncated&&<em>{t('webSearch.truncated')}</em>}</footer>}
			</article>
		})}</div>}
		{(responseTime>0||credits>0||textValue(payload.request_id))&&<div className="web-tool-meta">{responseTime>0&&<span>{responseTime.toFixed(2)}s</span>}{credits>0&&<span>{t('webSearch.credits',{count:credits})}</span>}{textValue(payload.request_id)&&<code title={textValue(payload.request_id)}>{textValue(payload.request_id)}</code>}</div>}
		{failures.length>0&&<section className="web-source-failures"><b>{t('webSearch.failures')}</b>{failures.map((failure,index)=>{const parsed=publicWebLink(textValue(failure.url));return <div key={`${textValue(failure.url)}_${index}`}><span>{parsed?.hostname||textValue(failure.url)}</span><small>{textValue(failure.error)}</small></div>})}</section>}
		{omitted>0&&<div className="web-tool-omitted">{t('webSearch.omitted',{count:omitted})}</div>}
	</div>
}

function StructuredArray({label,values}:{label:string;values:unknown[]}){
	const visible=values.slice(0,toolCollectionPreviewItems)
  const records=visible.map(jsonRecord).filter((item):item is JsonRecord=>!!item)
	if(records.length===visible.length&&records.length>0){const columns=[...new Set(records.flatMap(record=>Object.keys(record)))].slice(0,10);return <CompactTable title={`${label.replaceAll('_',' ')} · ${values.length} ITEMS`} columns={columns.map(column=>column.replaceAll('_',' '))} rows={records.map(record=>columns.map(column=>record[column]))}/>}
	return <div className="tool-array-section"><span>{label.replaceAll('_',' ')}</span><div>{visible.map((value,index)=><code key={index}>{displayValue(value)}</code>)}{values.length>visible.length&&<code>{i18n.t('tool.previewItemsOmitted',{count:values.length-visible.length})}</code>}</div></div>
}

function StructuredObject({label,value}:{label:string;value:JsonRecord}){
	const {entries,truncated}=limitedRecordEntries(value)
  return <section className="tool-object-section"><h4>{label.replaceAll('_',' ')}</h4><dl className="tool-generic-grid">{entries.map(([key,item])=><div key={key}><dt>{key.replaceAll('_',' ')}</dt><dd>{displayValue(item)}</dd></div>)}{truncated&&<div><dt>…</dt><dd>{i18n.t('tool.moreItemsOmitted')}</dd></div>}</dl></section>
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
  dismissApproval,
  onApproved,
  onNotice,
}: {
  approval: Approval;
  pendingCount: number;
  hosts: Host[];
  running: boolean;
  stopping: boolean;
  onStop: () => void;
  dismissApproval: (approvalID: string) => void;
  onApproved: (result: ApprovalExecutionResult) => void;
  onNotice: (message: string) => void;
}) {
  const { t } = useTranslation();
  const [note, setNote] = useState("");
  const [decisionBusy, setDecisionBusy] = useState<
    "" | "once" | "reject"
  >("");
  const [explanationBusy, setExplanationBusy] = useState(false);
  const [error, setError] = useState("");
  const [requestExpanded, setRequestExpanded] = useState(false);
  let request: Record<string, unknown> = {};
  try {
    request = JSON.parse(approval.request_json);
  } catch {
    request = { request: approval.request_json };
  }
  const script = textValue(request.script);
  const program = fullProgram(request);
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
  const rootExecution = executionPermission(request, hosts, approval.host_id, ...(sshTransfer ? [sourceHostID] : [])) === "root";
  const actionKind = script
    ? t("approval.actionScript")
    : t("approval.actionCommand");
  const approvalTitle = fileReadApproval
    ? rootExecution
      ? t(fileSearchApproval ? "approval.sudoSearchTitle" : "approval.sudoReadTitle")
      : t(fileSearchApproval ? "approval.searchTitle" : "approval.readTitle")
    : tunnelApproval
      ? t("approval.tunnelTitle")
    : interactiveShellApproval
      ? t("approval.sshShellTitle")
    : rootExecution
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
    ? rootExecution
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
    : rootExecution
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
  const tunnelDirection = textValue(request.direction) || "local";
  const tunnelLocalHost = textValue(request.local_host) || "127.0.0.1";
  const tunnelLocalPort = numberValue(request.local_port);
  const tunnelRemoteHost = textValue(request.remote_host);
  const tunnelRemotePort = numberValue(request.remote_port);
  const tunnelOperation = sshTunnelRoute(targetHost,tunnelDirection,tunnelLocalHost,tunnelLocalPort,tunnelRemoteHost,tunnelRemotePort,t('tunnels.automaticPort'));
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
    : program ||
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
  const executionIdentity = rootExecution ? "root" : "user";
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
      dismissApproval(approval.id);
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
      dismissApproval(approval.id);
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
    } catch (err) {
      setError(errorText(err));
    } finally {
      setExplanationBusy(false);
    }
  };
  const decisionDisabled = !!decisionBusy;
  return createPortal(
    <div className="approval-modal-backdrop">
      <section
        className={`approval-dialog ${rootExecution ? "elevated" : ""}`}
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
            {rootExecution && (
              <em>
                <ShieldAlert size={12} />
                root
              </em>
            )}
          </span>
          {rootExecution && (
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
          {change&&textValue(change.diff)?<DiffViewer change={change}/>:<CopyablePre value={script||(program&&operation===program?()=>fullProgram(request,true):operation)} preClassName="approval-command-preview"><HighlightedCode code={previewText(script || `${tunnelApproval||interactiveShellApproval?'':'$ '}${operation}`,toolOutputPreviewChars)} language={script?inferScriptLanguage(script):program?'bash':undefined} autoDetect/></CopyablePre>}
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
        <details className="approval-request-detail" open={requestExpanded} onToggle={event=>setRequestExpanded(event.currentTarget.open)}>
          <summary>{t("approval.requestDetails")}</summary>
          {requestExpanded&&<CopyablePre value={()=>JSON.stringify(request,null,2)}><HighlightedCode code={JSON.stringify(previewStructuredValue(request),null,2)} language="json"/></CopyablePre>}
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
                  : rootExecution
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
    </div>,
    document.body,
  );
}

function AuditRunDetail({run,req,hosts}:{run:Run;req:JsonRecord;hosts:Host[]}){
	const {t}=useTranslation()
	const [requestExpanded,setRequestExpanded]=useState(false)
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
	const permission=executionPermission(req,hosts,run.host_id,...(sshTransfer?[textValue(req.source_host_id)]:[]))
	const rootExecution=permission==='root'
	const tunnelMode=mode==='ssh_tunnel_start'
	const shellMode=mode==='ssh_shell_start'||mode==='workspace_shell_start'
	const shellModeLabel=mode==='ssh_shell_start'?`SSH Shell · ${t('sshShell.toolActions.start')}`:`Workspace Shell · ${t('sshShell.toolActions.start')}`
	const tunnelRoute=tunnelMode?sshTunnelRoute(destinationHost.name||destinationHost.id,textValue(req.direction)||'local',textValue(req.local_host)||'127.0.0.1',numberValue(req.local_port),textValue(req.remote_host),numberValue(req.remote_port),t('tunnels.automaticPort')):''
	const shellTarget=`${mode==='workspace_shell_start'?`${workspaceID}:${textValue(req.cwd)||'.'}`:destinationHost.name||destinationHost.id} · PTY`
	const fileTarget=`${workspaceID?`${workspaceID}:`:''}${filePath}`
	const commandText=shellMode?shellTarget:tunnelMode?tunnelRoute:workspaceUpload?`workspace_upload ${workspaceID}:${relativePath} → ${destinationHost.name||destinationHost.id}:${remotePath}`:workspaceDownload?`workspace_download ${destinationHost.name||destinationHost.id}:${remotePath} → ${workspaceID}:${relativePath}`:sshTransfer?`${[sourceHost.name||sourceHost.id,sourcePath].filter(Boolean).join(':')} → ${destinationHost.name||destinationHost.id}:${remotePath}`:searchMode||readMode?`${searchMode?'search':'read'} ${fileTarget}`:script?script:program?program:filePath?`${mode} ${fileTarget}`:toolLabel(run.tool_name||'')
	return <div className="audit-run-detail">
		<div className="audit-run-primary">
			<section className="audit-operation-pane">
				<div className="tool-command-head"><span>{shellMode?shellModeLabel:tunnelMode?t('tunnels.forwarding'):searchMode?t('tool.searchOperation'):readMode?t('tool.readOperation'):workspaceTransfer||sshTransfer||filePath?t('tool.fileOperation'):script?t('tool.fullScript'):t('tool.fullCommand')}</span>{(workspaceShellBackend||rootExecution)&&<div className="audit-operation-badges">{workspaceShellBackend&&<em><TerminalSquare size={12}/>{workspaceShellBackend==='host'?t('approval.hostShell'):'Bubblewrap'}</em>}{rootExecution&&<em><ShieldAlert size={12}/>root</em>}</div>}</div>
				<div className="tool-command-block"><CopyButton value={program&&commandText===program?()=>fullProgram(req,true):commandText}/><pre>{program&&commandText===program?<><span className="prompt-sign">$</span> <HighlightedCode code={previewText(program)} language="bash"/></>:<HighlightedCode code={previewText(commandText,toolOutputPreviewChars)} language={script?inferScriptLanguage(script):undefined} autoDetect/>}</pre></div>
				{change&&textValue(change.diff)&&<DiffViewer change={change}/>}
			</section>
			<aside className="audit-run-context">
				<dl className="audit-run-facts">
					<div><dt>{workspaceID&&!sshTransfer?t('common.workspace'):t('tool.targetHost')}</dt><dd>{workspaceID&&!sshTransfer?workspaceID:[destinationHost.name,destinationHost.id].filter(Boolean).join(' · ')||'—'}</dd></div>
					{sshTransfer&&<div><dt>{t('tool.sourceHost')}</dt><dd>{[sourceHost.name,sourceHost.id].filter(Boolean).join(' · ')||'—'}</dd></div>}
					<div><dt>{tunnelMode?t('tunnels.remoteEndpoint'):filePath?t('tool.filePath'):t('tool.workingDirectory')}</dt><dd>{tunnelMode?`${textValue(req.remote_host)}:${numberValue(req.remote_port)}`:filePath||textValue(req.cwd)||t('tool.defaultDirectory')}</dd></div>
					<div><dt>{t('tool.permission')}</dt><dd>{permission}</dd></div>
					<div><dt>{t('tool.duration')}</dt><dd>{formatDuration(undefined,run)}</dd></div>
				</dl>
				{textValue(req.reason)&&<div className="audit-run-purpose"><span>{t('tool.reason')}</span><p>{textValue(req.reason)}</p></div>}
			</aside>
		</div>
		{(run.stdout_redacted||run.stderr_redacted||run.error)&&<div className="tool-output-grid">{run.stdout_redacted&&<ToolOutputPanel kind="stdout" label="STDOUT · REDACTED" content={run.stdout_redacted} live={false} language={readMode?languageFromPath(filePath):undefined}/>} {run.stderr_redacted&&<ToolOutputPanel kind="stderr" label="STDERR · REDACTED" content={run.stderr_redacted} live={false}/>} {run.error&&!run.stderr_redacted&&<ToolOutputPanel kind="stderr" label={t('common.error')} content={run.error} live={false}/>}</div>}
		<details className="audit-request-detail" open={requestExpanded} onToggle={event=>setRequestExpanded(event.currentTarget.open)}>
			<summary><Braces size={14}/><span>{t('tool.normalizedRequest')}</span><ChevronRight size={14}/></summary>
			{requestExpanded&&<div className="audit-request-detail-body">
				<dl className="audit-request-meta"><div><dt>{t('common.operation')}</dt><dd>{toolLabel(run.tool_name||'')}</dd></div><div><dt>{t('tool.runId')}</dt><dd>{run.id}</dd></div></dl>
				{env&&hasRecordEntries(env)&&<CompactTable title={t('tool.environment')} columns={[t('tool.key'),t('tool.value')]} rows={recordTableRows(env)}/>}
				<CopyablePre value={()=>JSON.stringify(req,null,2)}><HighlightedCode code={JSON.stringify(previewStructuredValue(req),null,2)} language="json"/></CopyablePre>
			</div>}
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
		case'ssh_tunnel_start':return sshTunnelRoute(destinationName,textValue(req.direction)||'local',textValue(req.local_host)||'127.0.0.1',numberValue(req.local_port),textValue(req.remote_host),numberValue(req.remote_port),t('tunnels.automaticPort'))
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

const AuditRunRow=memo(function AuditRunRow({run,hosts}:{run:Run;hosts:Host[]}){
	const {t,i18n:instance}=useTranslation()
	const [detail,setDetail]=useState<Run|null>(null)
	const [loading,setLoading]=useState(false)
	const [error,setError]=useState('')
	const req=requestFromRun(run)||{request:run.request_json}
	const auditHost=hostIdentity(hosts,run.host_id)
	const workspaceID=textValue(req.workspace_id)
	const target=auditHost.name||(run.host_id.startsWith('workspace_')?workspaceID:run.host_id)||'—'
	const operation=auditOperationSummary(req,run,hosts,t)
	const open=async()=>{
		if(detail||loading)return
		setLoading(true);setError('')
		try{setDetail((await api.runDetail(run.id)).run)}
		catch(err){setError(errorText(err))}
		finally{setLoading(false)}
	}
	const resolved=detail||run
	const resolvedRequest=requestFromRun(resolved)||req
	return <details onToggle={event=>{if(event.currentTarget.open)void open()}}>
		<summary className="audit-row">
			<span>{new Date(run.started_at).toLocaleString(localeFor(instance.language))}</span>
			<span className="command">{operation}</span>
			<span className="audit-run-status"><span className={`run-status ${run.status}`}>{t(`statusLabels.${run.status}`,{defaultValue:run.status})}</span>{runAutoApproved(run)&&<span className="auto-approved"><ShieldCheck size={11}/>{t('approval.autoApproved')}</span>}</span>
			<span title={run.host_id}>{target}</span><span>{run.exit_code}</span><ChevronRight className="audit-run-chevron" size={15}/>
		</summary>
		<div className="run-detail">
			{loading?<div className="audit-loading" role="status"><LoaderCircle className="spin" size={16}/><span>{t('common.loading')}</span></div>:error?<div className="inline-error">{error}</div>:detail&&<AuditRunDetail run={resolved} req={resolvedRequest} hosts={hosts}/>}
		</div>
	</details>
})

type AuditDeleteTarget={kind:'session';id:string;title:string}|{kind:'all'}

type AuditPageProps={view:AuditView;onViewChange:(view:AuditView)=>void;mcpRefreshKey:number;runs:Run[];hosts:Host[];sessions:ChatSession[];ready:boolean;error:string;runsHasMore:boolean;loadingMore:boolean;onLoadMoreRuns:()=>Promise<string[]>;onDeleteRuns:(sessionID?:string|null)=>Promise<AuditRunDeleteResult>}

function AuditPage({view,onViewChange,mcpRefreshKey,...props}:AuditPageProps){
	const {t}=useTranslation()
	return <div className="audit-page page-stack">
		<div className="audit-view-tabs" role="tablist" aria-label={t('audit.views')}>
			<button type="button" role="tab" aria-selected={view==='runs'} className={view==='runs'?'active':''} onClick={()=>onViewChange('runs')}><History size={15}/>{t('audit.runHistory')}</button>
			<button type="button" role="tab" aria-selected={view==='mcp'} className={view==='mcp'?'active':''} onClick={()=>onViewChange('mcp')}><Activity size={15}/>{t('audit.mcpActivity')}</button>
		</div>
		{view==='runs'?<AuditRunsView {...props}/>:<MCPActivityView hosts={props.hosts} refreshKey={mcpRefreshKey}/>}
	</div>
}

function AuditRunsView({runs,hosts,sessions,ready,error,runsHasMore,loadingMore,onLoadMoreRuns,onDeleteRuns}:{runs:Run[];hosts:Host[];sessions:ChatSession[];ready:boolean;error:string;runsHasMore:boolean;loadingMore:boolean;onLoadMoreRuns:()=>Promise<string[]>;onDeleteRuns:(sessionID?:string|null)=>Promise<AuditRunDeleteResult>}) {
	const {t,i18n:instance}=useTranslation()
	const [query,setQuery]=useState('')
	const [deleteTarget,setDeleteTarget]=useState<AuditDeleteTarget|null>(null)
	const [deleting,setDeleting]=useState(false)
	const filtered=useMemo(()=>{
		const needle=query.toLowerCase()
		return runs.filter(run=>{
			const req=requestFromRun(run)
			const requestText=req?Object.values(req).flat().filter(value=>typeof value==='string').join('\n'):run.request_json
			return requestText.toLowerCase().includes(needle)
		})
	},[query,runs])
	const groups=useMemo(()=>{
		const titles=new Map(sessions.map(session=>[session.id,session.title]))
		const grouped=new Map<string,Run[]>()
		for(const run of filtered){const key=auditSessionID(run),items=grouped.get(key);if(items)items.push(run);else grouped.set(key,[run])}
		return [...grouped.entries()].map(([id,items])=>{
			items.sort((a,b)=>Date.parse(b.started_at)-Date.parse(a.started_at))
			return{id,title:id===directAuditSessionID?t('audit.direct'):id.startsWith('mcp_sess_')?t('audit.mcpRunGroup'):titles.get(id)||t('audit.missingConversation'),runs:items,latest:items[0]?.started_at,pending:items.filter(run=>run.status==='approval_required').length}
		}).sort((a,b)=>Date.parse(b.latest||'')-Date.parse(a.latest||''))
	},[filtered,sessions,t,instance.language])
	const groupIDs=useMemo(()=>groups.map(group=>group.id),[groups])
	const disclosure=useAuditGroupDisclosure(groupIDs,filtered.length)
	const confirmDelete=async()=>{
		if(!deleteTarget||deleting)return
		setDeleting(true)
		try{
			const result=deleteTarget.kind==='session'?await onDeleteRuns(deleteTarget.id===directAuditSessionID?'':deleteTarget.id):await onDeleteRuns(undefined)
			if(result.scope==='all'){
				const retainedRunIDs=new Set(result.retained_run_ids||[])
				const retainedGroupIDs=new Set(runs.filter(run=>retainedRunIDs.has(run.id)).map(auditSessionID))
				disclosure.forget(groupIDs.filter(id=>!retainedGroupIDs.has(id)))
			}
			else if(result.retained===0)disclosure.forget([deleteTarget.kind==='session'?deleteTarget.id:directAuditSessionID])
			setDeleteTarget(null)
		}catch{/* the application notification channel presents the actionable error */}
		finally{setDeleting(false)}
	}
	const deleteTitle=deleteTarget?.kind==='session'?t('audit.deleteSessionTitle',{title:deleteTarget.title}):t('audit.deleteAllTitle')
	if(!ready)return <div className="audit-loading panel" role="status"><LoaderCircle className="spin" size={16}/><span>{t('common.loading')}</span></div>
	return <div className="audit-runs-view page-stack">
		{error&&<div className="inline-error">{error}</div>}
		<div className="audit-toolbar"><div className="search-box"><Search size={16}/><input aria-label={t('common.search')} value={query} onChange={event=>setQuery(event.target.value)}/></div><span>{t('audit.counts',{sessions:groups.length,runs:filtered.length})}</span>{runs.length>0&&<button type="button" className="audit-clear-button" onClick={()=>setDeleteTarget({kind:'all'})}><Trash2 size={13}/>{t('audit.clear')}</button>}</div>
		<div className="audit-groups">{groups.map(group=><details className="audit-session panel" open={disclosure.expanded.has(group.id)} onToggle={event=>disclosure.setOpen(group.id,event.currentTarget.open)} key={group.id}>
			<summary className="audit-session-summary"><div className="audit-session-glyph"><History size={17}/></div><div className="audit-session-name"><b>{group.title}</b><span>{group.id===directAuditSessionID?t('audit.noSession'):group.id} · {t('audit.lastRun',{date:new Date(group.latest).toLocaleString(localeFor(instance.language))})}</span></div><div className="audit-session-stats">{group.pending>0&&<span className="pending-count"><b>{group.pending}</b> {t('audit.pending')}</span>}<button type="button" className="audit-session-delete danger" onClick={event=>{event.preventDefault();event.stopPropagation();setDeleteTarget({kind:'session',id:group.id,title:group.title})}}><Trash2 size={13}/>{t('audit.deleteSession')}</button></div><ChevronRight className="audit-session-chevron" size={17}/></summary>
			<div className="audit-table"><div className="audit-row audit-head"><span>{t('audit.columns.time')}</span><span>{t('audit.columns.operation')}</span><span>{t('audit.columns.status')}</span><span>{t('audit.columns.host')}</span><span>{t('audit.columns.exit')}</span><span aria-hidden="true"/></div>{group.runs.map(run=><AuditRunRow key={run.id} run={run} hosts={hosts}/>)}</div>
		</details>)}</div>
		{runsHasMore&&<button type="button" className="audit-load-more panel" disabled={loadingMore} onClick={()=>void onLoadMoreRuns().then(disclosure.reveal)}>{loadingMore?<LoaderCircle className="spin" size={14}/>:<History size={14}/>} {t('audit.loadMore')}</button>}
		{!runs.length&&<Empty icon={<History/>} title={t('audit.emptyTitle')}/>}
		{runs.length>0&&!groups.length&&<Empty icon={<Search/>} title={t('audit.noMatch')}/>}
		{deleteTarget&&<DestructiveConfirmDialog title={deleteTitle} busy={deleting} onCancel={()=>setDeleteTarget(null)} onConfirm={()=>void confirmDelete()}/>}
	</div>
}

type MCPCallOutput={stdout:string;stderr:string;transferredBytes:number;totalBytes:number}
const mcpLiveOutputChars=64<<10

function appendMCPOutput(current:string,content:string){
	const next=current+content
	return next.length<=mcpLiveOutputChars?next:next.slice(-mcpLiveOutputChars)
}

function MCPActivityView({hosts,refreshKey}:{hosts:Host[];refreshKey:number}){
	const {t,i18n:instance}=useTranslation()
	const [sessions,setSessions]=useState<MCPClientSession[]>([])
	const [selectedID,setSelectedID]=useState('')
	const [calls,setCalls]=useState<MCPToolCall[]>([])
	const [outputs,setOutputs]=useState<Record<string,MCPCallOutput>>({})
	const [query,setQuery]=useState('')
	const [loading,setLoading]=useState(true)
	const [error,setError]=useState('')
	const selectedIDRef=useRef('')
	const pendingEventsRef=useRef<MCPActivityEvent[]>([])
	const eventFrameRef=useRef(0)
	useEffect(()=>{selectedIDRef.current=selectedID},[selectedID])
	useEffect(()=>{
		let active=true
		api.mcpActivity().then(snapshot=>{if(!active)return;setSessions(snapshot.sessions||[]);setSelectedID(current=>current&&snapshot.sessions.some(session=>session.id===current)?current:snapshot.sessions[0]?.id||'');setError('')}).catch(err=>{if(active)setError(errorText(err))}).finally(()=>{if(active)setLoading(false)})
		return()=>{active=false}
	},[refreshKey])
	useEffect(()=>{
		if(!selectedID){setCalls([]);return}
		let active=true
		api.mcpActivity(selectedID).then(snapshot=>{if(active)setCalls(snapshot.calls||[])}).catch(err=>{if(active)setError(errorText(err))})
		return()=>{active=false}
	},[selectedID])
	const flushEvents=useCallback(()=>{
		eventFrameRef.current=0
		const events=pendingEventsRef.current.splice(0)
		if(!events.length)return
		setSessions(current=>{
			let next=[...current]
			for(const event of events){
				const index=next.findIndex(session=>session.id===event.session_id)
				if(event.type==='call_started'){
					const existing=index>=0?next[index]:undefined
					const session={...(event.session||existing||{id:event.session_id,transport:'',started_at:new Date().toISOString(),last_seen_at:new Date().toISOString(),call_count:0,running_calls:0}),call_count:(existing?.call_count||0)+1,running_calls:(existing?.running_calls||0)+1,last_seen_at:event.call?.started_at||new Date().toISOString()}
					if(index>=0)next[index]=session;else next.push(session)
				}else if(event.type==='call_finished'&&index>=0){
					next[index]={...next[index],running_calls:Math.max(0,next[index].running_calls-1),last_seen_at:event.call?.updated_at||next[index].last_seen_at}
				}
			}
			next.sort((a,b)=>Date.parse(b.last_seen_at)-Date.parse(a.last_seen_at))
			return next
		})
		setCalls(current=>{
			let next=[...current]
			for(const event of events){
				if(event.session_id!==selectedIDRef.current)continue
				const index=next.findIndex(call=>call.id===event.call_id)
				if(event.call){if(index>=0)next[index]=event.call;else next.unshift(event.call)}
				else if(index>=0&&(event.status||event.run_id))next[index]={...next[index],operation_status:event.status||next[index].operation_status,run_id:event.run_id||next[index].run_id,updated_at:new Date().toISOString()}
			}
			return next.sort((a,b)=>Date.parse(b.started_at)-Date.parse(a.started_at))
		})
		setOutputs(current=>{
			let next=current
			for(const event of events){
				if(event.session_id!==selectedIDRef.current||(!event.content&&event.type!=='call_progress'))continue
				if(next===current)next={...current}
				const output=next[event.call_id]||{stdout:'',stderr:'',transferredBytes:0,totalBytes:0}
				next[event.call_id]={...output,
					stdout:event.stream!=='stderr'&&event.content?appendMCPOutput(output.stdout,event.content):output.stdout,
					stderr:event.stream==='stderr'&&event.content?appendMCPOutput(output.stderr,event.content):output.stderr,
					transferredBytes:event.transferred_bytes??output.transferredBytes,totalBytes:event.total_bytes??output.totalBytes}
			}
			return next
		})
	},[])
	useEffect(()=>{
		const unsubscribe=subscribeApplicationEvents<MCPActivitySnapshot|MCPActivityEvent>('mcp_activity',event=>{
			if(event.type==='error'){setError(event.error||t('audit.mcpStreamFailed'));return}
			if(event.type!=='event'||!event.data)return
			if(event.mode==='snapshot'){const snapshot=event.data as MCPActivitySnapshot;setSessions(snapshot.sessions||[]);setSelectedID(current=>current&&snapshot.sessions.some(session=>session.id===current)?current:snapshot.sessions[0]?.id||'');return}
			const activity=event.data as MCPActivityEvent
			pendingEventsRef.current.push(activity);if(!eventFrameRef.current)eventFrameRef.current=window.requestAnimationFrame(flushEvents)
		})
		return()=>{unsubscribe();if(eventFrameRef.current)window.cancelAnimationFrame(eventFrameRef.current);eventFrameRef.current=0;pendingEventsRef.current=[]}
	},[flushEvents,t])
	const selected=sessions.find(session=>session.id===selectedID)
	const visibleCalls=useMemo(()=>{const needle=query.trim().toLowerCase();return needle?calls.filter(call=>`${call.tool_name}\n${call.arguments_json}\n${call.status}\n${call.error||''}`.toLowerCase().includes(needle)):calls},[calls,query])
	if(loading)return <div className="audit-loading panel" role="status"><LoaderCircle className="spin" size={16}/><span>{t('common.loading')}</span></div>
	return <div className="mcp-activity-layout">
		<aside className="mcp-session-list panel">
			<header><span>{t('audit.mcpSessions')}</span><em>{sessions.length}</em></header>
			<div>{sessions.map(session=><button type="button" className={session.id===selectedID?'active':''} onClick={()=>setSelectedID(session.id)} key={session.id}><span><b>{session.client_name||t('audit.mcpClient')}</b>{session.running_calls>0&&<em>{session.running_calls}</em>}</span><code title={session.id}>{session.id}</code><small>{session.transport||'—'} · {t('audit.mcpCalls',{count:session.call_count})} · {new Date(session.last_seen_at).toLocaleString(localeFor(instance.language))}</small></button>)}</div>
			{!sessions.length&&<Empty icon={<Activity/>} title={t('audit.noMCPActivity')}/>}
		</aside>
		<section className="mcp-call-panel">
			<div className="audit-toolbar"><div className="search-box"><Search size={16}/><input aria-label={t('common.search')} value={query} onChange={event=>setQuery(event.target.value)}/></div>{selected&&<span><code>{selected.client_name||t('audit.mcpClient')}</code> · {visibleCalls.length}</span>}</div>
			{error&&<div className="inline-error">{error}</div>}
			<div className="mcp-call-list">{visibleCalls.map(call=><MCPActivityCallCard key={call.id} call={call} output={outputs[call.id]} hosts={hosts}/>)}</div>
			{selected&&!calls.length&&<Empty icon={<Activity/>} title={t('audit.noMCPCalls')}/>}
			{selected&&calls.length>0&&!visibleCalls.length&&<Empty icon={<Search/>} title={t('audit.noMatch')}/>}
			{!selected&&sessions.length>0&&<Empty icon={<Activity/>} title={t('audit.selectMCPSession')}/>}
		</section>
	</div>
}

const MCPActivityCallCard=memo(function MCPActivityCallCard({call,output,hosts}:{call:MCPToolCall;output?:MCPCallOutput;hosts:Host[]}){
	const {t,i18n:instance}=useTranslation()
	const args=parseRecord(call.arguments_json)
	const summary=toolArgumentSummary(call.tool_name,args)
	const progress=output&&output.totalBytes>0?Math.min(100,Math.round(output.transferredBytes/output.totalBytes*100)):0
	return <details className={`mcp-call-card panel ${call.status}`}>
		<summary><span className="mcp-call-icon">{toolSummaryIcon(call.tool_name)}</span><span className="mcp-call-heading"><code>{call.tool_name}</code>{summary&&<small>{summary}</small>}</span><time>{new Date(call.started_at).toLocaleString(localeFor(instance.language))}</time><span className={`run-status ${call.status}`}>{t(`statusLabels.${call.status}`,{defaultValue:call.status})}</span><ChevronRight size={15}/></summary>
		<div className="mcp-call-body">
			<dl className="mcp-call-meta"><div><dt>{t('audit.callId')}</dt><dd><code>{call.id}</code></dd></div>{call.operation_status&&<div><dt>{t('common.status')}</dt><dd>{t(`statusLabels.${call.operation_status}`,{defaultValue:call.operation_status})}</dd></div>}{call.approval_id&&<div><dt>{t('audit.approvalId')}</dt><dd><code>{call.approval_id}</code></dd></div>}{call.task_id&&<div><dt>{t('audit.taskId')}</dt><dd><code>{call.task_id}</code></dd></div>}{call.shell_id&&<div><dt>Shell</dt><dd><code>{call.shell_id}</code></dd></div>}{call.tunnel_id&&<div><dt>Tunnel</dt><dd><code>{call.tunnel_id}</code></dd></div>}</dl>
			{call.error&&<div className="inline-error">{call.error}</div>}
			{progress>0&&<div className="file-transfer-progress" role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={progress}><div><span>{t('tool.transferProgress')}</span><b>{formatFileSize(output!.transferredBytes)} / {formatFileSize(output!.totalBytes)}</b></div><i><em style={{width:`${progress}%`}}/></i></div>}
			{output&&(output.stdout||output.stderr)&&<div className="tool-output-grid">{output.stdout&&<ToolOutputPanel kind="stdout" label="STDOUT" content={output.stdout} live={call.status==='running'}/>} {output.stderr&&<ToolOutputPanel kind="stderr" label="STDERR" content={output.stderr} live={call.status==='running'}/>}</div>}
			<LazyJSONDetails value={args}/>
			{call.run_id&&<MCPRunEvidence runID={call.run_id} hosts={hosts}/>}
		</div>
	</details>
})

function MCPRunEvidence({runID,hosts}:{runID:string;hosts:Host[]}){
	const {t}=useTranslation()
	const [detail,setDetail]=useState<Run|null>(null)
	const [loading,setLoading]=useState(false)
	const [error,setError]=useState('')
	const load=async()=>{if(detail||loading)return;setLoading(true);try{setDetail((await api.runDetail(runID)).run)}catch(err){setError(errorText(err))}finally{setLoading(false)}}
	return <details className="mcp-run-evidence" onToggle={event=>{if(event.currentTarget.open)void load()}}><summary><History size={14}/><span>{t('audit.runEvidence')}</span><code>{runID}</code><ChevronRight size={14}/></summary><div>{loading?<div className="audit-loading" role="status"><LoaderCircle className="spin" size={14}/><span>{t('common.loading')}</span></div>:error?<div className="inline-error">{error}</div>:detail&&<AuditRunDetail run={detail} req={requestFromRun(detail)||{request:detail.request_json}} hosts={hosts}/>}</div></details>
}



export default App
