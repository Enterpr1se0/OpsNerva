import { useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Home, LoaderCircle, Plus, Server, TerminalSquare, X } from 'lucide-react'
import { api } from '../../../api/api'
import { SSHHostHome, SSHShellCreateDialog, SSHShellTerminal } from '../../../features/ssh'
import { SFTPBrowser } from '../../../features/sftp'
import { errorText, errorStatus } from '../../../lib/utils'
import type { Host, SSHShell } from '../../../types'

export function SSHWorkspacePage({hosts,shells,onCreated,refresh,onError}:{hosts:Host[];shells:SSHShell[];onCreated:(shell:SSHShell)=>void;refresh:()=>Promise<void>;onError:(message:string)=>void}){
	const {t}=useTranslation()
	const [selectedShellID,setSelectedShellID]=useState(shells[0]?.id||'')
	const [creating,setCreating]=useState(false)
	const [connectingHostID,setConnectingHostID]=useState('')
	const closingShellIDsRef=useRef(new Set<string>())
	const [closingShellIDs,setClosingShellIDs]=useState<Set<string>>(new Set())
	const [dismissedShellIDs,setDismissedShellIDs]=useState<Set<string>>(new Set())
	const visibleShells=useMemo(()=>shells.filter(shell=>!dismissedShellIDs.has(shell.id)),[dismissedShellIDs,shells])
	const [previousShells,setPreviousShells]=useState(shells)
	if(previousShells!==shells){
		setPreviousShells(shells)
		if(selectedShellID&&!visibleShells.some(shell=>shell.id===selectedShellID))setSelectedShellID('')
		const listed=new Set(shells.map(shell=>shell.id))
		const retained=new Set([...dismissedShellIDs].filter(id=>listed.has(id)))
		if(retained.size!==dismissedShellIDs.size)setDismissedShellIDs(retained)
	}
	const selectedShell=visibleShells.find(shell=>shell.id===selectedShellID)
	const selectedHost=hosts.find(host=>host.id===selectedShell?.host_id)
	const created=(shell:SSHShell)=>{onCreated(shell);setSelectedShellID(shell.id);setCreating(false)}
	const connect=async(host:Host)=>{
		if(connectingHostID)return
		setConnectingHostID(host.id)
		try{created(await api.startSSHShell({host_id:host.id,surface:'workspace'}))}
		catch(err){onError(errorText(err))}
		finally{setConnectingHostID('')}
	}
	const close=async(shell:SSHShell)=>{
		if(closingShellIDsRef.current.has(shell.id))return
		closingShellIDsRef.current.add(shell.id)
		setClosingShellIDs(new Set(closingShellIDsRef.current))
		const dismiss=()=>{
			setDismissedShellIDs(current=>new Set(current).add(shell.id))
			setSelectedShellID(current=>current===shell.id?'':current)
		}
		try{
			await api.closeSSHShell(shell.id)
			dismiss()
			void refresh()
		}catch(err){
			if(errorStatus(err)===404){dismiss();void refresh()}
			else onError(errorText(err))
		}finally{
			closingShellIDsRef.current.delete(shell.id)
			setClosingShellIDs(new Set(closingShellIDsRef.current))
		}
	}
	if(!hosts.length)return <div className="ssh-workspace-empty panel"><Server size={28}/><b>{t('connections.noHosts')}</b></div>
	return <div className="ssh-workspace">
		{selectedHost?<SFTPBrowser key={selectedHost.id} host={selectedHost}/>:<SSHHostHome hosts={hosts} connectingHostID={connectingHostID} onConnect={connect}/>}
		<section className="ssh-workspace-terminal panel">
			<header className="ssh-terminal-tabs">
				<div><button type="button" className={`ssh-home-tab ${selectedShellID?'':'active'}`} onClick={()=>setSelectedShellID('')} title="Home" aria-label="Home"><Home size={16}/></button>{visibleShells.map(shell=>{const closing=closingShellIDs.has(shell.id);return <div className={`ssh-terminal-tab ${shell.id===selectedShellID?'active':''}`} key={shell.id}><button type="button" className="ssh-terminal-tab-select" disabled={closing} onClick={()=>setSelectedShellID(shell.id)}><i className={shell.status}/><span>{shell.host_name||shell.host_id}</span><small>{shell.elevated?'root':shell.user}</small></button><button type="button" className="ssh-terminal-tab-close" disabled={closing} onClick={event=>{event.stopPropagation();void close(shell)}} title={t('sshShell.closeSession')} aria-label={t('sshShell.closeSession')}>{closing?<LoaderCircle className="spin" size={11}/>:<X size={12}/>}</button></div>})}</div>
				<button type="button" className="ssh-new-terminal" onClick={()=>setCreating(true)}><Plus size={14}/> {t('sshWorkspace.newTerminal')}</button>
			</header>
			<div className="ssh-terminal-stage">
				{selectedShell?<SSHShellTerminal key={selectedShell.id} initialShell={selectedShell} embedded onClose={()=>setSelectedShellID('')} onChanged={()=>void refresh()} onError={onError}/>:<div className="ssh-terminal-empty"><TerminalSquare size={32}/><b>{t('sshWorkspace.noTerminal')}</b><button type="button" className="primary" onClick={()=>setCreating(true)}><Plus size={14}/> {t('sshWorkspace.newTerminal')}</button></div>}
			</div>
		</section>
		{creating&&<SSHShellCreateDialog hosts={hosts} surface="workspace" onCancel={()=>setCreating(false)} onCreated={created}/>}
	</div>
}
