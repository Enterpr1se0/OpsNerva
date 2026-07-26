import { FormEvent, memo, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import Markdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { useTranslation } from 'react-i18next'
import {
  Activity, BookOpen, Bot, BrainCircuit, Braces, Check, ChevronRight, CircleDot, Clock3, Cpu, Edit3, Eye, EyeOff, FileText, FolderOpen, FunctionSquare, History, ImagePlus, KeyRound, LockKeyhole, LogOut,
  Download, ListChecks, LoaderCircle, Plus, Power, RefreshCw, Save, Search, Send, Server, Settings2, ShieldAlert, ShieldCheck, SlidersHorizontal, Square, TerminalSquare, Trash2, UploadCloud, X, Zap,
} from 'lucide-react'
import { api, chatAttachmentURL, streamChat, workspaceFileEventsURL } from './api'
import i18n, { localeFor, type SupportedLanguage } from './i18n'
import type { AgentEvent, AgentPlan, Approval, ChatMessage, ChatSession, CommandReview, Health, Host, HostAuthType, HostInput, HostSudoMode, LLMToolCatalog, LLMToolDescriptor, LLMToolGuard, ManagedSkill, MCPServer, MCPServerInput, MCPTransport, ModelProvider, ModelProviderInput, ModelProviderKind, Run, ServerLogEntry, SystemSettings, SystemSettingsInput, ToolCapabilities, WebSearchSettings, WebSearchSettingsInput, WorkspaceCapability, WorkspaceFilePreview, WorkspaceInput, WorkspaceShellMode } from './types'

type Page = 'chat' | 'config' | 'extensions' | 'audit' | 'logs'
type ChatEntryImage = {id:string;name:string;mimeType:string;sizeBytes:number;url:string}
type PendingChatImage = {id:string;file:File;url:string}
type ChatEntry = { id: string; kind: 'user' | 'assistant' | 'tool' | 'reasoning' | 'error'; content: string; tool?: string; images?:ChatEntryImage[]; active?: boolean; streaming?: boolean; status?: 'pending' | 'completed' | 'failed' }
type ActiveChatStream = { id: string; sessionId: string; controller: AbortController }

function historyEntries(messages:ChatMessage[],sessionID:string):ChatEntry[]{
  return messages.map((item,index)=>({id:`history_${item.id||`${index}_${item.created_at}`}`,kind:item.role,content:item.content,tool:item.tool_name,status:item.status,images:item.attachments?.map(image=>({id:image.id,name:image.name,mimeType:image.mime_type,sizeBytes:image.size_bytes,url:chatAttachmentURL(sessionID,image.id)}))}))
}

function deactivateReasoning(entry:ChatEntry):ChatEntry{
	return entry.kind==='reasoning'&&entry.active?{...entry,active:false}:entry
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
function rememberSession(id: string) { try { localStorage.setItem('opspilot.activeSession', id) } catch { /* storage may be disabled */ } }
function recalledSession() { try { return localStorage.getItem('opspilot.activeSession') || '' } catch { return '' } }
function rememberWorkspace(id:string){try{if(id)localStorage.setItem('opspilot.activeWorkspace',id)}catch{/* storage may be disabled */}}
function recalledWorkspace(){try{return localStorage.getItem('opspilot.activeWorkspace')||''}catch{return''}}

function App() {
	const {t}=useTranslation()
	const [auth,setAuth]=useState<'checking'|'setup'|'authenticated'|'guest'>('checking')
  const [page, setPage] = useState<Page>('chat')
  const [health, setHealth] = useState<Health | null>(null)
  const [hosts, setHosts] = useState<Host[]>([])
  const [providers, setProviders] = useState<ModelProvider[]>([])
  const [settings, setSettings] = useState<SystemSettings | null>(null)
	const [capabilities,setCapabilities]=useState<ToolCapabilities>({workspaces:[]})
	const [toolCatalog,setToolCatalog]=useState<LLMToolCatalog|null>(null)
	const [skills,setSkills]=useState<ManagedSkill[]>([])
	const [mcpServers,setMCPServers]=useState<MCPServer[]>([])
  const [approvals, setApprovals] = useState<Approval[]>([])
  const [runs, setRuns] = useState<Run[]>([])
  const [error, setError] = useState('')
	const [agentStreaming,setAgentStreaming]=useState(false)

  const refresh = useCallback(async () => {
    try {
	  const [nextHealth, nextHosts, nextProviders, nextSettings, nextCapabilities, nextToolCatalog, nextSkills, nextMCPServers, nextApprovals, nextRuns] = await Promise.all([
		api.health(), api.hosts(), api.modelProviders(), api.systemSettings(), api.capabilities(), api.llmTools(), api.skills(), api.mcpServers(), api.approvals(), api.runs(),
      ])
	  setHealth(nextHealth); setHosts(nextHosts); setProviders(nextProviders); setSettings(nextSettings);setCapabilities(nextCapabilities);setToolCatalog(nextToolCatalog);setSkills(nextSkills);setMCPServers(nextMCPServers); setApprovals(nextApprovals); setRuns(nextRuns); setError('')
	} catch (err) { const message=errorText(err);if(/authentication required/i.test(message))setAuth('guest');setError(message) }
  }, [])
	const refreshApprovals=useCallback(async()=>{
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
		if(!('__TAURI_INTERNALS__' in window))return
		const handleContextMenu=(event:MouseEvent)=>{
			const target=event.target instanceof Element?event.target:null
			if(target?.closest('input, textarea, [contenteditable="true"]'))return
			event.preventDefault()
		}
		document.addEventListener('contextmenu',handleContextMenu)
		return()=>document.removeEventListener('contextmenu',handleContextMenu)
	},[])
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

  return <div className="app-shell">
    <aside className="sidebar">
      <div className="brand"><div className="brand-mark"><TerminalSquare size={23}/></div><div><strong>OpsPilot</strong></div></div>
      <nav>
        <Nav active={page === 'chat'} icon={<Bot/>} label={t('shell.nav.agent')} onClick={() => setPage('chat')}/>
        <Nav active={page === 'config'} icon={<Settings2/>} label={t('shell.nav.configuration')} onClick={() => setPage('config')}/>
		<Nav active={page === 'extensions'} icon={<Braces/>} label={t('shell.nav.extensions')} onClick={() => setPage('extensions')}/>
        <Nav active={page === 'audit'} icon={<History/>} label={t('shell.nav.audit')} onClick={() => setPage('audit')}/>
        <Nav active={page === 'logs'} icon={<FileText/>} label={t('shell.nav.logs')} onClick={() => setPage('logs')}/>
      </nav>
      <div className="sidebar-foot">
			<button className="logout-button" onClick={async()=>{try{await api.logout()}finally{setAuth('guest')}}}><LogOut size={15}/>{t('shell.signOut')}</button>
        <div className="build">v0.1.2</div>
      </div>
    </aside>
    <main>
	      <header className="topbar"><div><h1>{title}</h1></div><div className="top-actions">
        <LanguageSwitch/>
        <span className={`status ${health?.status === 'ok' ? 'online' : ''}`}><CircleDot size={14}/>{health?.status === 'ok' ? t('shell.online') : t('shell.disconnected')}</span>
        <button className="icon-button" onClick={refresh} title={t('shell.refresh')}><RefreshCw size={17}/></button>
      </div></header>
      {error && <div className="global-error"><ShieldAlert size={17}/>{error}<button onClick={() => setError('')}><X size={15}/></button></div>}
      <section className="workspace">
			{page === 'chat' && <ChatPage hosts={hosts} approvals={approvals} runs={runs} capabilities={capabilities} imageTypes={settings?.chat_image_allowed_types||defaultChatImageTypes} agentAvailable={!!health?.agent_available} modelName={health?.model?.model} refresh={refresh} refreshApprovals={refreshApprovals} onStreamingChange={setAgentStreaming}/>}
		{page === 'config' && <ConfigurationPage hosts={hosts} providers={providers} settings={settings} capabilities={capabilities} health={health} refresh={refresh}/>}
		{page === 'extensions' && <ExtensionsPage skills={skills} mcpServers={mcpServers} toolCatalog={toolCatalog} refresh={refresh}/>}
        {page === 'audit' && <AuditPage runs={runs} hosts={hosts}/>}
        {page === 'logs' && <LogsPage/>}
      </section>
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

function LoginPage({onAuthenticated}:{onAuthenticated:()=>void}){
	const {t}=useTranslation()
	const [password,setPassword]=useState('')
	const [busy,setBusy]=useState(false)
	const [error,setError]=useState('')
	const submit=async(event:FormEvent)=>{event.preventDefault();setBusy(true);setError('');try{await api.login(password);setPassword('');onAuthenticated()}catch(err){setError(errorText(err))}finally{setBusy(false)}}
		return <div className="auth-screen"><LanguageSwitch/><section className="login-card"><div className="login-mark"><TerminalSquare size={29}/></div><span>{t('auth.subtitle')}</span><h1>{t('auth.title')}</h1><form onSubmit={submit}><label><div className="login-input"><LockKeyhole size={17}/><PasswordInput aria-label={t('password.current')} autoComplete="current-password" value={password} onChange={event=>setPassword(event.target.value)} autoFocus required/></div></label>{error&&<div className="login-error"><ShieldAlert size={15}/>{error}</div>}<button className="primary" disabled={busy||password.length===0}>{busy?<LoaderCircle className="spin" size={17}/>:<ShieldCheck size={17}/>}<span>{busy?t('auth.authenticating'):t('auth.enter')}</span></button></form></section></div>
}

type ConfigurationSection = 'models' | 'hosts' | 'system'

function ConfigurationPage({hosts,providers,settings,capabilities,health,refresh}:{hosts:Host[];providers:ModelProvider[];settings:SystemSettings|null;capabilities:ToolCapabilities;health:Health|null;refresh:()=>Promise<void>}) {
  const {t}=useTranslation()
  const [section,setSection]=useState<ConfigurationSection>('models')
  const tabs:[ConfigurationSection,React.ReactNode,string,string][]=[
    ['models',<Cpu size={17}/>, t('config.tabs.models'), t('config.configured',{count:providers.length})],
    ['hosts',<Server size={17}/>, t('config.tabs.hosts'), t('config.registered',{count:hosts.length})],
    ['system',<SlidersHorizontal size={17}/>, t('config.tabs.system'), t('config.maxIterations',{count:settings?.agent_max_iterations??50})],
	  ]
	  return <div className="configuration-center page-stack">
	    <div className="configuration-tabs" role="tablist" aria-label={t('config.sections')}>{tabs.map(([id,icon,label,meta])=><button type="button" role="tab" aria-selected={section===id} className={section===id?'active':''} onClick={()=>setSection(id)} key={id}>{icon}<span><b>{label}</b><small>{meta}</small></span><ChevronRight size={15}/></button>)}</div>
    <div className="configuration-content" role="tabpanel">
      {section==='models'&&<ModelsPage providers={providers} health={health} refresh={refresh}/>} 
      {section==='hosts'&&<HostsPage hosts={hosts} refresh={refresh}/>}
	  {section==='system'&&<SystemSettingsPage settings={settings} providers={providers} capabilities={capabilities} modelStatus={health?.model} refresh={refresh}/>}
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
			<aside className={`tool-function-inspector panel ${selected?.enabled?'':'disabled'}`}>{selected?<><header><div className="tool-function-icon"><FunctionSquare size={18}/></div><span><small>{t('tools.functionDetail')}</small><code>{selected.name}</code></span><div className="tool-function-controls"><em className={selected.guard}>{toolGuardLabel(selected.guard)}</em><button className={selected.enabled?'enabled':''} role="switch" aria-checked={selected.enabled} onClick={()=>void setEnabled(selected)} disabled={busyName===selected.name} title={selected.enabled?t('tools.disableFunction'):t('tools.enableFunction')}>{busyName===selected.name?<LoaderCircle className="spin" size={14}/>:<Power size={14}/>}<span>{selected.enabled?t('common.enabled'):t('common.disabled')}</span></button></div></header><p className="tool-function-description">{selected.description}</p><dl className="tool-function-meta"><div><dt>{t('tools.category')}</dt><dd>{toolCategoryLabel(selected.category)}</dd></div><div><dt>{t('common.arguments')}</dt><dd>{parameters.length}</dd></div><div><dt>{t('tools.safetyGate')}</dt><dd>{toolGuardLabel(selected.guard)}</dd></div></dl><section className="tool-parameter-list"><h3>{t('tools.inputParameters')} <span>{t('tools.requiredCount',{count:parameters.filter(item=>item.required).length})}</span></h3>{parameters.length?parameters.map(parameter=><div key={parameter.name}><code>{parameter.name}</code><em>{parameter.type}</em>{parameter.required&&<b>{t('common.required')}</b>}{parameter.description&&<p>{parameter.description}</p>}</div>):<p className="tool-no-arguments">{t('tools.noArguments')}</p>}</section><details className="tool-schema-raw"><summary>{t('tools.rawSchema')} <ChevronRight size={13}/></summary><pre>{JSON.stringify(selected.input_schema,null,2)}</pre></details></>:<div className="tool-inspector-empty"><Braces size={26}/></div>}</aside>
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
				<section className="skill-editor panel">{loading?<div className="skill-editor-state"><LoaderCircle className="spin" size={21}/>{t('skills.loading')}</div>:selected?<><header><div><BookOpen size={17}/><span><small>{t('skills.managed')} · {selected.enabled?t('common.enabled'):t('common.disabled')}</small><code>{selected.name}</code></span></div><section><button className={selected.enabled?'skill-disable':'skill-enable'} disabled={toggling} onClick={toggleEnabled}>{toggling?<LoaderCircle className="spin" size={13}/>:selected.enabled?<X size={13}/>:<Check size={13}/>} {selected.enabled?t('common.disable'):t('common.enable')}</button><button disabled={!dirty||saving} onClick={save}>{saving?<LoaderCircle className="spin" size={13}/>:<Save size={13}/>} {saving?t('common.saving'):t('skills.saveChanges')}</button><button className="danger" onClick={()=>setDeleteName(selected.name)}><Trash2 size={13}/>{t('common.delete')}</button></section></header><div className="skill-editor-meta"><span><b>SHA256</b><code title={selected.content_sha256}>{selected.content_sha256?.slice(0,16)||'—'}</code></span><span><b>{t('common.files')}</b><code>{selected.file_count||1}</code></span><span><b>{t('common.size')}</b><code>{formatFileSize(selected.size_bytes||0)}</code></span><span><b>{t('common.updated')}</b><code>{selected.updated_at?new Date(selected.updated_at).toLocaleString(localeFor(instance.language)):'—'}</code></span></div><div className="skill-editor-split"><label><span>SKILL.md</span><textarea value={draft} spellCheck={false} onChange={event=>setDraft(event.target.value)}/></label><section><span>{t('skills.livePreview')}</span><div className="markdown-body"><Markdown skipHtml remarkPlugins={[remarkGfm]} components={{a:({href,children})=><a href={href} target="_blank" rel="noopener noreferrer">{children}</a>,img:({alt})=><span className="markdown-image-blocked">{t('skills.blockedImage',{alt:alt||t('common.image')})}</span>}}>{draft||t('skills.emptySkill')}</Markdown></div></section></div></>:<div className="skill-editor-state"><BookOpen size={25}/><b>{t('skills.select')}</b></div>}</section>
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

function SystemSettingsPage({settings,providers,capabilities,modelStatus,refresh}:{settings:SystemSettings|null;providers:ModelProvider[];capabilities:ToolCapabilities;modelStatus?:Health['model'];refresh:()=>Promise<void>}) {
  const {t}=useTranslation()
  const savedValue=settings?.agent_max_iterations??50
  const savedPrompt=settings?.system_prompt??''
	const defaultPrompt=settings?.default_system_prompt??''
  const savedExplanation=settings?.approval_explanations_enabled??true
	  const savedSubagentProvider=settings?.subagent_model_provider_id??''
	  const savedSubagentTimeout=settings?.subagent_timeout_seconds??30
	  const savedImageTypes=settings?.chat_image_allowed_types??defaultChatImageTypes
  const savedShellMode=settings?.workspace_shell_mode??'sandbox'
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
			<SettingsDisclosure icon={<BrainCircuit size={18}/>} title={t('settings.explanationSection')} meta={<span className={modelStatus?.explanation_agent_available?'ready':'offline'}><CircleDot size={9}/>{modelStatus?.explanation_agent_available?t('settings.runnerReady'):t('settings.modelUnavailable')}</span>}>
				<form onSubmit={submit('explanation')}><div className="subagent-settings"><label className="subagent-toggle"><span><b>{t('settings.commandAgent')}</b></span><input type="checkbox" checked={explanationEnabled} onChange={event=>toggleExplanation(event.target.checked)}/><i/></label><div className="subagent-config-grid"><label><span><b>{t('settings.modelProvider')}</b></span><select value={subagentProvider} onChange={event=>selectSubagentProvider(event.target.value)}><option value="">{t('settings.followMain')}</option>{providers.map(provider=><option value={provider.id} key={provider.id}>{provider.name} · {provider.model}</option>)}</select></label><label><span><b>{t('settings.requestTimeout')}</b></span><div className="subagent-timeout-input"><input aria-label={t('settings.timeout')} type="number" min="5" max="120" step="1" value={subagentTimeout} onChange={event=>updateSubagentTimeout(Number(event.target.value))}/><em>{t('settings.seconds',{count:subagentTimeout})}</em></div></label></div>{modelStatus?.explanation_error&&<div className="subagent-runtime-error"><ShieldAlert size={14}/><span>{modelStatus.explanation_error}</span></div>}</div><SettingsSectionFooter dirty={explanationDirty} busy={busy} saving={savingSection==='explanation'} onDiscard={()=>discard('explanation')}/></form>
			</SettingsDisclosure>
			<SettingsDisclosure icon={<ImagePlus size={18}/>} title={t('settings.chatImages')} meta={imageTypes.map(value=>value.replace('image/','').toUpperCase()).join(' · ')}>
				<form onSubmit={submit('images')}><div className="chat-image-formats">{[['image/png','PNG'],['image/jpeg','JPEG'],['image/webp','WebP'],['image/gif','GIF']].map(([value,label])=><label className={imageTypes.includes(value)?'active':''} key={value}><input type="checkbox" checked={imageTypes.includes(value)} disabled={imageTypes.length===1&&imageTypes.includes(value)} onChange={()=>toggleImageType(value)}/><span>{label}</span></label>)}</div><SettingsSectionFooter dirty={imagesDirty} busy={busy} saving={savingSection==='images'} onDiscard={()=>discard('images')}/></form>
			</SettingsDisclosure>
			<SettingsDisclosure icon={<TerminalSquare size={18}/>} title={t('settings.shellBackend')} meta={settings?.workspace_shell_platform||t('settings.detecting')}>
				<form onSubmit={submit('shell')}><div className="workspace-shell-modes" role="group" aria-label={t('settings.shellBackend')}><button type="button" className={shellMode==='sandbox'?'active':''} disabled={!settings?.workspace_sandbox_available} onClick={()=>selectShellMode('sandbox')}><ShieldCheck size={16}/><span><b>{t('settings.sandbox')}</b><small>{settings?.workspace_sandbox_available?t('settings.sandboxAvailable'):t('settings.unavailableHost')}</small></span></button><button type="button" className={`${shellMode==='host'?'active ':''}host`} disabled={!settings?.workspace_host_shell_available} onClick={()=>selectShellMode('host')}><TerminalSquare size={16}/><span><b>{t('settings.hostShell')}</b><small>{settings?.workspace_host_shell_available?`${settings.workspace_shell_name||t('settings.systemShell')} · ${t('settings.fullAuthority')}`:t('settings.noShell')}</small></span></button><button type="button" className={shellMode==='disabled'?'active':''} onClick={()=>selectShellMode('disabled')}><Power size={16}/><span><b>{t('settings.shellDisabled')}</b></span></button></div>{shellMode==='host'&&<div className="workspace-shell-warning"><ShieldAlert size={15}/><b>{t('settings.hostWarning')}</b></div>}{shellMode==='sandbox'&&!settings?.workspace_sandbox_available&&<div className="workspace-shell-warning"><ShieldAlert size={15}/><b>{t('settings.sandboxWarning')}</b></div>}<SettingsSectionFooter dirty={shellDirty} busy={busy} saving={savingSection==='shell'} onDiscard={()=>discard('shell')}/></form>
			</SettingsDisclosure>
		</div>
	<WorkspaceSettingsPanel workspaces={capabilities.workspaces} refresh={refresh} onNotice={setNotice}/>
	<WebSearchSettingsPanel refresh={refresh}/>
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

const defaultWebSearchInput:WebSearchSettingsInput={enabled:false,base_url:'https://api.tavily.com',api_key:'',proxy_url:'',proxy_username:'',proxy_password:'',timeout_seconds:20,max_results:10}

function WebSearchSettingsPanel({refresh}:{refresh:()=>Promise<void>}){
	const {t}=useTranslation()
	const [stored,setStored]=useState<WebSearchSettings|null>(null),[input,setInput]=useState<WebSearchSettingsInput>(defaultWebSearchInput)
	const [loading,setLoading]=useState(true),[busy,setBusy]=useState(''),[dirty,setDirty]=useState(false),[notice,setNotice]=useState('')
	const hasEffectiveAPIKey=!!input.api_key?.trim()||!!stored?.has_api_key&&!input.clear_api_key
	const applyStored=(value:WebSearchSettings)=>{setStored(value);setInput({enabled:value.enabled,base_url:value.base_url,api_key:'',proxy_url:value.proxy_url||'',proxy_username:value.proxy_username||'',proxy_password:'',timeout_seconds:value.timeout_seconds,max_results:value.max_results});setDirty(false)}
	useEffect(()=>{let active=true;api.webSearchSettings().then(value=>{if(active)applyStored(value)}).catch(err=>{if(active)setNotice(errorText(err))}).finally(()=>{if(active)setLoading(false)});return()=>{active=false}},[])
	const update=<K extends keyof WebSearchSettingsInput>(key:K,value:WebSearchSettingsInput[K])=>{setInput(current=>({...current,[key]:value}));setDirty(true);setNotice('')}
	const save=async()=>{setBusy('save');setNotice('');try{const value=await api.saveWebSearchSettings(input);applyStored(value);setNotice(t('webSearch.saved'));await refresh()}catch(err){setNotice(errorText(err))}finally{setBusy('')}}
	const test=async()=>{setBusy('test');setNotice('');try{const result=await api.testWebSearch();setNotice(t('webSearch.testPassed',{count:result.results.length}))}catch(err){setNotice(errorText(err))}finally{setBusy('')}}
	const clearKey=()=>{setInput(current=>({...current,enabled:false,api_key:'',clear_api_key:true}));setDirty(true);setNotice('')}
	const clearProxyPassword=()=>{setInput(current=>({...current,proxy_password:'',clear_proxy_password:true}));setDirty(true);setNotice('')}
	if(loading)return <SettingsDisclosure className="web-search-settings" icon={<Search size={18}/>} title={t('webSearch.title')} meta={t('common.loading')}><div className="settings-loading"><LoaderCircle className="spin" size={16}/>{t('common.loading')}</div></SettingsDisclosure>
	return <SettingsDisclosure className="web-search-settings" icon={<Search size={18}/>} title={t('webSearch.title')} meta={input.enabled?t('common.enabled'):t('common.disabled')}><label className="web-search-toggle"><span>{t('webSearch.title')}</span><input type="checkbox" checked={input.enabled} onChange={event=>update('enabled',event.target.checked)}/><i/><b>{input.enabled?t('common.enabled'):t('common.disabled')}</b></label><div className="web-search-grid"><label><span>{t('webSearch.baseURL')}</span><input value={input.base_url} onChange={event=>update('base_url',event.target.value)} placeholder="https://api.tavily.com"/></label><label><span>{t('webSearch.apiKey')}</span><PasswordInput value={input.api_key||''} onChange={event=>update('api_key',event.target.value)} placeholder={stored?.has_api_key?t('webSearch.savedSecret'):''}/></label><label><span>{t('webSearch.proxyURL')}</span><input value={input.proxy_url||''} onChange={event=>update('proxy_url',event.target.value)}/></label><label><span>{t('webSearch.proxyUsername')}</span><input value={input.proxy_username||''} onChange={event=>update('proxy_username',event.target.value)}/></label><label><span>{t('webSearch.proxyPassword')}</span><PasswordInput value={input.proxy_password||''} onChange={event=>update('proxy_password',event.target.value)} placeholder={stored?.has_proxy_password?t('webSearch.savedSecret'):''}/></label><label><span>{t('webSearch.timeout')}</span><input type="number" min="5" max="120" value={input.timeout_seconds} onChange={event=>update('timeout_seconds',Number(event.target.value))}/></label><label><span>{t('webSearch.maxResults')}</span><input type="number" min="1" max="20" value={input.max_results} onChange={event=>update('max_results',Number(event.target.value))}/></label></div>{notice&&<p>{notice}</p>}<footer><div>{stored?.has_api_key&&<button type="button" className="danger" onClick={clearKey}>{t('webSearch.clearKey')}</button>}{stored?.has_proxy_password&&<button type="button" className="danger" onClick={clearProxyPassword}>{t('webSearch.clearProxyPassword')}</button>}</div><button type="button" disabled={busy!==''||dirty||!stored?.enabled||!stored.has_api_key} onClick={()=>void test()}>{busy==='test'?<LoaderCircle className="spin" size={13}/>:<Search size={13}/>} {t('common.test')}</button><button type="button" className="primary" disabled={busy!==''||!dirty||input.enabled&&!hasEffectiveAPIKey} onClick={()=>void save()}>{busy==='save'?<LoaderCircle className="spin" size={13}/>:<Save size={13}/>} {t('common.save')}</button></footer></SettingsDisclosure>
}

function AdminPasswordPanel(){
		const {t}=useTranslation()
	const [current,setCurrent]=useState(''),[replacement,setReplacement]=useState(''),[confirmation,setConfirmation]=useState(''),[notice,setNotice]=useState(''),[busy,setBusy]=useState(false)
		const submit=async(event:FormEvent)=>{event.preventDefault();if(replacement!==confirmation){setNotice(t('password.mismatch'));return}setBusy(true);setNotice('');try{await api.changePassword(current,replacement);window.location.reload()}catch(err){setNotice(errorText(err))}finally{setBusy(false)}}
		return <form className="admin-password-form" onSubmit={submit}><SettingsDisclosure className="admin-password-panel" icon={<KeyRound size={18}/>} title={t('password.title')}><section><label><span>{t('password.current')}</span><PasswordInput autoComplete="current-password" value={current} onChange={event=>setCurrent(event.target.value)} required/></label><label><span>{t('password.replacement')}</span><PasswordInput autoComplete="new-password" minLength={12} placeholder={t('password.minimum')} value={replacement} onChange={event=>setReplacement(event.target.value)} required/></label><label><span>{t('password.confirmation')}</span><PasswordInput autoComplete="new-password" minLength={12} value={confirmation} onChange={event=>setConfirmation(event.target.value)} required/></label><button className="primary" disabled={busy||replacement.length<12}>{busy?t('password.changing'):t('password.change')}</button></section>{notice&&<p>{notice}</p>}</SettingsDisclosure></form>
}

function Nav({ active, icon, label, count, warn, onClick }: {active:boolean;icon:React.ReactNode;label:string;count?:number;warn?:boolean;onClick:()=>void}) {
  return <button className={`nav-item ${active ? 'active' : ''}`} onClick={onClick}>{icon}<span>{label}</span>{count !== undefined && <em className={warn ? 'warn' : ''}>{count}</em>}</button>
}

function ChatPage({ hosts, approvals, runs, capabilities, imageTypes, agentAvailable, modelName, refresh, refreshApprovals, onStreamingChange }: {hosts:Host[];approvals:Approval[];runs:Run[];capabilities:ToolCapabilities;imageTypes:string[];agentAvailable:boolean;modelName?:string;refresh:()=>Promise<void>;refreshApprovals:()=>Promise<void>;onStreamingChange:(streaming:boolean)=>void}) {
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
  const [running, setRunning] = useState(false)
  const [detachedRunning,setDetachedRunning]=useState(false)
	const [stopping,setStopping]=useState(false)
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
  const hostNames = useMemo(() => hosts.map((host) => host.name).join(', '), [hosts])
  const currentApprovals=useMemo(()=>sessionId?approvals.filter(item=>item.session_id===sessionId):[],[approvals,sessionId])
	const pendingExplanationID=currentApprovals.find(item=>item.ai_review?.status==='pending')?.id||''
	const sessionBusy=running||detachedRunning
	const selectedWorkspace=capabilities.workspaces.find(workspace=>workspace.id===workspaceID)||capabilities.workspaces[0]
	useEffect(()=>{if(!selectedWorkspace)return;if(workspaceID!==selectedWorkspace.id)setWorkspaceID(selectedWorkspace.id);rememberWorkspace(selectedWorkspace.id)},[selectedWorkspace,workspaceID])
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
  }, [])

  const loadSession = useCallback(async (id: string) => {
    const requestID=clientId()
    sessionLoadRef.current=requestID
    setLoadingSession(id)
    stickToLatest.current=true
    try {
      const state = await api.chatState(id)
      if(sessionLoadRef.current!==requestID)return
	      setEntries(historyEntries(state.messages||[],id));setDetachedRunning(!!state.active);setStopping(false);setPlan(state.plan||null);setWorkspaceID(state.workspace_id||'');setBoundWorkspaceID(state.workspace_id||'')
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
	    stickToLatest.current=true;setSessionId('');setBoundWorkspaceID('');setEntries([]); setMessage('');clearPendingImages(); setHistoryError(''); setReasoningSeen(false);setDetachedRunning(false);setStopping(false);setPlan(null); rememberSession(newSessionMarker)
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
    setApprovalNotice('');setReasoningSeen(false);setStopping(false);setRunning(true)
	    const entryImages=queryImages.map(image=>({id:image.id,name:image.file.name,mimeType:image.file.type,sizeBytes:image.file.size,url:image.url}))
	    setEntries((old) => [...old, { id: userEntryID, kind: 'user', content: query, images:entryImages, status:'pending' }, { id: 'streaming', kind: 'assistant', content: '', streaming:true }])
	    try {
	      await streamChat(sessionId, selectedWorkspace?.id||'', query, queryImages.map(image=>image.file), (frame: AgentEvent) => {
        if(!isAttached())return
	        if (frame.session_id) { querySessionID=frame.session_id;activeStreamRef.current!.sessionId=frame.session_id;setSessionId(frame.session_id);setBoundWorkspaceID(selectedWorkspace?.id||'');rememberSession(frame.session_id) }
        if (frame.type === 'approval') { setEntries(old=>old.map(item=>item.id===userEntryID?{...item,status:'completed'}:item));setApprovalNotice('');void refreshApprovals() }
        if (frame.type === 'reasoning' && frame.content) {
          setReasoningSeen(true)
          const reasoningID=`reasoning_${frame.segment_id||'current'}`
          setEntries((old) => {
            const existing=old.find((item)=>item.id===reasoningID)
            if(existing)return old.map((item)=>item.id===reasoningID?{...item,content:item.content+frame.content,active:true}:item)
            return [...old.filter((item)=>item.id!=='streaming').map(deactivateReasoning),{id:reasoningID,kind:'reasoning',content:frame.content!,active:true},{id:'streaming',kind:'assistant',content:'',streaming:true}]
          })
        }
        if (frame.type === 'message' && frame.content) {
          if (frame.role === 'tool') {setEntries((old) => [...old.filter((item) => item.id !== 'streaming').map(deactivateReasoning), { id: clientId(), kind: 'tool', content: frame.content!, tool: frame.tool_name }, { id: 'streaming', kind: 'assistant', content: '', streaming:true }]);if(frame.tool_name?.startsWith('ops_plan_')){const nextPlan=planFromToolContent(frame.content);if(nextPlan)setPlan(nextPlan)}if(/approval_id|approval_required/.test(frame.content))void refresh()}
          else setEntries((old) => old.map((item) => item.id === 'streaming' ? {...item, content: item.content + frame.content} : deactivateReasoning(item)))
        }
        if (frame.type === 'done') setEntries(old=>old.map(item=>item.id===userEntryID?{...item,status:'completed'}:item.id==='streaming'?{...item,streaming:false}:item))
			if (frame.type === 'interrupted') {setStopping(false);setDetachedRunning(false);setEntries(old=>[...old.filter(item=>item.id!=='streaming').map(item=>item.id===userEntryID?{...item,status:'failed' as const}:deactivateReasoning(item)),{id:clientId(),kind:'assistant',content:frame.content||t('chat.stopped')}])}
			if (frame.type === 'error') setEntries((old) => [...old.map(item=>item.id===userEntryID?{...item,status:'failed' as const}:item.id==='streaming'?{...item,streaming:false}:item), { id: clientId(), kind: 'error', content: frame.error || t('chat.agentError') }])
      },controller.signal)
    } catch (err) { if(isAttached())setEntries((old) => [...old.map(item=>item.id===userEntryID?{...item,status:'failed' as const}:item), { id: clientId(), kind: 'error', content: errorText(err) }]) }
    finally {
      if(!isAttached())return
      setEntries((old) => old.filter((item) => item.id !== 'streaming' || item.content !== '').map((item)=>item.id==='streaming'?{...item,streaming:false}:deactivateReasoning(item)))
      setRunning(false)
		setStopping(false)
	      if(querySessionID){try{const state=await api.chatState(querySessionID);if(!isAttached())return;setDetachedRunning(!!state.active);setPlan(state.plan||null);setBoundWorkspaceID(state.workspace_id||'');setEntries(old=>[...historyEntries(state.messages||[],querySessionID),...old.filter(item=>item.kind==='error'&&!item.id.startsWith('history_'))]);for(const image of queryImages){URL.revokeObjectURL(image.url);imageURLsRef.current.delete(image.url)}}catch{/* polling or the next reload will recover state */}}
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

  return <div className="chat-layout">
    <ChatWorkspacePanel key={selectedWorkspace?.id||''} workspaces={capabilities.workspaces} workspaceID={selectedWorkspace?.id||''} switching={workspaceSwitching} disabled={sessionBusy||!!loadingSession} bound={!!selectedWorkspace&&boundWorkspaceID===selectedWorkspace.id} onSelect={id=>void switchWorkspace(id)}/>
    <div className="chat-main panel">
	  <div className="panel-header"><div><Bot size={18}/><span>{t('chat.session')}</span></div><div className="chat-header-actions"><span className="session-id">{sessionId ? sessionId.slice(0, 20) : t('chat.newSession')}</span><button className="mobile-history-button" onClick={()=>setHistoryOpen(true)} title={t('chat.conversations')} aria-label={t('chat.openConversations')}><History size={15}/>{activeSessionCount>0&&<em>{activeSessionCount}</em>}</button></div></div>
      <div className="session-approval-slot">{currentApprovals.length>0&&<ApprovalDialog key={currentApprovals[0].id} approval={currentApprovals[0]} pendingCount={currentApprovals.length} hosts={hosts} running={sessionBusy} stopping={stopping} onStop={()=>void stopAgent()} refresh={refresh} onNotice={setApprovalNotice}/>} {approvalNotice&&currentApprovals.length===0&&<div className="approval-toast"><ShieldCheck size={14}/><span>{approvalNotice}</span><button onClick={()=>setApprovalNotice('')}><X size={13}/></button></div>}</div>
      <div className="session-plan-slot">{plan&&<SessionPlan plan={plan}/>}</div>
      <div className="messages" ref={messagesRef} onScroll={event=>{const element=event.currentTarget;stickToLatest.current=element.scrollHeight-element.scrollTop-element.clientHeight<90}}>
		{entries.length === 0 && <div className="empty-chat"><div className="radar"><Activity size={35}/></div><h2>{t('chat.emptyTitle')}</h2></div>}
        {entries.map((entry) => <ChatBubble key={entry.id} entry={entry} runs={runs} hosts={hosts}/>) }
		{running && !reasoningSeen && !streamingResponseStarted && <div className="thinking"><span/><span/><span/> {t('chat.waitingModel')}</div>}
		{detachedRunning&&!running&&<div className="thinking background-agent"><span/><span/><span/> {t('chat.backgroundRunning')}</div>}
      </div>
		  <form className="composer" onSubmit={submit}>
			  {sessionBusy&&<div className="llm-work-status" role="status" aria-live="polite"><LoaderCircle className="spin" size={13}/><b>{stopping?t('chat.stopping'):t('chat.running')}</b><button type="button" className="agent-stop-button" onClick={()=>void stopAgent()} disabled={stopping||!(activeStreamRef.current?.sessionId||sessionId)} title={t('chat.stopTitle')}><Square size={11} fill="currentColor"/>{t('chat.stop')}</button></div>}
			  <div className="context-line"><span><Server size={13}/>{hosts.length?t('chat.hostsCount',{count:hosts.length,names:hostNames}):t('chat.noHosts')}</span><span className="composer-workspace"><FolderOpen size={13}/>{selectedWorkspace?t('chat.workspaceSelected',{id:selectedWorkspace.id}):t('chat.noWorkspace')}</span>{selectedWorkspace?.access==='read_write'&&<QuickWorkspaceUpload workspace={selectedWorkspace}/>}<span><Cpu size={13}/>{modelName || t('chat.noModel')}</span></div>
			  {pendingImages.length>0&&<div className="composer-images">{pendingImages.map(image=><div key={image.id}><img src={image.url} alt={image.file.name}/><span title={image.file.name}>{image.file.name}</span><button type="button" onClick={()=>removePendingImage(image.id)} title={t('chat.removeImage')}><X size={11}/></button></div>)}</div>}
			  {imageNotice&&<div className="composer-image-notice">{imageNotice}<button type="button" onClick={()=>setImageNotice('')}><X size={11}/></button></div>}
			  <div className="input-row"><label className="image-attach-button" title={t('chat.addImages')}><ImagePlus size={18}/><input key={imageInputKey} type="file" accept={imageTypes.join(',')} multiple disabled={!agentAvailable||sessionBusy||workspaceSwitching||!!loadingSession} onChange={event=>addImages(Array.from(event.target.files||[]))}/></label><textarea value={message} onChange={(event) => setMessage(event.target.value)} onPaste={event=>{const files=Array.from(event.clipboardData.files).filter(file=>file.type.startsWith('image/'));if(files.length)addImages(files)}} placeholder={!agentAvailable?t('chat.configureModel'):loadingSession?t('chat.loadingConversation'):sessionBusy?t('chat.busyPlaceholder'):t('chat.prompt')} disabled={!agentAvailable||sessionBusy||workspaceSwitching||!!loadingSession} onKeyDown={(event) => { if (event.key === 'Enter' && !event.shiftKey) { event.preventDefault(); event.currentTarget.form?.requestSubmit() } }}/><button aria-label={t('common.next')} disabled={!agentAvailable || sessionBusy || workspaceSwitching || !!loadingSession || (!message.trim()&&!pendingImages.length)}><Send size={18}/></button></div>
		  </form>
    </div>
	{historyOpen&&<button className="conversation-backdrop" onClick={()=>setHistoryOpen(false)} aria-label={t('chat.closeConversations')}/>}
	<aside className={`context-panel conversation-panel panel ${historyOpen?'mobile-open':''}`}><div className="panel-header"><div><History size={17}/><span>{t('chat.conversations')}</span></div><section className="conversation-header-actions"><button className="new-chat-button" onClick={newChat} disabled={workspaceSwitching} title={t('chat.newConversation')}><Plus size={14}/>{t('common.new')}</button><button className="conversation-close-button" onClick={()=>setHistoryOpen(false)} title={t('chat.closeConversations')} aria-label={t('chat.closeConversations')}><X size={14}/></button></section></div><div className="session-list">
      {historyError&&<div className="history-error">{historyError}</div>}
	  {!sessions.length&&!historyError&&<div className="history-empty">{t('chat.noSaved')}</div>}
	  {sessions.map(session=>{const pending=approvals.filter(item=>item.session_id===session.id).length;const active=session.active||(session.id===sessionId&&sessionBusy);return <div className={`session-item ${session.id===sessionId?'active':''}`} key={session.id}><button className="session-open" onClick={()=>switchSession(session.id)} disabled={workspaceSwitching||loadingSession===session.id}><b>{session.title}{pending>0&&<em className="session-approval-count">{t('chat.approvalCount',{count:pending})}</em>}{active&&<em className="session-running-count">{t('chat.runningBadge')}</em>}</b><span>{new Date(session.updated_at).toLocaleString(localeFor(instance.language))} · {t('chat.messageCount',{count:session.message_count})} · {session.workspace_id||t('chat.noWorkspace')}</span></button><button className="session-delete" onClick={()=>{if(!active)setSessionDeleteCandidate(session)}} disabled={active||workspaceSwitching} title={active?t('chat.cannotDelete'):t('chat.deleteConversation')}><Trash2 size={13}/></button></div>})}
	</div><div className="session-summary"><Metric label={t('chat.saved')} value={sessions.length.toString()} tone="green"/><Metric label={t('chat.hosts')} value={hosts.length.toString()}/></div></aside>
	{sessionDeleteCandidate&&<DestructiveConfirmDialog label={t('chat.deleteDialogLabel')} title={t('chat.deleteTitle',{title:sessionDeleteCandidate.title})} description={t('chat.deleteText')} busy={deletingSession} onCancel={()=>setSessionDeleteCandidate(null)} onConfirm={()=>void removeSession()}/>}
  </div>
}

function formatFileSize(size:number){if(size<1024)return `${size} B`;if(size<1024*1024)return `${(size/1024).toFixed(1)} KiB`;return `${(size/1024/1024).toFixed(1)} MiB`}

type WorkspaceNotice={kind:'success'|'error';text:string}
type WorkspaceDeleteCandidate={workspaceID:string;path:string;type:'file'|'directory'}

function workspaceChildPath(path:string,name:string){return path==='.'?name:`${path}/${name}`}

function ChatWorkspacePanel({workspaces,workspaceID,switching,disabled,bound,onSelect}:{workspaces:WorkspaceCapability[];workspaceID:string;switching:boolean;disabled:boolean;bound:boolean;onSelect:(id:string)=>void}){
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
	const loadRequestRef=useRef(0),previewPathRef=useRef('')

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
	const requestEntryRemoval=(name:string,type:'file'|'directory')=>{
		if(workspace)setDeleteCandidate({workspaceID:workspace.id,path:workspaceChildPath(path,name),type})
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
	const up=()=>{if(path==='.')return;const parts=path.split('/');parts.pop();setPath(parts.join('/')||'.')}

	if(!workspace)return <aside className="workspace-browser-panel panel empty"><div className="panel-header"><div><FolderOpen size={17}/><span>{t('common.workspace')}</span></div></div><div className="workspace-empty"><FolderOpen size={23}/><span>{t('workspace.noConfigured')}</span></div></aside>
	return <>
		<aside className={`workspace-browser-panel panel ${dragging?'dragging':''}`} onDragEnter={dragEnter} onDragOver={dragOver} onDragLeave={dragLeave} onDrop={drop}>
			<div className="panel-header"><div><FolderOpen size={17}/><span>{t('common.workspace')}</span></div><select value={workspace.id} disabled={workspaces.length<2||disabled||switching} onChange={event=>onSelect(event.target.value)} aria-label={t('workspace.switchWorkspace')}>{workspaces.map(item=><option value={item.id} key={item.id}>{item.id}</option>)}</select></div>
			<div className="chat-workspace-head"><span><b>{workspace.id}</b>{(switching||bound)&&<small>{switching?t('workspace.switching'):t('workspace.boundToConversation')}</small>}</span><em className={workspace.access}>{workspace.access==='read_write'?t('workspace.readWrite'):t('workspace.readOnly')}</em></div>
			<div className="workspace-path-row"><button onClick={up} disabled={path==='.'} title={t('workspace.parent')}>‹</button><code title={path}>{path}</code>{workspace.access==='read_write'&&<label title={t('workspace.uploadFile')}><UploadCloud size={14}/><input key={inputKey} type="file" onChange={choose}/></label>}<button onClick={()=>synchronize(true)} title={t('workspace.refreshFiles')}><RefreshCw size={12}/></button></div>
			{file&&<div className="chat-upload-row"><input value={target} onChange={event=>setTarget(event.target.value)} aria-label={t('workspace.relativePath')}/><button onClick={()=>void upload()} disabled={uploading||!target.trim()}>{uploading?'...':t('common.upload')}</button><button onClick={()=>{setFile(null);setTarget('');setInputKey(value=>value+1)}} title={t('workspace.cancelUpload')}><X size={11}/></button></div>}
			<div className="workspace-file-list">{loading?<span className="workspace-files-state"><LoaderCircle className="spin" size={13}/>{t('common.loading')}</span>:error?<span className="workspace-files-state error">{error}</span>:entries.length?entries.map(entry=>{const fullPath=workspaceChildPath(path,entry.name);return <div className="workspace-file-row" key={`${entry.type}:${entry.name}`}><button className="workspace-file-open" onClick={()=>void openEntry(entry.name,entry.type)} title={entry.type==='file'?t('workspace.previewFile'):t('workspace.openDirectory')}>{previewLoading===fullPath?<LoaderCircle className="spin" size={13}/>:entry.type==='directory'?<FolderOpen size={13}/>:<FileText size={13}/>}<span>{entry.name}</span>{entry.type==='file'&&<small>{formatFileSize(entry.size??0)}</small>}</button>{workspace.access==='read_write'&&<button className="workspace-file-delete" onClick={()=>requestEntryRemoval(entry.name,entry.type)} disabled={deleting===fullPath} title={t('workspace.deleteEntry',{type:t(`workspace.${entry.type}`)})}><Trash2 size={12}/></button>}</div>}):<span className="workspace-files-state">{t('workspace.emptyDirectory')}</span>}</div>
			{notice&&<div className={`chat-workspace-notice ${notice.kind}`}>{notice.text}</div>}
			{dragging&&<div className="workspace-drop-overlay"><UploadCloud size={27}/><b>{t('workspace.dropFilesHere')}</b><span>{path}</span></div>}
		</aside>
		{preview&&<div className="workspace-preview-backdrop" role="presentation" onMouseDown={event=>{if(event.target===event.currentTarget)setPreview(null)}}><section className="workspace-preview-dialog" role="dialog" aria-modal="true" aria-label={`${t('workspace.previewFile')} ${preview.path}`}><header><div><FileText size={18}/><span><b>{preview.path}</b><small>{formatFileSize(preview.size)} · SHA-256 {preview.sha256}</small></span></div><button onClick={()=>setPreview(null)} title={t('workspace.closePreview')}><X size={16}/></button></header>{preview.binary?<div className="workspace-binary-preview"><FileText size={30}/><b>{t('workspace.binary')}</b></div>:<pre>{preview.content||''}</pre>}</section></div>}
		{deleteCandidate&&<DestructiveConfirmDialog label={t('workspace.permanentDelete')} title={t('workspace.deleteTitle',{path:`${deleteCandidate.workspaceID}:${deleteCandidate.path}`})} description={t('workspace.deleteDescription',{target:deleteCandidate.type==='directory'?t('workspace.deleteFolderTarget'):t('workspace.deleteFileTarget')})} busy={deleting===deleteCandidate.path} onCancel={()=>setDeleteCandidate(null)} onConfirm={()=>void removeEntry()}/>}
	</>
}

function QuickWorkspaceUpload({workspace}:{workspace:WorkspaceCapability}){
	const {t}=useTranslation()
	const [busy,setBusy]=useState(false),[status,setStatus]=useState(''),[inputKey,setInputKey]=useState(0)
	useEffect(()=>{if(status!=='uploaded')return;const timer=window.setTimeout(()=>setStatus(''),3000);return()=>window.clearTimeout(timer)},[status])
	const choose=async(event:React.ChangeEvent<HTMLInputElement>)=>{const file=event.target.files?.[0];if(!file)return;setBusy(true);setStatus('');try{await api.uploadWorkspaceFile(workspace.id,file,file.name);setStatus('uploaded')}catch(err){setStatus(errorText(err))}finally{setBusy(false);setInputKey(value=>value+1)}}
	return <label className={`quick-workspace-upload ${busy?'busy':''} ${status&&status!=='uploaded'?'error':''}`} title={status&&status!=='uploaded'?status:t('workspace.uploadTo',{id:workspace.id})}><UploadCloud size={12}/><b>{busy?t('common.uploading'):status==='uploaded'?t('workspace.uploadedShort'):status?t('workspace.uploadFailed'):t('workspace.uploadFile')}</b><input key={inputKey} type="file" disabled={busy} onChange={event=>void choose(event)}/></label>
}

function SessionPlan({plan}:{plan:AgentPlan}){
	const {t}=useTranslation()
  const [expanded,setExpanded]=useState(plan.status==='active')
  useEffect(()=>{if(plan.status==='active')setExpanded(true)},[plan.session_id,plan.status])
  const completed=plan.steps.filter(step=>step.status==='completed').length
  const current=plan.steps.find(step=>step.status==='in_progress'||step.status==='blocked')
  const progress=plan.steps.length?Math.round(completed/plan.steps.length*100):0
	return <details className={`session-plan ${plan.status}`} open={expanded} onToggle={event=>setExpanded(event.currentTarget.open)}><summary><span className="plan-icon"><ListChecks size={16}/></span><span className="plan-summary-copy"><b>{plan.goal}</b><small>{current?t(current.status==='blocked'?'plan.blockedAt':'plan.current',{current:current.number,total:plan.steps.length,title:current.title}):t('plan.completed',{completed,total:plan.steps.length})}</small></span><span className="plan-progress"><i><em style={{width:`${progress}%`}}/></i><b>{progress}%</b></span><span className={`plan-state ${plan.status}`}>{t(`statusLabels.${plan.status}`,{defaultValue:plan.status})}</span><ChevronRight size={14}/></summary><ol>{plan.steps.map(step=><li className={step.status} key={step.number}><span className="plan-step-marker">{step.status==='completed'?<Check size={12}/>:step.status==='in_progress'?<LoaderCircle size={12}/>:step.status==='blocked'?<ShieldAlert size={12}/>:step.number}</span><div><b>{step.title}</b>{step.evidence&&<p>{step.evidence}</p>}</div><em>{t(`statusLabels.${step.status}`,{defaultValue:step.status.replace('_',' ')})}</em></li>)}</ol></details>
}

const ChatBubble=memo(function ChatBubble({ entry, runs, hosts }: {entry: ChatEntry;runs:Run[];hosts:Host[]}) {
	const {t}=useTranslation()
  if (entry.kind === 'tool') return <ToolEventCard entry={entry} runs={runs} hosts={hosts}/>
  if (entry.kind === 'reasoning') return <ReasoningCard content={entry.content} active={!!entry.active}/>
  if (entry.kind === 'assistant' && !entry.content) return null
	return <div className={`bubble ${entry.kind} ${entry.status||''}`}><div className="avatar">{entry.kind === 'user' ? 'YOU' : entry.kind === 'error' ? '!' : <Bot size={17}/>}</div><div><span className="bubble-label">{entry.kind === 'user' ? <>{t('chat.operator')}{entry.status==='failed'&&<em>{t('chat.failedContext')}</em>}{entry.status==='pending'&&<em>{t('chat.processing')}</em>}</> : entry.kind === 'error' ? t('common.error') : 'OpsPilot'}</span>{entry.images&&entry.images.length>0&&<div className="message-images">{entry.images.map(image=><a href={image.url} target="_blank" rel="noopener noreferrer" title={`${image.name} · ${formatFileSize(image.sizeBytes)}`} key={image.id}><img src={image.url} alt={image.name}/><span>{image.name}</span></a>)}</div>}{entry.content&&<div className={`bubble-copy ${entry.kind==='assistant'&&!entry.streaming?'markdown-body':''}`}>{entry.kind==='assistant'&&!entry.streaming?<Markdown skipHtml remarkPlugins={[remarkGfm]} components={{a:({href,children})=><a href={href} target="_blank" rel="noopener noreferrer">{children}</a>,img:({alt})=><span className="markdown-image-blocked">{t('chat.blockedImage',{alt:alt||t('common.image')})}</span>}}>{entry.content}</Markdown>:entry.content}</div>}</div></div>
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
	return <section className="diff-viewer"><header><span><FileText size={14}/>{t('tool.fileEdit')}</span><div><em className="add">+{numberValue(change.additions)}</em><em className="delete">-{numberValue(change.deletions)}</em></div></header><div className="diff-scroll" role="table" aria-label={t('tool.diff')}><div className="diff-lines">{rows.map((row,index)=><div className={`diff-line ${row.kind}`} role="row" key={index}><span className="old-line">{row.oldLine??''}</span><span className="new-line">{row.newLine??''}</span><code>{row.text||' '}</code></div>)}</div></div></section>
}

function ToolEventCard({entry,runs,hosts}:{entry:ChatEntry;runs:Run[];hosts:Host[]}){
	const {t}=useTranslation()
  const payload=parseRecord(entry.content)
	const taskPayload=jsonRecord(payload.task)
	const resultPayload=jsonRecord(payload.result)
  const runID=textValue(payload.run_id)||textValue(taskPayload?.run_id)||textValue(resultPayload?.run_id)
  const run=runs.find(item=>item.id===runID)
  const display=jsonRecord(payload._display)
	const toolArguments=jsonRecord(display?.arguments)
	const request=jsonRecord(display?.request)||requestFromRun(run)
	const destinationHostID=textValue(display?.host_id)||run?.host_id||textValue(request?.host_id)||textValue(toolArguments?.host_id)||textValue(toolArguments?.destination_host_id)
	const destinationHost=hostIdentity(hosts,destinationHostID)
  const hostID=destinationHost.id
  const hostName=destinationHost.name||hostID||'—'
  const status=textValue(payload.status)||textValue(taskPayload?.status)||textValue(resultPayload?.status)||run?.status||'completed'
  const risk=textValue(display?.risk)||textValue(resultPayload?.risk)||run?.risk||''
	const program=request?fullProgram(request):''
	const script=request?textValue(request.script):''
	const change=jsonRecord(request?.change)||jsonRecord(payload.change)||jsonRecord(resultPayload?.change)
  const remotePath=request?textValue(request.remote_path):''
	const workspaceID=textValue(display?.workspace_id)||(request?textValue(request.workspace_id):'')
	const relativePath=request?textValue(request.relative_path):''
	const requestMode=request?textValue(request.mode):''
	const unifiedFileRead=entry.tool==='ssh_file_read'||entry.tool==='workspace_file_read'
	const fileSearchMode=unifiedFileRead&&(requestMode==='remote_search'||requestMode==='workspace_search')
	const fileReadMode=unifiedFileRead&&(requestMode==='remote_read'||requestMode==='workspace_read')
	const structuredFileOperation=fileReadMode||fileSearchMode
	const searchPattern=request?textValue(request.search_pattern):''
	const searchResult=jsonRecord(payload.search)||jsonRecord(resultPayload?.search)
	const searchMatchMode=(request?textValue(request.search_match_mode):'')||textValue(searchResult?.match_mode)
	const searchMatchModeLabel=searchMatchMode==='literal'?t('tool.matchModeLiteral'):searchMatchMode==='regex'?t('tool.matchModeRegex'):searchMatchMode||'—'
	const searchFound=searchResult?.found===true
	const workspaceShellBackend=request?textValue(request.workspace_shell_backend):''
	const workspaceTransfer=requestMode==='workspace_upload'||entry.tool==='workspace_file_upload'
	const sshTransfer=requestMode==='ssh_file_transfer'||entry.tool==='ssh_file_transfer'
	const workspaceTool=!!entry.tool?.startsWith('workspace_')
	const sourceHostID=(request?textValue(request.source_host_id):'')||textValue(toolArguments?.source_host_id)
	const sourcePath=(request?textValue(request.source_path):'')||textValue(toolArguments?.source_path)
	const sourceHost=hostIdentity(hosts,sourceHostID)
	const sourceHostName=sourceHost.name||sourceHost.id
	const file=jsonRecord(payload.file)||jsonRecord(resultPayload?.file)
	const filePath=textValue(file?.path)||remotePath||relativePath
	const fileTarget=`${workspaceID?`${workspaceID}:`:''}${filePath}`
	const eventToolLabel=structuredFileOperation?t(fileSearchMode?(workspaceID?'toolNames.workspace_file_search_mode':'toolNames.ssh_file_search_mode'):(workspaceID?'toolNames.workspace_file_read':'toolNames.ssh_file_read')):toolLabel(entry.tool||'')
	const fileOperationParameters:Array<Array<unknown>>=structuredFileOperation&&request?[
		...(workspaceID?[["workspace_id",workspaceID]]:[["host_id",hostID]]),
		["path",filePath],
		...(fileSearchMode?[["match_mode",searchMatchModeLabel],["pattern",searchPattern],["context_lines",numberValue(request.context_lines)]]:[
			...(workspaceID?[]:[["metadata_only",request.metadata_only===true]]),
			["max_bytes",numberValue(request.max_bytes)],
			["offset_bytes",numberValue(request.offset_bytes)],
			...(workspaceID?[]:[["tail_lines",numberValue(request.tail_lines)]])
		]),
		...(workspaceID?[]:[["elevated",request.elevated===true]])
	]:[]
	const transferSummary=workspaceTransfer?`${workspaceID}:${relativePath} → ${remotePath}`:sshTransfer?`${sourceHostName}:${sourcePath} → ${hostName}:${remotePath}`:''
  const planSteps=Array.isArray(payload.steps)?payload.steps.map(jsonRecord).filter((step):step is JsonRecord=>!!step):[]
  const planSummary=textValue(payload.goal)||textValue(planSteps.find(step=>textValue(step.status)==='in_progress'||textValue(step.status)==='blocked')?.title)
	const operation=filePath||(script?t('tool.bashScript'):program||eventToolLabel||t('tool.result'))
  const args=request&&Array.isArray(request.args)?request.args.map(value=>String(value)):[]
  const env=request?jsonRecord(request.env):undefined
	const rawStdout=textValue(payload.stdout)||textValue(resultPayload?.stdout)||run?.stdout_redacted||''
	const stdout=change?cleanFileChangeOutput(rawStdout):rawStdout
  const stderr=textValue(payload.stderr)||textValue(resultPayload?.stderr)||run?.stderr_redacted||run?.error||''
  const stdoutPreview=latestOutput(stdout)
	const commandSummary=transferSummary||(fileSearchMode?`${fileTarget} · ${searchMatchModeLabel} pattern=${JSON.stringify(searchPattern)}`:filePath)||program||(script?compactScript(script):'')||planSummary||operation
	const historyRuns=[...recordArray(payload.runs),...recordArray(resultPayload?.runs)]
	const historyHostIDs=[...new Set(historyRuns.map(item=>textValue(item.host_id)).filter(Boolean))]
	const listedHosts=[...recordArray(payload.hosts),...recordArray(resultPayload?.hosts)]
	const targets:ToolTarget[]=[]
	if(sshTransfer){
		if(sourceHost.id)targets.push({kind:'host',label:t('tool.sourceHost'),name:sourceHost.name,id:sourceHost.id})
		if(hostID)targets.push({kind:'host',label:t('tool.targetHost'),name:destinationHost.name,id:hostID})
	}else if(workspaceTransfer){
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
	<summary><div className="tool-summary-icon"><TerminalSquare size={15}/></div><div className="tool-summary-copy"><div className="tool-summary-operation"><b>{eventToolLabel||entry.tool||t('common.functions')}:</b><code title={commandSummary}>{commandSummary}</code></div>{targets.length>0&&<div className="tool-summary-targets">{targets.map((target,index)=><span className={`tool-target-chip ${target.kind}`} title={`${target.label}: ${[target.name,target.id].filter(Boolean).join(' · ')}`} key={`${target.kind}_${target.id||target.name}_${index}`}>{target.kind==='host'?<Server size={11}/>:target.kind==='workspace'?<FolderOpen size={11}/>:<ListChecks size={11}/>}<em>{target.label}</em>{target.name&&<b>{target.name}</b>}{target.id&&<code>{target.id}</code>}</span>)}</div>}</div><span className={`tool-status ${status}`}>{t(`statusLabels.${status}`,{defaultValue:status.replaceAll('_',' ')})}</span><ChevronRight size={14}/>{stdoutPreview&&<div className="tool-summary-preview"><span>{t('tool.latestStdout',{count:Math.min(3,stdoutPreview.split('\n').length)})}</span><pre>{stdoutPreview}</pre></div>}</summary>
    <div className="tool-event-body">
      {request?<div className="tool-execution-layout">
        <section className="tool-command-pane">
		  <div className="tool-command-head"><span>{structuredFileOperation?t(fileSearchMode?'tool.searchOperation':'tool.readOperation'):filePath?t('tool.fileOperation'):script?t('tool.fullScript'):t('tool.fullCommand')}</span>{workspaceShellBackend&&<em><TerminalSquare size={12}/>{workspaceShellBackend==='host'?t('approval.hostShell'):'Bubblewrap'}</em>}{request.elevated===true&&<em><ShieldAlert size={12}/>sudo / root</em>}</div>
			  <div className="tool-command-block">{workspaceTransfer?<pre>workspace_upload {workspaceID}:{relativePath} → {remotePath}</pre>:sshTransfer?<pre>{sourceHostName}:{sourcePath} → {hostName}:{remotePath}</pre>:structuredFileOperation?<pre>{fileSearchMode?'search':'read'} {fileTarget}</pre>:filePath?<pre>{requestMode} {workspaceID?`${workspaceID}:`:''}{filePath}</pre>:script?<pre>{script}</pre>:program?<pre><span className="prompt-sign">$</span> {program}</pre>:<pre>{requestMode} {remotePath}</pre>}</div>
		  {fileOperationParameters.length>0&&<CompactTable title={t('tool.actualParameters')} columns={[t('tool.parameter'),t('tool.value')]} rows={fileOperationParameters}/>}
		  {change&&textValue(change.diff)&&<DiffViewer change={change}/>}
		  {program&&<CompactTable title={t('tool.originalArgs')} columns={[t('tool.index'),t('tool.value')]} rows={[[0,textValue(request.program)],...args.map((arg,index)=>[index+1,JSON.stringify(arg)])]}/>}
		  {env&&Object.keys(env).length>0&&<CompactTable title={t('tool.environment')} columns={[t('tool.key'),t('tool.value')]} rows={Object.entries(env).map(([key,value])=>[key,String(value)])}/>}
        </section>
        <aside className="tool-context-pane">
			  <dl className="tool-context-grid"><div><dt>{workspaceTransfer||sshTransfer?t('tool.targetHost'):workspaceID?t('common.workspace'):t('tool.targetHost')}</dt><dd>{workspaceTransfer||sshTransfer?[destinationHost.name,hostID].filter(Boolean).join(' · '):workspaceID||[destinationHost.name,hostID].filter(Boolean).join(' · ')||'—'}</dd></div><div><dt>{workspaceTransfer||sshTransfer?t('tool.sourceFile'):filePath?t('tool.filePath'):t('tool.workingDirectory')}</dt><dd>{workspaceTransfer?`${workspaceID}:${relativePath}`:sshTransfer?`${[sourceHost.name,sourceHost.id].filter(Boolean).join(' · ')}:${sourcePath}`:filePath||textValue(request.cwd)||t('tool.defaultDirectory')}</dd></div><div><dt>{t('tool.permission')}</dt><dd>{workspaceShellBackend==='host'?t('tool.hostAuthority'):workspaceShellBackend==='sandbox'?t('tool.sandbox'):request.elevated===true?t('tool.managedSudo'):t('tool.normalUser')}</dd></div><div><dt>{t('common.risk')}</dt><dd>{risk?t(`riskLabels.${risk}`,{defaultValue:risk}):'—'}</dd></div><div><dt>{t('common.status')}</dt><dd>{t(`statusLabels.${status}`,{defaultValue:status})}</dd></div><div><dt>{t('tool.exitCode')}</dt><dd>{exitCode}</dd></div><div><dt>{t('tool.duration')}</dt><dd>{formatDuration(payload.duration??resultPayload?.duration,run)}</dd></div><div><dt>{t('tool.runId')}</dt><dd>{runID||'—'}</dd></div></dl>
		  {textValue(request.reason)&&<div className="tool-reason"><span>{t('tool.reason')}</span><p>{textValue(request.reason)}</p></div>}
		  {textValue(request.expected_changes)&&<div className="tool-reason change"><span>{t('tool.expectedChanges')}</span><p>{textValue(request.expected_changes)}</p></div>}
		  {textValue(request.rollback)&&<div className="tool-reason rollback"><span>{t('tool.rollback')}</span><p>{textValue(request.rollback)}</p></div>}
        </aside>
      </div>:<GenericToolResult payload={payload}/>} 
	  {file&&<FileMetadataPanel file={file}/>}
	  {fileSearchMode&&searchResult&&<div className={`file-search-result ${searchFound?'found':'empty'}`}><Search size={15}/><div><b>{t(searchFound?'tool.searchMatched':'tool.searchNoMatches')}</b><span>{searchMatchModeLabel} · {searchPattern}</span></div></div>}
	  {(textValue(payload.message)||textValue(payload.next_action))&&<div className={`tool-guidance ${payload.ok===false?'error':''}`}><ShieldAlert size={15}/><div><b>{textValue(payload.code)||t('tool.result')}</b>{textValue(payload.message)&&<p>{textValue(payload.message)}</p>}{textValue(payload.next_action)&&<small>{t('common.next')} · {textValue(payload.next_action)}</small>}</div></div>}
	  {instruction&&<div className="tool-instruction"><ShieldAlert size={15}/><div><b>{t('tool.operatorInstruction')}</b><p>{instruction}</p></div></div>}
      {(stdout||stderr)&&<div className="tool-output-grid">{stdout&&<div className="tool-output stdout"><span>STDOUT</span><pre>{stdout}</pre></div>}{stderr&&<div className="tool-output stderr"><span>{t('tool.stderrResult')}</span><pre>{stderr}</pre></div>}</div>}
	  <details className="tool-raw"><summary>{t('tool.rawJson')}</summary><pre>{JSON.stringify(rawPayload,null,2)}</pre></details>
    </div>
  </details>
}

function FileMetadataPanel({file}:{file:JsonRecord}){
		const {t}=useTranslation()
	const after=textValue(file.sha256),validator=textValue(file.validator)
		return <section className="file-metadata-panel"><div className="file-metadata-head"><FileText size={16}/><div><b>{t('tool.fileEvidence')}</b><span>{textValue(file.path)}</span></div>{file.validation_ok===true&&<em><Check size={12}/>{t('tool.validated')}</em>}</div><dl><div><dt>{t('tool.bytesRead')}</dt><dd>{typeof file.returned_bytes==='number'?`${file.returned_bytes} B`:'—'}</dd></div><div><dt>{t('tool.mode')}</dt><dd>{textValue(file.mode)||'—'}</dd></div><div><dt>{t('tool.owner')}</dt><dd>{[textValue(file.owner),textValue(file.group)].filter(Boolean).join(':')||'—'}</dd></div><div><dt>{t('tool.validator')}</dt><dd>{validator||'—'}</dd></div></dl>{after&&<div className="hash-row"><span>{t('tool.after')}</span><code>{after}</code></div>}{file.sensitive===true&&<div className="file-sensitive"><ShieldAlert size={13}/>{t('tool.sensitive')}</div>}</section>
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
			if(review.status==='pending')return <div className="command-review-panel pending" role="status" aria-live="polite"><div className="command-review-pending"><span className="review-agent-icon"><LoaderCircle className="spin" size={17}/></span><b>{t('approval.explanationWorking')}</b></div></div>
  const explanation=review.explanation
	  return <details className={`command-review-panel ${review.status}`}><summary><span className="review-agent-icon"><BrainCircuit size={17}/></span><span><b>{t('approval.explanationAgent')}</b><small>{review.status==='completed'?t('approval.explanationCompleted'):review.status==='degraded'?t('approval.explanationPartial'):t('approval.explanationUnavailable')}</small></span><em>{t(`riskLabels.${review.deterministic_risk}`,{defaultValue:review.deterministic_risk.replace('_',' ')})}</em><ChevronRight size={14}/></summary><div className="command-review-body">{explanation&&<section className="review-explanation"><div className="review-section-title"><span>AI</span><div><b>{t('approval.plainExplanation')}</b><small>{explanation.summary}</small></div></div><p>{explanation.mechanism}</p><div className="review-list-grid"><ReviewList title={t('approval.effects')} items={explanation.effects}/><ReviewList title={t('approval.risks')} items={explanation.risks} tone="warn"/><ReviewList title={t('approval.tips')} items={explanation.beginner_tips}/></div>{explanation.rollback_guide&&<div className="review-rollback"><b>{t('approval.rollbackGuide')}</b><p>{explanation.rollback_guide}</p></div>}</section>}{review.errors&&review.errors.length>0&&<div className="review-errors"><b>{t('approval.degradedInfo')}</b>{review.errors.map((item,index)=><code key={index}>{item}</code>)}</div>}<div className="review-meta">{t('common.model')} {review.model||t('common.unavailable')} · {new Date(review.reviewed_at).toLocaleString(localeFor(instance.language))}</div></div></details>
}

function ApprovalDialog({
  approval,
  pendingCount,
  hosts,
  running,
  stopping,
  onStop,
  refresh,
  onNotice,
}: {
  approval: Approval;
  pendingCount: number;
  hosts: Host[];
  running: boolean;
  stopping: boolean;
  onStop: () => void;
  refresh: () => Promise<void>;
  onNotice: (message: string) => void;
}) {
  const { t, i18n: instance } = useTranslation();
  const [note, setNote] = useState("");
  const [decisionBusy, setDecisionBusy] = useState<
    "" | "once" | "session" | "reject"
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
  const hostWorkspaceShell =
    requestMode === "workspace_shell" && workspaceShellBackend === "host";
  const fileReadApproval = [
    "remote_read",
    "remote_search",
    "workspace_read",
    "workspace_search",
  ].includes(requestMode);
  const fileSearchApproval = ["remote_search", "workspace_search"].includes(
    requestMode,
  );
  const directProgram = textValue(request.program).split("/").pop() || "";
  const directFileReadApproval =
    ["cat", "cut", "grep", "head", "less", "more", "tail"].includes(
      directProgram,
    ) ||
    (requestMode === "script" &&
      /(?:^|[\n;&|])\s*(?:\/[\w./-]+\/)?(?:cat|cut|grep|head|less|more|tail)(?:\s|$)/m.test(
        script,
      ));
  const oneTimeFileAccess = fileReadApproval || directFileReadApproval;
  const workspaceTransfer = requestMode === "workspace_upload";
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
    : elevated
    ? filePath
      ? t("approval.sudoFileTitle")
      : t("approval.sudoTitle", { kind: actionKind })
    : sshTransfer
      ? t("approval.transferTitle")
      : workspaceTransfer
        ? t("approval.uploadTitle")
      : hostWorkspaceShell
        ? t("approval.hostShellTitle")
        : filePath
          ? t("approval.fileTitle")
          : t("approval.executeTitle", { kind: actionKind });
  const commandLabel = fileReadApproval
    ? elevated
      ? t(fileSearchApproval ? "approval.rootSearchLabel" : "approval.rootReadLabel")
      : t(fileSearchApproval ? "approval.searchLabel" : "approval.readLabel")
    : sshTransfer
    ? t("approval.transferLabel")
    : workspaceTransfer
      ? t("approval.uploadLabel")
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
  const operation = sshTransfer
    ? `${sourceHost}:${sourcePath} → ${targetHost}:${remotePath}`
    : workspaceTransfer
      ? `${workspaceID}:${relativePath} → ${remotePath}`
    : fullProgram(request) ||
      script ||
      `${requestMode} ${filePath}${fileSearchApproval ? ` · ${searchMatchModeLabel} pattern=${JSON.stringify(textValue(request.search_pattern))}` : ""}`.trim() ||
      t("approval.pendingOperation");
  const targetHostIdentity = [targetHost, target?.id && target.id !== targetHost ? target.id : approval.host_id !== targetHost ? approval.host_id : ''].filter(Boolean).join(' · ')
  const sourceHostIdentity = [sourceHost, source?.id && source.id !== sourceHost ? source.id : sourceHostID !== sourceHost ? sourceHostID : ''].filter(Boolean).join(' · ')
  const hostName = workspaceTransfer || sshTransfer
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
              ...(workspaceID
                ? []
                : [["tail_lines", numberValue(request.tail_lines)]]),
            ]),
        ...(workspaceID ? [] : [["elevated", elevated]]),
      ]
    : [];
  const explanationPending = approval.ai_review?.status === "pending";
  const decide = async (scope: "once" | "session") => {
    setDecisionBusy(scope);
    setError("");
    try {
      const result = await api.approve(
        approval.id,
        note.trim() || "Reviewed and approved in the current Agent session.",
        scope,
      );
      onNotice(
        t("approval.approved", {
          status: t(`statusLabels.${result.status}`, {
            defaultValue: result.status,
          }),
          run: result.run_id,
        }),
      );
      await refresh();
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
      await refresh();
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
        className={`approval-dialog ${approval.risk} ${elevated ? "elevated" : ""}`}
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
          <em className={`risk ${approval.risk}`}>
            {t(`riskLabels.${approval.risk}`, {
              defaultValue: approval.risk.replace("_", " "),
            })}
          </em>
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
                  {workspaceTransfer
                    ? `${workspaceID}:${relativePath} -> ${remotePath}`
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
          {change&&textValue(change.diff)?<DiffViewer change={change}/>:<pre className="approval-command-preview">{script || `$ ${operation}`}</pre>}
          <dl>
            <div>
              <dt>
                {workspaceTransfer || sshTransfer
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
          {hostWorkspaceShell && (
            <div className="approval-host-shell-warning">
              <ShieldAlert size={14} />
              <span>{t("approval.hostShellWarning")}</span>
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
              ? t("approval.explanationWorking")
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
          <pre>{JSON.stringify(request, null, 2)}</pre>
        </details>
        <div className="approval-choice-grid">
          <button
            className="allow-once"
            disabled={decisionDisabled || stopping}
            onClick={() => decide("once")}
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
            className="allow-session"
            disabled={
              decisionDisabled ||
              stopping ||
              approval.risk === "critical" ||
              hostWorkspaceShell ||
              oneTimeFileAccess
            }
            onClick={() => decide("session")}
            title={
              oneTimeFileAccess
                ? t("approval.fileReadUnavailable")
                : hostWorkspaceShell
                ? t("approval.hostUnavailable")
                : approval.risk === "critical"
                  ? t("approval.criticalUnavailable")
                  : ""
            }
          >
            <ShieldCheck size={16} />
            <span>
              <b>
                {decisionBusy === "session"
                  ? t("approval.authorizing")
                  : elevated
                    ? t("approval.allowSessionSudo")
                    : t("approval.allowSession")}
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
  auth_type: "agent",
  private_key: "",
  known_hosts_file: "",
  proxy_jump_host_id: "",
  proxy_url: "",
  proxy_username: "",
  proxy_password: "",
  password: "",
  sudo_mode: "none",
  sudo_password: "",
};
function authLabel(value:HostAuthType){return i18n.t(value==='agent'?'hosts.authAgent':value==='key'?'hosts.authKey':'hosts.authPassword')}
function sudoLabel(value:HostSudoMode){return i18n.t(value==='none'?'hosts.sudoNone':value==='nopasswd'?'hosts.sudoNopasswd':'hosts.sudoPassword')}

function HostsPage({ hosts, refresh }: {hosts:Host[];refresh:()=>Promise<void>}) {
	const {t}=useTranslation()
  const [showForm, setShowForm] = useState(false); const [notice, setNotice] = useState(''); const [saving,setSaving]=useState(false);const [deletingHost,setDeletingHost]=useState('')
	const [deleteCandidate,setDeleteCandidate]=useState<Host|null>(null)
  const [form, setForm] = useState<HostInput>(emptyHostForm)
	const [privateKeyName,setPrivateKeyName]=useState(''),[privateKeyError,setPrivateKeyError]=useState(''),[existingPrivateKey,setExistingPrivateKey]=useState(false),[privateKeyInputKey,setPrivateKeyInputKey]=useState(0)
	const [hostKeys,setHostKeys]=useState<Record<string,{fingerprint:string;algorithm?:string;trusted:boolean}>>({}),[hostKeyErrors,setHostKeyErrors]=useState<Record<string,string>>({}),[hostKeyBusy,setHostKeyBusy]=useState('')
  const editing=!!form.id
	const resetPrivateKey=()=>{setPrivateKeyName('');setPrivateKeyError('');setExistingPrivateKey(false);setPrivateKeyInputKey(value=>value+1)}
	const openCreate=()=>{setForm(emptyHostForm);resetPrivateKey();setShowForm(true);setNotice('')}
	const openEdit=(host:Host)=>{setForm({id:host.id,name:host.name,address:host.address,port:host.port,user:host.user,auth_type:host.auth_type||'agent',private_key:'',known_hosts_file:host.known_hosts_file||'',proxy_jump_host_id:host.proxy_jump_host_id||'',proxy_url:host.proxy_url||'',proxy_username:host.proxy_username||'',proxy_password:'',password:'',sudo_mode:host.sudo_mode||'none',sudo_password:''});setPrivateKeyName('');setPrivateKeyError('');setExistingPrivateKey(host.auth_type==='key'&&host.has_private_key);setPrivateKeyInputKey(value=>value+1);setShowForm(true);setNotice('')}
	const setAuthType=(auth_type:HostAuthType)=>{setForm(current=>({...current,auth_type,password:'',private_key:auth_type==='key'?current.private_key:''}));if(auth_type!=='key'){setPrivateKeyName('');setPrivateKeyError('');setPrivateKeyInputKey(value=>value+1)}}
	const choosePrivateKey=async(event:React.ChangeEvent<HTMLInputElement>)=>{const selected=event.target.files?.[0];setPrivateKeyError('');if(!selected){setPrivateKeyName('');setForm(current=>({...current,private_key:''}));return}if(selected.size<=0||selected.size>maxPrivateKeyBytes){setPrivateKeyName('');setForm(current=>({...current,private_key:''}));setPrivateKeyError(t('hosts.keySizeError'));return}try{const content=await selected.text();setPrivateKeyName(selected.name);setForm(current=>({...current,private_key:content}))}catch(err){setPrivateKeyName('');setForm(current=>({...current,private_key:''}));setPrivateKeyError(errorText(err))}}
	const missingPrivateKey=form.auth_type==='key'&&!form.private_key&&!existingPrivateKey
	const scan = async (host:Host) => {setHostKeyBusy(`scan-${host.id}`);setHostKeyErrors(current=>({...current,[host.id]:''}));try{const key=await api.scanKey(host.id);setHostKeys(current=>({...current,[host.id]:key}))}catch(err){setHostKeyErrors(current=>({...current,[host.id]:errorText(err)}))}finally{setHostKeyBusy('')}}
	const trust = async (host:Host) => {const key=hostKeys[host.id];if(!key||key.trusted)return;setHostKeyBusy(`trust-${host.id}`);setHostKeyErrors(current=>({...current,[host.id]:''}));try{const trusted=await api.trustKey(host.id,key.fingerprint);setHostKeys(current=>({...current,[host.id]:{...trusted,trusted:true}}));setNotice(t('hosts.trusted',{fingerprint:trusted.fingerprint}))}catch(err){setHostKeyErrors(current=>({...current,[host.id]:errorText(err)}))}finally{setHostKeyBusy('')}}
	const save = async (event:FormEvent) => { event.preventDefault(); if(missingPrivateKey)return;setSaving(true); try { const saved=await api.saveHost(form); setShowForm(false); setForm(emptyHostForm);resetPrivateKey();setHostKeys(current=>{const next={...current};delete next[saved.id];return next});setHostKeyErrors(current=>{const next={...current};delete next[saved.id];return next}); setNotice(t('hosts.saved',{name:saved.name,action:editing?t('hosts.updated'):t('hosts.registered')})); await refresh();void scan(saved) } catch(err){setNotice(errorText(err))} finally{setSaving(false)} }
  const probe = async (host:Host) => { try { const info = await api.probe(host.id); setNotice(`${host.name}: ${Object.values(info).join(' · ')}`) } catch(err){setNotice(errorText(err))} }
	const remove=async()=>{const host=deleteCandidate;if(!host)return;setDeletingHost(host.id);setNotice('');try{await api.deleteHost(host.id);setNotice(t('hosts.deleted',{name:host.name}));await refresh()}catch(err){setNotice(errorText(err))}finally{setDeletingHost('');setDeleteCandidate(null)}}
		return <div className="page-stack"><div className="page-actions"><div/><button className="primary" onClick={openCreate}><Plus size={16}/>{t('hosts.add')}</button></div>
    {notice && <div className="notice">{notice}<button onClick={()=>setNotice('')}><X size={14}/></button></div>}
		{showForm && <form className="host-form panel" onSubmit={save}><div className="host-form-head"><div><h3>{editing?t('hosts.editTitle'):t('hosts.createTitle')}</h3></div><button type="button" className="close-button" title={t('common.close')} onClick={()=>setShowForm(false)}><X size={16}/></button></div><div className="form-grid host-fields">
	  <label><span>{t('hosts.name')}</span><input value={form.name} onChange={event=>setForm({...form,name:event.target.value})} required/></label>
	  <label><span>{t('hosts.address')}</span><input value={form.address} onChange={event=>setForm({...form,address:event.target.value})} required/></label>
	  <label><span>{t('hosts.port')}</span><input type="number" min="1" max="65535" value={form.port} onChange={event=>setForm({...form,port:Number(event.target.value)})} required/></label>
	  <label><span>{t('hosts.user')}</span><input value={form.user} onChange={event=>setForm({...form,user:event.target.value})} required/></label>
	  <label><span>{t('hosts.authentication')}</span><select value={form.auth_type} onChange={event=>setAuthType(event.target.value as HostAuthType)}>{(['agent','key','password'] as HostAuthType[]).map(mode=><option value={mode} key={mode}>{authLabel(mode)}</option>)}</select></label>
	  {form.auth_type==='password'&&<label><span>{t('hosts.sshPassword')}</span><PasswordInput autoComplete="new-password" value={form.password} onChange={event=>setForm({...form,password:event.target.value})} placeholder={editing?t('hosts.keepPassword'):t('common.required')} required={!editing}/></label>}
		  {form.auth_type==='key'&&<div className="private-key-field"><span>{t('hosts.privateKey')}</span><label className={`private-key-picker ${privateKeyError||missingPrivateKey?'invalid':''}`} title={privateKeyName||t('hosts.chooseKey')}><UploadCloud size={15}/><span><b>{privateKeyName||(existingPrivateKey?t('hosts.storedKey'):t('hosts.choosePrivateKey'))}</b>{!privateKeyName&&!existingPrivateKey&&<small>{t('hosts.keyLimit')}</small>}</span><input key={privateKeyInputKey} type="file" onChange={event=>void choosePrivateKey(event)}/></label>{(privateKeyError||missingPrivateKey)&&<small className="private-key-error">{privateKeyError||t('hosts.keyRequired')}</small>}</div>}
	  <label><span>{t('hosts.proxyJump')}</span><select value={form.proxy_jump_host_id} onChange={event=>setForm({...form,proxy_jump_host_id:event.target.value})}><option value="">{t('hosts.direct')}</option>{hosts.filter(host=>host.id!==form.id).map(host=><option value={host.id} key={host.id}>{host.name} · {host.user}@{host.address}:{host.port}</option>)}</select></label>
	  <label><span>{t('hosts.proxyURL')}</span><input value={form.proxy_url} onChange={event=>setForm({...form,proxy_url:event.target.value,...(!event.target.value?{proxy_username:'',proxy_password:''}:{})})}/></label>
	  {form.proxy_url&&<label><span>{t('hosts.proxyUsername')}</span><input autoComplete="off" value={form.proxy_username} onChange={event=>setForm({...form,proxy_username:event.target.value,proxy_password:event.target.value?form.proxy_password:''})}/></label>}
	  {form.proxy_url&&form.proxy_username&&<label><span>{t('hosts.proxyPassword')}</span><PasswordInput autoComplete="new-password" value={form.proxy_password} onChange={event=>setForm({...form,proxy_password:event.target.value})} placeholder={editing?t('hosts.keepPassword'):''}/></label>}
	  <label><span>{t('hosts.knownHosts')}</span><input value={form.known_hosts_file} onChange={event=>setForm({...form,known_hosts_file:event.target.value})} placeholder={t('hosts.useDefault')}/></label>
	  <label><span>{t('hosts.sudoPolicy')}</span><select value={form.sudo_mode} onChange={event=>setForm({...form,sudo_mode:event.target.value as HostSudoMode,sudo_password:''})}>{(['none','nopasswd','password'] as HostSudoMode[]).map(mode=><option value={mode} key={mode}>{sudoLabel(mode)}</option>)}</select></label>
	  {form.sudo_mode==='password'&&<label><span>{t('hosts.sudoPasswordLabel')}</span><PasswordInput autoComplete="new-password" value={form.sudo_password} onChange={event=>setForm({...form,sudo_password:event.target.value})} placeholder={editing?t('hosts.keepPassword'):t('common.required')} required={!editing}/></label>}
		</div><div className="form-actions"><button type="button" onClick={()=>setShowForm(false)}>{t('common.cancel')}</button><button className="primary" disabled={saving||!!privateKeyError||missingPrivateKey}>{saving?t('common.saving'):editing?t('hosts.update'):t('hosts.save')}</button></div></form>}
		<div className="host-grid">{hosts.map(host=>{const key=hostKeys[host.id],keyError=hostKeyErrors[host.id],scanning=hostKeyBusy===`scan-${host.id}`,trusting=hostKeyBusy===`trust-${host.id}`;return <article className="host-card panel" key={host.id}><div className="host-top"><div className="server-glyph"><Server size={22}/></div><div><h3>{host.name}</h3><span>{`${host.user}@${host.address}:${host.port}`}</span></div><span className={`host-key-state ${key?.trusted?'trusted':key?'untrusted':'unchecked'}`}>{scanning?t('hosts.checkingKey'):key?.trusted?t('hosts.trustedKey'):key?t('hosts.untrustedKey'):t('hosts.uncheckedKey')}</span></div><dl><div><dt>{t('hosts.authentication')}</dt><dd>{authLabel(host.auth_type||'agent')}</dd></div>{host.proxy_url&&<div><dt>{t('hosts.proxy')}</dt><dd>{host.proxy_url}</dd></div>}<div><dt>Sudo</dt><dd>{sudoLabel(host.sudo_mode||'none')}</dd></div><div><dt>{t('hosts.hostId')}</dt><dd>{host.id}</dd></div></dl>{(key||keyError)&&<div className={`host-key-review ${key?.trusted?'trusted':'untrusted'}`}>{key&&<><div><KeyRound size={14}/><span><b>{key.algorithm||t('hosts.hostKey')}</b><code title={key.fingerprint}>{key.fingerprint}</code></span></div>{!key.trusted&&<button className="trust" disabled={trusting} onClick={()=>void trust(host)}>{trusting?<LoaderCircle className="spin" size={13}/>:<ShieldCheck size={13}/>} {trusting?t('hosts.trustingKey'):t('hosts.trustKey')}</button>}</>}{keyError&&<span className="host-key-error">{keyError}</span>}</div>}<div className="card-actions"><button onClick={()=>void probe(host)}><Activity size={15}/>{t('hosts.probe')}</button><button disabled={scanning||trusting} onClick={()=>void scan(host)}>{scanning?<LoaderCircle className="spin" size={15}/>:<KeyRound size={15}/>} {t('hosts.checkKey')}</button><button onClick={()=>openEdit(host)}><Edit3 size={15}/>{t('common.edit')}</button><button className="danger" disabled={deletingHost===host.id} title={t('common.delete')} onClick={()=>setDeleteCandidate(host)}>{deletingHost===host.id?<LoaderCircle className="spin" size={15}/>:<Trash2 size={15}/>}</button></div></article>})}</div>
	{!hosts.length && <Empty icon={<Server/>} title={t('hosts.emptyTitle')}/>}
	{deleteCandidate&&<DestructiveConfirmDialog label={t('hosts.deleteDialogLabel')} title={t('hosts.deleteTitle',{name:deleteCandidate.name})} description={t('hosts.deleteText')} busy={deletingHost===deleteCandidate.id} onCancel={()=>setDeleteCandidate(null)} onConfirm={()=>void remove()}/>}
  </div>
}

const emptyProviderForm: ModelProviderInput = {name:'',kind:'openai',base_url:'',model:'gpt-4o-mini',api_key:'',proxy_url:'',proxy_username:'',proxy_password:''}
const providerLabels: Record<ModelProviderKind,string> = {
  openai: 'OpenAI', deepseek: 'DeepSeek', openai_compatible: 'OpenAI-compatible', ollama: 'Ollama',
}
const providerDefaults: Record<ModelProviderKind,Pick<ModelProviderInput,'base_url'|'model'>> = {
  openai: {base_url:'',model:'gpt-4o-mini'},
  deepseek: {base_url:'https://api.deepseek.com',model:'deepseek-v4-flash'},
  openai_compatible: {base_url:'',model:''},
  ollama: {base_url:'http://127.0.0.1:11434/v1',model:''},
}

function ModelsPage({providers,health,refresh}:{providers:ModelProvider[];health:Health|null;refresh:()=>Promise<void>}) {
	const {t,i18n:instance}=useTranslation()
  const [showForm,setShowForm]=useState(false)
  const [form,setForm]=useState<ModelProviderInput>(emptyProviderForm)
  const [notice,setNotice]=useState('')
  const [busy,setBusy]=useState('')
	const [deleteCandidate,setDeleteCandidate]=useState<ModelProvider|null>(null)
  const [catalog,setCatalog]=useState<string[]>([])
  const [discovering,setDiscovering]=useState(false)
  const editing=!!form.id
	const editingProvider=providers.find(provider=>provider.id===form.id)

  const openCreate=()=>{setForm(emptyProviderForm);setCatalog([]);setShowForm(true);setNotice('')}
  const openEdit=(provider:ModelProvider)=>{setForm({id:provider.id,name:provider.name,kind:provider.kind,base_url:provider.base_url||'',model:provider.model,api_key:'',proxy_url:provider.proxy_url||'',proxy_username:provider.proxy_username||'',proxy_password:''});setCatalog([]);setShowForm(true);setNotice('')}
  const changeKind=(kind:ModelProviderKind)=>{setCatalog([]);setForm({...form,kind,...providerDefaults[kind]})}
	const discover=async()=>{setDiscovering(true);try{const result=await api.discoverModels({id:form.id,kind:form.kind,base_url:form.base_url,api_key:form.api_key,proxy_url:form.proxy_url,proxy_username:form.proxy_username,proxy_password:form.proxy_password,clear_proxy_password:form.clear_proxy_password});setCatalog(result.models);setForm(current=>({...current,model:result.models.includes(current.model)?current.model:''}));setNotice(t('models.found',{count:result.count}))}catch(err){setCatalog([]);setNotice(errorText(err))}finally{setDiscovering(false)}}
	const testForm=async()=>{setBusy('test-form');try{const result=await api.testModelConfiguration({id:form.id,kind:form.kind,base_url:form.base_url,model:form.model,api_key:form.api_key,proxy_url:form.proxy_url,proxy_username:form.proxy_username,proxy_password:form.proxy_password,clear_proxy_password:form.clear_proxy_password});setNotice(t('models.healthy',{name:result.model,response:result.response,latency:result.latency_ms}))}catch(err){setNotice(errorText(err))}finally{setBusy('')}}
	const save=async(event:FormEvent)=>{event.preventDefault();setBusy('save');try{const saved=await api.saveModelProvider(form);setNotice(t('models.saved',{name:saved.name}));setShowForm(false);setForm(emptyProviderForm);await refresh()}catch(err){setNotice(errorText(err))}finally{setBusy('')}}
	const activate=async(provider:ModelProvider)=>{setBusy(provider.id);try{await api.activateModelProvider(provider.id);setNotice(t('models.activated',{name:provider.name}));await refresh()}catch(err){setNotice(errorText(err))}finally{setBusy('')}}
	const test=async(provider:ModelProvider)=>{setBusy(`test-${provider.id}`);try{const result=await api.testModelProvider(provider.id);setNotice(t('models.healthy',{name:provider.name,response:result.response,latency:result.latency_ms}))}catch(err){setNotice(errorText(err))}finally{setBusy('')}}
	const remove=async()=>{const provider=deleteCandidate;if(!provider)return;setBusy(`delete-${provider.id}`);setNotice('');try{await api.deleteModelProvider(provider.id);setNotice(t('models.deleted',{name:provider.name}));await refresh()}catch(err){setNotice(errorText(err))}finally{setBusy('');setDeleteCandidate(null)}}

  return <div className="page-stack">
	<div className="page-actions"><div/><button className="primary" onClick={openCreate}><Plus size={16}/>{t('models.add')}</button></div>
    {notice&&<div className="notice">{notice}<button onClick={()=>setNotice('')}><X size={14}/></button></div>}
	<div className="model-summary panel"><div><span>{t('models.activeRoute')}</span><b>{health?.model?.name||t('models.noModel')}</b>{health?.model?.model&&<small>{health.model.model}</small>}</div><div className={`model-signal ${health?.agent_available?'ready':''}`}><CircleDot size={16}/>{health?.agent_available?t('models.ready'):t('models.offline')}</div></div>
    {showForm&&<form className="model-form panel" onSubmit={save}>
		  <div className="model-form-head"><div><h3>{editing?t('models.editTitle'):t('models.newTitle')}</h3></div><button type="button" className="close-button" title={t('common.close')} onClick={()=>setShowForm(false)}><X size={16}/></button></div>
      <div className="form-grid model-fields">
		<label><span>{t('models.displayName')}</span><input value={form.name} onChange={event=>setForm({...form,name:event.target.value})} required/></label>
		<label><span>{t('models.providerType')}</span><select value={form.kind} onChange={event=>changeKind(event.target.value as ModelProviderKind)}>{(Object.keys(providerLabels) as ModelProviderKind[]).map(kind=><option key={kind} value={kind}>{providerLabels[kind]}</option>)}</select></label>
		<label className="model-id-field"><span className="field-title"><span>{t('models.modelId')}</span><button type="button" onClick={discover} disabled={discovering}><RefreshCw size={12}/>{discovering?t('models.fetching'):t('models.fetchModels')}</button></span>{catalog.length>0?<select value={form.model} onChange={event=>setForm({...form,model:event.target.value})} required><option value="">{t('models.selectModel')}</option>{catalog.map(model=><option value={model} key={model}>{model}</option>)}</select>:<input value={form.model} onChange={event=>setForm({...form,model:event.target.value})} placeholder={t('models.modelPlaceholder')} required/>}{catalog.length>0&&<small>{t('models.available',{count:catalog.length})} · <button type="button" onClick={()=>setCatalog([])}>{t('models.enterManually')}</button></small>}</label>
			<label><span>{t('models.apiKey')}</span><PasswordInput autoComplete="new-password" value={form.api_key} onChange={event=>{setCatalog([]);setForm({...form,api_key:event.target.value})}} placeholder={editing?t('models.keepKey'):''}/></label>
			<label className="base-url-field"><span>{t('models.baseUrl')}</span><input value={form.base_url} onChange={event=>{setCatalog([]);setForm({...form,base_url:event.target.value})}} placeholder={form.kind==='openai'?t('models.officialEndpoint'):''}/></label>
			<label className="proxy-url-field"><span>{t('models.proxyUrl')}</span><input value={form.proxy_url} onChange={event=>{setCatalog([]);setForm({...form,proxy_url:event.target.value,clear_proxy_password:false})}}/></label>
		<label className="proxy-credential-field"><span>{t('models.proxyUsername')}</span><input value={form.proxy_username} onChange={event=>{setCatalog([]);setForm({...form,proxy_username:event.target.value,clear_proxy_password:false})}}/></label>
			<label className="proxy-credential-field"><span>{t('models.proxyPassword')}</span><PasswordInput autoComplete="new-password" value={form.proxy_password} onChange={event=>{setCatalog([]);setForm({...form,proxy_password:event.target.value,clear_proxy_password:false})}} placeholder={editingProvider?.has_proxy_password&&!form.clear_proxy_password?t('models.keepProxyPassword'):''}/>{editingProvider?.has_proxy_password&&!form.clear_proxy_password&&<small><button type="button" onClick={()=>setForm({...form,proxy_password:'',clear_proxy_password:true})}>{t('models.clearProxyPassword')}</button></small>}</label>
      </div>
	  <div className="form-actions"><button type="button" onClick={()=>setShowForm(false)}>{t('common.cancel')}</button><button type="button" className="test-config" onClick={testForm} disabled={!!busy||!form.model}><Activity size={14}/>{busy==='test-form'?t('models.sendingHello'):t('models.testModel')}</button><button className="primary" disabled={!!busy}>{busy==='save'?t('common.saving'):t('models.saveProvider')}</button></div>
    </form>}
    <div className="model-grid">{providers.map(provider=><article className={`model-card panel ${provider.active?'active':''}`} key={provider.id}>
	  <div className="model-card-head"><div className="provider-glyph"><Cpu size={21}/></div><div><h3>{provider.name}</h3><span>{providerLabels[provider.kind]}</span></div>{provider.active&&<em><Zap size={12}/>{t('models.active')}</em>}</div>
      <div className="model-name">{provider.model}</div>
	  <dl><div><dt>{t('models.endpoint')}</dt><dd>{provider.base_url||t('models.providerDefault')}</dd></div><div><dt>{t('models.proxy')}</dt><dd>{provider.proxy_url||t('models.noProxy')}{provider.has_proxy_password?` · ${t('models.proxyAuth')}`:''}</dd></div><div><dt>{t('models.credential')}</dt><dd>{provider.has_api_key?t('models.encryptedKey'):t('models.noApiKey')}</dd></div><div><dt>{t('common.updated')}</dt><dd>{new Date(provider.updated_at).toLocaleString(localeFor(instance.language))}</dd></div></dl>
	  <div className="model-actions"><button onClick={()=>test(provider)} disabled={!!busy}><Activity size={14}/>{busy===`test-${provider.id}`?t('common.testing'):t('common.test')}</button><button onClick={()=>openEdit(provider)} disabled={!!busy}><Edit3 size={14}/>{t('common.edit')}</button>{!provider.active&&<button className="use-model" onClick={()=>activate(provider)} disabled={!!busy}><Zap size={14}/>{busy===provider.id?t('models.switching'):t('models.useModel')}</button>}<button className="danger" title={t('common.delete')} onClick={()=>setDeleteCandidate(provider)} disabled={!!busy}><Trash2 size={14}/></button></div>
    </article>)}</div>
		{!providers.length&&<Empty icon={<Cpu/>} title={t('models.emptyTitle')}/>}
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
	const workspaceTransfer=mode==='workspace_upload'
	const sshTransfer=mode==='ssh_file_transfer'
	const fileTarget=`${workspaceID?`${workspaceID}:`:''}${filePath}`
	const consumed=new Set(['program','args','script','cwd','reason','expected_changes','rollback','change','env','host_id','workspace_id','remote_path','relative_path','source_path','source_host_id','mode','elevated','workspace_shell_backend'])
	const extras=Object.entries(req).filter(([key,value])=>!consumed.has(key)&&value!==undefined&&value!==null&&value!==''&&!(Array.isArray(value)&&!value.length))
	return <>
		<div className="tool-execution-layout">
			<section className="tool-command-pane">
				<div className="tool-command-head"><span>{searchMode?t('tool.searchOperation'):readMode?t('tool.readOperation'):workspaceTransfer||sshTransfer||filePath?t('tool.fileOperation'):script?t('tool.fullScript'):t('tool.fullCommand')}</span>{workspaceShellBackend&&<em><TerminalSquare size={12}/>{workspaceShellBackend==='host'?t('approval.hostShell'):'Bubblewrap'}</em>}{req.elevated===true&&<em><ShieldAlert size={12}/>sudo / root</em>}</div>
				<div className="tool-command-block">{workspaceTransfer?<pre>workspace_upload {workspaceID}:{relativePath} → {remotePath}</pre>:sshTransfer?<pre>{[sourceHost.name||sourceHost.id,sourcePath].filter(Boolean).join(':')} → {destinationHost.name||destinationHost.id}:{remotePath}</pre>:searchMode||readMode?<pre>{searchMode?'search':'read'} {fileTarget}</pre>:script?<pre>{script}</pre>:program?<pre><span className="prompt-sign">$</span> {program}</pre>:filePath?<pre>{mode} {fileTarget}</pre>:<pre>{JSON.stringify(req,null,2)}</pre>}</div>
				{change&&textValue(change.diff)&&<DiffViewer change={change}/>}
				{program&&args.length>0&&<CompactTable title={t('tool.originalArgs')} columns={[t('tool.index'),t('tool.value')]} rows={[[0,textValue(req.program)],...args.map((arg,index)=>[index+1,JSON.stringify(arg)])]}/>}
				{env&&Object.keys(env).length>0&&<CompactTable title={t('tool.environment')} columns={[t('tool.key'),t('tool.value')]} rows={Object.entries(env).map(([key,value])=>[key,String(value)])}/>}
				{extras.length>0&&<CompactTable title={t('tool.actualParameters')} columns={[t('tool.parameter'),t('tool.value')]} rows={extras}/>}
			</section>
			<aside className="tool-context-pane">
				<dl className="tool-context-grid">
					<div><dt>{workspaceID&&!sshTransfer?t('common.workspace'):t('tool.targetHost')}</dt><dd>{workspaceID&&!sshTransfer?workspaceID:[destinationHost.name,destinationHost.id].filter(Boolean).join(' · ')||'—'}</dd></div>
					{sshTransfer&&<div><dt>{t('tool.sourceHost')}</dt><dd>{[sourceHost.name,sourceHost.id].filter(Boolean).join(' · ')||'—'}</dd></div>}
					<div><dt>{filePath?t('tool.filePath'):t('tool.workingDirectory')}</dt><dd>{filePath||textValue(req.cwd)||t('tool.defaultDirectory')}</dd></div>
					<div><dt>{t('tool.permission')}</dt><dd>{workspaceShellBackend==='host'?t('tool.hostAuthority'):workspaceShellBackend==='sandbox'?t('tool.sandbox'):req.elevated===true?t('tool.managedSudo'):t('tool.normalUser')}</dd></div>
					<div><dt>{t('tool.duration')}</dt><dd>{formatDuration(undefined,run)}</dd></div>
					<div><dt>{t('tool.runId')}</dt><dd>{run.id}</dd></div>
				</dl>
				{textValue(req.reason)&&<div className="tool-reason"><span>{t('tool.reason')}</span><p>{textValue(req.reason)}</p></div>}
				{textValue(req.expected_changes)&&<div className="tool-reason change"><span>{t('tool.expectedChanges')}</span><p>{textValue(req.expected_changes)}</p></div>}
				{textValue(req.rollback)&&<div className="tool-reason rollback"><span>{t('tool.rollback')}</span><p>{textValue(req.rollback)}</p></div>}
			</aside>
		</div>
		{(run.stdout_redacted||run.stderr_redacted||run.error)&&<div className="tool-output-grid">{run.stdout_redacted&&<div className="tool-output stdout"><span>STDOUT · REDACTED</span><pre>{run.stdout_redacted}</pre></div>}{run.stderr_redacted&&<div className="tool-output stderr"><span>STDERR · REDACTED</span><pre>{run.stderr_redacted}</pre></div>}{run.error&&!run.stderr_redacted&&<div className="tool-output stderr"><span>{t('common.error')}</span><pre>{run.error}</pre></div>}</div>}
		<details className="tool-raw"><summary>{t('tool.rawJson')}</summary><pre>{JSON.stringify(req,null,2)}</pre></details>
	</>
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
	return [...grouped.entries()].map(([id,items])=>{items.sort((a,b)=>Date.parse(b.started_at)-Date.parse(a.started_at));return{id,title:id==='__direct__'?t('audit.direct'):titles.get(id)||t('audit.missingConversation'),runs:items,latest:items[0]?.started_at,critical:items.filter(run=>run.risk==='critical'||run.risk==='forbidden').length,pending:items.filter(run=>run.status==='approval_required').length}}).sort((a,b)=>Date.parse(b.latest||'')-Date.parse(a.latest||''))
	},[filtered,sessions,t,instance.language])
	return <div className="page-stack"><div className="audit-toolbar"><div className="search-box"><Search size={16}/><input aria-label={t('common.search')} value={query} onChange={event=>setQuery(event.target.value)}/></div><span>{t('audit.counts',{sessions:groups.length,runs:filtered.length})}</span></div><div className="audit-groups">{groups.map(group=><details className="audit-session panel" key={group.id}><summary className="audit-session-summary"><div className="audit-session-glyph"><History size={17}/></div><div className="audit-session-name"><b>{group.title}</b><span>{group.id==='__direct__'?t('audit.noSession'):group.id} · {t('audit.lastRun',{date:new Date(group.latest).toLocaleString(localeFor(instance.language))})}</span></div><div className="audit-session-stats"><span><b>{group.runs.length}</b> {t('audit.runs')}</span>{group.critical>0&&<span className="critical-count"><b>{group.critical}</b> {t('audit.critical')}</span>}{group.pending>0&&<span className="pending-count"><b>{group.pending}</b> {t('audit.pending')}</span>}</div><ChevronRight className="audit-session-chevron" size={17}/></summary><div className="audit-table"><div className="audit-row audit-head"><span>{t('audit.columns.time')}</span><span>{t('audit.columns.operation')}</span><span>{t('audit.columns.risk')}</span><span>{t('audit.columns.status')}</span><span>{t('audit.columns.host')}</span><span>{t('audit.columns.exit')}</span></div>{group.runs.map(run=>{let req:Record<string,unknown>={};try{req=JSON.parse(run.request_json)}catch{req={request:run.request_json}};const args=Array.isArray(req.args)?req.args.join(' '):'';return <details key={run.id}><summary className="audit-row"><span>{new Date(run.started_at).toLocaleString(localeFor(instance.language))}</span><span className="command">{typeof req.program==='string'?`${req.program} ${args}`.trim():t('audit.bashScript')}</span><span><i className={`risk-dot ${run.risk}`}/>{t(`riskLabels.${run.risk}`,{defaultValue:run.risk})}</span><span className={`run-status ${run.status}`}>{t(`statusLabels.${run.status}`,{defaultValue:run.status})}</span><span>{run.host_id.slice(0,16)}</span><span>{run.exit_code}</span></summary><div className="run-detail"><AuditRunDetail run={run} req={req} hosts={hosts}/></div></details>})}</div></details>)}</div>{!runs.length&&<Empty icon={<History/>} title={t('audit.emptyTitle')}/>} {runs.length>0&&!groups.length&&<Empty icon={<Search/>} title={t('audit.noMatch')}/>}</div>
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
