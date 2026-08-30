import { useTranslation } from 'react-i18next'
import { ChevronRight, LoaderCircle, Server } from 'lucide-react'
import type { Host } from '../../../types'

export function SSHHostHome({hosts,connectingHostID,onConnect}:{hosts:Host[];connectingHostID:string;onConnect:(host:Host)=>void}){
	const {t}=useTranslation()
	return <aside className="sftp-browser ssh-host-home panel">
		<header><div><Server size={17}/><b>{t('config.tabs.hosts')}</b></div><span className="sftp-host">{t('sshWorkspace.hostCount',{count:hosts.length})}</span></header>
		<div className="ssh-host-home-list">{hosts.map(host=>{
			const connecting=connectingHostID===host.id
			return <button type="button" disabled={!!connectingHostID} onClick={()=>onConnect(host)} key={host.id}><span className="ssh-host-home-icon">{connecting?<LoaderCircle className="spin" size={16}/>:<Server size={16}/>}</span><span><b>{host.name}</b><small>{host.user}@{host.address}:{host.port}</small></span><ChevronRight size={15}/></button>
		})}</div>
	</aside>
}
