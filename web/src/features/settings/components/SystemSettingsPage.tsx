import { FormEvent, useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Bot, BrainCircuit, CircleDot, ImagePlus, Minimize2, Power, RefreshCw, ShieldAlert, ShieldCheck, SlidersHorizontal, TerminalSquare } from 'lucide-react'
import { api } from '../../../api/api'
import { AppSelect } from '../../../components/Controls'
import { SettingsDisclosure } from '../../../components/SettingsDisclosure'
import { WorkspaceSettingsPanel } from '../../workspace'
import { useNotifier } from '../../../lib/notifications'
import { DesktopApplicationPanel } from './DesktopApplicationPanel'
import { desktopRuntime, errorText } from '../../../lib/utils'
import type { Health, ModelProvider, Proxy, SystemSettings, SystemSettingsInput, ToolCapabilities, WorkspaceShellMode } from '../../../types'
import { ConfigurationTransferSettings } from './ConfigurationTransferSettings'
import { MCPServerModePanel } from './MCPServerModePanel'
import { WebSearchSettingsPanel } from './WebSearchSettingsPanel'
import { defaultChatImageTypes } from '../defaults'

function SettingsSectionFooter({dirty,busy,saving,onDiscard}:{dirty:boolean;busy:boolean;saving:boolean;onDiscard:()=>void}){
	const {t}=useTranslation()
	return <footer className="settings-section-footer"><button type="button" disabled={!dirty||busy} onClick={onDiscard}>{t('settings.discard')}</button><button type="submit" className="primary" disabled={!dirty||busy}>{saving?t('settings.applying'):t('settings.apply')}</button></footer>
}

type SystemSettingsSection='iterations'|'prompt'|'explanation'|'compression'|'images'|'shell'

