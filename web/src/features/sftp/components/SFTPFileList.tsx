import { memo, useMemo } from 'react'
import { useTranslation } from 'react-i18next'
import { Download, Edit3, FileText, FolderOpen, LoaderCircle, Trash2 } from 'lucide-react'
import { VirtualFileList } from '../../../components/VirtualFileList'
import { localeFor } from '../../../lib/i18n'
import { formatFileSize } from '../../../lib/utils'
import type { SFTPFileEntry } from '../../../types'

type Props={entries:SFTPFileEntry[];active:boolean;loading:boolean;error:string;hostID:string;path:string;busy:boolean;mutating:boolean;transferring:boolean;openingFile:string;onOpen:(entry:SFTPFileEntry)=>void;onDownload:(entry:SFTPFileEntry)=>void;onRename:(entry:SFTPFileEntry)=>void;onDelete:(entry:SFTPFileEntry)=>void}
const noEntries:SFTPFileEntry[]=[]
const entryKey=(entry:SFTPFileEntry)=>`${entry.type}:${entry.path}`

export const SFTPFileList=memo(function SFTPFileList({entries,active,loading,error,hostID,path,busy,mutating,transferring,openingFile,onOpen,onDownload,onRename,onDelete}:Props){
	const {t,i18n}=useTranslation()
	const dateFormat=useMemo(()=>new Intl.DateTimeFormat(localeFor(i18n.language),{year:'numeric',month:'numeric',day:'numeric',hour:'numeric',minute:'2-digit',second:'2-digit'}),[i18n.language])
	return <VirtualFileList key={path} entries={!hostID||loading||error?noEntries:entries} active={active} className="sftp-list" label={t('sshWorkspace.remotePath')} rowHeight={54} entryKey={entryKey} empty={<span className={`sftp-state ${error?'error':''}`}>{!hostID?t('connections.noHosts'):loading?<><LoaderCircle className="spin" size={14}/>{t('common.loading')}</>:error||t('workspace.emptyDirectory')}</span>} renderEntry={entry=><div className="sftp-row">
		<button type="button" className="sftp-entry" disabled={busy} onClick={()=>onOpen(entry)} title={entry.path}>{openingFile===entry.path?<LoaderCircle className="spin" size={14}/>:entry.type==='directory'?<FolderOpen size={14}/>:<FileText size={14}/>}<span><b>{entry.name}</b><small>{entry.mode} · {entry.type==='directory'?'—':formatFileSize(entry.size||0)} · {dateFormat.format(new Date(entry.modified_at))}</small></span></button>
		{entry.type!=='directory'&&<button type="button" disabled={mutating||transferring} onClick={()=>onDownload(entry)} title={t('common.download')}><Download size={12}/></button>}
		<button type="button" disabled={mutating||transferring} onClick={()=>onRename(entry)} title={t('sshWorkspace.rename')}><Edit3 size={12}/></button>
		<button type="button" className="danger" disabled={mutating||transferring} onClick={()=>onDelete(entry)} title={t('common.delete')}><Trash2 size={12}/></button>
	</div>}/>
})
