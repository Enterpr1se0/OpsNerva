import { invoke } from '@tauri-apps/api/core'
import { Code2 } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { SettingsDisclosure } from '../../../components/SettingsDisclosure'
import { useNotifier } from '../../../lib/notifications'
import { errorText } from '../../../lib/utils'

export function DesktopDeveloperToolsPanel() {
	const {t}=useTranslation()
	const notify=useNotifier()
	const open=async()=>{
		try{await invoke('open_developer_tools')}
		catch(error){notify(errorText(error),'error')}
	}
	return <SettingsDisclosure icon={<Code2 size={18}/>} title={t('settings.developerTools')}>
		<div className="settings-action-row"><button type="button" className="primary" onClick={()=>void open()}><Code2 size={15}/>{t('settings.openDeveloperTools')}</button></div>
	</SettingsDisclosure>
}