export function SystemSettingsPage({settings,providers,proxies,capabilities,modelStatus,refreshModels,refreshHosts,refreshProxies,refreshCapabilities,refreshHealth,onSettingsChanged,onOpenMCPActivity}:{settings:SystemSettings|null;providers:ModelProvider[];proxies:Proxy[];capabilities:ToolCapabilities;modelStatus?:Health['model'];refreshModels:()=>Promise<void>;refreshHosts:()=>Promise<void>;refreshProxies:()=>Promise<void>;refreshCapabilities:()=>Promise<void>;refreshHealth:()=>Promise<void>;onSettingsChanged:(settings:SystemSettings)=>void;onOpenMCPActivity:()=>void}) {
  const {t}=useTranslation()
	const notify=useNotifier()
  const savedValue=settings?.agent_max_iterations??50
  const savedPrompt=settings?.system_prompt??''
	const defaultPrompt=settings?.default_system_prompt??''
  const savedExplanation=settings?.approval_explanations_enabled??true
	  const savedSubagentProvider=settings?.subagent_model_provider_id??''
	  const savedAutomaticApprovalProvider=settings?.automatic_approval_model_provider_id??''
	  const savedSubagentTimeout=settings?.subagent_timeout_seconds??30
	  const savedCompressionEnabled=settings?.context_compression_enabled??true
	  const savedCompressionPercent=settings?.context_compression_threshold_percent??70
	  const savedImageTypes=settings?.chat_image_allowed_types??defaultChatImageTypes
  const savedShellMode=settings?.workspace_shell_mode??(settings?.workspace_shell_platform==='windows'?'host':'sandbox')
  const [maxIterations,setMaxIterations]=useState(savedValue)
  const [systemPrompt,setSystemPrompt]=useState(savedPrompt)
  const [explanationEnabled,setExplanationEnabled]=useState(savedExplanation)
  const [subagentProvider,setSubagentProvider]=useState(savedSubagentProvider)
	const [automaticApprovalProvider,setAutomaticApprovalProvider]=useState(savedAutomaticApprovalProvider)
	  const [subagentTimeout,setSubagentTimeout]=useState(savedSubagentTimeout)
	  const [compressionEnabled,setCompressionEnabled]=useState(savedCompressionEnabled)
	  const [compressionPercent,setCompressionPercent]=useState(savedCompressionPercent)
	  const [imageTypes,setImageTypes]=useState(savedImageTypes)
  const [shellMode,setShellMode]=useState<WorkspaceShellMode>(savedShellMode)
	const [iterationsDirty,setIterationsDirty]=useState(false)
	const [promptDirty,setPromptDirty]=useState(false)
	const [explanationDirty,setExplanationDirty]=useState(false)
	const [compressionDirty,setCompressionDirty]=useState(false)
	const [imagesDirty,setImagesDirty]=useState(false)
	const [shellDirty,setShellDirty]=useState(false)
	const [savingSection,setSavingSection]=useState<SystemSettingsSection|''>('')
	useEffect(()=>{if(!iterationsDirty)setMaxIterations(savedValue)},[savedValue,iterationsDirty])
	useEffect(()=>{if(!promptDirty)setSystemPrompt(savedPrompt)},[savedPrompt,promptDirty])
	useEffect(()=>{if(!explanationDirty){setExplanationEnabled(savedExplanation);setSubagentProvider(savedSubagentProvider);setAutomaticApprovalProvider(savedAutomaticApprovalProvider);setSubagentTimeout(savedSubagentTimeout)}},[savedExplanation,savedSubagentProvider,savedAutomaticApprovalProvider,savedSubagentTimeout,explanationDirty])
	useEffect(()=>{if(!compressionDirty){setCompressionEnabled(savedCompressionEnabled);setCompressionPercent(savedCompressionPercent)}},[savedCompressionEnabled,savedCompressionPercent,compressionDirty])
	useEffect(()=>{if(!imagesDirty)setImageTypes(savedImageTypes)},[savedImageTypes,imagesDirty])
	useEffect(()=>{if(!shellDirty)setShellMode(savedShellMode)},[savedShellMode,shellDirty])
	const update=(value:number)=>{setMaxIterations(Math.max(5,Math.min(100,value||5)));setIterationsDirty(true)}
	const updateSystemPrompt=(value:string)=>{setSystemPrompt(value);setPromptDirty(true)}
	const restoreDefaultPrompt=()=>{setSystemPrompt(defaultPrompt);setPromptDirty(true)}
	const toggleExplanation=(value:boolean)=>{setExplanationEnabled(value);setExplanationDirty(true)}
	const selectSubagentProvider=(value:string)=>{setSubagentProvider(value);setExplanationDirty(true)}
	const selectAutomaticApprovalProvider=(value:string)=>{setAutomaticApprovalProvider(value);setExplanationDirty(true)}
	const updateSubagentTimeout=(value:number)=>{setSubagentTimeout(Math.max(5,Math.min(120,value||5)));setExplanationDirty(true)}
	const toggleCompression=(value:boolean)=>{setCompressionEnabled(value);setCompressionDirty(true)}
	const updateCompressionPercent=(value:number)=>{setCompressionPercent(Math.max(50,Math.min(90,value||50)));setCompressionDirty(true)}
	const toggleImageType=(value:string)=>{setImageTypes(current=>current.includes(value)?current.length===1?current:current.filter(item=>item!==value):[...current,value]);setImagesDirty(true)}
	const selectShellMode=(value:WorkspaceShellMode)=>{setShellMode(value);setShellDirty(true)}
	const discard=(section:SystemSettingsSection)=>{
		switch(section){
		case 'iterations':setMaxIterations(savedValue);setIterationsDirty(false);break
		case 'prompt':setSystemPrompt(savedPrompt);setPromptDirty(false);break
		case 'explanation':setExplanationEnabled(savedExplanation);setSubagentProvider(savedSubagentProvider);setAutomaticApprovalProvider(savedAutomaticApprovalProvider);setSubagentTimeout(savedSubagentTimeout);setExplanationDirty(false);break
		case 'compression':setCompressionEnabled(savedCompressionEnabled);setCompressionPercent(savedCompressionPercent);setCompressionDirty(false);break
		case 'images':setImageTypes(savedImageTypes);setImagesDirty(false);break
		case 'shell':setShellMode(savedShellMode);setShellDirty(false);break
		}
	}
	const save=async(section:SystemSettingsSection)=>{
		const input:SystemSettingsInput={agent_max_iterations:section==='iterations'?maxIterations:savedValue}
		switch(section){
		case 'prompt':input.system_prompt=systemPrompt;break
		case 'explanation':input.approval_explanations_enabled=explanationEnabled;input.subagent_model_provider_id=subagentProvider;input.automatic_approval_model_provider_id=automaticApprovalProvider;input.subagent_timeout_seconds=subagentTimeout;break
		case 'compression':input.context_compression_enabled=compressionEnabled;input.context_compression_threshold_percent=compressionPercent;break
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
			case 'compression':setCompressionEnabled(result.context_compression_enabled);setCompressionPercent(result.context_compression_threshold_percent);setCompressionDirty(false);break
			case 'images':setImageTypes(result.chat_image_allowed_types);setImagesDirty(false);break
			case 'shell':setShellMode(result.workspace_shell_mode);setShellDirty(false);break
			}
			onSettingsChanged(result)
			notify(t('settings.saved'))
			if(section==='shell')await refreshCapabilities()
			else if(section==='explanation')await refreshHealth()
		}catch(err){notify(errorText(err),'error')}finally{setSavingSection('')}
	}
	const submit=(section:SystemSettingsSection)=>(event:FormEvent)=>{event.preventDefault();void save(section)}
	const busy=!!savingSection
  return <div className="system-settings page-stack">

		<div className="settings-form">
			<ConfigurationTransferSettings refreshModels={refreshModels} refreshHosts={refreshHosts} refreshProxies={refreshProxies} refreshCapabilities={refreshCapabilities} refreshHealth={refreshHealth}/>
			<SettingsDisclosure icon={<SlidersHorizontal size={18}/>} title={t('settings.maxIterations')} meta={<strong>{maxIterations}</strong>}>
				<form onSubmit={submit('iterations')}><div className="iteration-editor"><input aria-label={t('settings.maxIterations')} type="range" min="5" max="100" step="1" value={maxIterations} onChange={event=>update(Number(event.target.value))}/><label><span>{t('settings.rounds')}</span><input type="number" min="5" max="100" value={maxIterations} onChange={event=>update(Number(event.target.value))}/></label></div><div className="iteration-presets"><span>{t('settings.quickPresets')}</span>{[20,50,100].map(value=><button type="button" className={maxIterations===value?'active':''} onClick={()=>update(value)} key={value}><b>{value}</b></button>)}</div><SettingsSectionFooter dirty={iterationsDirty} busy={busy} saving={savingSection==='iterations'} onDiscard={()=>discard('iterations')}/></form>
			</SettingsDisclosure>
			<SettingsDisclosure icon={<Minimize2 size={18}/>} title={t('settings.contextCompression')} meta={compressionEnabled?`${compressionPercent}%`:t('settings.compressionOff')}>
				<form onSubmit={submit('compression')}><div className="subagent-settings"><label className="subagent-toggle"><span><b>{t('settings.autoCompression')}</b></span><input type="checkbox" checked={compressionEnabled} onChange={event=>toggleCompression(event.target.checked)}/><i/></label><div className="iteration-editor"><input aria-label={t('settings.compressionThreshold')} type="range" min="50" max="90" step="5" value={compressionPercent} disabled={!compressionEnabled} onChange={event=>updateCompressionPercent(Number(event.target.value))}/><label><span>{t('settings.compressionThreshold')}</span><input type="number" min="50" max="90" step="5" value={compressionPercent} disabled={!compressionEnabled} onChange={event=>updateCompressionPercent(Number(event.target.value))}/></label></div></div><SettingsSectionFooter dirty={compressionDirty} busy={busy} saving={savingSection==='compression'} onDiscard={()=>discard('compression')}/></form>
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
			<WorkspaceSettingsPanel workspaces={capabilities.workspaces} refresh={refreshCapabilities} onNotify={notify}/>
			<MCPServerModePanel settings={settings} onChanged={onSettingsChanged} onOpenActivity={onOpenMCPActivity}/>
			<WebSearchSettingsPanel proxies={proxies}/>
			{desktopRuntime&&<DesktopApplicationPanel/>}
		</div>
  </div>
}
