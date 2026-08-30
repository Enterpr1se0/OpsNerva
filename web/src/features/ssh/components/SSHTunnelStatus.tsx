import { FormEvent, useState } from 'react'
import { createPortal } from 'react-dom'
import { useTranslation } from 'react-i18next'
import { Cable, Edit3, LoaderCircle, Plus, Save, ShieldAlert, Square, X } from 'lucide-react'
import type { Host, SSHTunnel } from '../../../types'
import { api } from '../../../api/api'
import { AppSelect } from '../../../components/Controls'
import { errorText, formatFileSize, sshTunnelRoute } from '../../../lib/utils'
import { useAutoCollapseDetails } from '../../../lib/hooks'
import { localeFor } from '../../../lib/i18n'

export function SSHTunnelStatus({tunnels,hosts,open,onOpenChange,onStop,onCreated,onUpdated,onRefresh}:{tunnels:SSHTunnel[];hosts:Host[];open:boolean;onOpenChange:(open:boolean)=>void;onStop:(id:string)=>Promise<void>;onCreated:(tunnel:SSHTunnel)=>void;onUpdated:(previousID:string,tunnel:SSHTunnel)=>void;onRefresh:()=>void}){
	const {t,i18n:instance}=useTranslation()
	const [stopping,setStopping]=useState('')
	const [creating,setCreating]=useState(false)
	const [editing,setEditing]=useState<SSHTunnel|null>(null)
	const detailsRef=useAutoCollapseDetails(open,()=>onOpenChange(false))
	return <>
		<details ref={detailsRef} className="ssh-tunnel-status" open={open} onToggle={event=>onOpenChange(event.currentTarget.open)}>
			<summary title={t('tunnels.title')}><Cable size={14}/><span>{t('tunnels.short')}</span><em>{tunnels.length}</em></summary>
			<div className="ssh-tunnel-popover">
				<header><span><Cable size={15}/><b>{t('tunnels.title')}</b></span><button type="button" disabled={!hosts.length} onClick={()=>{onOpenChange(false);setCreating(true)}}><Plus size={13}/>{t('tunnels.create')}</button></header>
				<div>
					{tunnels.map(tunnel=><section className={`${tunnel.status} ${stopping===tunnel.id?'closing':''}`} key={tunnel.id}>
						<div className="ssh-tunnel-route"><i/><code>{sshTunnelRoute(tunnel.host_name||tunnel.host_id,tunnel.direction,tunnel.local_host,tunnel.local_port,tunnel.remote_host,tunnel.remote_port)}</code></div>
						<dl><div><dt>{t('common.status')}</dt><dd>{tunnel.status==='retrying'&&tunnel.reconnect_attempt?t('tunnels.reconnecting',{attempt:tunnel.reconnect_attempt}):t(`statusLabels.${tunnel.status}`,{defaultValue:tunnel.status})}</dd></div><div><dt>{t('tunnels.connections')}</dt><dd>{tunnel.active_connections} / {tunnel.total_connections}</dd></div><div><dt>{t('tunnels.traffic')}</dt><dd>↑ {formatFileSize(tunnel.bytes_sent)} · ↓ {formatFileSize(tunnel.bytes_received)}</dd></div><div><dt>{t('tunnels.started')}</dt><dd>{new Date(tunnel.started_at).toLocaleTimeString(localeFor(instance.language))}</dd></div></dl>
						<div className="ssh-tunnel-meta"><span>{tunnel.direction==='reverse'?'-R':'-L'} · {tunnel.proxy_used?t('tunnels.viaProxy'):t('tunnels.direct')}</span><code>{tunnel.id}</code><button className="edit" type="button" disabled={stopping===tunnel.id||tunnel.status==='retrying'} onClick={()=>{onOpenChange(false);setEditing(tunnel)}}><Edit3 size={12}/>{t('common.edit')}</button><button type="button" disabled={stopping===tunnel.id} onClick={async()=>{setStopping(tunnel.id);try{await onStop(tunnel.id)}finally{setStopping('')}}}>{stopping===tunnel.id?<LoaderCircle className="spin" size={12}/>:<Square size={10} fill="currentColor"/>}{t('tunnels.stop')}</button></div>
						{tunnel.error&&<p><ShieldAlert size={12}/>{tunnel.error}</p>}
					</section>)}
					{!tunnels.length&&<div className="ssh-tunnel-empty">{hosts.length?t('tunnels.empty'):t('connections.noHosts')}</div>}
				</div>
			</div>
		</details>
		{creating&&<SSHTunnelEditorDialog hosts={hosts} onCancel={()=>setCreating(false)} onSaved={tunnel=>{onCreated(tunnel);setCreating(false)}}/>}
		{editing&&<SSHTunnelEditorDialog hosts={hosts} tunnel={editing} onCancel={()=>setEditing(null)} onSaved={tunnel=>{onUpdated(editing.id,tunnel);setEditing(null)}} onFailed={onRefresh}/>}
	</>
}

