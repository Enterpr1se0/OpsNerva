import { FormEvent, Suspense, lazy, memo, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { createPortal, flushSync } from 'react-dom'
import { useTranslation } from 'react-i18next'
import { getCurrentWindow } from '@tauri-apps/api/window'
import { Activity, Bot, BrainCircuit, Braces, Check, ChevronRight, CircleDot,  Edit3, FileText, History, ImagePlus, Maximize2,  Minus, Monitor, Moon, PanelLeftOpen, Sun, ListChecks, ListPlus, LoaderCircle, LogOut, Plus, RefreshCw, Save, Send, Settings2, ShieldAlert, Square, TerminalSquare, Trash2, UserRound, X, Zap } from 'lucide-react'
import { api, reconnectChatStream, streamChat } from './api/api'
import { subscribeApplicationEvents } from './api/appEvents'
import { CopyButton } from './components/CopyButton'
import { ApprovalDialog } from './features/approval/ApprovalDialog'
import i18n, { localeFor, type SupportedLanguage } from './lib/i18n'
import { useLiveSSHTasks, type LiveSSHTaskSnapshot, type LiveSSHTaskTarget } from './lib/liveTasks'
import { streamTextTail, type StreamText } from './api/streamText'
import { PasswordInput } from './components/PasswordInput'
import { SSHShellStatus, SSHShellTerminal, SSHTunnelStatus, sshShellActive } from './features/ssh'
import { ChatWorkspacePanel, SSHWorkspacePage } from './features/workspace'
import { useNotifier, NotificationContext, type NotificationSink, type AppNotification } from './lib/notifications'
import { DestructiveConfirmDialog } from './components/DestructiveConfirmDialog'
import { FileTransferProvider } from './features/sftp'
import { useAuditData, type AuditView } from './features/audit'
import { useChatCardDisclosure, type ChatDisclosurePositionHandler } from './features/chat'
import {  useDocumentVisible } from './lib/hooks'
import { desktopRuntime, errorStatus, errorText, formatFileSize, clientId, compactTokenCount } from './lib/utils'
import type { AgentEvent, AgentTask, AgentTaskList, Approval,  AuthStatus, ChatQueueMode, ChatSession, ChatSessionDelta, ChatState, ChatTokenUsage, Health, Host, LLMToolCatalog, ManagedSkill, MCPServer, ModelProvider,  Proxy, QueuedChatMessage, Run, SSHShell, SSHTunnel, SystemSettings, ToolCapabilities } from './types'
import { ChatActivityStatus } from './features/chat/ChatActivityStatus'
import { ComposerControls } from './features/chat/ComposerControls'
import type { ContextUsage } from './features/chat/types'
import { insertQueuedMessage, queuedMessageEntries, historyEntries, deactivateReasoning, settledTurnEntries, prependHistoryEntries, mergePersistedToolEntries, updateToolRunStatus, agentFrameAffectsEntries, reduceAgentEntryFrames } from './features/chat/chatEntries'
import { tasksFromToolContent, groupedTaskToolEntries, latestLiveSSHTaskTargets } from './features/chat/taskEntries'
import { jsonRecord, parseRecord, textValue } from './features/tools/payload'
import { ToolEventCard } from './features/tools/components/ToolEventCard'
import { TaskToolGroupCard } from './features/chat/TaskToolGroupCard'
import { ChatVisibilityContext } from './features/chat/ChatVisibilityContext'
import { defaultChatImageTypes } from './features/settings/defaults'
import type { PendingChatImage, ChatEntry, ChatRenderItem, ModelRetryState, ConnectionRetryState } from './features/chat/types'
import '@xterm/xterm/css/xterm.css'
const ConfigurationPage=lazy(()=>import('./features/settings/ConfigurationPage').then(module=>({default:module.ConfigurationPage})))
const ExtensionsPage=lazy(()=>import('./features/extensions/ExtensionsPage').then(module=>({default:module.ExtensionsPage})))
const AuditPage=lazy(()=>import('./features/audit/AuditPage').then(module=>({default:module.AuditPage})))
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
	const refresh=useCallback(()=>api.authStatus().then(setAuth).catch(err=>setError(errorText(err))).finally(()=>setLoading(false)),[])
	useEffect(()=>{void refresh()},[refresh])
	useEffect(()=>{const unauthorized=()=>setAuth(current=>current?.enabled?{enabled:true,authenticated:false}:current);window.addEventListener('opsnerva:unauthorized',unauthorized);return()=>window.removeEventListener('opsnerva:unauthorized',unauthorized)},[])
	if(loading)return <AppFrame><div className="auth-screen"><LoaderCircle className="spin" size={24}/></div></AppFrame>
	if(error&&!auth)return <AppFrame><div className="auth-screen"><section className="auth-card panel"><ShieldAlert size={24}/><div className="auth-error" role="alert">{error}</div><button className="primary" onClick={()=>{setLoading(true);setError('');void refresh()}}>{t('common.retry')}</button></section></div></AppFrame>
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

	const refreshToolCatalog=useCallback(()=>api.llmTools().then(setToolCatalog).catch(err=>notify(errorText(err),'error')),[notify])
	const refreshSkills=useCallback(()=>api.skills().then(setSkills).catch(err=>notify(errorText(err),'error')),[notify])
	const refreshMCPServers=useCallback(()=>api.mcpServers().then(setMCPServers).catch(err=>notify(errorText(err),'error')),[notify])
	const refreshExtensions=useCallback(async()=>{await Promise.all([refreshToolCatalog(),refreshSkills(),refreshMCPServers()])},[refreshMCPServers,refreshSkills,refreshToolCatalog])
	const refreshHealth=useCallback(()=>api.health().then(next=>setHealth(current=>keepEquivalentHealth(current,next))).catch(err=>notify(errorText(err),'error')),[notify])
	const refreshHosts=useCallback(()=>api.hosts().then(setHosts).catch(err=>notify(errorText(err),'error')),[notify])
	const refreshProviders=useCallback(()=>api.modelProviders().then(setProviders).catch(err=>notify(errorText(err),'error')),[notify])
	const refreshModels=useCallback(async()=>{await Promise.all([refreshProviders(),refreshHealth()])},[refreshHealth,refreshProviders])
	const refreshProxies=useCallback(()=>api.proxies().then(setProxies).catch(err=>notify(errorText(err),'error')),[notify])
	const refreshSettings=useCallback(()=>api.systemSettings().then(setSettings).catch(err=>notify(errorText(err),'error')),[notify])
	const refreshCapabilities=useCallback(()=>api.capabilities().then(setCapabilities).catch(err=>notify(errorText(err),'error')),[notify])
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
	const taskSessionID=tasks?.session_id
	const hasTasks=!!tasks
	const [taskDisclosureOwner,setTaskDisclosureOwner]=useState({sessionID:taskSessionID,hasTasks})
	if(taskDisclosureOwner.sessionID!==taskSessionID||taskDisclosureOwner.hasTasks!==hasTasks){
		setTaskDisclosureOwner({sessionID:taskSessionID,hasTasks})
		setTasksExpanded(hasTasks)
	}
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
              setTasks(state.tasks?.items?.length?state.tasks:null)
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

export default App
