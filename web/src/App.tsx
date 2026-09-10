import { FormEvent, Suspense, lazy, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { createPortal, flushSync } from 'react-dom'
import { useTranslation } from 'react-i18next'
import { getCurrentWindow } from '@tauri-apps/api/window'
import { Bot, Braces, Check, CircleDot, FileText, History, Maximize2, Minus, Monitor, Moon, Sun, LoaderCircle, LogOut, RefreshCw, Settings2, ShieldAlert, TerminalSquare, X } from 'lucide-react'
import { api } from './api/api'
import { subscribeApplicationEvents } from './api/appEvents'
import type { SupportedLanguage } from './lib/i18n'
import { PasswordInput } from './components/PasswordInput'
import { SSHShellStatus, SSHShellTerminal, SSHTunnelStatus, sshShellActive } from './features/ssh'
import { SSHWorkspacePage } from './features/workspace'
import { NotificationContext, type NotificationSink, type AppNotification } from './lib/notifications'
import { FileTransferProvider } from './features/sftp'
import { useAuditHistory, type AuditView } from './features/audit'
import { desktopRuntime, errorStatus, errorText, clientId, keepEquivalent } from './lib/utils'
import type { Approval, AuthStatus, Health, Host, LLMToolCatalog, ManagedSkill, MCPServer, ModelProvider, Proxy, SSHShell, SSHTunnel, SystemSettings, ToolCapabilities } from './types'
import { ChatPage } from './features/chat/ChatPage'
import { defaultChatImageTypes } from './features/settings/defaults'
import '@xterm/xterm/css/xterm.css'
const ConfigurationPage=lazy(()=>import('./features/settings/ConfigurationPage').then(module=>({default:module.ConfigurationPage})))
const ExtensionsPage=lazy(()=>import('./features/extensions/ExtensionsPage').then(module=>({default:module.ExtensionsPage})))
const AuditPage=lazy(()=>import('./features/audit/AuditPage').then(module=>({default:module.AuditPage})))
const LogsPage=lazy(()=>import('./features/logs/LogsPage').then(module=>({default:module.LogsPage})))

type Page = 'chat' | 'ssh' | 'config' | 'extensions' | 'audit' | 'logs'
const pageVisualOrder:Page[]=['chat','ssh','extensions','audit','logs','config']
function topbarShell(shell:SSHShell){
	return shell.kind==='workspace'?shell.surface==='workspace_agent':shell.surface!=='workspace'
}

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

const desktopWindow=desktopRuntime?getCurrentWindow():null
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
	const audit=useAuditHistory(page==='audit'&&auditView==='runs',notify)
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
			if(event.data.shell){
				const shell=event.data.shell
				setSSHShells(current=>applyLifecycleDelta(current,shell,event.data!.removed))
				setSelectedShell(current=>current?.id===shell.id?shell:current)
			}
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
	const updateSSHShell=useCallback((shell:SSHShell)=>{
		rememberSSHShell(shell)
		setSelectedShell(current=>current?.id===shell.id?shell:current)
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
		<SSHShellStatus shells={topbarShells} hosts={hosts} open={openConnectionPanel==='shell'} onOpenChange={open=>setOpenConnectionPanel(current=>open?'shell':current==='shell'?null:current)} onOpen={shell=>{setOpenConnectionPanel(null);setSelectedShell(shell)}} onClose={closeSSHShell} onCreated={registerSSHShell} onReconnected={updateSSHShell}/>
        <LanguageSwitch/>
		<ThemeSwitch preference={themePreference} onChange={setThemePreference}/>
        <span className={`status ${health?.status === 'ok' ? 'online' : ''}`}><CircleDot size={14}/>{health?.status === 'ok' ? t('shell.online') : t('shell.disconnected')}</span>
        <button className={`icon-button ${refreshing?'refreshing':''}`} onClick={()=>void manualRefresh()} disabled={refreshing} title={t(refreshing?'common.refreshing':'shell.refresh')} aria-label={t(refreshing?'common.refreshing':'shell.refresh')}><RefreshCw size={17}/></button>
      </div></header>
      <section ref={workspaceRef} className={`workspace workspace-${page}`}>
			<ChatPage visible={page==='chat'} onActivate={activateChat}
				hosts={hosts} providers={providers} approvals={approvals} workspaceShells={workspaceShells}
				capabilities={capabilities} settings={settings} imageTypes={settings?.chat_image_allowed_types||defaultChatImageTypes}
					agentAvailable={!!health?.agent_available} modelName={health?.model?.model} contextWindow={health?.model?.context_window||0} refreshConnections={refreshConnections}
				dismissApproval={dismissApproval} onCreateWorkspaceShell={createWorkspaceShell} onOpenWorkspaceShell={setSelectedShell} onWorkspaceShellStarted={observeAgentWorkspaceShell} onSettingsChanged={setSettings}
				onHostChanged={hostChanged}
				onModelChanged={modelChanged}
				sidebarTarget={chatSidebarTarget} onSessionDeleted={removeSessionState} onError={reportError}
			/>
			{page === 'ssh' && <SSHWorkspacePage
				hosts={hosts} shells={sshShells.filter(shell=>shell.kind!=='workspace'&&shell.surface==='workspace')}
				onCreated={rememberSSHShell} onReconnected={updateSSHShell} refresh={refreshConnections} onError={reportError}
			/>}
		<Suspense fallback={<div className="panel" role="status">{t('common.loading')}</div>}>
		{page === 'config' && <ConfigurationPage hosts={hosts} providers={providers} proxies={proxies} settings={settings} capabilities={capabilities} health={health} refreshModels={refreshModels} refreshHosts={refreshHosts} refreshProxies={refreshProxies} refreshCapabilities={refreshCapabilities} refreshHealth={refreshHealth} onSettingsChanged={setSettings} onOpenMCPActivity={()=>{setAuditView('mcp');navigate('audit')}}/>}
		{page === 'extensions' && <ExtensionsPage skills={skills} mcpServers={mcpServers} toolCatalog={toolCatalog} refreshSkills={refreshSkills} refreshMCPServers={refreshMCPServers} refreshToolCatalog={refreshToolCatalog} onToolCatalogChanged={setToolCatalog}/>}
		{page === 'audit' && <AuditPage view={auditView} onViewChange={setAuditView} mcpRefreshKey={mcpActivityRefresh} history={audit} hosts={hosts} notify={notify}/>}
        {page === 'logs' && <LogsPage/>}
		</Suspense>
      </section>
	      {selectedShell&&<SSHShellTerminal
			key={selectedShell.id}
			initialShell={sshShells.find(shell=>shell.id===selectedShell.id)||selectedShell}
			relatedShells={selectedShell.kind==='workspace'?sshShells.filter(shell=>shell.kind==='workspace'&&shell.workspace_id===selectedShell.workspace_id&&sshShellActive(shell.status)):[]}
			onSelect={setSelectedShell}
			onClose={()=>setSelectedShell(null)}
			onChanged={()=>void refreshConnections()}
			onReconnected={updateSSHShell}
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



function Nav({ active, icon, label, count, warn, onClick }: {active:boolean;icon:React.ReactNode;label:string;count?:number;warn?:boolean;onClick:()=>void}) {
  return <button className={`nav-item ${active ? 'active' : ''}`} onClick={onClick} title={label} aria-label={label} aria-current={active?'page':undefined}>{icon}<span>{label}</span>{count !== undefined && <em className={warn ? 'warn' : ''}>{count}</em>}</button>
}

export default App