export function SSHTunnelEditorDialog({hosts,tunnel,onCancel,onSaved,onFailed}:{hosts:Host[];tunnel?:SSHTunnel;onCancel:()=>void;onSaved:(tunnel:SSHTunnel)=>void;onFailed?:()=>void}){
	const {t}=useTranslation()
	const [hostID,setHostID]=useState(tunnel?.host_id||hosts[0]?.id||'')
	const [direction,setDirection]=useState<'local'|'reverse'>(tunnel?.direction||'local')
	const [localHost,setLocalHost]=useState(tunnel?.local_host||'127.0.0.1')
	const [localPort,setLocalPort]=useState(tunnel?String(tunnel.local_port):'')
	const [remoteHost,setRemoteHost]=useState(tunnel?.remote_host||'127.0.0.1')
	const [remotePort,setRemotePort]=useState(tunnel?String(tunnel.remote_port):'')
	const [busy,setBusy]=useState(false)
	const [error,setError]=useState('')
	const submit=async(event:FormEvent)=>{
		event.preventDefault()
		const remote=remotePort===''?0:Number(remotePort),local=localPort===''?0:Number(localPort)
		if(!hostID||!localHost.trim()||!remoteHost.trim()){setError(t('common.required'));return}
		const localInvalid=!Number.isInteger(local)||local<0||local>65535||(direction==='reverse'&&local===0)
		const remoteInvalid=!Number.isInteger(remote)||remote<0||remote>65535||(direction==='local'&&remote===0)
		if(localInvalid||remoteInvalid){setError(t('tunnels.portRange'));return}
		setBusy(true);setError('')
		try{
			const config={direction,local_host:localHost.trim(),local_port:local,remote_host:remoteHost.trim(),remote_port:remote}
			const saved=tunnel?await api.updateSSHTunnel(tunnel.id,{host_id:hostID,...config}):await api.startSSHTunnel({host_id:hostID,...config})
			onSaved(saved)
		}catch(err){setError(errorText(err));onFailed?.()}
		finally{setBusy(false)}
	}
	return createPortal(<div className="connection-dialog-backdrop" onMouseDown={event=>{if(event.target===event.currentTarget&&!busy)onCancel()}}>
		<form className="connection-dialog panel" role="dialog" aria-modal="true" aria-labelledby="tunnel-editor-title" noValidate onSubmit={submit}>
			<header><span><Cable size={20}/><span><small>{t('tunnels.title')}</small><h2 id="tunnel-editor-title">{t(tunnel?'tunnels.edit':'tunnels.create')}</h2></span></span><button type="button" disabled={busy} onClick={onCancel}><X size={16}/></button></header>
			<div className="connection-dialog-fields">
				<label><span>{t('common.host')}</span><AppSelect portal value={hostID} ariaLabel={t('common.host')} onChange={setHostID} options={hosts.map(host=>({value:host.id,label:`${host.name} · ${host.user}@${host.address}:${host.port}`}))}/></label>
				<label><span>{t('tunnels.direction')}</span><AppSelect portal value={direction} ariaLabel={t('tunnels.direction')} onChange={value=>setDirection(value as 'local'|'reverse')} options={[{value:'local',label:t('tunnels.localForward')},{value:'reverse',label:t('tunnels.reverseForward')}]}/></label>
				<label><span>{t(direction==='local'?'tunnels.localListenHost':'tunnels.localTargetHost')}</span><input value={localHost} onChange={event=>setLocalHost(event.target.value)}/></label>
				<label><span>{t(direction==='local'?'tunnels.localListenPort':'tunnels.localTargetPort')}</span><input inputMode="numeric" value={localPort} onChange={event=>setLocalPort(event.target.value.replace(/\D/g,'').slice(0,5))} placeholder={direction==='local'?t('tunnels.automaticPort'):undefined} autoFocus={direction==='reverse'}/></label>
				<label><span>{t(direction==='local'?'tunnels.remoteTargetHost':'tunnels.remoteListenHost')}</span><input value={remoteHost} onChange={event=>setRemoteHost(event.target.value)}/></label>
				<label><span>{t(direction==='local'?'tunnels.remoteTargetPort':'tunnels.remoteListenPort')}</span><input inputMode="numeric" value={remotePort} onChange={event=>setRemotePort(event.target.value.replace(/\D/g,'').slice(0,5))} placeholder={direction==='reverse'?t('tunnels.automaticPort'):undefined} autoFocus={direction==='local'}/></label>
			</div>
			{error&&<p className="connection-dialog-error"><ShieldAlert size={14}/>{error}</p>}
			<footer><button type="button" disabled={busy} onClick={onCancel}>{t('common.cancel')}</button><button className="primary" disabled={busy||!hostID}>{busy?<LoaderCircle className="spin" size={14}/>:tunnel?<Save size={14}/>:<Plus size={14}/>} {busy?t(tunnel?'common.saving':'tunnels.starting'):t(tunnel?'common.save':'tunnels.start')}</button></footer>
		</form>
	</div>,document.body)
}
