import { memo, useState } from 'react'
import { createPortal } from 'react-dom'
import { useTranslation } from 'react-i18next'
import { BrainCircuit, Check, ChevronRight, Cpu, Minimize2, LoaderCircle, Server, ShieldAlert, ShieldCheck } from 'lucide-react'
import { api } from '../../api/api'
import { localeFor } from '../../lib/i18n'
import { useAutoCollapseDetails } from '../../lib/hooks'
import { errorText } from '../../lib/utils'
import type { ApprovalMode, Host, ModelProvider, ModelReasoningEffort, SystemSettings } from '../../types'
import { hostInputWithAgentState, hostSupportsRoot } from '../ssh/hostAccess'
import { reasoningEfforts } from '../settings/defaults'
import type { ContextUsage } from './types'

type ComposerControlsProps={
	sessionId:string
	sessionBusy:boolean
	loadingSession:boolean
	workspaceSwitching:boolean
	compressingContext:boolean
	settings:SystemSettings|null
	hosts:Host[]
	providers:ModelProvider[]
	modelName?:string
	contextUsage:ContextUsage
	onSettingsChanged:(settings:SystemSettings)=>void
	onHostChanged:(host:Host)=>void
	onModelChanged:(provider:ModelProvider)=>void
	onError:(message:string)=>void
	onCompress:()=>void
}

export const ComposerControls=memo(function ComposerControls({sessionId,sessionBusy,loadingSession,workspaceSwitching,compressingContext,settings,hosts,providers,modelName,contextUsage,onSettingsChanged,onHostChanged,onModelChanged,onError,onCompress}:ComposerControlsProps){
	const {t}=useTranslation()
	return <div className="context-line"><ApprovalModeStatus settings={settings} onChanged={onSettingsChanged} onError={onError}/><ComposerHostSelector hosts={hosts} disabled={sessionBusy} rootDisabled={!!loadingSession||workspaceSwitching} onChanged={onHostChanged} onError={onError}/><div className="composer-model-controls"><button type="button" className="context-compress-button" disabled={!sessionId||sessionBusy||!!loadingSession||compressingContext} onClick={onCompress} title={t('chat.compressContext')} aria-label={t('chat.compressContext')}>{compressingContext?<LoaderCircle className="spin" size={13}/>:<Minimize2 size={13}/>}</button><ContextUsageRing usage={contextUsage}/><ComposerReasoningSelector providers={providers} disabled={sessionBusy} onChanged={onModelChanged} onError={onError}/><ComposerModelSelector providers={providers} fallbackModel={modelName} disabled={sessionBusy} onChanged={onModelChanged} onError={onError}/></div></div>
})

function ContextUsageRing({usage}:{usage:ContextUsage}){
	const {t,i18n:instance}=useTranslation()
	const known=usage.window>0
	const percent=known?Math.min(100,Math.max(0,Math.round(usage.tokens/usage.window*100))):0
	const label=t(known?'chat.contextUsage':'chat.contextUsageUnknown',{used:usage.tokens.toLocaleString(localeFor(instance.language)),limit:usage.window.toLocaleString(localeFor(instance.language))})
	return <span className={`context-usage-ring ${percent>=90?'danger':percent>=70?'warn':''}`} role={known?'meter':'status'} aria-label={label} aria-valuemin={known?0:undefined} aria-valuemax={known?usage.window:undefined} aria-valuenow={known?usage.tokens:undefined} title={label}>
		<svg viewBox="0 0 36 36" aria-hidden="true"><circle className="context-usage-track" cx="18" cy="18" r="15.5"/><circle className="context-usage-value" cx="18" cy="18" r="15.5" pathLength="100" strokeDasharray={`${percent} 100`}/></svg>
	</span>
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

function ComposerHostSelector({hosts,disabled,rootDisabled,onChanged,onError}:{hosts:Host[];disabled:boolean;rootDisabled:boolean;onChanged:(host:Host)=>void;onError:(message:string)=>void}){
	const {t}=useTranslation()
	const [open,setOpen]=useState(false)
	const [busy,setBusy]=useState('')
	const [rootBusy,setRootBusy]=useState('')
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
	const toggleRoot=async(host:Host)=>{
		if(rootDisabled||rootBusy||!hostSupportsRoot(host))return
		setRootBusy(host.id)
		try{onChanged(await api.setHostAgentRootAccess(host.id,!host.agent_root_enabled))}catch(err){onError(errorText(err))}
		finally{setRootBusy('')}
	}
	return <details ref={detailsRef} className="composer-selector composer-hosts" open={open} onToggle={event=>setOpen(event.currentTarget.open)}>
		<summary title={t('chat.switchHosts')} aria-label={t('chat.switchHosts')}><Server size={13}/><span>{label}</span>{activeHosts.some(host=>host.agent_root_enabled)&&<ShieldAlert size={12}/>}<ChevronRight size={11}/></summary>
		<div className="composer-selector-menu composer-host-menu">
			{hosts.map(host=>{const rootAvailable=hostSupportsRoot(host),rootLabel=rootAvailable?`${t('chat.rootAccess')} · ${t(host.agent_root_enabled?'common.enabled':'common.disabled')}`:`${t('chat.rootAccess')} · ${t('common.unavailable')}`;return <div className="composer-host-option" key={host.id}><button type="button" className={host.agent_enabled?'active':''} disabled={disabled||!!busy} onClick={()=>void toggle(host)}><span><Server size={13}/><b>{host.name}</b></span>{busy===host.id?<LoaderCircle className="spin" size={13}/>:<em>{t(host.agent_enabled?'common.disable':'common.enable')}</em>}</button><button type="button" className={`composer-host-root ${host.agent_root_enabled?'active':''}`} disabled={rootDisabled||!rootAvailable||!!rootBusy&&rootBusy!==host.id} onClick={()=>void toggleRoot(host)} title={rootLabel} aria-label={`${host.name} · ${rootLabel}`}>{rootBusy===host.id?<LoaderCircle className="spin" size={13}/>:<ShieldAlert size={13}/>}</button></div>})}
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
		<summary title={t('models.reasoningEffort')} aria-label={t('models.reasoningEffort')}>{busy?<LoaderCircle className="spin" size={13}/>:<BrainCircuit size={13}/>}<span>{current||'—'}</span><ChevronRight size={11}/></summary>
		<div className="composer-selector-menu composer-reasoning-menu">
			{reasoningEfforts.map(value=><button type="button" className={value===current?'active':''} disabled={!active||disabled||busy} onClick={()=>void apply(value)} key={value}><span><b>{value}</b></span>{value===current&&<Check size={13}/>}</button>)}
		</div>
	</details>
}

function FullAccessConfirmDialog({onCancel,onConfirm}:{onCancel:()=>void;onConfirm:()=>void}){
	const {t}=useTranslation()
	return createPortal(<div className="destructive-dialog-backdrop" onMouseDown={event=>{if(event.target===event.currentTarget)onCancel()}}><section className="destructive-dialog panel" role="dialog" aria-modal="true" aria-labelledby="full-access-dialog-title"><header><ShieldAlert size={21}/><h2 id="full-access-dialog-title">{t('settings.fullAccessTitle')}</h2></header><footer><button type="button" autoFocus onClick={onCancel}>{t('common.cancel')}</button><button type="button" className="danger" onClick={onConfirm}><ShieldAlert size={14}/>{t('common.enable')}</button></footer></section></div>,document.body)
}
