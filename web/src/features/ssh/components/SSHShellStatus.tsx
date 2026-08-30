import { FormEvent, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { useTranslation } from 'react-i18next'
import { ChevronRight, LoaderCircle, Plus, Power, ShieldAlert, TerminalSquare, X } from 'lucide-react'
import type { Host, SSHShell } from '../../../types'
import { api } from '../../../api/api'
import { AppSelect } from '../../../components/Controls'
import { errorText } from '../../../lib/utils'
import { useAutoCollapseDetails } from '../../../lib/hooks'

export function SSHShellStatus({shells,hosts,open,onOpenChange,onOpen,onClose,onCreated}:{shells:SSHShell[];hosts:Host[];open:boolean;onOpenChange:(open:boolean)=>void;onOpen:(shell:SSHShell)=>void;onClose:(id:string)=>Promise<void>;onCreated:(shell:SSHShell)=>void}){
	const {t}=useTranslation()
	const [creating,setCreating]=useState(false)
	const closingShellIDsRef=useRef(new Set<string>())
	const [closingShellIDs,setClosingShellIDs]=useState<Set<string>>(new Set())
	const detailsRef=useAutoCollapseDetails(open,()=>onOpenChange(false))
	const close=async(id:string)=>{
		if(closingShellIDsRef.current.has(id))return
		closingShellIDsRef.current.add(id)
		setClosingShellIDs(new Set(closingShellIDsRef.current))
		try{await onClose(id)}
		finally{
			closingShellIDsRef.current.delete(id)
			setClosingShellIDs(new Set(closingShellIDsRef.current))
		}
	}
	return <>
		<details ref={detailsRef} className="ssh-shell-status" open={open} onToggle={event=>onOpenChange(event.currentTarget.open)}>
			<summary title={t('sshShell.title')}><TerminalSquare size={14}/><span>{t('sshShell.short')}</span><em>{shells.length}</em></summary>
			<div className="ssh-shell-popover">
				<header><span><TerminalSquare size={15}/><b>{t('sshShell.title')}</b></span><button type="button" disabled={!hosts.length} onClick={()=>{onOpenChange(false);setCreating(true)}}><Plus size={13}/>{t('sshShell.create')}</button></header>
				<div>
					{shells.map(shell=>{const closing=closingShellIDs.has(shell.id);return <section className={`ssh-shell-entry ${shell.status} ${closing?'closing':''}`} key={shell.id}>
						<button type="button" className="ssh-shell-open" disabled={closing} onClick={()=>onOpen(shell)}>
							<span><i/><b>{shell.kind==='workspace'?`${t('common.workspace')} · ${shell.workspace_id}`:shell.host_name||shell.host_id}</b><code>{shell.kind==='workspace'?t('workspace.agent'):shell.elevated?'root':shell.user}</code></span>
							<small>{shell.cwd||(shell.kind==='workspace'?'.':'~')}</small>
							<ChevronRight size={14}/>
						</button>
						<button type="button" className="ssh-shell-quick-close" disabled={closing} onClick={()=>void close(shell.id)} title={t('sshShell.closeSession')} aria-label={t('sshShell.closeSession')}>{closing?<LoaderCircle className="spin" size={12}/>:<Power size={13}/>}</button>
					</section>})}
					{!shells.length&&<div className="ssh-shell-empty">{hosts.length?t('sshShell.empty'):t('connections.noHosts')}</div>}
				</div>
			</div>
		</details>
		{creating&&<SSHShellCreateDialog hosts={hosts} onCancel={()=>setCreating(false)} onCreated={shell=>{onCreated(shell);setCreating(false)}}/>}
	</>
}

export function SSHShellCreateDialog({hosts,surface,onCancel,onCreated}:{hosts:Host[];surface?:'quick'|'workspace';onCancel:()=>void;onCreated:(shell:SSHShell)=>void}){
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
