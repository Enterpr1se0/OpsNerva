import { FormEvent, useState } from 'react'
import { createPortal } from 'react-dom'
import { useTranslation } from 'react-i18next'
import { invoke } from '@tauri-apps/api/core'
import { Activity, Check, ChevronRight, CircleDot, Edit3, KeyRound, LoaderCircle, LogOut, Plus, RefreshCw, Save, ShieldAlert, Trash2, X, Zap } from 'lucide-react'
import { api } from '../../../api/api'
import { AppSelect } from '../../../components/Controls'
import { localeFor } from '../../../lib/i18n'
import { useNotifier } from '../../../lib/notifications'
import { DestructiveConfirmDialog } from '../../../components/DestructiveConfirmDialog'
import { desktopRuntime, errorText } from '../../../lib/utils'
import type { MCPServer, MCPServerInput, MCPTransport } from '../../../types'
import { Empty, FloatingPageActions } from '../../../components/PageLayout'
import { parseMCPImport, parseMCPPairs } from '../mcpConfiguration'

type MCPFormState = {
	id:string;name:string;transport:MCPTransport;command:string;argsText:string;cwd:string;url:string;envText:string;headersText:string;enabled:boolean;clearEnv:boolean;clearHeaders:boolean
}

const mcpImportExample=JSON.stringify({mcpServers:{'cloudflare-api':{url:'https://mcp.cloudflare.com/mcp'}}},null,2)

