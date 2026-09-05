import { invoke } from '@tauri-apps/api/core'
import { Code2, Minimize2, Monitor } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { SettingsDisclosure } from '../../../components/SettingsDisclosure'
import { useNotifier } from '../../../lib/notifications'
import { errorText } from '../../../lib/utils'

export function DesktopApplicationPanel() {
	const {t}=useTranslation()
	const notify=useNotifier()
	const invokeDesktop=async(command:'enter_lightweight_mode'|'open_developer_tools')=>{
		try{await invoke(command)}
		catch(error){notify(errorText(error),'error')}
	}
	return <SettingsDisclosure icon={<Monitor size={18}/>} title={t('settings.desktopApplication')}>
		<div className="settings-action-row">
			<button type="button" className="primary" onClick={()=>void invokeDesktop('enter_lightweight_mode')}><Minimize2 size={15}/>{t('settings.lightweightMode')}</button>
			<button type="button" onClick={()=>void invokeDesktop('open_developer_tools')}><Code2 size={15}/>{t('settings.openDeveloperTools')}</button>
		</div>
	</SettingsDisclosure>
}
