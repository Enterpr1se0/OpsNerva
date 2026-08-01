import { FormEvent, memo, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import Markdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { useTranslation } from 'react-i18next'
import type { TFunction } from 'i18next'
import type { Terminal as XTermInstance } from '@xterm/xterm'
import { invoke } from '@tauri-apps/api/core'
import '@xterm/xterm/css/xterm.css'
import {
  Activity, BookOpen, Bot, BrainCircuit, Braces, Check, ChevronLeft, ChevronRight, CircleDot, Clock3, Copy, Cpu, Edit3, Eye, EyeOff, FileText, FolderOpen, FolderOutput, FunctionSquare, History, ImagePlus, KeyRound, LockKeyhole, LogOut, Minimize2, PanelLeftClose, PanelLeftOpen, PanelRightClose, PanelRightOpen,
  Cable, Download, ListChecks, LoaderCircle, Plus, Power, RefreshCw, Save, Search, Send, Server, Settings2, ShieldAlert, ShieldCheck, SlidersHorizontal, Square, TerminalSquare, Trash2, UploadCloud, UserRound, X, Zap,
} from 'lucide-react'
import { api, chatAttachmentURL, sftpDownloadURL, sshShellEventsURL, streamChat, workspaceDownloadURL, workspaceFileEventsURL } from './api'
import { CopyButton, CopyablePre } from './CopyButton'
import i18n, { localeFor, type SupportedLanguage } from './i18n'
import { TextFileEditor } from './TextFileEditor'
import type { AgentEvent, AgentPlan, Approval, ApprovalExecutionResult, ApprovalMode, ChatMessage, ChatSession, CommandReview, Health, Host, HostAuthType, HostInput, HostSudoMode, LLMToolCatalog, LLMToolDescriptor, LLMToolGuard, ManagedSkill, MCPServer, MCPServerInput, MCPTransport, ModelProvider, ModelProviderInput, ModelProviderKind, Proxy, ProxyInput, Run, ServerLogEntry, SFTPFileEntry, SSHShell, SSHShellEvent, SSHTunnel, SystemSettings, SystemSettingsInput, ToolCapabilities, WebSearchSettings, WebSearchSettingsInput, WorkspaceCapability, WorkspaceFilePreview, WorkspaceInput, WorkspaceShellMode } from './types'

type Page = 'chat' | 'ssh' | 'config' | 'extensions' | 'audit' | 'logs'
type ChatEntryImage = {id:string;name:string;mimeType:string;sizeBytes:number;url:string}
type PendingChatImage = {id:string;file:File;url:string}
type ChatEntry = { id: string; kind: 'user' | 'assistant' | 'tool' | 'reasoning' | 'error'; content: string; tool?: string; toolCallId?:string; runId?:string; transient?:boolean; liveStdout?:string; liveStderr?:string; transferredBytes?:number; transferTotalBytes?:number; images?:ChatEntryImage[]; active?: boolean; streaming?: boolean; status?: 'pending' | 'completed' | 'failed' }
type ModelRetryState = {attempt:number;max:number;readyAt:number}
type ActiveChatStream = { id: string; sessionId: string; controller: AbortController }

function historyEntries(messages:ChatMessage[],sessionID:string):ChatEntry[]{
  return messages.map((item,index)=>({id:`history_${item.id||`${index}_${item.created_at}`}`,kind:item.role,content:item.content,tool:item.tool_name,status:item.status,images:item.attachments?.map(image=>({id:image.id,name:image.name,mimeType:image.mime_type,sizeBytes:image.size_bytes,url:chatAttachmentURL(sessionID,image.id)}))}))
}

function deactivateReasoning(entry:ChatEntry):ChatEntry{
	return entry.kind==='reasoning'&&entry.active?{...entry,active:false}:entry
}

function toolContentWithStatus(content:string,status:string,runID?:string){
	try{
		const payload=JSON.parse(content)
		if(payload&&typeof payload==='object'&&!Array.isArray(payload))return JSON.stringify({...payload,status,...(runID?{run_id:runID}:{})})
	}catch{/* keep malformed tool output visible */}
	return JSON.stringify({status,...(runID?{run_id:runID}:{}),value:content})
}

function updateActiveToolStatus(entries:ChatEntry[],status:string,runID?:string){
	let index=-1
	for(let itemIndex=entries.length-1;itemIndex>=0;itemIndex--){
		if(entries[itemIndex].kind==='tool'&&entries[itemIndex].transient){index=itemIndex;break}
	}
	if(index<0)return entries
	return entries.map((item,itemIndex)=>itemIndex===index?{...item,content:toolContentWithStatus(item.content,status,runID),runId:runID||item.runId}:item)
}

function updateToolRunStatus(entries:ChatEntry[],runID:string,status:string){
	if(!runID)return entries
	return entries.map(item=>{
		if(item.kind!=='tool')return item
		const payload=parseRecord(item.content)
		const result=jsonRecord(payload.result),task=jsonRecord(payload.task)
		const itemRunID=item.runId||textValue(payload.run_id)||textValue(result?.run_id)||textValue(task?.run_id)
		const currentStatus=textValue(payload.status)||textValue(result?.status)||textValue(task?.status)
		if(itemRunID===runID&&status==='in_progress'&&!item.transient&&['completed','partial','failed','interrupted','rejected','denied','expired'].includes(currentStatus))return item
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
		index=entries.findIndex(item=>{
			if(item.kind!=='tool')return false
			const payload=parseRecord(item.content),result=jsonRecord(payload.result),task=jsonRecord(payload.task)
			return (item.runId||textValue(payload.run_id)||textValue(result?.run_id)||textValue(task?.run_id))===runID
		})
	}
	if(index<0){
		for(let itemIndex=entries.length-1;itemIndex>=0;itemIndex--){
			if(entries[itemIndex].kind==='tool'&&entries[itemIndex].transient){index=itemIndex;break}
		}
	}
	if(index<0)return entries
	const status=frame.status==='running'?'in_progress':frame.status||'in_progress'
	return entries.map((item,itemIndex)=>{
		if(itemIndex!==index)return item
		const currentPayload=parseRecord(item.content)
		const currentStatus=textValue(currentPayload.status)||textValue(jsonRecord(currentPayload.result)?.status)||textValue(jsonRecord(currentPayload.task)?.status)
		if(!item.transient&&status==='in_progress'&&currentStatus!=='running'&&currentStatus!=='in_progress')return item
		const content=toolContentWithStatus(item.content,status,runID)
		const chunk=frame.content||''
		return {
			...item,
			content,
			tool:frame.tool_name||item.tool,
			toolCallId:callID||item.toolCallId,
			runId:runID||item.runId,
			liveStdout:frame.stream==='stdout'?(item.liveStdout||'')+chunk:item.liveStdout,
			liveStderr:frame.stream==='stderr'?(item.liveStderr||'')+chunk:item.liveStderr,
			transferredBytes:frame.stream==='progress'&&typeof frame.transferred_bytes==='number'?frame.transferred_bytes:item.transferredBytes,
			transferTotalBytes:frame.stream==='progress'&&typeof frame.total_bytes==='number'?frame.total_bytes:item.transferTotalBytes,
			transient:status==='in_progress'||status==='approval_required',
		}
	})
}

function planFromToolContent(content:string):AgentPlan|null{
  try{const value=JSON.parse(content) as AgentPlan&{found?:boolean;plan?:AgentPlan};const plan=value?.plan||value;return plan&&typeof plan.goal==='string'&&Array.isArray(plan.steps)?plan:null}catch{return null}
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
function rememberSession(id: string) { try { localStorage.setItem('opspilot.activeSession', id) } catch { /* storage may be disabled */ } }
function recalledSession() { try { return localStorage.getItem('opspilot.activeSession') || '' } catch { return '' } }
function rememberWorkspace(id:string){try{if(id)localStorage.setItem('opspilot.activeWorkspace',id)}catch{/* storage may be disabled */}}
function recalledWorkspace(){try{return localStorage.getItem('opspilot.activeWorkspace')||''}catch{return''}}
function rememberSidebarCollapsed(collapsed:boolean){try{localStorage.setItem('opspilot.sidebarCollapsed',String(collapsed))}catch{/* storage may be disabled */}}
function recalledSidebarCollapsed(){try{return localStorage.getItem('opspilot.sidebarCollapsed')==='true'}catch{return false}}
type ChatPanel='workspace'|'conversations'
function rememberChatPanelCollapsed(panel:ChatPanel,collapsed:boolean){try{localStorage.setItem(`opspilot.chatPanel.${panel}`,String(collapsed))}catch{/* storage may be disabled */}}
function recalledChatPanelCollapsed(panel:ChatPanel){try{return localStorage.getItem(`opspilot.chatPanel.${panel}`)==='true'}catch{return false}}

function App() {
	const {t}=useTranslation()
	const [auth,setAuth]=useState<'checking'|'setup'|'authenticated'|'guest'>('checking')
  const [page, setPage] = useState<Page>('chat')
  const [sidebarCollapsed,setSidebarCollapsed]=useState(recalledSidebarCollapsed)
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
  const [error, setError] = useState('')
	const [agentStreaming,setAgentStreaming]=useState(false)

  const refresh = useCallback(async () => {
    try {
	  const [nextHealth, nextHosts, nextProviders, nextProxies, nextSettings, nextCapabilities, nextToolCatalog, nextSkills, nextMCPServers, nextApprovals, nextRuns, nextSSHTunnels, nextSSHShells] = await Promise.all([
		api.health(), api.hosts(), api.modelProviders(), api.proxies(), api.systemSettings(), api.capabilities(), api.llmTools(), api.skills(), api.mcpServers(), api.approvals(), api.runs(), api.sshTunnels(), api.sshShells(),
      ])
	  setHealth(nextHealth); setHosts(nextHosts); setProviders(nextProviders); setProxies(nextProxies);setSettings(nextSettings);setCapabilities(nextCapabilities);setToolCatalog(nextToolCatalog);setSkills(nextSkills);setMCPServers(nextMCPServers); setApprovals(nextApprovals); setRuns(nextRuns);setSSHTunnels(nextSSHTunnels.tunnels||[]);setSSHShells(nextSSHShells.shells||[]); setError('')
	} catch (err) { const message=errorText(err);if(/authentication required/i.test(message))setAuth('guest');setError(message) }
  }, [])
	const refreshApprovals=useCallback(async(decidedID?:string)=>{
		if(decidedID)setApprovals(current=>current.filter(item=>item.id!==decidedID))
		try{setApprovals(await api.approvals())}
		catch(err){const message=errorText(err);if(/authentication required/i.test(message))setAuth('guest');setError(message)}
	},[])

	useEffect(()=>{
		void (async()=>{
			try{
				const status=await api.authStatus()
				if(!status.initialized){setAuth('setup');return}
				await api.authSession()
				setAuth('authenticated')
			}catch{setAuth('guest')}
		})()
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
	useEffect(() => { if(auth==='authenticated')void refresh() }, [auth,refresh])
	useEffect(() => {
		if(auth!=='authenticated'||agentStreaming)return
		const timer=window.setInterval(()=>{if(document.visibilityState==='visible')void refresh()},10000)
		return()=>window.clearInterval(timer)
	},[auth,agentStreaming,refresh])

	if(auth==='checking')return <div className="auth-screen"><div className="auth-loading"><LoaderCircle className="spin" size={25}/><span>{t('shell.securing')}</span></div></div>
	if(auth==='setup')return <SetupPage onAuthenticated={()=>setAuth('authenticated')} onRequiresLogin={()=>setAuth('guest')}/>
	if(auth==='guest')return <LoginPage onAuthenticated={()=>setAuth('authenticated')}/>

  const title = t(`shell.pageTitles.${page}`)
	const stopSSHTunnel=async(id:string)=>{
		try{
			await api.stopSSHTunnel(id)
			setSSHTunnels(current=>current.filter(item=>item.id!==id))
		}catch(err){
			setError(errorText(err))
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
		catch(err){setError(errorText(err))}
	}
	const observeAgentWorkspaceShell=(shell:SSHShell)=>{
		rememberSSHShell(shell)
		setSelectedShell(current=>current||shell)
	}
	const toggleSidebar=()=>setSidebarCollapsed(current=>{const next=!current;rememberSidebarCollapsed(next);return next})

  return <div className={`app-shell ${sidebarCollapsed?'sidebar-collapsed':''}`}>
    <aside className="sidebar">
      <div className="brand"><div className="brand-mark"><TerminalSquare size={23}/></div><div className="brand-name"><strong>OpsNerva</strong></div><button className="sidebar-toggle" onClick={toggleSidebar} title={t(sidebarCollapsed?'shell.expandSidebar':'shell.collapseSidebar')} aria-label={t(sidebarCollapsed?'shell.expandSidebar':'shell.collapseSidebar')}>{sidebarCollapsed?<PanelLeftOpen size={17}/>:<PanelLeftClose size={17}/>}</button></div>
      <nav>
        <Nav active={page === 'chat'} icon={<Bot/>} label={t('shell.nav.agent')} onClick={() => setPage('chat')}/>
        <Nav active={page === 'ssh'} icon={<TerminalSquare/>} label={t('shell.nav.ssh')} onClick={() => setPage('ssh')}/>
        <Nav active={page === 'config'} icon={<Settings2/>} label={t('shell.nav.configuration')} onClick={() => setPage('config')}/>
		<Nav active={page === 'extensions'} icon={<Braces/>} label={t('shell.nav.extensions')} onClick={() => setPage('extensions')}/>
        <Nav active={page === 'audit'} icon={<History/>} label={t('shell.nav.audit')} onClick={() => setPage('audit')}/>
        <Nav active={page === 'logs'} icon={<FileText/>} label={t('shell.nav.logs')} onClick={() => setPage('logs')}/>
      </nav>
      <div className="sidebar-foot">
			<button className="logout-button" title={t('shell.signOut')} aria-label={t('shell.signOut')} onClick={async()=>{try{await api.logout()}finally{setAuth('guest')}}}><LogOut size={15}/><span>{t('shell.signOut')}</span></button>
        <div className="build">v0.1.7</div>
      </div>
    </aside>
    <main>
	      <header className="topbar"><div><h1>{title}</h1></div><div className="top-actions">
        <SSHTunnelStatus tunnels={sshTunnels} hosts={hosts} open={openConnectionPanel==='tunnel'} onOpenChange={open=>setOpenConnectionPanel(current=>open?'tunnel':current==='tunnel'?null:current)} onStop={stopSSHTunnel} onCreated={registerSSHTunnel}/>
		<SSHShellStatus shells={sshShells.filter(topbarShell)} hosts={hosts} open={openConnectionPanel==='shell'} onOpenChange={open=>setOpenConnectionPanel(current=>open?'shell':current==='shell'?null:current)} onOpen={shell=>{setOpenConnectionPanel(null);setSelectedShell(shell)}} onCreated={registerSSHShell}/>
        <LanguageSwitch/>
        <span className={`status ${health?.status === 'ok' ? 'online' : ''}`}><CircleDot size={14}/>{health?.status === 'ok' ? t('shell.online') : t('shell.disconnected')}</span>
        <button className="icon-button" onClick={refresh} title={t('shell.refresh')}><RefreshCw size={17}/></button>
      </div></header>
      {error && <div className="global-error"><ShieldAlert size={17}/>{error}<button onClick={() => setError('')}><X size={15}/></button></div>}
      <section className="workspace">
			{page === 'chat' && <ChatPage
				hosts={hosts} approvals={approvals} runs={runs} workspaceShells={sshShells.filter(shell=>shell.kind==='workspace')}
				capabilities={capabilities} settings={settings} imageTypes={settings?.chat_image_allowed_types||defaultChatImageTypes}
				agentAvailable={!!health?.agent_available} modelName={health?.model?.model} refresh={refresh}
				refreshApprovals={refreshApprovals} onCreateWorkspaceShell={createWorkspaceShell} onOpenWorkspaceShell={setSelectedShell} onWorkspaceShellStarted={observeAgentWorkspaceShell} onSettingsChanged={setSettings}
				onError={setError} onStreamingChange={setAgentStreaming}
			/>}
			{page === 'ssh' && <SSHWorkspacePage
				hosts={hosts} shells={sshShells.filter(shell=>shell.kind!=='workspace'&&shell.surface==='workspace')}
				onCreated={rememberSSHShell} refresh={refresh} onError={message=>setError(message)}
			/>}
		{page === 'config' && <ConfigurationPage hosts={hosts} providers={providers} proxies={proxies} settings={settings} capabilities={capabilities} health={health} refresh={refresh}/>}
		{page === 'extensions' && <ExtensionsPage skills={skills} mcpServers={mcpServers} toolCatalog={toolCatalog} refresh={refresh}/>}
        {page === 'audit' && <AuditPage runs={runs} hosts={hosts}/>}
        {page === 'logs' && <LogsPage/>}
      </section>
	      {selectedShell&&<SSHShellTerminal key={selectedShell.id} initialShell={selectedShell} relatedShells={selectedShell.kind==='workspace'?sshShells.filter(shell=>shell.kind==='workspace'&&shell.workspace_id===selectedShell.workspace_id&&sshShellActive(shell.status)):[]} onSelect={setSelectedShell} onClose={()=>setSelectedShell(null)} onChanged={()=>void refresh()} onError={message=>setError(message)}/>}
    </main>
  </div>
}

function LanguageSwitch(){
	const {t,i18n:instance}=useTranslation()
	const language:SupportedLanguage=instance.resolvedLanguage?.startsWith('zh')?'zh':'en'
	return <div className="language-switch" role="group" aria-label={t('language.label')}>
		<button type="button" className={language==='zh'?'active':''} aria-pressed={language==='zh'} onClick={()=>void instance.changeLanguage('zh')}>{t('language.chinese')}</button>
		<button type="button" className={language==='en'?'active':''} aria-pressed={language==='en'} onClick={()=>void instance.changeLanguage('en')}>{t('language.english')}</button>
	</div>
}

function ApprovalModeStatus({settings,onChanged,onError}:{settings:SystemSettings|null;onChanged:(settings:SystemSettings)=>void;onError:(message:string)=>void}){
	const {t}=useTranslation()
	const mode=settings?.approval_mode??'manual'
	const [open,setOpen]=useState(false)
	const [busy,setBusy]=useState(false)
	const [confirmFullAccess,setConfirmFullAccess]=useState(false)
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
		<details className={`approval-mode-status ${mode}`} open={open} onToggle={event=>setOpen(event.currentTarget.open)}>
			<summary title={t('settings.approvalMode')} onClick={event=>{if(busy)event.preventDefault()}}>{busy?<LoaderCircle className="spin" size={13}/>:<ShieldCheck size={13}/>}<span>{t(`settings.approvalMode_${mode}`)}</span><ChevronRight size={12}/></summary>
			<div className="approval-mode-menu">
				{(['manual','auto','full_access'] as ApprovalMode[]).map(value=><button type="button" className={value===mode?'active':''} disabled={busy||!settings} onClick={()=>select(value)} key={value}><span>{t(`settings.approvalMode_${value}`)}</span>{value===mode&&<Check size={13}/>}</button>)}
			</div>
		</details>
		{confirmFullAccess&&<FullAccessConfirmDialog onCancel={()=>setConfirmFullAccess(false)} onConfirm={()=>{setConfirmFullAccess(false);void apply('full_access')}}/>}
	</>
}

function SSHTunnelStatus({tunnels,hosts,open,onOpenChange,onStop,onCreated}:{tunnels:SSHTunnel[];hosts:Host[];open:boolean;onOpenChange:(open:boolean)=>void;onStop:(id:string)=>Promise<void>;onCreated:(tunnel:SSHTunnel)=>void}){
	const {t,i18n:instance}=useTranslation()
	const [stopping,setStopping]=useState('')
	const [creating,setCreating]=useState(false)
	return <>
		<details className="ssh-tunnel-status" open={open} onToggle={event=>onOpenChange(event.currentTarget.open)}>
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
		setBusy(true);setError('')
		try{
			const tunnel=await api.startSSHTunnel({host_id:hostID,remote_host:remoteHost.trim(),remote_port:Number(remotePort),local_port:localPort===''?0:Number(localPort)})
			onCreated(tunnel)
		}catch(err){setError(errorText(err))}
		finally{setBusy(false)}
	}
	return createPortal(<div className="connection-dialog-backdrop" onMouseDown={event=>{if(event.target===event.currentTarget&&!busy)onCancel()}}>
		<form className="connection-dialog panel" role="dialog" aria-modal="true" aria-labelledby="new-tunnel-title" onSubmit={submit}>
			<header><span><Cable size={20}/><span><small>{t('tunnels.title')}</small><h2 id="new-tunnel-title">{t('tunnels.create')}</h2></span></span><button type="button" disabled={busy} onClick={onCancel}><X size={16}/></button></header>
			<div className="connection-dialog-fields">
				<label><span>{t('common.host')}</span><select value={hostID} onChange={event=>setHostID(event.target.value)} required>{hosts.map(host=><option value={host.id} key={host.id}>{host.name} · {host.user}@{host.address}:{host.port}</option>)}</select></label>
				<label><span>{t('tunnels.remoteHost')}</span><input value={remoteHost} onChange={event=>setRemoteHost(event.target.value)} required/></label>
				<label><span>{t('tunnels.remotePort')}</span><input type="number" min="1" max="65535" value={remotePort} onChange={event=>setRemotePort(event.target.value)} required autoFocus/></label>
				<label><span>{t('tunnels.localPort')}</span><input type="number" min="1" max="65535" value={localPort} onChange={event=>setLocalPort(event.target.value)} placeholder={t('tunnels.automaticPort')}/></label>
			</div>
			{error&&<p className="connection-dialog-error"><ShieldAlert size={14}/>{error}</p>}
			<footer><button type="button" disabled={busy} onClick={onCancel}>{t('common.cancel')}</button><button className="primary" disabled={busy||!hostID}>{busy?<LoaderCircle className="spin" size={14}/>:<Plus size={14}/>} {busy?t('tunnels.starting'):t('tunnels.start')}</button></footer>
		</form>
	</div>,document.body)
}

function SSHShellStatus({shells,hosts,open,onOpenChange,onOpen,onCreated}:{shells:SSHShell[];hosts:Host[];open:boolean;onOpenChange:(open:boolean)=>void;onOpen:(shell:SSHShell)=>void;onCreated:(shell:SSHShell)=>void}){
	const {t}=useTranslation()
	const [creating,setCreating]=useState(false)
	return <>
		<details className="ssh-shell-status" open={open} onToggle={event=>onOpenChange(event.currentTarget.open)}>
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

function SSHShellCreateDialog({hosts,onCancel,onCreated}:{hosts:Host[];onCancel:()=>void;onCreated:(shell:SSHShell)=>void}){
	const {t}=useTranslation()
	const [hostID,setHostID]=useState(hosts[0]?.id||'')
	const [busy,setBusy]=useState(false)
	const [error,setError]=useState('')
	const submit=async(event:FormEvent)=>{
		event.preventDefault()
		setBusy(true);setError('')
		try{
			const shell=await api.startSSHShell({host_id:hostID})
			onCreated(shell)
		}catch(err){setError(errorText(err))}
		finally{setBusy(false)}
	}
	return createPortal(<div className="connection-dialog-backdrop" onMouseDown={event=>{if(event.target===event.currentTarget&&!busy)onCancel()}}>
		<form className="connection-dialog compact panel" role="dialog" aria-modal="true" aria-labelledby="new-shell-title" onSubmit={submit}>
			<header><span><TerminalSquare size={20}/><span><small>{t('sshShell.title')}</small><h2 id="new-shell-title">{t('sshShell.create')}</h2></span></span><button type="button" disabled={busy} onClick={onCancel}><X size={16}/></button></header>
			<div className="connection-dialog-fields single">
				<label><span>{t('common.host')}</span><select value={hostID} onChange={event=>setHostID(event.target.value)} required autoFocus>{hosts.map(host=><option value={host.id} key={host.id}>{host.name} · {host.user}@{host.address}:{host.port}</option>)}</select></label>
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
				<div className="ssh-shell-terminal-state">{workspaceShell&&relatedShells.length>1&&<select value={shell.id} onChange={event=>{const selected=relatedShells.find(item=>item.id===event.target.value);if(selected)onSelect?.(selected)}} aria-label={t('workspace.switchTerminal')}>{relatedShells.map(item=><option value={item.id} key={item.id}>{t(item.surface==='workspace_agent'?'workspace.agent':'workspace.operator')} · {item.cwd||'.'}</option>)}</select>}<em className={shell.status}>{t(`statusLabels.${shell.status}`,{defaultValue:shell.status})}</em><code>{shell.elevated?'root':shell.user}</code>{!embedded&&<button type="button" onClick={onClose} title={t('common.close')}><X size={16}/></button>}</div>
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
	const [hostID,setHostID]=useState(hosts[0]?.id||'')
	const [selectedShellID,setSelectedShellID]=useState(shells[0]?.id||'')
	const [starting,setStarting]=useState(false)
	useEffect(()=>{
		if(!hosts.some(host=>host.id===hostID))setHostID(hosts[0]?.id||'')
	},[hostID,hosts])
	useEffect(()=>{
		if(!shells.some(shell=>shell.id===selectedShellID))setSelectedShellID(shells[0]?.id||'')
	},[selectedShellID,shells])
	const selectedShell=shells.find(shell=>shell.id===selectedShellID)
	const startShell=async()=>{
		if(!hostID||starting)return
		setStarting(true)
		try{
			const shell=await api.startSSHShell({host_id:hostID,surface:'workspace'})
			onCreated(shell);setSelectedShellID(shell.id)
		}catch(err){onError(errorText(err))}
		finally{setStarting(false)}
	}
	if(!hosts.length)return <div className="ssh-workspace-empty panel"><Server size={28}/><b>{t('connections.noHosts')}</b></div>
	return <div className="ssh-workspace">
		<SFTPBrowser key={hostID} hosts={hosts} hostID={hostID} onHostChange={setHostID}/>
		<section className="ssh-workspace-terminal panel">
			<header className="ssh-terminal-tabs">
				<div>{shells.map(shell=><button type="button" className={shell.id===selectedShellID?'active':''} onClick={()=>setSelectedShellID(shell.id)} key={shell.id}><i className={shell.status}/><span>{shell.host_name||shell.host_id}</span><small>{shell.elevated?'root':shell.user}</small></button>)}</div>
				<button type="button" className="ssh-new-terminal" disabled={starting||!hostID} onClick={()=>void startShell()}>{starting?<LoaderCircle className="spin" size={14}/>:<Plus size={14}/>} {t('sshWorkspace.newTerminal')}</button>
			</header>
			<div className="ssh-terminal-stage">
				{selectedShell?<SSHShellTerminal key={selectedShell.id} initialShell={selectedShell} embedded onClose={()=>setSelectedShellID('')} onChanged={()=>void refresh()} onError={onError}/>:<div className="ssh-terminal-empty"><TerminalSquare size={32}/><b>{t('sshWorkspace.noTerminal')}</b><button type="button" className="primary" disabled={starting} onClick={()=>void startShell()}>{starting?<LoaderCircle className="spin" size={14}/>:<Plus size={14}/>} {t('sshWorkspace.newTerminal')}</button></div>}
			</div>
		</section>
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

function SFTPBrowser({hosts,hostID,onHostChange}:{hosts:Host[];hostID:string;onHostChange:(id:string)=>void}){
	const {t,i18n:instance}=useTranslation()
	const [path,setPath]=useState('')
	const [pathInput,setPathInput]=useState('')
	const [entries,setEntries]=useState<SFTPFileEntry[]>([])
	const [loading,setLoading]=useState(true)
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
		<aside className={`sftp-browser panel ${dragging?'dragging':''}`} onDragEnter={event=>{if(acceptsFiles(event)){event.preventDefault();setDragging(true)}}} onDragOver={event=>{if(acceptsFiles(event)){event.preventDefault();event.dataTransfer.dropEffect=busy?'none':'copy'}}} onDragLeave={event=>{event.preventDefault();if(!(event.relatedTarget instanceof Node&&event.currentTarget.contains(event.relatedTarget)))setDragging(false)}} onDrop={event=>{if(!acceptsFiles(event))return;event.preventDefault();setDragging(false);if(!busy)void uploadFiles(Array.from(event.dataTransfer.files))}}>
			<header><div><FolderOpen size={17}/><b>SFTP</b></div><select value={hostID} onChange={event=>onHostChange(event.target.value)}>{hosts.map(host=><option value={host.id} key={host.id}>{host.name} · {host.user}@{host.address}</option>)}</select></header>
			<form className="sftp-path" onSubmit={event=>{event.preventDefault();void load(pathInput)}}><button type="button" disabled={!path||path==='/'} onClick={()=>void load(remoteParentPath(path))} title={t('workspace.parent')}>‹</button><input value={pathInput} onChange={event=>setPathInput(event.target.value)} aria-label={t('sshWorkspace.remotePath')}/><button type="submit" disabled={loading}><ChevronRight size={13}/></button><button type="button" disabled={loading} onClick={()=>void load(path)} title={t('common.refresh')}><RefreshCw className={loading?'spin':''} size={13}/></button></form>
			<div className="sftp-actions"><button type="button" disabled={busy||!path} onClick={()=>setNameEditor({mode:'create'})}><Plus size={13}/>{t('sshWorkspace.newDirectory')}</button><label className={busy?'disabled':''}><UploadCloud size={13}/>{t('common.upload')}<input key={inputKey} type="file" multiple disabled={busy||!path} onChange={event=>void uploadFiles(Array.from(event.target.files||[]))}/></label></div>
			<div className="sftp-list">{loading?<span className="sftp-state"><LoaderCircle className="spin" size={14}/>{t('common.loading')}</span>:listError?<span className="sftp-state error">{listError}</span>:entries.length?entries.map(entry=><div className="sftp-row" key={`${entry.type}:${entry.path}`}><button type="button" className="sftp-entry" onClick={()=>entry.type==='directory'?void load(entry.path):void openTextFile(entry)} title={entry.path}>{openingFile===entry.path?<LoaderCircle className="spin" size={14}/>:entry.type==='directory'?<FolderOpen size={14}/>:<FileText size={14}/>}<span><b>{entry.name}</b><small>{entry.mode} · {entry.type==='directory'?'—':formatFileSize(entry.size||0)} · {new Date(entry.modified_at).toLocaleString(localeFor(instance.language))}</small></span></button>{entry.type!=='directory'&&<button type="button" onClick={()=>download(entry)} title={t('common.download')}><Download size={12}/></button>}<button type="button" onClick={()=>setNameEditor({mode:'rename',entry})} title={t('sshWorkspace.rename')}><Edit3 size={12}/></button><button type="button" className="danger" onClick={()=>setDeleteCandidate({entry})} title={t('common.delete')}><Trash2 size={12}/></button></div>):<span className="sftp-state">{t('workspace.emptyDirectory')}</span>}</div>
			{notice&&<div className={`sftp-notice ${noticeError?'error':''}`}>{notice}<button onClick={()=>setNotice('')}><X size={11}/></button></div>}
			{dragging&&<div className="sftp-drop"><UploadCloud size={28}/><b>{t('workspace.dropFilesHere')}</b></div>}
		</aside>
		{nameEditor&&<SFTPNameDialog mode={nameEditor.mode} initialName={nameEditor.mode==='rename'?nameEditor.entry.name:''} busy={busy} onCancel={()=>setNameEditor(null)} onConfirm={name=>void saveName(name)}/>}
		{deleteCandidate&&<DestructiveConfirmDialog label={t('sshWorkspace.deleteLabel')} title={t('sshWorkspace.deleteTitle',{name:deleteCandidate.entry.name})} description={t('sshWorkspace.deleteDescription',{path:deleteCandidate.entry.path})} busy={busy} onCancel={()=>setDeleteCandidate(null)} onConfirm={()=>void remove()}/>}
		{overwriteCandidate&&<SFTPOverwriteDialog path={overwriteCandidate.path} busy={busy} onCancel={()=>setOverwriteCandidate(null)} onConfirm={()=>void overwrite()}/>}
		{textFile&&<TextFileEditor path={textFile.entry.path} meta={`${textFile.entry.mode} · ${formatFileSize(textFile.entry.size||0)} · ${textFile.encoding.toUpperCase()} · ${new Date(textFile.entry.modified_at).toLocaleString(localeFor(instance.language))}`} content={textFile.content} binary={textFile.binary} editable onClose={()=>setTextFile(null)} onSave={saveTextFile} onDownload={()=>download(textFile.entry)}/>}
	</>
}

function SFTPNameDialog({mode,initialName,busy,onCancel,onConfirm}:{mode:'create'|'rename';initialName:string;busy:boolean;onCancel:()=>void;onConfirm:(name:string)=>void}){
	const {t}=useTranslation()
	const [name,setName]=useState(initialName)
	const valid=!!name.trim()&&name!=='.'&&name!=='..'&&!name.includes('/')
	return createPortal(<div className="connection-dialog-backdrop" onMouseDown={event=>{if(event.target===event.currentTarget&&!busy)onCancel()}}><form className="connection-dialog compact panel" onSubmit={event=>{event.preventDefault();if(valid)onConfirm(name)}}><header><span><FolderOpen size={19}/><span><small>SFTP</small><h2>{t(mode==='create'?'sshWorkspace.newDirectory':'sshWorkspace.rename')}</h2></span></span><button type="button" disabled={busy} onClick={onCancel}><X size={15}/></button></header><div className="connection-dialog-fields single"><label><span>{t('sshWorkspace.name')}</span><input value={name} onChange={event=>setName(event.target.value)} autoFocus required/></label></div><footer><button type="button" disabled={busy} onClick={onCancel}>{t('common.cancel')}</button><button className="primary" disabled={busy||!valid}>{busy?<LoaderCircle className="spin" size={13}/>:<Save size={13}/>} {t('common.save')}</button></footer></form></div>,document.body)
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


function SetupPage({onAuthenticated,onRequiresLogin}:{onAuthenticated:()=>void;onRequiresLogin:()=>void}){
	const {t}=useTranslation()
	const [password,setPassword]=useState('')
	const [confirmation,setConfirmation]=useState('')
	const [busy,setBusy]=useState(false)
	const [error,setError]=useState('')
	const submit=async(event:FormEvent)=>{
		event.preventDefault()
		if(password!==confirmation){setError(t('password.mismatch'));return}
		setBusy(true);setError('')
		try{await api.initializePassword(password);setPassword('');setConfirmation('');onAuthenticated()}
		catch(err){
			try{if((await api.authStatus()).initialized){onRequiresLogin();return}}catch{/* keep the initialization error */}
			setError(errorText(err))
		}finally{setBusy(false)}
	}
	return <div className="auth-screen"><LanguageSwitch/><section className="login-card"><div className="login-mark"><KeyRound size={29}/></div><span>{t('auth.setupLabel')}</span><h1>{t('auth.setupTitle')}</h1><p>{t('auth.setupText')}</p><form onSubmit={submit}><label><span>{t('password.replacement')}</span><div className="login-input"><LockKeyhole size={17}/><PasswordInput aria-label={t('password.replacement')} autoComplete="new-password" minLength={12} value={password} onChange={event=>setPassword(event.target.value)} autoFocus required/></div></label><label><span>{t('password.confirmation')}</span><div className="login-input"><ShieldCheck size={17}/><PasswordInput aria-label={t('password.confirmation')} autoComplete="new-password" minLength={12} value={confirmation} onChange={event=>setConfirmation(event.target.value)} required/></div></label>{error&&<div className="login-error"><ShieldAlert size={15}/>{error}</div>}<button className="primary" disabled={busy||password.length<12||confirmation.length<12}>{busy?<LoaderCircle className="spin" size={17}/>:<ShieldCheck size={17}/>}<span>{busy?t('auth.initializing'):t('auth.initialize')}</span></button></form></section></div>
}

function DestructiveConfirmDialog({label,title,description,busy,onCancel,onConfirm}:{label:string;title:string;description:string;busy:boolean;onCancel:()=>void;onConfirm:()=>void}){
	const {t}=useTranslation()
	useEffect(()=>{const close=(event:KeyboardEvent)=>{if(event.key==='Escape'&&!busy)onCancel()};window.addEventListener('keydown',close);return()=>window.removeEventListener('keydown',close)},[busy,onCancel])
	return <div className="destructive-dialog-backdrop" onMouseDown={event=>{if(event.target===event.currentTarget&&!busy)onCancel()}}><section className="destructive-dialog panel" role="dialog" aria-modal="true" aria-labelledby="destructive-dialog-title"><header><Trash2 size={21}/><span><small>{label}</small><h2 id="destructive-dialog-title">{title}</h2></span></header><p>{description}</p><footer><button type="button" autoFocus disabled={busy} onClick={onCancel}>{t('common.cancel')}</button><button type="button" className="danger" disabled={busy} onClick={onConfirm}>{busy?<LoaderCircle className="spin" size={14}/>:<Trash2 size={14}/>} {busy?t('common.deleting'):t('common.delete')}</button></footer></section></div>
}

function FullAccessConfirmDialog({onCancel,onConfirm}:{onCancel:()=>void;onConfirm:()=>void}){
	const {t}=useTranslation()
	return createPortal(<div className="destructive-dialog-backdrop" onMouseDown={event=>{if(event.target===event.currentTarget)onCancel()}}><section className="destructive-dialog panel" role="dialog" aria-modal="true" aria-labelledby="full-access-dialog-title"><header><ShieldAlert size={21}/><span><small>FULL ACCESS</small><h2 id="full-access-dialog-title">{t('settings.fullAccessTitle')}</h2></span></header><p>{t('settings.fullAccessConfirm')}</p><footer><button type="button" autoFocus onClick={onCancel}>{t('common.cancel')}</button><button type="button" className="danger" onClick={onConfirm}><ShieldAlert size={14}/>{t('common.enable')}</button></footer></section></div>,document.body)
}

function LoginPage({onAuthenticated}:{onAuthenticated:()=>void}){
	const {t}=useTranslation()
	const [password,setPassword]=useState('')
	const [busy,setBusy]=useState(false)
	const [error,setError]=useState('')
	const submit=async(event:FormEvent)=>{event.preventDefault();setBusy(true);setError('');try{await api.login(password);setPassword('');onAuthenticated()}catch(err){setError(errorText(err))}finally{setBusy(false)}}
		return <div className="auth-screen"><LanguageSwitch/><section className="login-card"><div className="login-mark"><TerminalSquare size={29}/></div><span>{t('auth.subtitle')}</span><h1>{t('auth.title')}</h1><form onSubmit={submit}><label><div className="login-input"><LockKeyhole size={17}/><PasswordInput aria-label={t('password.current')} autoComplete="current-password" value={password} onChange={event=>setPassword(event.target.value)} autoFocus required/></div></label>{error&&<div className="login-error"><ShieldAlert size={15}/>{error}</div>}<button className="primary" disabled={busy||password.length===0}>{busy?<LoaderCircle className="spin" size={17}/>:<ShieldCheck size={17}/>}<span>{busy?t('auth.authenticating'):t('auth.enter')}</span></button></form></section></div>
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
	  const addressToggleLabel=t(showAddresses?'config.hideAddresses':'config.showAddresses')
	  return <div className="configuration-center page-stack">
	    <div className="configuration-tabs-row"><div className="configuration-tabs" role="tablist" aria-label={t('config.sections')}>{tabs.map(([id,icon,label,meta])=><button type="button" role="tab" aria-selected={section===id} className={section===id?'active':''} onClick={()=>setSection(id)} key={id}>{icon}<span><b>{label}</b><small>{meta}</small></span><ChevronRight size={15}/></button>)}</div><button type="button" className={`icon-button configuration-address-toggle ${showAddresses?'active':''}`} aria-label={addressToggleLabel} title={addressToggleLabel} onClick={()=>setShowAddresses(value=>!value)}>{showAddresses?<EyeOff size={17}/>:<Eye size={17}/>}</button></div>
    <div className="configuration-content" role="tabpanel">
	  {section==='models'&&<ModelsPage providers={providers} proxies={proxies} health={health} showAddresses={showAddresses} refresh={refresh}/>}
	  {section==='hosts'&&<HostsPage hosts={hosts} proxies={proxies} showAddresses={showAddresses} refresh={refresh}/>}
	  {section==='proxies'&&<ProxiesPage proxies={proxies} showAddresses={showAddresses} refresh={refresh}/>}
	  {section==='system'&&<SystemSettingsPage settings={settings} providers={providers} proxies={proxies} capabilities={capabilities} modelStatus={health?.model} refresh={refresh}/>}
    </div>
  </div>
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
			<div className="extension-tabs configuration-tabs" role="tablist" aria-label={t('extensions.sections')}>{tabs.map(([id,icon,label,meta])=><button type="button" role="tab" aria-selected={section===id} className={section===id?'active':''} onClick={()=>setSection(id)} key={id}>{icon}<span><b>{label}</b><small>{meta}</small></span><ChevronRight size={15}/></button>)}</div>
		<div className="configuration-content" role="tabpanel">
			{section==='overview'&&<div className="extension-overview"><button className="panel" onClick={()=>setSection('skills')}><div><BookOpen size={21}/></div><span><h3>Skills</h3></span><strong>{enabledSkills}<small>{t('extensions.enabledUnit')}</small></strong><ChevronRight size={16}/></button><button className="panel" onClick={()=>setSection('mcp')}><div><Zap size={21}/></div><span><h3>{t('extensions.tabs.mcp')}</h3></span><strong>{readyMCP}<small>{t('extensions.readyUnit')}</small></strong><ChevronRight size={16}/></button><button className="panel" onClick={()=>setSection('tools')}><div><FunctionSquare size={21}/></div><span><h3>{t('extensions.tabs.tools')}</h3></span><strong>{toolCatalog?.count??0}<small>{t('extensions.functionsUnit')}</small></strong><ChevronRight size={16}/></button></div>}
			{section==='skills'&&<SkillsPage skills={skills} refresh={refresh}/>}
			{section==='mcp'&&<MCPServersPage servers={mcpServers} refresh={refresh}/>}
			{section==='tools'&&<LLMToolsPage catalog={toolCatalog} refresh={refresh}/>}
		</div>
	</div>
}

type MCPFormState = {
	id?:string;name:string;transport:MCPTransport;command:string;argsText:string;cwd:string;url:string;envText:string;headersText:string;enabled:boolean;clearEnv:boolean;clearHeaders:boolean
}

const emptyMCPForm:MCPFormState={name:'',transport:'stdio',command:'',argsText:'',cwd:'',url:'',envText:'',headersText:'',enabled:false,clearEnv:false,clearHeaders:false}

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
	const [form,setForm]=useState<MCPFormState|null>(null)
	const [busy,setBusy]=useState('')
	const [notice,setNotice]=useState('')
	const [error,setError]=useState('')
	const [deleteCandidate,setDeleteCandidate]=useState<MCPServer|null>(null)
	const openCreate=()=>{setForm({...emptyMCPForm});setNotice('');setError('')}
	const openEdit=(server:MCPServer)=>{setForm({id:server.id,name:server.name,transport:server.transport,command:server.command||'',argsText:(server.args||[]).join('\n'),cwd:server.cwd||'',url:server.url||'',envText:'',headersText:'',enabled:server.enabled,clearEnv:false,clearHeaders:false});setNotice('');setError('')}
	const save=async(event:FormEvent)=>{event.preventDefault();if(!form)return;setBusy('save');setError('');try{
		const input:MCPServerInput={id:form.id,name:form.name.trim(),transport:form.transport,command:form.transport==='stdio'?form.command.trim():'',args:form.transport==='stdio'?form.argsText.split(/\r?\n/).map(item=>item.trim()).filter(Boolean):[],cwd:form.transport==='stdio'?form.cwd.trim():'',url:form.transport==='streamable_http'?form.url.trim():'',enabled:form.enabled}
		if(!form.id||form.envText.trim()||form.clearEnv)input.env=form.clearEnv?{}:parseMCPPairs(form.envText,'env')
		if(!form.id||form.headersText.trim()||form.clearHeaders)input.headers=form.clearHeaders?{}:parseMCPPairs(form.headersText,'header')
			const saved=await api.saveMCPServer(input);setForm(null);setNotice(`${t('mcp.saved',{name:saved.name,status:t(`statusLabels.${saved.status}`,{defaultValue:saved.status})})}${saved.last_error?` · ${saved.last_error}`:''}`);await refresh()
	}catch(err){setError(errorText(err))}finally{setBusy('')}}
	const test=async(server:MCPServer)=>{setBusy(`test-${server.id}`);setError('');try{const result=await api.testMCPServer(server.id);setNotice(t('mcp.healthy',{count:result.tool_count,latency:result.latency_ms}))}catch(err){setError(errorText(err))}finally{setBusy('')}}
	const toggle=async(server:MCPServer)=>{setBusy(`toggle-${server.id}`);setError('');try{const result=await api.setMCPServerEnabled(server.id,!server.enabled);setNotice(`${t('mcp.toggled',{name:result.name,state:result.enabled?t('common.enabled'):t('common.disabled'),status:t(`statusLabels.${result.status}`,{defaultValue:result.status})})}${result.last_error?` · ${result.last_error}`:''}`);await refresh()}catch(err){setError(errorText(err))}finally{setBusy('')}}
	const retry=async(server:MCPServer)=>{setBusy(`retry-${server.id}`);setError('');try{const result=await api.retryMCPServer(server.id);setNotice(t('mcp.reconnected',{name:result.name,count:result.tool_count}));await refresh()}catch(err){setError(errorText(err));await refresh()}finally{setBusy('')}}
	const remove=async()=>{if(!deleteCandidate)return;const server=deleteCandidate;setBusy(`delete-${server.id}`);setError('');try{await api.deleteMCPServer(server.id);setNotice(t('mcp.deleted',{name:server.name}));await refresh()}catch(err){setError(errorText(err))}finally{setBusy('');setDeleteCandidate(null)}}
		return <div className="mcp-page page-stack">
			<div className="page-actions"><div/><button className="primary" onClick={openCreate}><Plus size={15}/>{t('mcp.add')}</button></div>
		<div className="mcp-boundary-note"><ShieldAlert size={16}/><div><b>{t('mcp.boundary')}</b><span>{t('mcp.boundaryText')}</span></div></div>
		{notice&&<div className="notice">{notice}<button onClick={()=>setNotice('')}><X size={14}/></button></div>}
		{error&&<div className="skill-error"><ShieldAlert size={15}/>{error}<button onClick={()=>setError('')}><X size={14}/></button></div>}
		{form&&<form className="mcp-form panel" onSubmit={save}><header><div><Zap size={19}/><span><h3>{form.id?form.name||t('mcp.server'):t('mcp.connect')}</h3></span></div><button type="button" onClick={()=>setForm(null)} title={t('common.close')}><X size={15}/></button></header><div className="mcp-form-grid"><label><span>{t('mcp.displayName')}</span><input value={form.name} onChange={event=>setForm({...form,name:event.target.value})} required/></label><label><span>{t('mcp.transport')}</span><select value={form.transport} onChange={event=>setForm({...form,transport:event.target.value as MCPTransport})}><option value="stdio">{t('mcp.localProcess')}</option><option value="streamable_http">Streamable HTTP</option></select></label>{form.transport==='stdio'?<><label><span>{t('mcp.command')}</span><input value={form.command} onChange={event=>setForm({...form,command:event.target.value})} required/></label><label><span>{t('mcp.cwd')}</span><input value={form.cwd} onChange={event=>setForm({...form,cwd:event.target.value})}/></label><label className="mcp-wide"><span>{t('mcp.args')}</span><textarea value={form.argsText} onChange={event=>setForm({...form,argsText:event.target.value})}/></label></>:<label className="mcp-wide"><span>{t('mcp.endpoint')}</span><input value={form.url} onChange={event=>setForm({...form,url:event.target.value})} required/></label>}<label className="mcp-wide"><span>{t('mcp.env')}</span><textarea value={form.envText} onChange={event=>setForm({...form,envText:event.target.value,clearEnv:false})} placeholder={form.id?t('mcp.preserve'):''}/>{form.id&&<small><label><input type="checkbox" checked={form.clearEnv} onChange={event=>setForm({...form,clearEnv:event.target.checked,envText:event.target.checked?'':form.envText})}/> {t('mcp.clearEnv')}</label></small>}</label><label className="mcp-wide"><span>{t('mcp.headers')}</span><textarea value={form.headersText} onChange={event=>setForm({...form,headersText:event.target.value,clearHeaders:false})} placeholder={form.id?t('mcp.preserve'):''}/>{form.id&&<small><label><input type="checkbox" checked={form.clearHeaders} onChange={event=>setForm({...form,clearHeaders:event.target.checked,headersText:event.target.checked?'':form.headersText})}/> {t('mcp.clearHeaders')}</label></small>}</label></div><footer><label className="mcp-enable-on-save"><input type="checkbox" checked={form.enabled} onChange={event=>setForm({...form,enabled:event.target.checked})}/><i/><span><b>{t('mcp.enableAfterSave')}</b></span></label><button type="button" onClick={()=>setForm(null)}>{t('common.cancel')}</button><button className="primary" disabled={busy==='save'}>{busy==='save'?<LoaderCircle className="spin" size={14}/>:<Save size={14}/>} {busy==='save'?t('common.saving'):t('mcp.saveServer')}</button></footer></form>}
		<div className="mcp-grid">{servers.map(server=><article className={`mcp-card panel ${server.status}`} key={server.id}><header><div className="mcp-card-icon"><Zap size={19}/></div><span><h3>{server.name}</h3><code>{server.transport==='stdio'?server.command:server.url}</code></span><em className={server.status}><CircleDot size={9}/>{t(`statusLabels.${server.status}`,{defaultValue:server.status})}</em></header><dl><div><dt>{t('mcp.discoveredTools')}</dt><dd>{server.tool_count}</dd></div><div><dt>{t('mcp.secrets')}</dt><dd>{t('mcp.configuredSecrets',{count:(server.env_keys?.length||0)+(server.header_keys?.length||0)})}</dd></div><div><dt>{t('mcp.lastConnected')}</dt><dd>{server.connected_at?new Date(server.connected_at).toLocaleString(localeFor(instance.language)):'—'}</dd></div></dl>{server.last_error&&<div className="mcp-card-error"><ShieldAlert size={13}/><span>{server.last_error}</span></div>}<div className="mcp-actions"><button onClick={()=>void test(server)} disabled={!!busy}><Activity size={13}/>{busy===`test-${server.id}`?t('common.testing'):t('common.test')}</button><button onClick={()=>openEdit(server)} disabled={!!busy}><Edit3 size={13}/>{t('common.edit')}</button>{server.enabled&&server.status!=='ready'&&<button onClick={()=>void retry(server)} disabled={!!busy}><RefreshCw className={busy===`retry-${server.id}`?'spin':''} size={13}/>{t('common.retry')}</button>}<button className={server.enabled?'disable':'enable'} onClick={()=>void toggle(server)} disabled={!!busy}>{busy===`toggle-${server.id}`?<LoaderCircle className="spin" size={13}/>:server.enabled?<X size={13}/>:<Check size={13}/>} {server.enabled?t('common.disable'):t('common.enable')}</button><button className="danger" title={t('common.delete')} onClick={()=>setDeleteCandidate(server)} disabled={!!busy}><Trash2 size={13}/></button></div>{server.tools?.length?<details className="mcp-tools"><summary>{t('mcp.modelTools',{count:server.tools.length})} <ChevronRight size={13}/></summary><div>{server.tools.map(item=><section key={item.exposed_name}><code>{item.exposed_name}</code><span>{t('mcp.remote')} · {item.name}</span><p>{item.description}</p></section>)}</div></details>:null}</article>)}</div>
		{!servers.length&&<Empty icon={<Zap/>} title={t('mcp.emptyTitle')}/>}
		{deleteCandidate&&<DestructiveConfirmDialog label={t('mcp.deleteDialogLabel')} title={t('mcp.deleteTitle',{name:deleteCandidate.name})} description={t('mcp.deleteText')} busy={busy===`delete-${deleteCandidate.id}`} onCancel={()=>setDeleteCandidate(null)} onConfirm={()=>void remove()}/>}
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
			<dl><div><dt>{t('common.agent')}</dt><dd>{catalog?.agent||'ops-pilot'}</dd></div><div><dt>{t('common.model')}</dt><dd>{catalog?.model||t('tools.notLoaded')}</dd></div><div><dt>{t('common.functions')}</dt><dd>{catalog?.count??0} / {catalog?.total??0}</dd></div><div><dt>{t('tools.execution')}</dt><dd>{catalog?.execution_mode||'sequential'}</dd></div></dl>
			<button className="tool-catalog-refresh" onClick={refreshCatalog} disabled={refreshing}><RefreshCw className={refreshing?'spin':''} size={14}/>{refreshing?t('common.refreshing'):t('tools.refreshSnapshot')}</button>
		</section>
			{error&&<div className="tool-function-error"><ShieldAlert size={15}/><span>{error}</span><button onClick={()=>setError('')} title={t('common.dismiss')}><X size={14}/></button></div>}
		<div className="tool-catalog-metrics"><Metric label={t('tools.enabledFunctions')} value={String(catalog?.count??0)} tone="green"/><Metric label={t('tools.availableFunctions')} value={String(catalog?.total??0)}/><Metric label={t('tools.readOnlyEnabled')} value={String(readOnlyCount)}/><Metric label={t('tools.approvalEnabled')} value={String(protectedCount)} tone="amber"/></div>
		<div className="tool-catalog-toolbar panel"><label><Search size={15}/><input value={query} onChange={event=>setQuery(event.target.value)} placeholder={t('tools.searchPlaceholder')}/></label><select value={category} onChange={event=>setCategory(event.target.value)}><option value="all">{t('tools.allCategories',{count:tools.length})}</option>{categories.map(value=><option value={value} key={value}>{toolCategoryLabel(value)} · {tools.filter(tool=>tool.category===value).length}</option>)}</select><span>{t('tools.visible',{count:filtered.length})}</span></div>
		{!catalog?<div className="tool-catalog-loading panel"><LoaderCircle className="spin" size={20}/>{t('tools.loadingSnapshot')}</div>:!catalog.loaded?<Empty icon={<FunctionSquare/>} title={t('tools.runtimeMissing')} text={t('tools.runtimeMissingText')}/>:<div className="tool-catalog-browser">
			<section className="tool-function-list panel">{filtered.length?filtered.map(tool=>{const count=toolParameters(tool).length;return <button className={`${selected?.name===tool.name?'active':''} ${tool.enabled?'':'disabled'}`} onClick={()=>setSelectedName(tool.name)} key={tool.name}><div className="tool-function-icon"><Braces size={16}/></div><span><code>{tool.name}</code><p>{tool.description}</p><small><em>{toolCategoryLabel(tool.category)}</em><i className={tool.guard}>{toolGuardLabel(tool.guard)}</i>{!tool.enabled&&<i className="disabled">{t('tools.disabled')}</i>}</small></span><b>{count}<small>{t('tools.argsUnit')}</small></b><ChevronRight size={14}/></button>}):<div className="tool-filter-empty"><Search size={20}/><b>{t('tools.noMatch')}</b></div>}</section>
			<aside className={`tool-function-inspector panel ${selected?.enabled?'':'disabled'}`}>{selected?<><header><div className="tool-function-icon"><FunctionSquare size={18}/></div><span><small>{t('tools.functionDetail')}</small><code>{selected.name}</code></span><div className="tool-function-controls"><em className={selected.guard}>{toolGuardLabel(selected.guard)}</em><button className={selected.enabled?'enabled':''} role="switch" aria-checked={selected.enabled} onClick={()=>void setEnabled(selected)} disabled={busyName===selected.name} title={selected.enabled?t('tools.disableFunction'):t('tools.enableFunction')}>{busyName===selected.name?<LoaderCircle className="spin" size={14}/>:<Power size={14}/>}<span>{selected.enabled?t('common.enabled'):t('common.disabled')}</span></button></div></header><p className="tool-function-description">{selected.description}</p><dl className="tool-function-meta"><div><dt>{t('tools.category')}</dt><dd>{toolCategoryLabel(selected.category)}</dd></div><div><dt>{t('common.arguments')}</dt><dd>{parameters.length}</dd></div><div><dt>{t('tools.safetyGate')}</dt><dd>{toolGuardLabel(selected.guard)}</dd></div></dl><section className="tool-parameter-list"><h3>{t('tools.inputParameters')} <span>{t('tools.requiredCount',{count:parameters.filter(item=>item.required).length})}</span></h3>{parameters.length?parameters.map(parameter=><div key={parameter.name}><code>{parameter.name}</code><em>{parameter.type}</em>{parameter.required&&<b>{t('common.required')}</b>}{parameter.description&&<p>{parameter.description}</p>}</div>):<p className="tool-no-arguments">{t('tools.noArguments')}</p>}</section><details className="tool-schema-raw"><summary>{t('tools.rawSchema')} <ChevronRight size={13}/></summary><CopyablePre>{JSON.stringify(selected.input_schema,null,2)}</CopyablePre></details></>:<div className="tool-inspector-empty"><Braces size={26}/></div>}</aside>
		</div>}
	</div>
}

function SkillsPage({skills,refresh}:{skills:ManagedSkill[];refresh:()=>Promise<void>}){
	const {t,i18n:instance}=useTranslation()
	const [query,setQuery]=useState('')
	const [selectedName,setSelectedName]=useState('')
	const [selected,setSelected]=useState<ManagedSkill|null>(null)
	const [draft,setDraft]=useState('')
	const [loading,setLoading]=useState(false)
	const [saving,setSaving]=useState(false)
	const [uploading,setUploading]=useState(false)
	const [uploadOpen,setUploadOpen]=useState(false)
	const [uploadName,setUploadName]=useState('')
	const [uploadFile,setUploadFile]=useState<File|null>(null)
	const [deleteName,setDeleteName]=useState('')
	const [deleting,setDeleting]=useState(false)
	const [toggling,setToggling]=useState(false)
	const [notice,setNotice]=useState('')
	const [error,setError]=useState('')
	const filtered=useMemo(()=>{const needle=query.trim().toLowerCase();return skills.filter(skill=>!needle||`${skill.name} ${skill.summary}`.toLowerCase().includes(needle))},[skills,query])
	useEffect(()=>{if(!skills.length){setSelectedName('');setSelected(null);setDraft('');return}if(!selectedName||!skills.some(skill=>skill.name===selectedName))setSelectedName(skills[0].name)},[skills,selectedName])
	useEffect(()=>{if(!selectedName)return;let cancelled=false;setLoading(true);setError('');api.skill(selectedName).then(skill=>{if(cancelled)return;setSelected(skill);setDraft(skill.content||'')}).catch(err=>{if(!cancelled)setError(errorText(err))}).finally(()=>{if(!cancelled)setLoading(false)});return()=>{cancelled=true}},[selectedName])
	const dirty=!!selected&&draft!==selected.content
	const selectFile=(file:File|null)=>{setUploadFile(file);if(file&&!uploadName){const base=file.name.replace(/\.(markdown|md|zip)$/i,'').replace(/[^A-Za-z0-9_.-]+/g,'-').replace(/^-+|-+$/g,'').slice(0,64);setUploadName(base)}}
	const upload=async(event:FormEvent)=>{event.preventDefault();if(!uploadFile)return;setUploading(true);setError('');setNotice('');try{const result=await api.uploadSkill(uploadName.trim(),uploadFile);await refresh();setSelectedName(result.name);setSelected(result);setDraft(result.content||'');setUploadOpen(false);setUploadName('');setUploadFile(null);setNotice(t('skills.uploaded',{name:result.name}))}catch(err){setError(errorText(err))}finally{setUploading(false)}}
	const save=async()=>{if(!selected)return;setSaving(true);setError('');setNotice('');try{const result=await api.saveSkill(selected.name,draft);setSelected(result);setDraft(result.content||'');await refresh();setNotice(t('skills.saved',{name:result.name}))}catch(err){setError(errorText(err))}finally{setSaving(false)}}
	const permanentlyDelete=async()=>{if(!deleteName)return;setDeleting(true);setError('');try{await api.deleteSkill(deleteName);setDeleteName('');setSelectedName('');setSelected(null);setDraft('');await refresh();setNotice(t('skills.deleted',{name:deleteName}))}catch(err){setError(errorText(err))}finally{setDeleting(false)}}
	const toggleEnabled=async()=>{if(!selected)return;setToggling(true);setError('');setNotice('');try{const result=await api.setSkillEnabled(selected.name,!selected.enabled);setSelected(result);setDraft(result.content||draft);await refresh();setNotice(t(result.enabled?'skills.toggledEnabled':'skills.toggledDisabled',{name:result.name}))}catch(err){setError(errorText(err))}finally{setToggling(false)}}

	return <div className="skills-page page-stack">
			<div className="page-actions"><div/><button className="primary" onClick={()=>{setUploadOpen(value=>!value);setError('')}}><UploadCloud size={15}/>{uploadOpen?t('skills.closeUpload'):t('skills.uploadSkill')}</button></div>
		{notice&&<div className="notice">{notice}<button onClick={()=>setNotice('')}><X size={14}/></button></div>}
		{error&&<div className="skill-error"><ShieldAlert size={15}/>{error}<button onClick={()=>setError('')}><X size={14}/></button></div>}
		{uploadOpen&&<form className="skill-upload-panel panel" onSubmit={upload}><div><div className="skill-upload-icon"><UploadCloud size={20}/></div><span><b>{t('skills.uploadPackage')}</b><small>{t('skills.packageHelp')}</small></span></div><label><span>{t('skills.skillName')}</span><input value={uploadName} onChange={event=>setUploadName(event.target.value)} pattern="[A-Za-z0-9][A-Za-z0-9_.-]{0,63}" required/></label><label className="skill-file-picker"><FileText size={15}/><span><b>{uploadFile?.name||t('skills.choosePackage')}</b><small>{uploadFile?formatFileSize(uploadFile.size):t('skills.maxPackage')}</small></span><input type="file" accept=".md,.markdown,.zip,text/markdown,application/zip" onChange={event=>selectFile(event.target.files?.[0]||null)} required/></label><button className="primary" disabled={uploading||!uploadFile||!uploadName.trim()}>{uploading?<LoaderCircle className="spin" size={14}/>:<UploadCloud size={14}/>} {uploading?t('common.uploading'):t('skills.uploadActivate')}</button></form>}
		<section className="skill-registry-summary panel"><div><BookOpen size={19}/><span><b>{t('skills.summary',{enabled:skills.filter(skill=>skill.enabled).length,total:skills.length})}</b></span></div><label><Search size={14}/><input value={query} onChange={event=>setQuery(event.target.value)} placeholder={t('skills.search')}/></label></section>
		<div className="skill-manager-layout">
			<section className="skill-list panel">{filtered.length?filtered.map(skill=><button className={`${selectedName===skill.name?'active':''} ${skill.enabled?'':'disabled'}`} onClick={()=>setSelectedName(skill.name)} key={skill.name}><div className="skill-card-icon"><BookOpen size={16}/></div><span><code>{skill.name}</code>{skill.summary&&<p>{skill.summary}</p>}<small><em className={skill.enabled?'enabled':'disabled'}>{skill.enabled?t('common.enabled'):t('common.disabled')}</em>{skill.file_count||1} {t('common.files')} · {formatFileSize(skill.size_bytes||0)}{skill.updated_at?` · ${new Date(skill.updated_at).toLocaleDateString(localeFor(instance.language))}`:''}</small></span><ChevronRight size={14}/></button>):<div className="skill-list-empty"><BookOpen size={23}/><b>{skills.length?t('skills.noMatch'):t('skills.noneInstalled')}</b></div>}</section>
				<section className="skill-editor panel">{loading?<div className="skill-editor-state"><LoaderCircle className="spin" size={21}/>{t('skills.loading')}</div>:selected?<><header><div><BookOpen size={17}/><span><small>{t('skills.managed')} · {selected.enabled?t('common.enabled'):t('common.disabled')}</small><code>{selected.name}</code></span></div><section><button className={selected.enabled?'skill-disable':'skill-enable'} disabled={toggling} onClick={toggleEnabled}>{toggling?<LoaderCircle className="spin" size={13}/>:selected.enabled?<X size={13}/>:<Check size={13}/>} {selected.enabled?t('common.disable'):t('common.enable')}</button><button disabled={!dirty||saving} onClick={save}>{saving?<LoaderCircle className="spin" size={13}/>:<Save size={13}/>} {saving?t('common.saving'):t('skills.saveChanges')}</button><button className="danger" onClick={()=>setDeleteName(selected.name)}><Trash2 size={13}/>{t('common.delete')}</button></section></header><div className="skill-editor-meta"><span><b>SHA256</b><code title={selected.content_sha256}>{selected.content_sha256?.slice(0,16)||'—'}</code></span><span><b>{t('common.files')}</b><code>{selected.file_count||1}</code></span><span><b>{t('common.size')}</b><code>{formatFileSize(selected.size_bytes||0)}</code></span><span><b>{t('common.updated')}</b><code>{selected.updated_at?new Date(selected.updated_at).toLocaleString(localeFor(instance.language)):'—'}</code></span></div><div className="skill-editor-split"><label><span>SKILL.md</span><textarea value={draft} spellCheck={false} onChange={event=>setDraft(event.target.value)}/></label><section><span>{t('skills.livePreview')}</span><div className="markdown-body"><Markdown skipHtml remarkPlugins={[remarkGfm]} components={{a:({href,children})=><a href={href} target="_blank" rel="noopener noreferrer">{children}</a>,img:({alt})=><span className="markdown-image-blocked">{t('skills.blockedImage',{alt:alt||t('common.image')})}</span>,pre:({children})=><CopyablePre>{children}</CopyablePre>}}>{draft||t('skills.emptySkill')}</Markdown></div></section></div></>:<div className="skill-editor-state"><BookOpen size={25}/><b>{t('skills.select')}</b></div>}</section>
		</div>
		{deleteName&&<div className="skill-delete-backdrop"><section className="skill-delete-dialog panel" role="dialog" aria-modal="true"><div><Trash2 size={21}/><span><small>{t('skills.permanentDelete')}</small><h3>{t('skills.deleteTitle',{name:deleteName})}</h3></span></div><p>{t('skills.deleteText')}</p><footer><button disabled={deleting} onClick={()=>setDeleteName('')}>{t('common.cancel')}</button><button className="danger" disabled={deleting} onClick={permanentlyDelete}>{deleting?<LoaderCircle className="spin" size={14}/>:<Trash2 size={14}/>} {deleting?t('common.deleting'):t('skills.permanentlyDelete')}</button></footer></section></div>}
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
  const savedValue=settings?.agent_max_iterations??50
  const savedPrompt=settings?.system_prompt??''
	const defaultPrompt=settings?.default_system_prompt??''
  const savedExplanation=settings?.approval_explanations_enabled??true
	  const savedSubagentProvider=settings?.subagent_model_provider_id??''
	  const savedSubagentTimeout=settings?.subagent_timeout_seconds??30
	  const savedImageTypes=settings?.chat_image_allowed_types??defaultChatImageTypes
  const savedShellMode=settings?.workspace_shell_mode??(settings?.workspace_shell_platform==='windows'?'host':'sandbox')
  const [maxIterations,setMaxIterations]=useState(savedValue)
  const [systemPrompt,setSystemPrompt]=useState(savedPrompt)
  const [explanationEnabled,setExplanationEnabled]=useState(savedExplanation)
  const [subagentProvider,setSubagentProvider]=useState(savedSubagentProvider)
	  const [subagentTimeout,setSubagentTimeout]=useState(savedSubagentTimeout)
	  const [imageTypes,setImageTypes]=useState(savedImageTypes)
  const [shellMode,setShellMode]=useState<WorkspaceShellMode>(savedShellMode)
	const [iterationsDirty,setIterationsDirty]=useState(false)
	const [promptDirty,setPromptDirty]=useState(false)
	const [explanationDirty,setExplanationDirty]=useState(false)
	const [imagesDirty,setImagesDirty]=useState(false)
	const [shellDirty,setShellDirty]=useState(false)
	const [savingSection,setSavingSection]=useState<SystemSettingsSection|''>('')
	const [notice,setNotice]=useState('')
	useEffect(()=>{if(!iterationsDirty)setMaxIterations(savedValue)},[savedValue,iterationsDirty])
	useEffect(()=>{if(!promptDirty)setSystemPrompt(savedPrompt)},[savedPrompt,promptDirty])
	useEffect(()=>{if(!explanationDirty){setExplanationEnabled(savedExplanation);setSubagentProvider(savedSubagentProvider);setSubagentTimeout(savedSubagentTimeout)}},[savedExplanation,savedSubagentProvider,savedSubagentTimeout,explanationDirty])
	useEffect(()=>{if(!imagesDirty)setImageTypes(savedImageTypes)},[savedImageTypes,imagesDirty])
	useEffect(()=>{if(!shellDirty)setShellMode(savedShellMode)},[savedShellMode,shellDirty])
	const update=(value:number)=>{setMaxIterations(Math.max(5,Math.min(100,value||5)));setIterationsDirty(true);setNotice('')}
	const updateSystemPrompt=(value:string)=>{setSystemPrompt(value);setPromptDirty(true);setNotice('')}
	const restoreDefaultPrompt=()=>{setSystemPrompt(defaultPrompt);setPromptDirty(true);setNotice('')}
	const toggleExplanation=(value:boolean)=>{setExplanationEnabled(value);setExplanationDirty(true);setNotice('')}
	const selectSubagentProvider=(value:string)=>{setSubagentProvider(value);setExplanationDirty(true);setNotice('')}
	const updateSubagentTimeout=(value:number)=>{setSubagentTimeout(Math.max(5,Math.min(120,value||5)));setExplanationDirty(true);setNotice('')}
	const toggleImageType=(value:string)=>{setImageTypes(current=>current.includes(value)?current.length===1?current:current.filter(item=>item!==value):[...current,value]);setImagesDirty(true);setNotice('')}
	const selectShellMode=(value:WorkspaceShellMode)=>{setShellMode(value);setShellDirty(true);setNotice('')}
	const discard=(section:SystemSettingsSection)=>{
		switch(section){
		case 'iterations':setMaxIterations(savedValue);setIterationsDirty(false);break
		case 'prompt':setSystemPrompt(savedPrompt);setPromptDirty(false);break
		case 'explanation':setExplanationEnabled(savedExplanation);setSubagentProvider(savedSubagentProvider);setSubagentTimeout(savedSubagentTimeout);setExplanationDirty(false);break
		case 'images':setImageTypes(savedImageTypes);setImagesDirty(false);break
		case 'shell':setShellMode(savedShellMode);setShellDirty(false);break
		}
		setNotice('')
	}
	const save=async(section:SystemSettingsSection)=>{
		const input:SystemSettingsInput={agent_max_iterations:section==='iterations'?maxIterations:savedValue}
		switch(section){
		case 'prompt':input.system_prompt=systemPrompt;break
		case 'explanation':input.approval_explanations_enabled=explanationEnabled;input.subagent_model_provider_id=subagentProvider;input.subagent_timeout_seconds=subagentTimeout;break
		case 'images':input.chat_image_allowed_types=imageTypes;break
		case 'shell':input.workspace_shell_mode=shellMode;break
		}
		setSavingSection(section)
		try{
			const result=await api.saveSystemSettings(input)
			switch(section){
			case 'iterations':setMaxIterations(result.agent_max_iterations);setIterationsDirty(false);break
			case 'prompt':setSystemPrompt(result.system_prompt);setPromptDirty(false);break
			case 'explanation':setExplanationEnabled(result.approval_explanations_enabled);setSubagentProvider(result.subagent_model_provider_id);setSubagentTimeout(result.subagent_timeout_seconds);setExplanationDirty(false);break
			case 'images':setImageTypes(result.chat_image_allowed_types);setImagesDirty(false);break
			case 'shell':setShellMode(result.workspace_shell_mode);setShellDirty(false);break
			}
			setNotice(t('settings.saved'))
			await refresh()
		}catch(err){setNotice(errorText(err))}finally{setSavingSection('')}
	}
	const submit=(section:SystemSettingsSection)=>(event:FormEvent)=>{event.preventDefault();void save(section)}
	const busy=!!savingSection
  return <div className="system-settings page-stack">

    {notice&&<div className="notice">{notice}<button onClick={()=>setNotice('')}><X size={14}/></button></div>}
		<div className="settings-form">
			<SettingsDisclosure icon={<SlidersHorizontal size={18}/>} title={t('settings.maxIterations')} meta={<strong>{maxIterations}</strong>}>
				<form onSubmit={submit('iterations')}><div className="iteration-editor"><input aria-label={t('settings.maxIterations')} type="range" min="5" max="100" step="1" value={maxIterations} onChange={event=>update(Number(event.target.value))}/><label><span>{t('settings.rounds')}</span><input type="number" min="5" max="100" value={maxIterations} onChange={event=>update(Number(event.target.value))}/></label></div><div className="iteration-presets"><span>{t('settings.quickPresets')}</span>{[20,50,100].map(value=><button type="button" className={maxIterations===value?'active':''} onClick={()=>update(value)} key={value}><b>{value}</b></button>)}</div><SettingsSectionFooter dirty={iterationsDirty} busy={busy} saving={savingSection==='iterations'} onDiscard={()=>discard('iterations')}/></form>
			</SettingsDisclosure>
			<SettingsDisclosure icon={<Bot size={18}/>} title={t('settings.systemPrompt')} meta={systemPrompt.length?t('settings.systemPromptCharacters',{count:systemPrompt.length}):undefined}>
				<form onSubmit={submit('prompt')}><div className="system-prompt-actions"><button type="button" disabled={systemPrompt===defaultPrompt} onClick={restoreDefaultPrompt}><RefreshCw size={13}/>{t('settings.restoreDefaultPrompt')}</button></div><textarea className="system-prompt-input" aria-label={t('settings.systemPrompt')} spellCheck={false} value={systemPrompt} onChange={event=>updateSystemPrompt(event.target.value)}/><small className="system-prompt-count">{systemPrompt.length?t('settings.systemPromptCharacters',{count:systemPrompt.length}):t('settings.emptySystemPrompt')}</small><SettingsSectionFooter dirty={promptDirty} busy={busy} saving={savingSection==='prompt'} onDiscard={()=>discard('prompt')}/></form>
			</SettingsDisclosure>
			<SettingsDisclosure icon={<BrainCircuit size={18}/>} title={t('settings.explanationSection')} meta={<span className={modelStatus?.approval_agent_available?'ready':'offline'}><CircleDot size={9}/>{modelStatus?.approval_agent_available?t('settings.runnerReady'):t('settings.modelUnavailable')}</span>}>
				<form onSubmit={submit('explanation')}><div className="subagent-settings"><label className="subagent-toggle"><span><b>{t('settings.commandAgent')}</b></span><input type="checkbox" checked={explanationEnabled} onChange={event=>toggleExplanation(event.target.checked)}/><i/></label><div className="subagent-config-grid"><label><span><b>{t('settings.modelProvider')}</b></span><select value={subagentProvider} onChange={event=>selectSubagentProvider(event.target.value)}><option value="">{t('settings.followMain')}</option>{providers.map(provider=><option value={provider.id} key={provider.id}>{provider.name} · {provider.model}</option>)}</select></label><label><span><b>{t('settings.requestTimeout')}</b></span><div className="subagent-timeout-input"><input aria-label={t('settings.timeout')} type="number" min="5" max="120" step="1" value={subagentTimeout} onChange={event=>updateSubagentTimeout(Number(event.target.value))}/><em>{t('settings.seconds',{count:subagentTimeout})}</em></div></label></div>{modelStatus?.approval_error&&<div className="subagent-runtime-error"><ShieldAlert size={14}/><span>{modelStatus.approval_error}</span></div>}</div><SettingsSectionFooter dirty={explanationDirty} busy={busy} saving={savingSection==='explanation'} onDiscard={()=>discard('explanation')}/></form>
			</SettingsDisclosure>
			<SettingsDisclosure icon={<ImagePlus size={18}/>} title={t('settings.chatImages')} meta={imageTypes.map(value=>value.replace('image/','').toUpperCase()).join(' · ')}>
				<form onSubmit={submit('images')}><div className="chat-image-formats">{[['image/png','PNG'],['image/jpeg','JPEG'],['image/webp','WebP'],['image/gif','GIF']].map(([value,label])=><label className={imageTypes.includes(value)?'active':''} key={value}><input type="checkbox" checked={imageTypes.includes(value)} disabled={imageTypes.length===1&&imageTypes.includes(value)} onChange={()=>toggleImageType(value)}/><span>{label}</span></label>)}</div><SettingsSectionFooter dirty={imagesDirty} busy={busy} saving={savingSection==='images'} onDiscard={()=>discard('images')}/></form>
			</SettingsDisclosure>
			<SettingsDisclosure icon={<TerminalSquare size={18}/>} title={t('settings.shellBackend')} meta={settings?.workspace_shell_platform||t('settings.detecting')}>
				<form onSubmit={submit('shell')}><div className="workspace-shell-modes" role="group" aria-label={t('settings.shellBackend')}><button type="button" className={shellMode==='sandbox'?'active':''} disabled={!settings?.workspace_sandbox_available} onClick={()=>selectShellMode('sandbox')}><ShieldCheck size={16}/><span><b>{t('settings.sandbox')}</b><small>{settings?.workspace_sandbox_available?t('settings.sandboxAvailable'):t('settings.unavailableHost')}</small></span></button><button type="button" className={`${shellMode==='host'?'active ':''}host`} disabled={!settings?.workspace_host_shell_available} onClick={()=>selectShellMode('host')}><TerminalSquare size={16}/><span><b>{t('settings.hostShell')}</b><small>{settings?.workspace_host_shell_available?`${settings.workspace_shell_name||t('settings.systemShell')} · ${t('settings.fullAuthority')}`:t('settings.noShell')}</small></span></button><button type="button" className={shellMode==='disabled'?'active':''} onClick={()=>selectShellMode('disabled')}><Power size={16}/><span><b>{t('settings.shellDisabled')}</b></span></button></div>{shellMode==='host'&&<div className="workspace-shell-warning"><ShieldAlert size={15}/><b>{t('settings.hostWarning')}</b></div>}{shellMode==='sandbox'&&!settings?.workspace_sandbox_available&&<div className="workspace-shell-warning"><ShieldAlert size={15}/><b>{t('settings.sandboxWarning')}</b></div>}<SettingsSectionFooter dirty={shellDirty} busy={busy} saving={savingSection==='shell'} onDiscard={()=>discard('shell')}/></form>
			</SettingsDisclosure>
		</div>
	<WorkspaceSettingsPanel workspaces={capabilities.workspaces} refresh={refresh} onNotice={setNotice}/>
	<MCPServerModePanel settings={settings} refresh={refresh}/>
	<WebSearchSettingsPanel proxies={proxies} refresh={refresh}/>
	<AdminPasswordPanel/>
  </div>
}

function WorkspaceSettingsPanel({workspaces,refresh,onNotice}:{workspaces:WorkspaceCapability[];refresh:()=>Promise<void>;onNotice:(value:string)=>void}){
	const {t}=useTranslation()
	const empty:WorkspaceInput={id:'',access:'read_only'}
	const [open,setOpen]=useState(false),[editing,setEditing]=useState(''),[input,setInput]=useState<WorkspaceInput>(empty),[busy,setBusy]=useState(''),[deleteCandidate,setDeleteCandidate]=useState<WorkspaceCapability|null>(null)
	const beginCreate=()=>{setEditing('');setInput(empty);setOpen(true);onNotice('')}
	const beginEdit=(workspace:WorkspaceCapability)=>{setEditing(workspace.id);setInput({id:workspace.id,access:workspace.access});setOpen(true);onNotice('')}
	const close=()=>{setOpen(false);setEditing('');setInput(empty)}
	const save=async()=>{if(!input.id.trim())return;setBusy('save');onNotice('');try{if(editing)await api.updateWorkspace(editing,{...input,id:editing});else await api.createWorkspace({...input,id:input.id.trim()});await refresh();onNotice(editing?t('workspace.settingsUpdated',{id:editing}):t('workspace.settingsCreated',{id:input.id.trim()}));close()}catch(err){onNotice(errorText(err))}finally{setBusy('')}}
	const remove=async()=>{if(!deleteCandidate)return;const workspace=deleteCandidate;setBusy(`delete-${workspace.id}`);onNotice('');try{await api.deleteWorkspace(workspace.id);await refresh();onNotice(t('workspace.settingsRemoved',{id:workspace.id}));if(editing===workspace.id)close()}catch(err){onNotice(errorText(err))}finally{setBusy('');setDeleteCandidate(null)}}
	return <SettingsDisclosure className="workspace-settings" icon={<FolderOpen size={18}/>} title={t('settings.capabilities')} meta={t('workspace.registeredCount',{count:workspaces.length})}><div className="workspace-settings-actions"><button type="button" onClick={beginCreate}><Plus size={13}/>{t('workspace.add')}</button></div>{open&&<div className="workspace-settings-editor"><label><span>{t('workspace.id')}</span><input value={input.id} disabled={!!editing} maxLength={64} onChange={event=>setInput(current=>({...current,id:event.target.value}))}/></label><label><span>{t('workspace.permission')}</span><select value={input.access} onChange={event=>setInput(current=>({...current,access:event.target.value as WorkspaceInput['access']}))}><option value="read_only">{t('workspace.readOnly')}</option><option value="read_write">{t('workspace.readWrite')}</option></select></label><div><button type="button" onClick={close}>{t('common.cancel')}</button><button type="button" className="primary" disabled={busy==='save'||!input.id.trim()} onClick={()=>void save()}>{busy==='save'?<LoaderCircle className="spin" size={13}/>:<Save size={13}/>} {t('common.save')}</button></div></div>}<div className="workspace-settings-list">{workspaces.map(workspace=><div className="workspace-settings-row" key={workspace.id}><code>{workspace.id}</code><em className={workspace.access}>{workspace.access==='read_write'?t('workspace.readWrite'):t('workspace.readOnly')}</em><button type="button" title={t('common.edit')} onClick={()=>beginEdit(workspace)}><Edit3 size={13}/></button><button type="button" className="danger" disabled={busy===`delete-${workspace.id}`} title={t('workspace.remove')} onClick={()=>setDeleteCandidate(workspace)}>{busy===`delete-${workspace.id}`?<LoaderCircle className="spin" size={13}/>:<Trash2 size={13}/>}</button></div>)}{!workspaces.length&&<div className="workspace-settings-empty">{t('settings.noWorkspace')}</div>}</div>{deleteCandidate&&<DestructiveConfirmDialog label={t('workspace.removeDialogLabel')} title={t('workspace.removeTitle',{id:deleteCandidate.id})} description={t('workspace.removeText')} busy={busy===`delete-${deleteCandidate.id}`} onCancel={()=>setDeleteCandidate(null)} onConfirm={()=>void remove()}/>}</SettingsDisclosure>
}

const defaultWebSearchInput:WebSearchSettingsInput={enabled:false,base_url:'https://api.tavily.com',api_key:'',proxy_id:'',timeout_seconds:20,max_results:10}

function MCPServerModePanel({settings,refresh}:{settings:SystemSettings|null;refresh:()=>Promise<void>}){
	const {t}=useTranslation()
	const [busy,setBusy]=useState<'start'|'stop'|'rotate'|''>(''),[token,setToken]=useState(''),[notice,setNotice]=useState('')
	const enabled=!!settings?.mcp_http_enabled
	const endpoint=`${window.location.origin}/mcp`
	useEffect(()=>{if(!enabled)setToken('')},[enabled])
	const update=async(nextEnabled:boolean,rotate=false)=>{
		if(!settings)return
		setBusy(rotate?'rotate':nextEnabled?'start':'stop')
		setNotice('')
		try{
			const result=await api.saveSystemSettings({
				agent_max_iterations:settings.agent_max_iterations,
				mcp_http_enabled:nextEnabled,
				rotate_mcp_http_token:rotate,
			})
			setToken(result.mcp_http_token||'')
			setNotice(t(nextEnabled?'mcpServerMode.started':'mcpServerMode.stopped'))
			if(desktopRuntime)await invoke('set_tray_mode',{enabled:result.mcp_http_enabled}).catch(()=>{})
			await refresh()
		}catch(err){setNotice(errorText(err))}finally{setBusy('')}
	}
	const copy=async(value:string,message:string)=>{
		try{await navigator.clipboard.writeText(value);setNotice(message)}
		catch(err){setNotice(errorText(err))}
	}
	const hideToTray=async()=>{
		try{await invoke('hide_to_tray')}
		catch(err){setNotice(errorText(err))}
	}
	return <SettingsDisclosure className="mcp-server-mode" icon={<Braces size={18}/>} title={t('mcpServerMode.title')} meta={enabled?t('common.enabled'):t('common.disabled')}>
		<div className="mcp-server-mode-fields">
			<label><span>{t('mcpServerMode.endpoint')}</span><div><input readOnly value={endpoint}/><button type="button" title={t('common.copy')} onClick={()=>void copy(endpoint,t('mcpServerMode.endpointCopied'))}><Copy size={13}/></button></div></label>
			{enabled&&<label><span>{t('mcpServerMode.token')}</span><div><input readOnly type={token?'text':'password'} value={token||'••••••••••••••••'} /><button type="button" disabled={!token} title={t('common.copy')} onClick={()=>void copy(token,t('mcpServerMode.tokenCopied'))}><Copy size={13}/></button></div>{!token&&settings?.mcp_http_token_configured&&<small>{t('mcpServerMode.tokenStored')}</small>}</label>}
		</div>
		{notice&&<p>{notice}</p>}
		<footer>
			{enabled&&desktopRuntime&&<button type="button" disabled={!!busy} onClick={()=>void hideToTray()}><Minimize2 size={13}/>{t('mcpServerMode.hideToTray')}</button>}
			{enabled&&<button type="button" disabled={!!busy} onClick={()=>void update(true,true)}>{busy==='rotate'?<LoaderCircle className="spin" size={13}/>:<RefreshCw size={13}/>} {t('mcpServerMode.rotate')}</button>}
			<button type="button" className={enabled?'danger':'primary'} disabled={!!busy||!settings} onClick={()=>void update(!enabled)}>{busy?<LoaderCircle className="spin" size={13}/>:<Power size={13}/>} {t(enabled?'mcpServerMode.stop':'mcpServerMode.start')}</button>
		</footer>
	</SettingsDisclosure>
}

function WebSearchSettingsPanel({proxies,refresh}:{proxies:Proxy[];refresh:()=>Promise<void>}){
	const {t}=useTranslation()
	const [stored,setStored]=useState<WebSearchSettings|null>(null),[input,setInput]=useState<WebSearchSettingsInput>(defaultWebSearchInput)
	const [loading,setLoading]=useState(true),[busy,setBusy]=useState(''),[dirty,setDirty]=useState(false),[notice,setNotice]=useState('')
	const hasEffectiveAPIKey=!!input.api_key?.trim()||!!stored?.has_api_key&&!input.clear_api_key
	const applyStored=(value:WebSearchSettings)=>{setStored(value);setInput({enabled:value.enabled,base_url:value.base_url,api_key:'',proxy_id:value.proxy_id||'',timeout_seconds:value.timeout_seconds,max_results:value.max_results});setDirty(false)}
	useEffect(()=>{let active=true;api.webSearchSettings().then(value=>{if(active)applyStored(value)}).catch(err=>{if(active)setNotice(errorText(err))}).finally(()=>{if(active)setLoading(false)});return()=>{active=false}},[])
	const update=<K extends keyof WebSearchSettingsInput>(key:K,value:WebSearchSettingsInput[K])=>{setInput(current=>({...current,[key]:value}));setDirty(true);setNotice('')}
	const save=async()=>{setBusy('save');setNotice('');try{const value=await api.saveWebSearchSettings(input);applyStored(value);setNotice(t('webSearch.saved'));await refresh()}catch(err){setNotice(errorText(err))}finally{setBusy('')}}
	const test=async()=>{setBusy('test');setNotice('');try{const result=await api.testWebSearch();setNotice(t('webSearch.testPassed',{count:result.results.length}))}catch(err){setNotice(errorText(err))}finally{setBusy('')}}
	const clearKey=()=>{setInput(current=>({...current,enabled:false,api_key:'',clear_api_key:true}));setDirty(true);setNotice('')}
	if(loading)return <SettingsDisclosure className="web-search-settings" icon={<Search size={18}/>} title={t('webSearch.title')} meta={t('common.loading')}><div className="settings-loading"><LoaderCircle className="spin" size={16}/>{t('common.loading')}</div></SettingsDisclosure>
	return <SettingsDisclosure className="web-search-settings" icon={<Search size={18}/>} title={t('webSearch.title')} meta={input.enabled?t('common.enabled'):t('common.disabled')}><label className="web-search-toggle"><span>{t('webSearch.title')}</span><input type="checkbox" checked={input.enabled} onChange={event=>update('enabled',event.target.checked)}/><i/><b>{input.enabled?t('common.enabled'):t('common.disabled')}</b></label><div className="web-search-grid"><label><span>{t('webSearch.baseURL')}</span><input value={input.base_url} onChange={event=>update('base_url',event.target.value)} placeholder="https://api.tavily.com"/></label><label><span>{t('webSearch.apiKey')}</span><PasswordInput value={input.api_key||''} onChange={event=>update('api_key',event.target.value)} placeholder={stored?.has_api_key?t('webSearch.savedSecret'):''}/></label><label><span>{t('common.proxy')}</span><select value={input.proxy_id||''} onChange={event=>update('proxy_id',event.target.value)}><option value="">{t('common.direct')}</option>{proxies.map(proxy=><option value={proxy.id} key={proxy.id}>{proxy.name} · {proxy.url}</option>)}</select></label><label><span>{t('webSearch.timeout')}</span><input type="number" min="5" max="120" value={input.timeout_seconds} onChange={event=>update('timeout_seconds',Number(event.target.value))}/></label><label><span>{t('webSearch.maxResults')}</span><input type="number" min="1" max="20" value={input.max_results} onChange={event=>update('max_results',Number(event.target.value))}/></label></div>{notice&&<p>{notice}</p>}<footer><div>{stored?.has_api_key&&<button type="button" className="danger" onClick={clearKey}>{t('webSearch.clearKey')}</button>}</div><button type="button" disabled={busy!==''||dirty||!stored?.enabled||!stored.has_api_key} onClick={()=>void test()}>{busy==='test'?<LoaderCircle className="spin" size={13}/>:<Search size={13}/>} {t('common.test')}</button><button type="button" className="primary" disabled={busy!==''||!dirty||input.enabled&&!hasEffectiveAPIKey} onClick={()=>void save()}>{busy==='save'?<LoaderCircle className="spin" size={13}/>:<Save size={13}/>} {t('common.save')}</button></footer></SettingsDisclosure>
}

function AdminPasswordPanel(){
		const {t}=useTranslation()
	const [current,setCurrent]=useState(''),[replacement,setReplacement]=useState(''),[confirmation,setConfirmation]=useState(''),[notice,setNotice]=useState(''),[busy,setBusy]=useState(false)
		const submit=async(event:FormEvent)=>{event.preventDefault();if(replacement!==confirmation){setNotice(t('password.mismatch'));return}setBusy(true);setNotice('');try{await api.changePassword(current,replacement);window.location.reload()}catch(err){setNotice(errorText(err))}finally{setBusy(false)}}
		return <form className="admin-password-form" onSubmit={submit}><SettingsDisclosure className="admin-password-panel" icon={<KeyRound size={18}/>} title={t('password.title')}><section><label><span>{t('password.current')}</span><PasswordInput autoComplete="current-password" value={current} onChange={event=>setCurrent(event.target.value)} required/></label><label><span>{t('password.replacement')}</span><PasswordInput autoComplete="new-password" minLength={12} placeholder={t('password.minimum')} value={replacement} onChange={event=>setReplacement(event.target.value)} required/></label><label><span>{t('password.confirmation')}</span><PasswordInput autoComplete="new-password" minLength={12} value={confirmation} onChange={event=>setConfirmation(event.target.value)} required/></label><button className="primary" disabled={busy||replacement.length<12}>{busy?t('password.changing'):t('password.change')}</button></section>{notice&&<p>{notice}</p>}</SettingsDisclosure></form>
}

function Nav({ active, icon, label, count, warn, onClick }: {active:boolean;icon:React.ReactNode;label:string;count?:number;warn?:boolean;onClick:()=>void}) {
  return <button className={`nav-item ${active ? 'active' : ''}`} onClick={onClick} title={label} aria-label={label}>{icon}<span>{label}</span>{count !== undefined && <em className={warn ? 'warn' : ''}>{count}</em>}</button>
}

function ChatPage({ hosts, approvals, runs, workspaceShells, capabilities, settings, imageTypes, agentAvailable, modelName, refresh, refreshApprovals, onCreateWorkspaceShell, onOpenWorkspaceShell, onWorkspaceShellStarted, onSettingsChanged, onError, onStreamingChange }: {hosts:Host[];approvals:Approval[];runs:Run[];workspaceShells:SSHShell[];capabilities:ToolCapabilities;settings:SystemSettings|null;imageTypes:string[];agentAvailable:boolean;modelName?:string;refresh:()=>Promise<void>;refreshApprovals:(decidedID?:string)=>Promise<void>;onCreateWorkspaceShell:(workspaceID:string)=>Promise<void>;onOpenWorkspaceShell:(shell:SSHShell)=>void;onWorkspaceShellStarted:(shell:SSHShell)=>void;onSettingsChanged:(settings:SystemSettings)=>void;onError:(message:string)=>void;onStreamingChange:(streaming:boolean)=>void}) {
	const {t,i18n:instance}=useTranslation()
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
  const [historyOpen,setHistoryOpen]=useState(false)
  const [workspacePanelCollapsed,setWorkspacePanelCollapsed]=useState(()=>recalledChatPanelCollapsed('workspace'))
  const [conversationPanelCollapsed,setConversationPanelCollapsed]=useState(()=>recalledChatPanelCollapsed('conversations'))
  const [running, setRunning] = useState(false)
  const [detachedRunning,setDetachedRunning]=useState(false)
	const [stopping,setStopping]=useState(false)
	const [modelRetry,setModelRetry]=useState<ModelRetryState|null>(null)
	const [retryClock,setRetryClock]=useState(0)
  const [reasoningSeen, setReasoningSeen] = useState(false)
  const [plan,setPlan]=useState<AgentPlan|null>(null)
	const [approvalNotice,setApprovalNotice]=useState('')
	const [workspaceID,setWorkspaceID]=useState(recalledWorkspace)
	const [boundWorkspaceID,setBoundWorkspaceID]=useState('')
	const [workspaceSwitching,setWorkspaceSwitching]=useState(false)
  const messagesRef=useRef<HTMLDivElement>(null)
  const stickToLatest=useRef(true)
	  const activeStreamRef=useRef<ActiveChatStream|null>(null)
	  const imageURLsRef=useRef(new Set<string>())
  const sessionLoadRef=useRef('')
  const agentHosts = useMemo(() => hosts.filter(host => host.agent_enabled), [hosts])
  const hostNames = useMemo(() => agentHosts.map(host => host.name).join(', '), [agentHosts])
  const currentApprovals=useMemo(()=>sessionId?approvals.filter(item=>item.session_id===sessionId):[],[approvals,sessionId])
	const pendingExplanationID=currentApprovals.find(item=>item.ai_review?.status==='pending')?.id||''
	const sessionBusy=running||detachedRunning
	const selectedWorkspace=capabilities.workspaces.find(workspace=>workspace.id===workspaceID)||capabilities.workspaces[0]
	useEffect(()=>{if(!selectedWorkspace)return;if(workspaceID!==selectedWorkspace.id)setWorkspaceID(selectedWorkspace.id);rememberWorkspace(selectedWorkspace.id)},[selectedWorkspace,workspaceID])
	useEffect(()=>{
		if(!modelRetry)return
		setRetryClock(Date.now())
		const timer=window.setInterval(()=>setRetryClock(Date.now()),1000)
		return()=>window.clearInterval(timer)
	},[modelRetry])
	useEffect(()=>{onStreamingChange(running)},[running,onStreamingChange])
	useEffect(()=>()=>onStreamingChange(false),[onStreamingChange])
	useEffect(()=>()=>{sessionLoadRef.current='';const stream=activeStreamRef.current;activeStreamRef.current=null;stream?.controller.abort()},[])
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
  }, [])

  const loadSession = useCallback(async (id: string) => {
    const requestID=clientId()
    sessionLoadRef.current=requestID
    setLoadingSession(id)
    stickToLatest.current=true
    try {
      const state = await api.chatState(id)
      if(sessionLoadRef.current!==requestID)return
	      setEntries(historyEntries(state.messages||[],id));setDetachedRunning(!!state.active);setStopping(false);setModelRetry(null);setPlan(state.plan||null);setWorkspaceID(state.workspace_id||'');setBoundWorkspaceID(state.workspace_id||'')
      setSessionId(id); rememberSession(id); setHistoryError('')
      void refresh()
    } catch (err) { if(sessionLoadRef.current===requestID)setHistoryError(errorText(err)) }
    finally { if(sessionLoadRef.current===requestID)setLoadingSession('') }
  }, [refresh])

  useEffect(()=>{
    if(!sessionId||running||!detachedRunning)return
    let active=true
    const sync=async()=>{
	      try{const state=await api.chatState(sessionId);if(!active)return;setDetachedRunning(!!state.active);setPlan(state.plan||null);setEntries(old=>[...historyEntries(state.messages||[],sessionId),...old.filter(item=>item.kind==='error'&&!item.id.startsWith('history_'))]);if(!state.active){setStopping(false);void refreshSessions()}}
      catch(err){if(active)setHistoryError(errorText(err))}
    }
    void sync();const timer=window.setInterval(()=>void sync(),2500)
    return()=>{active=false;window.clearInterval(timer)}
  },[sessionId,running,detachedRunning,refreshSessions])

  const activeSessionCount=useMemo(()=>sessions.filter(item=>item.active).length,[sessions])
  useEffect(()=>{
    if(!activeSessionCount)return
    const timer=window.setInterval(()=>{if(document.visibilityState==='visible')void refreshSessions()},2500)
    return()=>window.clearInterval(timer)
  },[activeSessionCount,refreshSessions])

  useEffect(()=>{
    if(!stickToLatest.current)return
    const frame=window.requestAnimationFrame(()=>{const container=messagesRef.current;if(container)container.scrollTop=container.scrollHeight})
    return()=>window.cancelAnimationFrame(frame)
  },[entries,loadingSession])

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
    detachActiveStream()
    sessionLoadRef.current=''
    setLoadingSession('')
    setHistoryOpen(false)
	    stickToLatest.current=true;setSessionId('');setBoundWorkspaceID('');setEntries([]); setMessage('');clearPendingImages(); setHistoryError(''); setReasoningSeen(false);setDetachedRunning(false);setStopping(false);setModelRetry(null);setPlan(null); rememberSession(newSessionMarker)
    void refreshSessions()
  }

	const switchSession = (id:string) => {
		if(workspaceSwitching)return
    setHistoryOpen(false)
    if(id===sessionId){
      if(loadingSession){sessionLoadRef.current='';setLoadingSession('')}
      return
    }
    detachActiveStream()
		setStopping(false)
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
      if (session.id === sessionId) newChat()
      await refreshSessions()
    } catch (err) { setHistoryError(errorText(err)) }
    finally { setDeletingSession(false); setSessionDeleteCandidate(null) }
  }

	  const sendQuery = async (query:string,queryImages:PendingChatImage[]) => {
	    query=query.trim(); if((!query&&!queryImages.length)||sessionBusy||loadingSession||workspaceSwitching)return
    let querySessionID=sessionId
    const userEntryID=clientId()
    const streamID=clientId()
    const controller=new AbortController()
    activeStreamRef.current={id:streamID,sessionId:sessionId,controller}
    const isAttached=()=>activeStreamRef.current?.id===streamID
    stickToLatest.current=true
    setApprovalNotice('');setReasoningSeen(false);setStopping(false);setModelRetry(null);setRunning(true)
	    const entryImages=queryImages.map(image=>({id:image.id,name:image.file.name,mimeType:image.file.type,sizeBytes:image.file.size,url:image.url}))
	    setEntries((old) => [...old, { id: userEntryID, kind: 'user', content: query, images:entryImages, status:'pending' }, { id: 'streaming', kind: 'assistant', content: '', streaming:true }])
	    try {
      await streamChat(sessionId, selectedWorkspace?.id||'', query, queryImages.map(image=>image.file), (frame: AgentEvent) => {
        if(!isAttached())return
	        if (frame.session_id) { querySessionID=frame.session_id;activeStreamRef.current!.sessionId=frame.session_id;setSessionId(frame.session_id);setBoundWorkspaceID(selectedWorkspace?.id||'');rememberSession(frame.session_id) }
        if(frame.type==='retry'){const now=Date.now();setRetryClock(now);setModelRetry({attempt:frame.retry_attempt||1,max:frame.retry_max||1,readyAt:now+(frame.retry_delay_ms||0)})}
        else if(['approval','reasoning','tool','tool_output','message','done','interrupted','error'].includes(frame.type))setModelRetry(null)
        if (frame.type === 'approval') { setEntries(old=>updateActiveToolStatus(old.map(item=>item.id===userEntryID?{...item,status:'completed'}:item),'approval_required',frame.run_id));setApprovalNotice('');void refreshApprovals() }
        if(frame.type==='tool_output'){
          setEntries(old=>appendToolOutput(old,frame))
        }
        if (frame.type === 'reasoning' && frame.content) {
          setReasoningSeen(true)
          const reasoningID=`reasoning_${frame.segment_id||'current'}`
          setEntries((old) => {
            const existing=old.find((item)=>item.id===reasoningID)
            if(existing)return old.map((item)=>item.id===reasoningID?{...item,content:item.content+frame.content,active:true}:item)
            return [...old.filter((item)=>item.id!=='streaming').map(deactivateReasoning),{id:reasoningID,kind:'reasoning',content:frame.content!,active:true},{id:'streaming',kind:'assistant',content:'',streaming:true}]
          })
        }
        if (frame.type === 'tool' && frame.content) {
		  if(frame.tool_name==='workspace_shell'){
			  const shell=workspaceShellStartedByTool(frame.content)
			  if(shell)onWorkspaceShellStarted(shell)
		  }
          setEntries(old=>{
            const callID=frame.tool_call_id||''
            let index=callID?old.findIndex(item=>item.kind==='tool'&&item.toolCallId===callID):-1
            if(index<0&&!callID){
              for(let itemIndex=old.length-1;itemIndex>=0;itemIndex--){
                const item=old[itemIndex]
                if(item.kind==='tool'&&item.transient&&item.tool===frame.tool_name){index=itemIndex;break}
              }
            }
            const transient=frame.status==='in_progress'
            if(index>=0)return old.map((item,itemIndex)=>itemIndex===index?{...item,content:frame.content!,tool:frame.tool_name||item.tool,toolCallId:callID||item.toolCallId,runId:textValue(parseRecord(frame.content!).run_id)||item.runId,liveStdout:transient?item.liveStdout:undefined,liveStderr:transient?item.liveStderr:undefined,transient}:item)
            const entry:ChatEntry={id:callID?`tool_${callID}`:clientId(),kind:'tool',content:frame.content!,tool:frame.tool_name,toolCallId:callID||undefined,runId:textValue(parseRecord(frame.content!).run_id)||undefined,transient}
            return [...old.filter(item=>item.id!=='streaming').map(deactivateReasoning),entry,{id:'streaming',kind:'assistant',content:'',streaming:true}]
          })
          if(frame.tool_name?.startsWith('ops_plan_')){const nextPlan=planFromToolContent(frame.content);if(nextPlan)setPlan(nextPlan)}
          if(/approval_id|approval_required/.test(frame.content))void refresh()
        }
        if (frame.type === 'message' && frame.content) setEntries((old) => old.map((item) => item.id === 'streaming' ? {...item, content: item.content + frame.content} : deactivateReasoning(item)))
        if (frame.type === 'done') setEntries(old=>old.map(item=>item.id===userEntryID?{...item,status:'completed'}:item.id==='streaming'?{...item,streaming:false}:item))
			if (frame.type === 'interrupted') {setStopping(false);setDetachedRunning(false);setEntries(old=>[...updateActiveToolStatus(old.filter(item=>item.id!=='streaming').map(item=>item.id===userEntryID?{...item,status:'failed' as const}:deactivateReasoning(item)),'interrupted'),{id:clientId(),kind:'assistant',content:frame.content||t('chat.stopped')}])}
			if (frame.type === 'error') setEntries((old) => [...updateActiveToolStatus(old.map(item=>item.id===userEntryID?{...item,status:'failed' as const}:item.id==='streaming'?{...item,streaming:false}:item),'failed'), { id: clientId(), kind: 'error', content: frame.error || t('chat.agentError') }])
      },controller.signal)
    } catch (err) { if(isAttached()){setModelRetry(null);setEntries((old) => [...updateActiveToolStatus(old.map(item=>item.id===userEntryID?{...item,status:'failed' as const}:item),'failed'), { id: clientId(), kind: 'error', content: errorText(err) }])} }
    finally {
      if(!isAttached())return
      setModelRetry(null)
      setEntries((old) => old.filter((item) => item.id !== 'streaming' || item.content !== '').map((item)=>item.id==='streaming'?{...item,streaming:false}:deactivateReasoning(item)))
      setRunning(false)
		setStopping(false)
	      if(querySessionID){try{const state=await api.chatState(querySessionID);if(!isAttached())return;setDetachedRunning(!!state.active);setPlan(state.plan||null);setBoundWorkspaceID(state.workspace_id||'');setEntries(old=>[...historyEntries(state.messages||[],querySessionID),...old.filter(item=>(item.kind==='error'&&!item.id.startsWith('history_'))||(item.kind==='tool'&&item.transient))]);for(const image of queryImages){URL.revokeObjectURL(image.url);imageURLsRef.current.delete(image.url)}}catch{/* polling or the next reload will recover state */}}
      if(!isAttached())return
      activeStreamRef.current=null
      void refreshSessions();void refresh()
    }
  }

	  const submit = (event: FormEvent) => {event.preventDefault();const query=message.trim();if((!query&&!pendingImages.length)||sessionBusy||loadingSession||workspaceSwitching)return;const images=pendingImages;setMessage('');setPendingImages([]);setImageInputKey(value=>value+1);setImageNotice('');void sendQuery(query,images)}
	const stopAgent = async () => {
		const targetSessionID=activeStreamRef.current?.sessionId||sessionId
		if(!targetSessionID||!sessionBusy||stopping)return
		setStopping(true)
		let requested=false
		try{
			const result=await api.cancelChatSession(targetSessionID)
			requested=result.cancelled
			if(!result.cancelled){const state=await api.chatState(targetSessionID);setDetachedRunning(!!state.active);setPlan(state.plan||null);setEntries(historyEntries(state.messages||[],targetSessionID));void refreshSessions();void refresh()}
		}catch(err){setEntries(old=>[...old,{id:clientId(),kind:'error',content:t('chat.stopFailed',{message:errorText(err)})}])}
		finally{if(!requested)setStopping(false)}
  }
  const streamingResponseStarted=entries.some((item)=>item.id==='streaming'&&item.content!=='')
  const retryDelay=modelRetry?Math.max(0,Math.ceil((modelRetry.readyAt-retryClock)/1000)):0
  const modelRetryLabel=modelRetry?t(retryDelay>0?'chat.retryWaiting':'chat.retryingModel',{
	  attempt:modelRetry.attempt,
	  max:modelRetry.max,
	  delay:retryDelay,
  }):''
	const setChatPanelCollapsed=(panel:ChatPanel,collapsed:boolean)=>{
		rememberChatPanelCollapsed(panel,collapsed)
		if(panel==='workspace')setWorkspacePanelCollapsed(collapsed)
		else{setConversationPanelCollapsed(collapsed);if(collapsed)setHistoryOpen(false)}
	}

  return <div className={`chat-layout ${workspacePanelCollapsed?'workspace-panel-collapsed':''} ${conversationPanelCollapsed?'conversation-panel-collapsed':''}`}>
		<ChatWorkspacePanel key={selectedWorkspace?.id||''} workspaces={capabilities.workspaces} workspaceID={selectedWorkspace?.id||''} shells={workspaceShells} switching={workspaceSwitching} disabled={sessionBusy||!!loadingSession} bound={!!selectedWorkspace&&boundWorkspaceID===selectedWorkspace.id} onSelect={id=>void switchWorkspace(id)} onCreateShell={onCreateWorkspaceShell} onOpenShell={onOpenWorkspaceShell} onCollapse={()=>setChatPanelCollapsed('workspace',true)}/>
    <div className="chat-main panel">
	  <div className="panel-header"><div><Bot size={18}/><span>{t('chat.session')}</span>{workspacePanelCollapsed&&<button className="chat-panel-open-button" onClick={()=>setChatPanelCollapsed('workspace',false)} title={t('workspace.expandPanel')} aria-label={t('workspace.expandPanel')}><PanelLeftOpen size={15}/></button>}</div><div className="chat-header-actions">{conversationPanelCollapsed&&<button className="chat-panel-open-button conversation-panel-open-button" onClick={()=>setChatPanelCollapsed('conversations',false)} title={t('chat.expandConversations')} aria-label={t('chat.expandConversations')}><PanelRightOpen size={15}/></button>}<span className="session-id">{sessionId ? sessionId.slice(0, 20) : t('chat.newSession')}</span><button className="mobile-history-button" onClick={()=>setHistoryOpen(true)} title={t('chat.conversations')} aria-label={t('chat.openConversations')}><History size={15}/>{activeSessionCount>0&&<em>{activeSessionCount}</em>}</button></div></div>
      <div className="session-approval-slot">{currentApprovals.length>0&&<ApprovalDialog key={currentApprovals[0].id} approval={currentApprovals[0]} pendingCount={currentApprovals.length} hosts={hosts} running={sessionBusy} stopping={stopping} onStop={()=>void stopAgent()} refresh={refresh} refreshApprovals={refreshApprovals} onApproved={result=>{setEntries(old=>updateToolRunStatus(old,result.run_id,result.status==='running'?'in_progress':result.status));if(result.shell?.kind==='workspace')onWorkspaceShellStarted(result.shell)}} onNotice={setApprovalNotice}/>} {approvalNotice&&currentApprovals.length===0&&<div className="approval-toast"><ShieldCheck size={14}/><span>{approvalNotice}</span><button onClick={()=>setApprovalNotice('')}><X size={13}/></button></div>}</div>
      <div className="session-plan-slot">{plan&&<SessionPlan plan={plan}/>}</div>
      <div className="messages" ref={messagesRef} onScroll={event=>{const element=event.currentTarget;stickToLatest.current=element.scrollHeight-element.scrollTop-element.clientHeight<90}}>
		{entries.length === 0 && <div className="empty-chat"><div className="radar"><Activity size={35}/></div><h2>{t('chat.emptyTitle')}</h2></div>}
        {entries.map((entry) => <ChatBubble key={entry.id} entry={entry} runs={runs} hosts={hosts}/>) }
		{running&&modelRetry&&<div className="thinking"><span/><span/><span/> {modelRetryLabel}</div>}
		{running&&!modelRetry&&!reasoningSeen&&!streamingResponseStarted&&<div className="thinking"><span/><span/><span/> {t('chat.waitingModel')}</div>}
		{detachedRunning&&!running&&<div className="thinking background-agent"><span/><span/><span/> {t('chat.backgroundRunning')}</div>}
      </div>
		  <form className="composer" onSubmit={submit}>
			  {sessionBusy&&<div className="llm-work-status" role="status" aria-live="polite"><LoaderCircle className="spin" size={13}/><b>{stopping?t('chat.stopping'):modelRetryLabel||t('chat.running')}</b><button type="button" className="agent-stop-button" onClick={()=>void stopAgent()} disabled={stopping||!(activeStreamRef.current?.sessionId||sessionId)} title={t('chat.stopTitle')}><Square size={11} fill="currentColor"/>{t('chat.stop')}</button></div>}
			  <div className="context-line"><ApprovalModeStatus settings={settings} onChanged={onSettingsChanged} onError={onError}/><span className="composer-hosts" title={agentHosts.length?t('chat.hostsCount',{count:agentHosts.length,names:hostNames}):t('chat.noHosts')}><Server size={13}/>{agentHosts.length?t('chat.hostsCount',{count:agentHosts.length,names:hostNames}):t('chat.noHosts')}</span><span className="composer-model" title={modelName || t('chat.noModel')}><Cpu size={13}/>{modelName || t('chat.noModel')}</span></div>
			  {pendingImages.length>0&&<div className="composer-images">{pendingImages.map(image=><div key={image.id}><img src={image.url} alt={image.file.name}/><span title={image.file.name}>{image.file.name}</span><button type="button" onClick={()=>removePendingImage(image.id)} title={t('chat.removeImage')}><X size={11}/></button></div>)}</div>}
			  {imageNotice&&<div className="composer-image-notice">{imageNotice}<button type="button" onClick={()=>setImageNotice('')}><X size={11}/></button></div>}
			  <div className="input-row"><label className="image-attach-button" title={t('chat.addImages')}><ImagePlus size={18}/><input key={imageInputKey} type="file" accept={imageTypes.join(',')} multiple disabled={!agentAvailable||sessionBusy||workspaceSwitching||!!loadingSession} onChange={event=>addImages(Array.from(event.target.files||[]))}/></label><textarea value={message} onChange={(event) => setMessage(event.target.value)} onPaste={event=>{const files=Array.from(event.clipboardData.files).filter(file=>file.type.startsWith('image/'));if(files.length)addImages(files)}} placeholder={!agentAvailable?t('chat.configureModel'):loadingSession?t('chat.loadingConversation'):sessionBusy?t('chat.busyPlaceholder'):t('chat.prompt')} disabled={!agentAvailable||sessionBusy||workspaceSwitching||!!loadingSession} onKeyDown={(event) => { if (event.key === 'Enter' && !event.shiftKey) { event.preventDefault(); event.currentTarget.form?.requestSubmit() } }}/><button aria-label={t('common.next')} disabled={!agentAvailable || sessionBusy || workspaceSwitching || !!loadingSession || (!message.trim()&&!pendingImages.length)}><Send size={18}/></button></div>
		  </form>
    </div>
	{historyOpen&&<button className="conversation-backdrop" onClick={()=>setHistoryOpen(false)} aria-label={t('chat.closeConversations')}/>}
	<aside className={`context-panel conversation-panel panel ${historyOpen?'mobile-open':''}`}><div className="panel-header"><div><History size={17}/><span>{t('chat.conversations')}</span></div><section className="conversation-header-actions"><button className="new-chat-button" onClick={newChat} disabled={workspaceSwitching} title={t('chat.newConversation')}><Plus size={14}/>{t('common.new')}</button><button className="conversation-collapse-button" onClick={()=>setChatPanelCollapsed('conversations',true)} title={t('chat.collapseConversations')} aria-label={t('chat.collapseConversations')}><PanelRightClose size={14}/></button><button className="conversation-close-button" onClick={()=>setHistoryOpen(false)} title={t('chat.closeConversations')} aria-label={t('chat.closeConversations')}><X size={14}/></button></section></div><div className="session-list">
      {historyError&&<div className="history-error">{historyError}</div>}
	  {!sessions.length&&!historyError&&<div className="history-empty">{t('chat.noSaved')}</div>}
	  {sessions.map(session=>{const pending=approvals.filter(item=>item.session_id===session.id).length;const active=session.active||(session.id===sessionId&&sessionBusy);return <div className={`session-item ${session.id===sessionId?'active':''}`} key={session.id}><button className="session-open" onClick={()=>switchSession(session.id)} disabled={workspaceSwitching||loadingSession===session.id}><b>{session.title}{pending>0&&<em className="session-approval-count">{t('chat.approvalCount',{count:pending})}</em>}{active&&<em className="session-running-count">{t('chat.runningBadge')}</em>}</b><span>{new Date(session.updated_at).toLocaleString(localeFor(instance.language))} · {t('chat.messageCount',{count:session.message_count})} · {session.workspace_id||t('chat.noWorkspace')}</span></button><button className="session-delete" onClick={()=>{if(!active)setSessionDeleteCandidate(session)}} disabled={active||workspaceSwitching} title={active?t('chat.cannotDelete'):t('chat.deleteConversation')}><Trash2 size={13}/></button></div>})}
	</div><div className="session-summary"><Metric label={t('chat.saved')} value={sessions.length.toString()} tone="green"/><Metric label={t('chat.hosts')} value={agentHosts.length.toString()}/></div></aside>
	{sessionDeleteCandidate&&<DestructiveConfirmDialog label={t('chat.deleteDialogLabel')} title={t('chat.deleteTitle',{title:sessionDeleteCandidate.title})} description={t('chat.deleteText')} busy={deletingSession} onCancel={()=>setSessionDeleteCandidate(null)} onConfirm={()=>void removeSession()}/>}
  </div>
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
			<div className="panel-header"><div><FolderOpen size={17}/><span>{t('common.workspace')}</span></div><div className="workspace-panel-actions"><button type="button" disabled={!workspace.shell||startingShell} onClick={()=>void createShell()} title={t('workspace.newTerminal')}>{startingShell?<LoaderCircle className="spin" size={14}/>:<TerminalSquare size={14}/>}</button><select value={workspace.id} disabled={workspaces.length<2||disabled||switching} onChange={event=>onSelect(event.target.value)} aria-label={t('workspace.switchWorkspace')}>{workspaces.map(item=><option value={item.id} key={item.id}>{item.id}</option>)}</select><button type="button" onClick={onCollapse} title={t('workspace.collapsePanel')} aria-label={t('workspace.collapsePanel')}><PanelLeftClose size={14}/></button></div></div>
			<div className="workspace-summary"><div className="chat-workspace-head"><span><b>{workspace.id}</b>{(switching||bound)&&<small>{switching?t('workspace.switching'):t('workspace.boundToConversation')}</small>}</span><em className={workspace.access}>{workspace.access==='read_write'?t('workspace.readWrite'):t('workspace.readOnly')}</em></div>{activeShells.length>0&&<div className="workspace-shell-sessions">{activeShells.map(shell=><button type="button" onClick={()=>onOpenShell(shell)} title={shell.id} key={shell.id}><i className={shell.status}/><b>{t(shell.surface==='workspace_agent'?'workspace.agent':'workspace.operator')}</b><code>{shell.cwd||'.'}</code></button>)}</div>}</div>
			<div className="workspace-path-row"><button onClick={up} disabled={path==='.'} title={t('workspace.parent')}>‹</button><code title={path}>{path}</code>{workspace.access==='read_write'&&<label title={t('workspace.uploadFile')}><UploadCloud size={14}/><input key={inputKey} type="file" onChange={choose}/></label>}<button onClick={()=>synchronize(true)} title={t('workspace.refreshFiles')}><RefreshCw size={12}/></button></div>
			{file&&<div className="chat-upload-row"><input value={target} onChange={event=>setTarget(event.target.value)} aria-label={t('workspace.relativePath')}/><button onClick={()=>void upload()} disabled={uploading||!target.trim()}>{uploading?'...':t('common.upload')}</button><button onClick={()=>{setFile(null);setTarget('');setInputKey(value=>value+1)}} title={t('workspace.cancelUpload')}><X size={11}/></button></div>}
			<div className="workspace-file-list">{loading?<span className="workspace-files-state"><LoaderCircle className="spin" size={13}/>{t('common.loading')}</span>:error?<span className="workspace-files-state error">{error}</span>:entries.length?entries.map(entry=>{const fullPath=workspaceChildPath(path,entry.name);return <div className="workspace-file-row" key={`${entry.type}:${entry.name}`}><button className="workspace-file-open" onClick={()=>void openEntry(entry.name,entry.type)} title={entry.type==='file'?t('workspace.previewFile'):t('workspace.openDirectory')}>{previewLoading===fullPath?<LoaderCircle className="spin" size={13}/>:entry.type==='directory'?<FolderOpen size={13}/>:<FileText size={13}/>}<span>{entry.name}</span>{entry.type==='file'&&<small>{formatFileSize(entry.size??0)}</small>}</button>{(entry.type==='file'||desktopRuntime&&entry.type==='directory'||workspace.access==='read_write')&&<div className="workspace-file-actions">{entry.type==='file'&&<button className="workspace-file-download" onClick={()=>download(fullPath,entry.name)} title={t('common.download')}><Download size={12}/></button>}{desktopRuntime&&entry.type==='directory'&&<button className="workspace-file-reveal" onClick={()=>void revealDirectory(fullPath)} title={t('workspace.revealDirectory')}><FolderOutput size={12}/></button>}{workspace.access==='read_write'&&<button className="workspace-file-delete" onClick={()=>requestEntryRemoval(entry.name,entry.type)} disabled={deleting===fullPath} title={t('workspace.deleteEntry',{type:t(`workspace.${entry.type}`)})}><Trash2 size={12}/></button>}</div>}</div>}):<span className="workspace-files-state">{t('workspace.emptyDirectory')}</span>}</div>
			{notice&&<div className={`chat-workspace-notice ${notice.kind}`}>{notice.text}</div>}
			{dragging&&<div className="workspace-drop-overlay"><UploadCloud size={27}/><b>{t('workspace.dropFilesHere')}</b><span>{path}</span></div>}
		</aside>
		{preview&&<TextFileEditor path={preview.path} meta={`${formatFileSize(preview.size)} · SHA-256 ${preview.sha256}`} content={preview.content||''} binary={preview.binary} editable={workspace.access==='read_write'} onClose={()=>setPreview(null)} onSave={savePreview} onDownload={()=>download(preview.path,preview.path.split('/').at(-1)||'download')}/>}
		{deleteCandidate&&<DestructiveConfirmDialog label={t('workspace.permanentDelete')} title={t('workspace.deleteTitle',{path:`${deleteCandidate.workspaceID}:${deleteCandidate.path}`})} description={t('workspace.deleteDescription',{target:deleteCandidate.type==='directory'?t('workspace.deleteFolderTarget'):t('workspace.deleteFileTarget')})} busy={deleting===deleteCandidate.path} onCancel={()=>setDeleteCandidate(null)} onConfirm={()=>void removeEntry()}/>}
	</>
}

function SessionPlan({plan}:{plan:AgentPlan}){
	const {t}=useTranslation()
  const [expanded,setExpanded]=useState(plan.status==='active')
  useEffect(()=>{if(plan.status==='active')setExpanded(true)},[plan.session_id,plan.status])
	const completed=plan.steps.filter(step=>step.status==='completed'||step.status==='skipped').length
  const current=plan.steps.find(step=>step.status==='in_progress'||step.status==='blocked')
  const progress=plan.steps.length?Math.round(completed/plan.steps.length*100):0
	return <details className={`session-plan ${plan.status}`} open={expanded} onToggle={event=>setExpanded(event.currentTarget.open)}><summary><span className="plan-icon"><ListChecks size={16}/></span><span className="plan-summary-copy"><b>{plan.goal}</b><small>{current?t(current.status==='blocked'?'plan.blockedAt':'plan.current',{current:current.number,total:plan.steps.length,title:current.title}):t('plan.completed',{completed,total:plan.steps.length})}</small></span><span className="plan-progress"><i><em style={{width:`${progress}%`}}/></i><b>{progress}%</b></span><span className={`plan-state ${plan.status}`}>{t(`statusLabels.${plan.status}`,{defaultValue:plan.status})}</span><ChevronRight size={14}/></summary><ol>{plan.steps.map(step=><li className={step.status} key={step.number}><span className="plan-step-marker">{step.status==='completed'?<Check size={12}/>:step.status==='skipped'?<ChevronRight size={12}/>:step.status==='in_progress'?<LoaderCircle size={12}/>:step.status==='blocked'?<ShieldAlert size={12}/>:step.number}</span><div><b>{step.title}</b>{step.evidence&&<p>{step.evidence}</p>}</div><em>{t(`statusLabels.${step.status}`,{defaultValue:step.status.replace('_',' ')})}</em></li>)}</ol></details>
}

const ChatBubble=memo(function ChatBubble({ entry, runs, hosts }: {entry: ChatEntry;runs:Run[];hosts:Host[]}) {
	const {t}=useTranslation()
  if (entry.kind === 'tool') return <ToolEventCard entry={entry} runs={runs} hosts={hosts}/>
  if (entry.kind === 'reasoning') return <ReasoningCard content={entry.content} active={!!entry.active}/>
  if (entry.kind === 'assistant' && !entry.content) return null
	return <div className={`bubble ${entry.kind} ${entry.status||''}`}><div className="avatar">{entry.kind === 'user' ? <UserRound size={17}/> : entry.kind === 'error' ? '!' : <Bot size={17}/>}</div><div><span className="bubble-label">{entry.kind === 'user' ? <>{t('chat.operator')}{entry.status==='failed'&&<em>{t('chat.responseFailed')}</em>}{entry.status==='pending'&&<em>{t('chat.processing')}</em>}</> : entry.kind === 'error' ? t('common.error') : 'OpsNerva'}</span>{entry.images&&entry.images.length>0&&<div className="message-images">{entry.images.map(image=><a href={image.url} target="_blank" rel="noopener noreferrer" title={`${image.name} · ${formatFileSize(image.sizeBytes)}`} key={image.id}><img src={image.url} alt={image.name}/><span>{image.name}</span></a>)}</div>}{entry.content&&<div className={`bubble-copy ${entry.kind==='assistant'&&!entry.streaming?'markdown-body':''}`}>{entry.kind==='assistant'&&!entry.streaming?<Markdown skipHtml remarkPlugins={[remarkGfm]} components={{a:({href,children})=><a href={href} target="_blank" rel="noopener noreferrer">{children}</a>,img:({alt})=><span className="markdown-image-blocked">{t('chat.blockedImage',{alt:alt||t('common.image')})}</span>,pre:({children})=><CopyablePre>{children}</CopyablePre>}}>{entry.content}</Markdown>:entry.content}</div>}</div></div>
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
function jsonRecord(value:unknown):JsonRecord|undefined{return value!==null&&typeof value==='object'&&!Array.isArray(value)?value as JsonRecord:undefined}
function parseRecord(value:string):JsonRecord{try{return jsonRecord(JSON.parse(value))||{value:JSON.parse(value)}}catch{return{value}}}
function requestFromRun(run?:Run):JsonRecord|undefined{if(!run)return;try{return jsonRecord(JSON.parse(run.request_json))}catch{return}}
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

function ToolEventCard({entry,runs,hosts}:{entry:ChatEntry;runs:Run[];hosts:Host[]}){
	const {t}=useTranslation()
  const payload=parseRecord(entry.content)
	const taskPayload=jsonRecord(payload.task)
	const resultPayload=jsonRecord(payload.result)
  const runID=entry.runId||textValue(payload.run_id)||textValue(taskPayload?.run_id)||textValue(resultPayload?.run_id)
	const run=runs.find(item=>item.id===runID)
	const display=jsonRecord(payload._display)
	const toolArguments=jsonRecord(display?.arguments)
	const request=jsonRecord(display?.request)||requestFromRun(run)
	const shellPayload=jsonRecord(payload.shell)||jsonRecord(resultPayload?.shell)
	const destinationHostID=textValue(display?.host_id)||run?.host_id||textValue(request?.host_id)||textValue(toolArguments?.host_id)||textValue(toolArguments?.destination_host_id)||textValue(shellPayload?.host_id)
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
	const shellOutput=textValue(payload.recent_output)||textValue(resultPayload?.recent_output)||shellEvents
		.filter(event=>['stdout','stderr'].includes(textValue(event.stream)))
		.map(event=>textValue(event.content))
		.join('')
	const shellInput=textValue(toolArguments?.input)
	const shellInputDisplay=`${shellInput}${toolArguments?.submit===true&&!/[\r\n]$/.test(shellInput)?' ↵':''}`
	const shellActionLabel=shellAction?t(`sshShell.toolActions.${shellAction}`,{defaultValue:t('sshShell.short')}):t('sshShell.short')
	const shellSummary=shellOperation?(shellAction==='start'
		?`${workspaceID?`${workspaceID}:${request?textValue(request.cwd)||'.':textValue(toolArguments?.cwd)||'.'}`:`${hostName}:${request?textValue(request.cwd)||'~':textValue(toolArguments?.cwd)||'~'}`} · PTY`
		:shellAction==='input'
			?shellInputDisplay
			:shellAction==='status'
				?latestOutput(shellOutput,1)||shellID
				:shellAction==='list'
					?String(numberValue(payload.count)||numberValue(resultPayload?.count))
					:shellID):''
	const shellPrimaryContent=shellAction==='input'?shellInputDisplay:shellAction==='status'?shellOutput:shellSummary
	const shellPrimaryAction=shellOperation&&(shellAction==='input'||shellAction==='status')
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
	const workspaceTransfer=workspaceUpload||workspaceDownload
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
	const fileOperationParameters:Array<Array<unknown>>=structuredFileOperation&&request?[
		...(workspaceID?[["workspace_id",workspaceID]]:[["host_id",hostID]]),
		["path",filePath],
		...(fileSearchMode?[["match_mode",searchMatchModeLabel],["pattern",searchPattern],["context_lines",numberValue(request.context_lines)]]:[
			...(workspaceID?[]:[["metadata_only",request.metadata_only===true]]),
			["full_content",request.full_content===true],
			["max_bytes",numberValue(request.max_bytes)],
			["offset_bytes",numberValue(request.offset_bytes)],
			["tail_lines",numberValue(request.tail_lines)]
		]),
		...(workspaceID?[]:[["elevated",request.elevated===true]])
	]:[]
	const transferSummary=tunnelRoute||shellSummary||(workspaceUpload?`${workspaceID}:${relativePath} → ${hostName}:${remotePath}`:workspaceDownload?`${hostName}:${remotePath} → ${workspaceID}:${relativePath}`:sshTransfer?`${sourceHostName}:${sourcePath} → ${hostName}:${remotePath}`:'')
  const planSteps=Array.isArray(payload.steps)?payload.steps.map(jsonRecord).filter((step):step is JsonRecord=>!!step):[]
  const planSummary=textValue(payload.goal)||textValue(planSteps.find(step=>textValue(step.status)==='in_progress'||textValue(step.status)==='blocked')?.title)
	const operation=filePath||(script?t('tool.bashScript'):program||eventToolLabel||t('tool.result'))
  const args=request&&Array.isArray(request.args)?request.args.map(value=>String(value)):[]
  const env=request?jsonRecord(request.env):undefined
	const rawStdout=shellAction==='status'?shellOutput:textValue(payload.stdout)||textValue(resultPayload?.stdout)||entry.liveStdout||run?.stdout_redacted||''
	const stdout=change?cleanFileChangeOutput(rawStdout):rawStdout
	  const stderr=textValue(payload.stderr)||textValue(resultPayload?.stderr)||entry.liveStderr||run?.stderr_redacted||run?.error||''
	const outputView=textValue(payload.output_view)||textValue(resultPayload?.output_view)
	const stdoutOmitted=numberValue(payload.stdout_omitted_bytes)||numberValue(resultPayload?.stdout_omitted_bytes)
	const stderrOmitted=numberValue(payload.stderr_omitted_bytes)||numberValue(resultPayload?.stderr_omitted_bytes)
	const waitDeadlineReached=payload.wait_deadline_reached===true||resultPayload?.wait_deadline_reached===true
	const transferTotal=entry.transferTotalBytes||0
	const transferred=Math.min(entry.transferredBytes||0,transferTotal)
	const transferPercent=transferTotal>0?Math.min(100,Math.round(transferred/transferTotal*100)):0
	const outputLabel=(label:string,omitted:number)=>omitted>0?`${label} · ${outputView.toUpperCase()} · ${t('tool.outputOmitted',{count:omitted})}`:label
  const stdoutPreview=latestOutput(stdout)
	const commandSummary=transferSummary||(fileSearchMode?`${fileTarget} · ${searchMatchModeLabel} pattern=${JSON.stringify(searchPattern)}`:filePath)||program||(script?compactScript(script):'')||planSummary||operation
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
  const resultExitCode=resultPayload?.exit_code
  const exitCode=typeof payload.exit_code==='number'?payload.exit_code:typeof resultExitCode==='number'?resultExitCode:run?.exit_code??'—'
  return <details className={`tool-event tool-event-rich ${status}`} open={expanded} onToggle={event=>setExpanded(event.currentTarget.open)}>
	<summary><div className="tool-summary-icon"><TerminalSquare size={15}/></div><div className="tool-summary-copy"><div className="tool-summary-operation"><b>{eventToolLabel||entry.tool||t('common.functions')}:</b><code title={commandSummary}>{commandSummary}</code></div>{targets.length>0&&<div className="tool-summary-targets">{targets.map((target,index)=><span className={`tool-target-chip ${target.kind}`} title={`${target.label}: ${[target.name,target.id].filter(Boolean).join(' · ')}`} key={`${target.kind}_${target.id||target.name}_${index}`}>{target.kind==='host'?<Server size={11}/>:target.kind==='workspace'?<FolderOpen size={11}/>:<ListChecks size={11}/>}<em>{target.label}</em>{target.name&&<b>{target.name}</b>}{target.id&&<code>{target.id}</code>}</span>)}</div>}</div><span className={`tool-status ${status}`}>{t(`statusLabels.${status}`,{defaultValue:status.replaceAll('_',' ')})}</span><ChevronRight size={14}/>{stdoutPreview&&<div className="tool-summary-preview"><span>{shellAction==='status'?shellActionLabel:t('tool.latestStdout',{count:Math.min(3,stdoutPreview.split('\n').length)})}</span><pre>{stdoutPreview}</pre></div>}</summary>
    <div className="tool-event-body">
	  {shellPrimaryAction&&<section className="tool-command-pane"><div className="tool-command-head"><span>{shellActionLabel}</span></div><div className="tool-command-block"><CopyButton value={shellPrimaryContent||'—'}/><pre>{shellPrimaryContent||'—'}</pre></div></section>}
      {(shellOperation||entry.tool==='ssh_exec'||entry.tool==='ssh_run_script')&&toolArguments&&<CompactTable title={t('tool.actualParameters')} columns={[t('tool.parameter'),t('tool.value')]} rows={Object.entries(toolArguments).map(([key,value])=>[key,value])}/>}
      {request?<div className="tool-execution-layout">
        <section className="tool-command-pane">
		  <div className="tool-command-head"><span>{shellOperation?t('sshShell.interactive'):tunnelOperation?t('tunnels.forwarding'):structuredFileOperation?t(fileSearchMode?'tool.searchOperation':'tool.readOperation'):filePath?t('tool.fileOperation'):script?t('tool.fullScript'):t('tool.fullCommand')}</span>{workspaceShellBackend&&<em><TerminalSquare size={12}/>{workspaceShellBackend==='host'?t('approval.hostShell'):'Bubblewrap'}</em>}{request.elevated===true&&<em><ShieldAlert size={12}/>sudo / root</em>}</div>
			  <div className="tool-command-block"><CopyButton value={script||program||commandSummary}/>{shellOperation?<pre>{shellSummary}</pre>:tunnelOperation?<pre>{tunnelRoute||requestMode}</pre>:workspaceUpload?<pre>workspace_upload {workspaceID}:{relativePath} → {hostName}:{remotePath}</pre>:workspaceDownload?<pre>workspace_download {hostName}:{remotePath} → {workspaceID}:{relativePath}</pre>:sshTransfer?<pre>{sourceHostName}:{sourcePath} → {hostName}:{remotePath}</pre>:structuredFileOperation?<pre>{fileSearchMode?'search':'read'} {fileTarget}</pre>:filePath?<pre>{requestMode} {workspaceID?`${workspaceID}:`:''}{filePath}</pre>:script?<pre>{script}</pre>:program?<pre><span className="prompt-sign">$</span> {program}</pre>:<pre>{requestMode} {remotePath}</pre>}</div>
		  {fileOperationParameters.length>0&&<CompactTable title={t('tool.actualParameters')} columns={[t('tool.parameter'),t('tool.value')]} rows={fileOperationParameters}/>}
		  {change&&textValue(change.diff)&&<DiffViewer change={change}/>}
		  {program&&<CompactTable title={t('tool.originalArgs')} columns={[t('tool.index'),t('tool.value')]} rows={[[0,textValue(request.program)],...args.map((arg,index)=>[index+1,JSON.stringify(arg)])]}/>}
		  {env&&Object.keys(env).length>0&&<CompactTable title={t('tool.environment')} columns={[t('tool.key'),t('tool.value')]} rows={Object.entries(env).map(([key,value])=>[key,String(value)])}/>}
        </section>
        <aside className="tool-context-pane">
			  <dl className="tool-context-grid"><div><dt>{workspaceUpload||sshTransfer?t('tool.targetHost'):workspaceID?t('common.workspace'):t('tool.targetHost')}</dt><dd>{workspaceUpload||sshTransfer?[destinationHost.name,hostID].filter(Boolean).join(' · '):workspaceID||[destinationHost.name,hostID].filter(Boolean).join(' · ')||'—'}</dd></div><div><dt>{tunnelOperation?t('tunnels.remoteEndpoint'):workspaceTransfer||sshTransfer?t('tool.sourceFile'):filePath?t('tool.filePath'):t('tool.workingDirectory')}</dt><dd>{tunnelOperation?`${tunnelRemoteHost}:${tunnelRemotePort}`:workspaceUpload?`${workspaceID}:${relativePath}`:workspaceDownload?`${hostName}:${remotePath}`:sshTransfer?`${[sourceHost.name,sourceHost.id].filter(Boolean).join(' · ')}:${sourcePath}`:filePath||textValue(request.cwd)||t('tool.defaultDirectory')}</dd></div><div><dt>{t('tool.permission')}</dt><dd>{workspaceShellBackend==='host'?t('tool.hostAuthority'):workspaceShellBackend==='sandbox'?t('tool.sandbox'):request.elevated===true?t('tool.managedSudo'):t('tool.normalUser')}</dd></div><div><dt>{t('common.status')}</dt><dd>{t(`statusLabels.${status}`,{defaultValue:status})}{waitDeadlineReached?` · ${t('tool.waitDeadline')}`:''}</dd></div><div><dt>{t('tool.exitCode')}</dt><dd>{exitCode}</dd></div><div><dt>{t('tool.duration')}</dt><dd>{formatDuration(payload.duration??resultPayload?.duration,run)}</dd></div><div><dt>{t('tool.runId')}</dt><dd>{runID||'—'}</dd></div></dl>
		  {textValue(request.reason)&&<div className="tool-reason"><span>{t('tool.reason')}</span><p>{textValue(request.reason)}</p></div>}
        </aside>
      </div>:!shellPrimaryAction&&<GenericToolResult payload={payload}/>}
	  {file&&<FileMetadataPanel file={file}/>}
	  {fileSearchMode&&searchResult&&<div className={`file-search-result ${searchFound?'found':'empty'}`}><Search size={15}/><div><b>{t(searchFound?'tool.searchMatched':'tool.searchNoMatches')}</b><span>{searchMatchModeLabel} · {searchPattern}</span></div></div>}
	  {(textValue(payload.message)||textValue(payload.next_action))&&<div className={`tool-guidance ${payload.ok===false||['failed','denied','interrupted'].includes(status)?'error':''}`}><ShieldAlert size={15}/><div><b>{textValue(payload.code)||t('tool.result')}</b>{textValue(payload.message)&&<p>{textValue(payload.message)}</p>}{textValue(payload.next_action)&&<small>{t('common.next')} · {textValue(payload.next_action)}</small>}</div></div>}
	  {instruction&&<div className="tool-instruction"><ShieldAlert size={15}/><div><b>{t('tool.operatorInstruction')}</b><p>{instruction}</p></div></div>}
	  {sshTransfer&&transferTotal>0&&<div className="file-transfer-progress" role="progressbar" aria-valuemin={0} aria-valuemax={transferTotal} aria-valuenow={transferred}><div><span>{t('tool.transferProgress')}</span><b>{formatFileSize(transferred)} / {formatFileSize(transferTotal)}</b></div><i><em style={{width:`${transferPercent}%`}}/></i></div>}
	      {((stdout&&shellAction!=='status')||stderr)&&<div className="tool-output-grid">{stdout&&shellAction!=='status'&&<ToolOutputPanel kind="stdout" label={outputLabel('STDOUT',stdoutOmitted)} content={stdout} live={status==='in_progress'}/>} {stderr&&<ToolOutputPanel kind="stderr" label={outputLabel(t('tool.stderrResult'),stderrOmitted)} content={stderr} live={status==='in_progress'}/>}</div>}
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

function FileMetadataPanel({file}:{file:JsonRecord}){
	const {t}=useTranslation()
	const after=textValue(file.sha256),validator=textValue(file.validator)
	return <section className="file-metadata-panel"><div className="file-metadata-head"><FileText size={16}/><div><b>{t('tool.fileEvidence')}</b><span>{textValue(file.path)}</span></div>{file.validation_ok===true&&<em><Check size={12}/>{t('tool.validated')}</em>}</div><dl><div><dt>{t('tool.bytesRead')}</dt><dd>{typeof file.returned_bytes==='number'?`${file.returned_bytes} B`:'—'}</dd></div>{file.has_more===true&&<div><dt>{t('tool.nextOffset')}</dt><dd>{numberValue(file.next_offset)}</dd></div>}<div><dt>{t('tool.mode')}</dt><dd>{textValue(file.mode)||'—'}</dd></div><div><dt>{t('tool.owner')}</dt><dd>{[textValue(file.owner),textValue(file.group)].filter(Boolean).join(':')||'—'}</dd></div><div><dt>{t('tool.validator')}</dt><dd>{validator||'—'}</dd></div></dl>{after&&<div className="hash-row"><span>{t('tool.after')}</span><code>{after}</code></div>}{file.sensitive===true&&<div className="file-sensitive"><ShieldAlert size={13}/>{t('tool.sensitive')}</div>}</section>
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

function GenericToolResult({payload}:{payload:JsonRecord}){
	const {t}=useTranslation()
  const hidden=new Set(['_display','stdout','stderr','operator_instruction'])
  const entries=Object.entries(payload).filter(([key])=>!hidden.has(key))
  const scalars=entries.filter(([,value])=>value===null||typeof value==='string'||typeof value==='number'||typeof value==='boolean')
  const arrays=entries.filter(([,value])=>Array.isArray(value))
  const objects=entries.filter(([,value])=>!!jsonRecord(value))
  return <div className="tool-structured-result">
    {scalars.length>0&&<dl className="tool-generic-grid">{scalars.map(([key,value])=><div key={key}><dt>{key.replaceAll('_',' ')}</dt><dd>{displayValue(value)}</dd></div>)}</dl>}
    {arrays.map(([key,value])=><StructuredArray key={key} label={key} values={value as unknown[]}/>)}
    {objects.map(([key,value])=><StructuredObject key={key} label={key} value={value as JsonRecord}/>)}
	{!entries.length&&<div className="tool-generic-note">{t('tool.emptyResult')}</div>}
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
              <dt>{t("approval.deadline")}</dt>
              <dd>
                <Clock3 size={12} />
                {new Date(approval.expires_at).toLocaleTimeString(
                  localeFor(instance.language),
                )}
              </dd>
            </div>
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

function HostsPage({ hosts, proxies, showAddresses, refresh }: {hosts:Host[];proxies:Proxy[];showAddresses:boolean;refresh:()=>Promise<void>}) {
	const {t}=useTranslation()
  const [showForm, setShowForm] = useState(false); const [notice, setNotice] = useState(''); const [saving,setSaving]=useState(false);const [deletingHost,setDeletingHost]=useState('')
	const [deleteCandidate,setDeleteCandidate]=useState<Host|null>(null)
  const [form, setForm] = useState<HostInput>(emptyHostForm)
	const [privateKeyName,setPrivateKeyName]=useState(''),[privateKeyError,setPrivateKeyError]=useState(''),[existingPrivateKey,setExistingPrivateKey]=useState(false),[privateKeyInputKey,setPrivateKeyInputKey]=useState(0)
	const [hostKeys,setHostKeys]=useState<Record<string,{fingerprint:string;algorithm?:string;trusted:boolean}>>({}),[hostKeyErrors,setHostKeyErrors]=useState<Record<string,string>>({}),[hostKeyBusy,setHostKeyBusy]=useState('')
  const editing=!!form.id
	const resetPrivateKey=()=>{setPrivateKeyName('');setPrivateKeyError('');setExistingPrivateKey(false);setPrivateKeyInputKey(value=>value+1)}
	const openCreate=()=>{setForm(emptyHostForm);resetPrivateKey();setShowForm(true);setNotice('')}
	const openEdit=(host:Host)=>{setForm({id:host.id,name:host.name,address:host.address,port:host.port,user:host.user,agent_enabled:host.agent_enabled,auth_type:host.auth_type||'agent',private_key:'',known_hosts_file:host.known_hosts_file||'',proxy_jump_host_id:host.proxy_jump_host_id||'',proxy_id:host.proxy_id||'',password:'',sudo_mode:host.sudo_mode||'none',sudo_password:''});setPrivateKeyName('');setPrivateKeyError('');setExistingPrivateKey(host.auth_type==='key'&&host.has_private_key);setPrivateKeyInputKey(value=>value+1);setShowForm(true);setNotice('')}
	const setAuthType=(auth_type:HostAuthType)=>{setForm(current=>({...current,auth_type,password:'',private_key:auth_type==='key'?current.private_key:''}));if(auth_type!=='key'){setPrivateKeyName('');setPrivateKeyError('');setPrivateKeyInputKey(value=>value+1)}}
	const choosePrivateKey=async(event:React.ChangeEvent<HTMLInputElement>)=>{const selected=event.target.files?.[0];setPrivateKeyError('');if(!selected){setPrivateKeyName('');setForm(current=>({...current,private_key:''}));return}if(selected.size<=0||selected.size>maxPrivateKeyBytes){setPrivateKeyName('');setForm(current=>({...current,private_key:''}));setPrivateKeyError(t('hosts.keySizeError'));return}try{const content=await selected.text();setPrivateKeyName(selected.name);setForm(current=>({...current,private_key:content}))}catch(err){setPrivateKeyName('');setForm(current=>({...current,private_key:''}));setPrivateKeyError(errorText(err))}}
	const missingPrivateKey=form.auth_type==='key'&&!form.private_key&&!existingPrivateKey
	const scan = async (host:Host) => {setHostKeyBusy(`scan-${host.id}`);setHostKeyErrors(current=>({...current,[host.id]:''}));try{const key=await api.scanKey(host.id);setHostKeys(current=>({...current,[host.id]:key}))}catch(err){setHostKeyErrors(current=>({...current,[host.id]:errorText(err)}))}finally{setHostKeyBusy('')}}
	const trust = async (host:Host) => {const key=hostKeys[host.id];if(!key||key.trusted)return;setHostKeyBusy(`trust-${host.id}`);setHostKeyErrors(current=>({...current,[host.id]:''}));try{const trusted=await api.trustKey(host.id,key.fingerprint);setHostKeys(current=>({...current,[host.id]:{...trusted,trusted:true}}));setNotice(t('hosts.trusted',{fingerprint:trusted.fingerprint}))}catch(err){setHostKeyErrors(current=>({...current,[host.id]:errorText(err)}))}finally{setHostKeyBusy('')}}
	const save = async (event:FormEvent) => { event.preventDefault(); if(missingPrivateKey)return;setSaving(true); try { const saved=await api.saveHost(form); setShowForm(false); setForm(emptyHostForm);resetPrivateKey();setHostKeys(current=>{const next={...current};delete next[saved.id];return next});setHostKeyErrors(current=>{const next={...current};delete next[saved.id];return next}); setNotice(t('hosts.saved',{name:saved.name,action:editing?t('hosts.updated'):t('hosts.registered')})); await refresh();void scan(saved) } catch(err){setNotice(errorText(err))} finally{setSaving(false)} }
  const probe = async (host:Host) => { try { const info = await api.probe(host.id); setNotice(`${host.name}: ${Object.values(info).join(' · ')}`) } catch(err){setNotice(errorText(err))} }
	const remove=async()=>{const host=deleteCandidate;if(!host)return;setDeletingHost(host.id);setNotice('');try{await api.deleteHost(host.id);setNotice(t('hosts.deleted',{name:host.name}));await refresh()}catch(err){setNotice(errorText(err))}finally{setDeletingHost('');setDeleteCandidate(null)}}
		return <div className="page-stack">{!showForm&&<div className="page-actions"><div/><button className="primary" onClick={openCreate}><Plus size={16}/>{t('hosts.add')}</button></div>}
    {notice && <div className="notice">{notice}<button onClick={()=>setNotice('')}><X size={14}/></button></div>}
		{showForm && <ConfigurationEditorPage icon={<Server size={22}/>} title={editing?t('hosts.editTitle'):t('hosts.createTitle')} busy={saving} onBack={()=>setShowForm(false)}><form className="host-form configuration-editor-form panel" onSubmit={save}><div className="form-grid host-fields">
	  <label><span>{t('hosts.name')}</span><input value={form.name} onChange={event=>setForm({...form,name:event.target.value})} required/></label>
	  <label><span>{t('hosts.address')}</span><input value={form.address} onChange={event=>setForm({...form,address:event.target.value})} required/></label>
	  <label><span>{t('hosts.port')}</span><input type="number" min="1" max="65535" value={form.port} onChange={event=>setForm({...form,port:Number(event.target.value)})} required/></label>
	  <label><span>{t('hosts.user')}</span><input value={form.user} onChange={event=>setForm({...form,user:event.target.value})} required/></label>
	  <label className="host-agent-toggle"><span>Agent</span><input type="checkbox" checked={form.agent_enabled} onChange={event=>setForm({...form,agent_enabled:event.target.checked})}/><i/></label>
	  <label><span>{t('hosts.authentication')}</span><select value={form.auth_type} onChange={event=>setAuthType(event.target.value as HostAuthType)}>{(['agent','key','password'] as HostAuthType[]).map(mode=><option value={mode} key={mode}>{authLabel(mode)}</option>)}</select></label>
	  {form.auth_type==='password'&&<label><span>{t('hosts.sshPassword')}</span><PasswordInput autoComplete="new-password" value={form.password} onChange={event=>setForm({...form,password:event.target.value})} placeholder={editing?t('hosts.keepPassword'):t('common.required')} required={!editing}/></label>}
	  {form.auth_type==='key'&&<div className="private-key-field"><span>{t('hosts.privateKey')}</span><label className={`private-key-picker ${privateKeyError||missingPrivateKey?'invalid':''}`} title={privateKeyName||t('hosts.chooseKey')}><UploadCloud size={15}/><span><b>{privateKeyName||(existingPrivateKey?t('hosts.storedKey'):t('hosts.choosePrivateKey'))}</b>{!privateKeyName&&!existingPrivateKey&&<small>{t('hosts.keyLimit')}</small>}</span><input key={privateKeyInputKey} type="file" onChange={event=>void choosePrivateKey(event)}/></label>{(privateKeyError||missingPrivateKey)&&<small className="private-key-error">{privateKeyError||t('hosts.keyRequired')}</small>}</div>}
	  <label><span>{t('hosts.proxyJump')}</span><select value={form.proxy_jump_host_id} onChange={event=>setForm({...form,proxy_jump_host_id:event.target.value})}><option value="">{t('hosts.direct')}</option>{hosts.filter(host=>host.id!==form.id).map(host=><option value={host.id} key={host.id}>{host.name} · {host.user}@{host.address}:{host.port}</option>)}</select></label>
	  <label><span>{t('common.proxy')}</span><select value={form.proxy_id} onChange={event=>setForm({...form,proxy_id:event.target.value})}><option value="">{t('hosts.direct')}</option>{proxies.filter(proxy=>proxy.ssh_compatible).map(proxy=><option value={proxy.id} key={proxy.id}>{proxy.name} · {proxy.url}</option>)}</select></label>
	  <label><span>{t('hosts.knownHosts')}</span><input value={form.known_hosts_file} onChange={event=>setForm({...form,known_hosts_file:event.target.value})} placeholder={t('hosts.useDefault')}/></label>
	  <label><span>{t('hosts.sudoPolicy')}</span><select value={form.sudo_mode} onChange={event=>setForm({...form,sudo_mode:event.target.value as HostSudoMode,sudo_password:''})}>{(['none','nopasswd','password'] as HostSudoMode[]).map(mode=><option value={mode} key={mode}>{sudoLabel(mode)}</option>)}</select></label>
	  {form.sudo_mode==='password'&&<label><span>{t('hosts.sudoPasswordLabel')}</span><PasswordInput autoComplete="new-password" value={form.sudo_password} onChange={event=>setForm({...form,sudo_password:event.target.value})} placeholder={editing?t('hosts.keepPassword'):t('common.required')} required={!editing}/></label>}
		</div><div className="form-actions"><button type="button" onClick={()=>setShowForm(false)}>{t('common.cancel')}</button><button className="primary" disabled={saving||!!privateKeyError||missingPrivateKey}>{saving?t('common.saving'):editing?t('hosts.update'):t('hosts.save')}</button></div></form></ConfigurationEditorPage>}
		{!showForm&&<div className="host-grid">{hosts.map(host=>{const key=hostKeys[host.id]||host.host_key,keyError=hostKeyErrors[host.id],scanning=hostKeyBusy===`scan-${host.id}`,trusting=hostKeyBusy===`trust-${host.id}`,proxy=proxies.find(item=>item.id===host.proxy_id);return <article className="host-card panel" key={host.id}><div className="host-top"><div className="server-glyph"><Server size={22}/></div><div><h3>{host.name}</h3><span>{`${host.user}@${showAddresses?host.address:'••••••'}:${host.port}`}</span></div><div className="host-top-states"><span className={`host-agent-state ${host.agent_enabled?'active':''}`} title="Agent"><Bot size={13}/></span><span className={`host-key-state ${key?.trusted?'trusted':key?'untrusted':'unchecked'}`}>{scanning?t('hosts.checkingKey'):key?.trusted?t('hosts.trustedKey'):key?t('hosts.untrustedKey'):t('hosts.uncheckedKey')}</span></div></div><dl><div><dt>{t('hosts.authentication')}</dt><dd>{authLabel(host.auth_type||'agent')}</dd></div>{proxy&&<div><dt>{t('hosts.proxy')}</dt><dd title={showAddresses?proxy.url:undefined}>{proxy.name}</dd></div>}<div><dt>Sudo</dt><dd>{sudoLabel(host.sudo_mode||'none')}</dd></div><div><dt>{t('hosts.hostId')}</dt><dd>{host.id}</dd></div></dl>{(key||keyError)&&<div className={`host-key-review ${key?.trusted?'trusted':'untrusted'}`}>{key&&<><div><KeyRound size={14}/><span><b>{key.algorithm||t('hosts.hostKey')}</b><code title={key.fingerprint}>{key.fingerprint}</code></span></div>{!key.trusted&&<button className="trust" disabled={trusting} onClick={()=>void trust(host)}>{trusting?<LoaderCircle className="spin" size={13}/>:<ShieldCheck size={13}/>} {trusting?t('hosts.trustingKey'):t('hosts.trustKey')}</button>}</>}{keyError&&<span className="host-key-error">{keyError}</span>}</div>}<div className="card-actions"><button onClick={()=>void probe(host)}><Activity size={15}/>{t('hosts.probe')}</button><button disabled={scanning||trusting} onClick={()=>void scan(host)}>{scanning?<LoaderCircle className="spin" size={15}/>:<KeyRound size={15}/>} {t('hosts.checkKey')}</button><button onClick={()=>openEdit(host)}><Edit3 size={15}/>{t('common.edit')}</button><button className="danger" disabled={deletingHost===host.id} title={t('common.delete')} onClick={()=>setDeleteCandidate(host)}>{deletingHost===host.id?<LoaderCircle className="spin" size={15}/>:<Trash2 size={15}/>}</button></div></article>})}</div>}
	{!showForm&&!hosts.length && <Empty icon={<Server/>} title={t('hosts.emptyTitle')}/>}
	{deleteCandidate&&<DestructiveConfirmDialog label={t('hosts.deleteDialogLabel')} title={t('hosts.deleteTitle',{name:deleteCandidate.name})} description={t('hosts.deleteText')} busy={deletingHost===deleteCandidate.id} onCancel={()=>setDeleteCandidate(null)} onConfirm={()=>void remove()}/>}
  </div>
}

const emptyProxyForm:ProxyInput={name:'',url:'',username:'',password:''}

function ProxiesPage({proxies,showAddresses,refresh}:{proxies:Proxy[];showAddresses:boolean;refresh:()=>Promise<void>}){
	const {t,i18n:instance}=useTranslation()
	const [form,setForm]=useState<ProxyInput>(emptyProxyForm)
	const [showForm,setShowForm]=useState(false)
	const [busy,setBusy]=useState('')
	const [notice,setNotice]=useState('')
	const [deleteCandidate,setDeleteCandidate]=useState<Proxy|null>(null)
	const editing=!!form.id
	const editingProxy=proxies.find(proxy=>proxy.id===form.id)
	const preservesPassword=!!editingProxy?.has_password&&form.username===(editingProxy.username||'')&&!form.clear_password
	const openCreate=()=>{setForm(emptyProxyForm);setShowForm(true);setNotice('')}
	const openEdit=(proxy:Proxy)=>{setForm({id:proxy.id,name:proxy.name,url:proxy.url,username:proxy.username||'',password:''});setShowForm(true);setNotice('')}
	const save=async(event:FormEvent)=>{event.preventDefault();setBusy('save');setNotice('');try{const saved=await api.saveProxy(form);setNotice(t('proxies.saved',{name:saved.name}));setForm(emptyProxyForm);setShowForm(false);await refresh()}catch(err){setNotice(errorText(err))}finally{setBusy('')}}
	const test=async(proxy:Proxy)=>{setBusy(`test-${proxy.id}`);setNotice('');try{const result=await api.testProxy(proxy.id);setNotice(t('proxies.testPassed',{name:proxy.name,status:result.status_code||0,latency:result.latency_ms}))}catch(err){setNotice(errorText(err))}finally{setBusy('')}}
	const remove=async()=>{const proxy=deleteCandidate;if(!proxy)return;setBusy(`delete-${proxy.id}`);setNotice('');try{await api.deleteProxy(proxy.id);setNotice(t('proxies.deleted',{name:proxy.name}));if(form.id===proxy.id){setForm(emptyProxyForm);setShowForm(false)}await refresh()}catch(err){setNotice(errorText(err))}finally{setBusy('');setDeleteCandidate(null)}}
	return <div className="page-stack">
		{!showForm&&<div className="page-actions"><div><p>{t('proxies.title')}</p><span>{t('proxies.description')}</span></div><button className="primary" onClick={openCreate}><Plus size={16}/>{t('proxies.add')}</button></div>}
		{notice&&<div className="notice">{notice}<button onClick={()=>setNotice('')}><X size={14}/></button></div>}
		{showForm&&<ConfigurationEditorPage icon={<Cable size={22}/>} title={editing?t('proxies.editTitle'):t('proxies.createTitle')} busy={busy==='save'} onBack={()=>setShowForm(false)}><form className="proxy-form configuration-editor-form panel" onSubmit={save}><div className="form-grid proxy-fields"><label><span>{t('proxies.name')}</span><input value={form.name} maxLength={128} onChange={event=>setForm({...form,name:event.target.value})} required/></label><label className="proxy-address-field"><span>{t('proxies.url')}</span><input value={form.url} onChange={event=>setForm({...form,url:event.target.value})} placeholder="socks5://127.0.0.1:1080" required/></label><label><span>{t('proxies.username')}</span><input autoComplete="off" value={form.username} onChange={event=>setForm({...form,username:event.target.value,password:event.target.value?form.password:'',clear_password:false})}/></label><label><span>{t('proxies.password')}</span><PasswordInput autoComplete="new-password" value={form.password} disabled={!form.username} onChange={event=>setForm({...form,password:event.target.value,clear_password:false})} placeholder={preservesPassword?t('proxies.keepPassword'):''}/>{preservesPassword&&<small><button type="button" onClick={()=>setForm({...form,password:'',clear_password:true})}>{t('proxies.clearPassword')}</button></small>}</label></div><div className="form-actions"><button type="button" onClick={()=>setShowForm(false)}>{t('common.cancel')}</button><button className="primary" disabled={busy==='save'}>{busy==='save'?t('common.saving'):t('common.save')}</button></div></form></ConfigurationEditorPage>}
		{!showForm&&<div className="proxy-grid">{proxies.map(proxy=><article className="proxy-card panel" key={proxy.id}><div className="proxy-card-head"><div><Cable size={20}/></div><span><h3>{proxy.name}</h3><code>{showAddresses?proxy.url:'••••••'}</code></span>{proxy.ssh_compatible&&<em>SSH</em>}</div><dl><div><dt>{t('proxies.authentication')}</dt><dd>{proxy.username?`${proxy.username}${proxy.has_password?` · ${t('proxies.passwordSaved')}`:''}`:t('proxies.noAuthentication')}</dd></div><div><dt>{t('common.updated')}</dt><dd>{new Date(proxy.updated_at).toLocaleString(localeFor(instance.language))}</dd></div></dl><div className="card-actions"><button disabled={!!busy} onClick={()=>void test(proxy)}>{busy===`test-${proxy.id}`?<LoaderCircle className="spin" size={14}/>:<Activity size={14}/>} {t('common.test')}</button><button disabled={!!busy} onClick={()=>openEdit(proxy)}><Edit3 size={14}/>{t('common.edit')}</button><button className="danger" disabled={!!busy} title={t('common.delete')} onClick={()=>setDeleteCandidate(proxy)}><Trash2 size={14}/></button></div></article>)}</div>}
		{!showForm&&!proxies.length&&<Empty icon={<Cable/>} title={t('proxies.emptyTitle')}/>}
		{deleteCandidate&&<DestructiveConfirmDialog label={t('proxies.deleteDialogLabel')} title={t('proxies.deleteTitle',{name:deleteCandidate.name})} description={t('proxies.deleteText')} busy={busy===`delete-${deleteCandidate.id}`} onCancel={()=>setDeleteCandidate(null)} onConfirm={()=>void remove()}/>}
	</div>
}

const emptyProviderForm: ModelProviderInput = {name:'',kind:'openai',base_url:'',model:'gpt-4o-mini',api_key:'',proxy_id:'',user_agent:''}
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

function ModelsPage({providers,proxies,health,showAddresses,refresh}:{providers:ModelProvider[];proxies:Proxy[];health:Health|null;showAddresses:boolean;refresh:()=>Promise<void>}) {
	const {t,i18n:instance}=useTranslation()
  const [showForm,setShowForm]=useState(false)
  const [form,setForm]=useState<ModelProviderInput>(emptyProviderForm)
  const [notice,setNotice]=useState('')
  const [busy,setBusy]=useState('')
	const [deleteCandidate,setDeleteCandidate]=useState<ModelProvider|null>(null)
  const [catalog,setCatalog]=useState<string[]>([])
  const [discovering,setDiscovering]=useState(false)
  const editing=!!form.id

  const openCreate=()=>{setForm(emptyProviderForm);setCatalog([]);setShowForm(true);setNotice('')}
  const openEdit=(provider:ModelProvider)=>{setForm({id:provider.id,name:provider.name,kind:provider.kind,base_url:provider.base_url||'',model:provider.model,api_key:'',proxy_id:provider.proxy_id||'',user_agent:provider.user_agent||''});setCatalog([]);setShowForm(true);setNotice('')}
  const changeKind=(kind:ModelProviderKind)=>{setCatalog([]);setForm({...form,kind,...providerDefaults[kind]})}
	const discover=async()=>{setDiscovering(true);try{const {name:_name,model:_model,...payload}=form;const result=await api.discoverModels(payload);setCatalog(result.models);setForm(current=>({...current,model:result.models.includes(current.model)?current.model:''}));setNotice(t('models.found',{count:result.count}))}catch(err){setCatalog([]);setNotice(errorText(err))}finally{setDiscovering(false)}}
	const testForm=async()=>{setBusy('test-form');try{const {name:_name,...payload}=form;const result=await api.testModelConfiguration(payload);setNotice(t('models.healthy',{name:result.model,response:result.response,latency:result.latency_ms}))}catch(err){setNotice(errorText(err))}finally{setBusy('')}}
	const save=async(event:FormEvent)=>{event.preventDefault();setBusy('save');try{const saved=await api.saveModelProvider(form);setNotice(t('models.saved',{name:saved.name}));setShowForm(false);setForm(emptyProviderForm);await refresh()}catch(err){setNotice(errorText(err))}finally{setBusy('')}}
	const activate=async(provider:ModelProvider)=>{setBusy(provider.id);try{await api.activateModelProvider(provider.id);setNotice(t('models.activated',{name:provider.name}));await refresh()}catch(err){setNotice(errorText(err))}finally{setBusy('')}}
	const test=async(provider:ModelProvider)=>{setBusy(`test-${provider.id}`);try{const result=await api.testModelProvider(provider.id);setNotice(t('models.healthy',{name:provider.name,response:result.response,latency:result.latency_ms}))}catch(err){setNotice(errorText(err))}finally{setBusy('')}}
	const remove=async()=>{const provider=deleteCandidate;if(!provider)return;setBusy(`delete-${provider.id}`);setNotice('');try{await api.deleteModelProvider(provider.id);setNotice(t('models.deleted',{name:provider.name}));await refresh()}catch(err){setNotice(errorText(err))}finally{setBusy('');setDeleteCandidate(null)}}

  return <div className="page-stack">
	{!showForm&&<div className="page-actions"><div/><button className="primary" onClick={openCreate}><Plus size={16}/>{t('models.add')}</button></div>}
    {notice&&<div className="notice">{notice}<button onClick={()=>setNotice('')}><X size={14}/></button></div>}
	{!showForm&&<div className="model-summary panel"><div><span>{t('models.activeRoute')}</span><b>{health?.model?.name||t('models.noModel')}</b>{health?.model?.model&&<small>{health.model.model}</small>}</div><div className={`model-signal ${health?.agent_available?'ready':''}`}><CircleDot size={16}/>{health?.agent_available?t('models.ready'):t('models.offline')}</div></div>}
    {showForm&&<ConfigurationEditorPage icon={<Cpu size={22}/>} title={editing?t('models.editTitle'):t('models.newTitle')} busy={!!busy} onBack={()=>setShowForm(false)}><form className="model-form configuration-editor-form panel" onSubmit={save}>
      <div className="form-grid model-fields">
		<label><span>{t('models.displayName')}</span><input value={form.name} onChange={event=>setForm({...form,name:event.target.value})} required/></label>
		<label><span>{t('models.providerType')}</span><select value={form.kind} onChange={event=>changeKind(event.target.value as ModelProviderKind)}>{(Object.keys(providerLabels) as ModelProviderKind[]).map(kind=><option key={kind} value={kind}>{providerLabels[kind]}</option>)}</select></label>
		<label className="model-id-field"><span className="field-title"><span>{t('models.modelId')}</span><button type="button" onClick={discover} disabled={discovering}><RefreshCw size={12}/>{discovering?t('models.fetching'):t('models.fetchModels')}</button></span>{catalog.length>0?<select value={form.model} onChange={event=>setForm({...form,model:event.target.value})} required><option value="">{t('models.selectModel')}</option>{catalog.map(model=><option value={model} key={model}>{model}</option>)}</select>:<input value={form.model} onChange={event=>setForm({...form,model:event.target.value})} placeholder={t('models.modelPlaceholder')} required/>}{catalog.length>0&&<small>{t('models.available',{count:catalog.length})} · <button type="button" onClick={()=>setCatalog([])}>{t('models.enterManually')}</button></small>}</label>
			<label><span>{t('models.apiKey')}</span><PasswordInput autoComplete="new-password" value={form.api_key} onChange={event=>{setCatalog([]);setForm({...form,api_key:event.target.value})}} placeholder={editing?t('models.keepKey'):''}/></label>
			<label className="base-url-field"><span>{t('models.baseUrl')}</span><input value={form.base_url} onChange={event=>{setCatalog([]);setForm({...form,base_url:event.target.value})}} placeholder={form.kind==='openai'?t('models.officialEndpoint'):''}/></label>
			<label><span>{t('models.userAgent')}</span><input value={form.user_agent} onChange={event=>{setCatalog([]);setForm({...form,user_agent:event.target.value})}} placeholder={t('models.userAgentHint')}/></label>
			<label className="proxy-select-field"><span>{t('common.proxy')}</span><select value={form.proxy_id} onChange={event=>{setCatalog([]);setForm({...form,proxy_id:event.target.value})}}><option value="">{t('common.direct')}</option>{proxies.map(proxy=><option value={proxy.id} key={proxy.id}>{proxy.name} · {proxy.url}</option>)}</select></label>
      </div>
	  <div className="form-actions"><button type="button" onClick={()=>setShowForm(false)}>{t('common.cancel')}</button><button type="button" className="test-config" onClick={testForm} disabled={!!busy||!form.model}><Activity size={14}/>{busy==='test-form'?t('models.sendingHello'):t('models.testModel')}</button><button className="primary" disabled={!!busy}>{busy==='save'?t('common.saving'):t('models.saveProvider')}</button></div>
    </form></ConfigurationEditorPage>}
    {!showForm&&<div className="model-grid">{providers.map(provider=>{const proxy=proxies.find(item=>item.id===provider.proxy_id);return <article className={`model-card panel ${provider.active?'active':''}`} key={provider.id}>
	  <div className="model-card-head"><div className="provider-glyph"><Cpu size={21}/></div><div><h3>{provider.name}</h3><span>{providerLabels[provider.kind]}</span></div>{provider.active&&<em><Zap size={12}/>{t('models.active')}</em>}</div>
      <div className="model-name">{provider.model}</div>
	  <dl><div><dt>{t('models.endpoint')}</dt><dd>{provider.base_url?(showAddresses?provider.base_url:'••••••'):t('models.providerDefault')}</dd></div><div><dt>{t('models.proxy')}</dt><dd title={showAddresses?proxy?.url:undefined}>{proxy?.name||t('models.noProxy')}</dd></div>{provider.user_agent&&<div><dt>{t('models.userAgent')}</dt><dd>{provider.user_agent}</dd></div>}<div><dt>{t('models.credential')}</dt><dd>{provider.has_api_key?t('models.encryptedKey'):t('models.noApiKey')}</dd></div><div><dt>{t('common.updated')}</dt><dd>{new Date(provider.updated_at).toLocaleString(localeFor(instance.language))}</dd></div></dl>
	  <div className="model-actions"><button onClick={()=>test(provider)} disabled={!!busy}><Activity size={14}/>{busy===`test-${provider.id}`?t('common.testing'):t('common.test')}</button><button onClick={()=>openEdit(provider)} disabled={!!busy}><Edit3 size={14}/>{t('common.edit')}</button>{!provider.active&&<button className="use-model" onClick={()=>activate(provider)} disabled={!!busy}><Zap size={14}/>{busy===provider.id?t('models.switching'):t('models.useModel')}</button>}<button className="danger" title={t('common.delete')} onClick={()=>setDeleteCandidate(provider)} disabled={!!busy}><Trash2 size={14}/></button></div>
    </article>})}</div>}
		{!showForm&&!providers.length&&<Empty icon={<Cpu/>} title={t('models.emptyTitle')}/>}
		{deleteCandidate&&<DestructiveConfirmDialog
			label={t('models.deleteDialogLabel')}
			title={t('models.deleteTitle',{name:deleteCandidate.name})}
			description={`${t('models.deleteText')}${deleteCandidate.active?` ${t('models.deleteActiveText')}`:''}`}
			busy={busy===`delete-${deleteCandidate.id}`}
			onCancel={()=>setDeleteCandidate(null)}
			onConfirm={()=>void remove()}
		/>}
  </div>
}

function AuditRunDetail({run,req,hosts}:{run:Run;req:JsonRecord;hosts:Host[]}){
	const {t}=useTranslation()
	const toolArguments=run.tool_arguments_json?parseRecord(run.tool_arguments_json):undefined
	const script=textValue(req.script)
	const program=textValue(req.program)?fullProgram(req):''
	const args=Array.isArray(req.args)?req.args.map(value=>String(value)):[]
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
	const tunnelRoute=tunnelMode?sshTunnelRoute(destinationHost.name||destinationHost.id,textValue(req.remote_host),numberValue(req.remote_port),numberValue(req.local_port),t('tunnels.automaticPort')):''
	const shellTarget=`${mode==='workspace_shell_start'?`${workspaceID}:${textValue(req.cwd)||'.'}`:destinationHost.name||destinationHost.id} · PTY`
	const fileTarget=`${workspaceID?`${workspaceID}:`:''}${filePath}`
	const commandText=shellMode?shellTarget:tunnelMode?tunnelRoute:workspaceUpload?`workspace_upload ${workspaceID}:${relativePath} → ${destinationHost.name||destinationHost.id}:${remotePath}`:workspaceDownload?`workspace_download ${destinationHost.name||destinationHost.id}:${remotePath} → ${workspaceID}:${relativePath}`:sshTransfer?`${[sourceHost.name||sourceHost.id,sourcePath].filter(Boolean).join(':')} → ${destinationHost.name||destinationHost.id}:${remotePath}`:searchMode||readMode?`${searchMode?'search':'read'} ${fileTarget}`:script?script:program?program:filePath?`${mode} ${fileTarget}`:JSON.stringify(req,null,2)
	const consumed=new Set(['program','args','script','cwd','reason','change','env','host_id','workspace_id','remote_path','relative_path','source_path','source_host_id','mode','elevated','workspace_shell_backend','remote_host','remote_port','local_port'])
	const extras=Object.entries(req).filter(([key,value])=>!consumed.has(key)&&value!==undefined&&value!==null&&value!==''&&!(Array.isArray(value)&&!value.length))
	return <>
		{toolArguments&&<CompactTable title={`${toolLabel(run.tool_name||'')} · ${t('tool.actualParameters')}`} columns={[t('tool.parameter'),t('tool.value')]} rows={Object.entries(toolArguments).map(([key,value])=>[key,value])}/>}
		<div className="tool-execution-layout">
			<section className="tool-command-pane">
				<div className="tool-command-head"><span>{shellMode?`${t('sshShell.toolActions.start')} Shell`:tunnelMode?t('tunnels.forwarding'):searchMode?t('tool.searchOperation'):readMode?t('tool.readOperation'):workspaceTransfer||sshTransfer||filePath?t('tool.fileOperation'):script?t('tool.fullScript'):t('tool.fullCommand')}</span>{workspaceShellBackend&&<em><TerminalSquare size={12}/>{workspaceShellBackend==='host'?t('approval.hostShell'):'Bubblewrap'}</em>}{req.elevated===true&&<em><ShieldAlert size={12}/>sudo / root</em>}</div>
				<div className="tool-command-block"><CopyButton value={commandText}/><pre>{program&&commandText===program?<><span className="prompt-sign">$</span> {program}</>:commandText}</pre></div>
				{change&&textValue(change.diff)&&<DiffViewer change={change}/>}
				{program&&args.length>0&&<CompactTable title={t('tool.originalArgs')} columns={[t('tool.index'),t('tool.value')]} rows={[[0,textValue(req.program)],...args.map((arg,index)=>[index+1,JSON.stringify(arg)])]}/>}
				{env&&Object.keys(env).length>0&&<CompactTable title={t('tool.environment')} columns={[t('tool.key'),t('tool.value')]} rows={Object.entries(env).map(([key,value])=>[key,String(value)])}/>}
				{extras.length>0&&<CompactTable title={t('tool.actualParameters')} columns={[t('tool.parameter'),t('tool.value')]} rows={extras}/>}
			</section>
			<aside className="tool-context-pane">
				<dl className="tool-context-grid">
					<div><dt>{workspaceID&&!sshTransfer?t('common.workspace'):t('tool.targetHost')}</dt><dd>{workspaceID&&!sshTransfer?workspaceID:[destinationHost.name,destinationHost.id].filter(Boolean).join(' · ')||'—'}</dd></div>
					{sshTransfer&&<div><dt>{t('tool.sourceHost')}</dt><dd>{[sourceHost.name,sourceHost.id].filter(Boolean).join(' · ')||'—'}</dd></div>}
					<div><dt>{tunnelMode?t('tunnels.remoteEndpoint'):filePath?t('tool.filePath'):t('tool.workingDirectory')}</dt><dd>{tunnelMode?`${textValue(req.remote_host)}:${numberValue(req.remote_port)}`:filePath||textValue(req.cwd)||t('tool.defaultDirectory')}</dd></div>
					<div><dt>{t('tool.permission')}</dt><dd>{workspaceShellBackend==='host'?t('tool.hostAuthority'):workspaceShellBackend==='sandbox'?t('tool.sandbox'):req.elevated===true?t('tool.managedSudo'):t('tool.normalUser')}</dd></div>
					<div><dt>{t('tool.duration')}</dt><dd>{formatDuration(undefined,run)}</dd></div>
					<div><dt>{t('tool.runId')}</dt><dd>{run.id}</dd></div>
				</dl>
				{textValue(req.reason)&&<div className="tool-reason"><span>{t('tool.reason')}</span><p>{textValue(req.reason)}</p></div>}
			</aside>
		</div>
		{(run.stdout_redacted||run.stderr_redacted||run.error)&&<div className="tool-output-grid">{run.stdout_redacted&&<ToolOutputPanel kind="stdout" label="STDOUT · REDACTED" content={run.stdout_redacted} live={false}/>} {run.stderr_redacted&&<ToolOutputPanel kind="stderr" label="STDERR · REDACTED" content={run.stderr_redacted} live={false}/>} {run.error&&!run.stderr_redacted&&<ToolOutputPanel kind="stderr" label={t('common.error')} content={run.error} live={false}/>}</div>}
		<details className="tool-raw"><summary>{t('tool.normalizedRequest')}</summary><CopyablePre>{JSON.stringify(req,null,2)}</CopyablePre></details>
	</>
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
		case'ssh_shell_start':return `${t('sshShell.toolActions.start')} Shell`
		case'workspace_shell_start':return `${t('sshShell.toolActions.start')} · ${workspaceID}:${textValue(req.cwd)||'.'}`
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
	return <div className="page-stack"><div className="audit-toolbar"><div className="search-box"><Search size={16}/><input aria-label={t('common.search')} value={query} onChange={event=>setQuery(event.target.value)}/></div><span>{t('audit.counts',{sessions:groups.length,runs:filtered.length})}</span></div><div className="audit-groups">{groups.map(group=><details className="audit-session panel" key={group.id}><summary className="audit-session-summary"><div className="audit-session-glyph"><History size={17}/></div><div className="audit-session-name"><b>{group.title}</b><span>{group.id==='__direct__'?t('audit.noSession'):group.id} · {t('audit.lastRun',{date:new Date(group.latest).toLocaleString(localeFor(instance.language))})}</span></div><div className="audit-session-stats"><span><b>{group.runs.length}</b> {t('audit.runs')}</span>{group.pending>0&&<span className="pending-count"><b>{group.pending}</b> {t('audit.pending')}</span>}</div><ChevronRight className="audit-session-chevron" size={17}/></summary><div className="audit-table"><div className="audit-row audit-head"><span>{t('audit.columns.time')}</span><span>{t('audit.columns.operation')}</span><span>{t('audit.columns.status')}</span><span>{t('audit.columns.host')}</span><span>{t('audit.columns.exit')}</span></div>{group.runs.map(run=>{let req:Record<string,unknown>={};try{req=JSON.parse(run.request_json)}catch{req={request:run.request_json}};const auditHost=hostIdentity(hosts,run.host_id);const workspaceID=textValue(req.workspace_id);const target=auditHost.name||(run.host_id.startsWith('workspace_')?workspaceID:run.host_id)||'—';const operation=auditOperationSummary(req,run,hosts,t);return <details key={run.id}><summary className="audit-row"><span>{new Date(run.started_at).toLocaleString(localeFor(instance.language))}</span><span className="command">{operation}</span><span className={`run-status ${run.status}`}>{t(`statusLabels.${run.status}`,{defaultValue:run.status})}</span><span title={run.host_id}>{target}</span><span>{run.exit_code}</span></summary><div className="run-detail"><AuditRunDetail run={run} req={req} hosts={hosts}/></div></details>})}</div></details>)}</div>{!runs.length&&<Empty icon={<History/>} title={t('audit.emptyTitle')}/>} {runs.length>0&&!groups.length&&<Empty icon={<Search/>} title={t('audit.noMatch')}/>}</div>
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
	  <label><span>{t('logs.minimumLevel')}</span><select value={level} onChange={event=>setLevel(event.target.value)}><option value="debug">Debug+</option><option value="info">Info+</option><option value="warn">Warn+</option><option value="error">Error</option></select></label>
	  <label><span>{t('logs.component')}</span><select value={component} onChange={event=>setComponent(event.target.value)}><option value="">{t('logs.allComponents')}</option>{components.map(item=><option value={item} key={item}>{item}</option>)}</select></label>
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
