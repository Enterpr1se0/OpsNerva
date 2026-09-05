import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Activity, Braces, Copy, LoaderCircle, Power, RefreshCw } from 'lucide-react'
import { api } from '../../../api/api'
import { SettingsDisclosure } from '../../../components/SettingsDisclosure'
import { useNotifier } from '../../../lib/notifications'
import { errorText } from '../../../lib/utils'
import type { SystemSettings } from '../../../types'

export function MCPServerModePanel({settings,onChanged,onOpenActivity}:{settings:SystemSettings|null;onChanged:(settings:SystemSettings)=>void;onOpenActivity:()=>void}){
	const {t}=useTranslation()
	const notify=useNotifier()
	const [busy,setBusy]=useState<'start'|'stop'|'rotate'|''>(''),[token,setToken]=useState('')
	const enabled=!!settings?.mcp_http_enabled
	const endpoint=`${window.location.origin}/mcp`
	const [wasEnabled,setWasEnabled]=useState(enabled)
	if(wasEnabled!==enabled){setWasEnabled(enabled);if(!enabled)setToken('')}
	const update=async(nextEnabled:boolean,rotate=false)=>{
		if(!settings)return
		setBusy(rotate?'rotate':nextEnabled?'start':'stop')
		try{
			const result=await api.saveSystemSettings({
				agent_max_iterations:settings.agent_max_iterations,
				mcp_http_enabled:nextEnabled,
				rotate_mcp_http_token:rotate,
			})
			setToken(result.mcp_http_token||'')
			onChanged(result)
			notify(t(nextEnabled?'mcpServerMode.started':'mcpServerMode.stopped'))
		}catch(err){notify(errorText(err),'error')}finally{setBusy('')}
	}
	const copy=async(value:string,message:string)=>{
		try{await navigator.clipboard.writeText(value);notify(message)}
		catch(err){notify(errorText(err),'error')}
	}
	return <SettingsDisclosure className="mcp-server-mode" icon={<Braces size={18}/>} title={t('mcpServerMode.title')} meta={enabled?t('common.enabled'):t('common.disabled')}>
		<div className="mcp-server-mode-fields">
			<label><span>{t('mcpServerMode.endpoint')}</span><div><input readOnly value={endpoint}/><button type="button" title={t('common.copy')} onClick={()=>void copy(endpoint,t('mcpServerMode.endpointCopied'))}><Copy size={13}/></button></div></label>
			{enabled&&<label><span>{t('mcpServerMode.token')}</span><div><input readOnly type={token?'text':'password'} value={token||'••••••••••••••••'} /><button type="button" disabled={!token} title={t('common.copy')} onClick={()=>void copy(token,t('mcpServerMode.tokenCopied'))}><Copy size={13}/></button></div>{!token&&settings?.mcp_http_token_configured&&<small>{t('mcpServerMode.tokenStored')}</small>}</label>}
		</div>
		<footer>
			<button type="button" onClick={onOpenActivity}><Activity size={13}/>{t('mcpServerMode.activity')}</button>
			{enabled&&<button type="button" disabled={!!busy} onClick={()=>void update(true,true)}>{busy==='rotate'?<LoaderCircle className="spin" size={13}/>:<RefreshCw size={13}/>} {t('mcpServerMode.rotate')}</button>}
			<button type="button" className={enabled?'danger':'primary'} disabled={!!busy||!settings} onClick={()=>void update(!enabled)}>{busy?<LoaderCircle className="spin" size={13}/>:<Power size={13}/>} {t(enabled?'mcpServerMode.stop':'mcpServerMode.start')}</button>
		</footer>
	</SettingsDisclosure>
}
