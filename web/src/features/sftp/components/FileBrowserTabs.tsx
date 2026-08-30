import { useTranslation } from 'react-i18next'
import { FolderOpen, Server } from 'lucide-react'
import type { FileBrowserMode } from '../types'

export function FileBrowserTabs({mode,onChange}:{mode:FileBrowserMode;onChange:(mode:FileBrowserMode)=>void}){
	const {t}=useTranslation()
	return <div className="file-browser-tabs" role="tablist"><button type="button" className={mode==='workspace'?'active':''} role="tab" aria-selected={mode==='workspace'} onClick={()=>onChange('workspace')}><FolderOpen size={14}/><span>{t('common.workspace')}</span></button><button type="button" className={mode==='sftp'?'active':''} role="tab" aria-selected={mode==='sftp'} onClick={()=>onChange('sftp')}><Server size={14}/><span>SFTP</span></button></div>
}