export function MCPServersPage({servers,refreshServers,refreshToolCatalog}:{servers:MCPServer[];refreshServers:()=>Promise<void>;refreshToolCatalog:()=>Promise<void>}){
	const {t,i18n:instance}=useTranslation()
	const notify=useNotifier()
	const [form,setForm]=useState<MCPFormState|null>(null)
	const [importConfig,setImportConfig]=useState<string|null>(null)
	const [busy,setBusy]=useState('')
	const [error,setError]=useState('')
	const [deleteCandidate,setDeleteCandidate]=useState<MCPServer|null>(null)
	const [authorizing,setAuthorizing]=useState('')
	const refresh=async()=>{await Promise.all([refreshServers(),refreshToolCatalog()])}
	const openCreate=()=>{setForm(null);setImportConfig('');setError('')}
	const closeCreate=()=>{if(busy==='import')return;setImportConfig(null);setError('')}
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
		return <div className="mcp-page page-stack has-floating-actions">
			{importConfig===null&&!form&&<FloatingPageActions><button className="primary" onClick={openCreate}><Plus size={15}/>{t('mcp.add')}</button></FloatingPageActions>}
		{error&&importConfig===null&&<div className="skill-error"><ShieldAlert size={15}/>{error}<button onClick={()=>setError('')}><X size={14}/></button></div>}
		{importConfig!==null&&createPortal(<div className="connection-dialog-backdrop" onMouseDown={event=>{if(event.target===event.currentTarget)closeCreate()}}><form className="mcp-form mcp-import-form extension-dialog panel" role="dialog" aria-modal="true" aria-labelledby="mcp-import-title" onSubmit={importServers}><header><div><Zap size={19}/><span><h3 id="mcp-import-title">{t('mcp.importConfig')}</h3></span></div><button type="button" disabled={busy==='import'} onClick={closeCreate} title={t('common.close')}><X size={15}/></button></header><div className="mcp-import-body"><textarea autoFocus spellCheck={false} aria-label={t('mcp.config')} value={importConfig} onChange={event=>setImportConfig(event.target.value)} placeholder={mcpImportExample}/></div>{error&&<div className="connection-dialog-error" role="alert"><ShieldAlert size={14}/><span>{error}</span></div>}<footer><span className="mcp-form-spacer"/><button type="button" disabled={busy==='import'} onClick={closeCreate}>{t('common.cancel')}</button><button className="primary" disabled={busy==='import'||!importConfig.trim()}>{busy==='import'?<LoaderCircle className="spin" size={14}/>:<Plus size={14}/>} {busy==='import'?t('mcp.importing'):t('mcp.import')}</button></footer></form></div>,document.body)}
		{form&&<form className="mcp-form panel" noValidate onSubmit={save}><header><div><Zap size={19}/><span><h3>{form.name||t('mcp.server')}</h3></span></div><button type="button" onClick={()=>setForm(null)} title={t('common.close')}><X size={15}/></button></header><div className="mcp-form-grid"><label><span>{t('mcp.displayName')}</span><input value={form.name} onChange={event=>setForm({...form,name:event.target.value})}/></label><label><span>{t('mcp.transport')}</span><AppSelect value={form.transport} ariaLabel={t('mcp.transport')} onChange={value=>setForm({...form,transport:value as MCPTransport})} options={[{value:'stdio',label:t('mcp.localProcess')},{value:'streamable_http',label:'Streamable HTTP'}]}/></label>{form.transport==='stdio'?<><label><span>{t('mcp.command')}</span><input value={form.command} onChange={event=>setForm({...form,command:event.target.value})}/></label><label><span>{t('mcp.cwd')}</span><input value={form.cwd} onChange={event=>setForm({...form,cwd:event.target.value})}/></label><label className="mcp-wide"><span>{t('mcp.args')}</span><textarea value={form.argsText} onChange={event=>setForm({...form,argsText:event.target.value})}/></label></>:<label className="mcp-wide"><span>{t('mcp.endpoint')}</span><input value={form.url} onChange={event=>setForm({...form,url:event.target.value})}/></label>}<label className="mcp-wide"><span>{t('mcp.env')}</span><textarea value={form.envText} onChange={event=>setForm({...form,envText:event.target.value,clearEnv:false})} placeholder={t('mcp.preserve')}/><small><label><input type="checkbox" checked={form.clearEnv} onChange={event=>setForm({...form,clearEnv:event.target.checked,envText:event.target.checked?'':form.envText})}/> {t('mcp.clearEnv')}</label></small></label><label className="mcp-wide"><span>{t('mcp.headers')}</span><textarea value={form.headersText} onChange={event=>setForm({...form,headersText:event.target.value,clearHeaders:false})} placeholder={t('mcp.preserve')}/><small><label><input type="checkbox" checked={form.clearHeaders} onChange={event=>setForm({...form,clearHeaders:event.target.checked,headersText:event.target.checked?'':form.headersText})}/> {t('mcp.clearHeaders')}</label></small></label></div><footer><label className="mcp-enable-on-save"><input type="checkbox" checked={form.enabled} onChange={event=>setForm({...form,enabled:event.target.checked})}/><i/><span><b>{t('mcp.enableAfterSave')}</b></span></label><button type="button" onClick={()=>setForm(null)}>{t('common.cancel')}</button><button className="primary" disabled={busy==='save'}>{busy==='save'?<LoaderCircle className="spin" size={14}/>:<Save size={14}/>} {busy==='save'?t('common.saving'):t('mcp.saveServer')}</button></footer></form>}
		<div className="mcp-grid">{servers.map(server=><article className={`mcp-card panel ${server.status}`} key={server.id}><header><div className="mcp-card-icon"><Zap size={19}/></div><span><h3>{server.name}</h3><code>{server.transport==='stdio'?server.command:server.url}</code></span><em className={server.status}><CircleDot size={9}/>{t(`statusLabels.${server.status}`,{defaultValue:server.status})}</em></header><dl><div><dt>{t('mcp.discoveredTools')}</dt><dd>{server.tool_count}</dd></div><div><dt>{t('mcp.secrets')}</dt><dd>{server.oauth_configured?t('mcp.oauth'):t('mcp.configuredSecrets',{count:(server.env_keys?.length||0)+(server.header_keys?.length||0)})}</dd></div><div><dt>{t('mcp.lastConnected')}</dt><dd>{server.connected_at?new Date(server.connected_at).toLocaleString(localeFor(instance.language)):'—'}</dd></div></dl>{server.last_error&&<div className="mcp-card-error"><ShieldAlert size={13}/><span>{server.last_error}</span></div>}<div className="mcp-actions"><button onClick={()=>void test(server)} disabled={!!busy||authorizing===server.id}><Activity size={13}/>{busy===`test-${server.id}`?t('common.testing'):t('common.test')}</button><button onClick={()=>openEdit(server)} disabled={!!busy||authorizing===server.id}><Edit3 size={13}/>{t('common.edit')}</button>{server.transport==='streamable_http'&&(server.status==='error'||server.oauth_configured)&&<button onClick={()=>void authorize(server)} disabled={!!busy||!!authorizing}><KeyRound size={13}/>{authorizing===server.id?t('mcp.authorizing'):server.oauth_configured?t('mcp.reauthorize'):t('mcp.authorize')}</button>}{server.oauth_configured&&<button title={t('mcp.clearAuthorization')} onClick={()=>void clearOAuth(server)} disabled={!!busy||!!authorizing}><LogOut size={13}/></button>}{server.enabled&&server.status!=='ready'&&authorizing!==server.id&&<button onClick={()=>void retry(server)} disabled={!!busy}><RefreshCw className={busy===`retry-${server.id}`?'spin':''} size={13}/>{t('common.retry')}</button>}<button className={server.enabled?'disable':'enable'} onClick={()=>void toggle(server)} disabled={!!busy||authorizing===server.id}>{busy===`toggle-${server.id}`?<LoaderCircle className="spin" size={13}/>:server.enabled?<X size={13}/>:<Check size={13}/>} {server.enabled?t('common.disable'):t('common.enable')}</button><button className="danger" title={t('common.delete')} onClick={()=>setDeleteCandidate(server)} disabled={!!busy||authorizing===server.id}><Trash2 size={13}/></button></div>{server.tools?.length?<details className="mcp-tools"><summary>{t('mcp.modelTools',{count:server.tools.length})} <ChevronRight size={13}/></summary><div>{server.tools.map(item=><section key={item.exposed_name}><code>{item.exposed_name}</code><span>{t('mcp.remote')} · {item.name}</span><p>{item.description}</p></section>)}</div></details>:null}</article>)}</div>
		{!servers.length&&<Empty icon={<Zap/>} title={t('mcp.emptyTitle')}/>}
		{deleteCandidate&&<DestructiveConfirmDialog title={t('mcp.deleteTitle',{name:deleteCandidate.name})} busy={busy===`delete-${deleteCandidate.id}`} onCancel={()=>setDeleteCandidate(null)} onConfirm={()=>void remove()}/>}
	</div>
}
