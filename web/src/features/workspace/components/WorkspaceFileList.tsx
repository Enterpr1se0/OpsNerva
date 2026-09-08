import { memo } from 'react'
import { useTranslation } from 'react-i18next'
import { Download, FileText, FolderOpen, FolderOutput, LoaderCircle, Trash2 } from 'lucide-react'
import { VirtualFileList } from '../../../components/VirtualFileList'
import { desktopRuntime, formatFileSize } from '../../../lib/utils'
import type { WorkspaceFileEntry } from '../../../types'
import { workspaceChildPath } from '../utils'

type Props={entries:WorkspaceFileEntry[];active:boolean;loading:boolean;error:string;path:string;writable:boolean;transferring:boolean;opening:string;deleting:string;onOpen:(name:string,type:WorkspaceFileEntry['type'])=>void;onDownload:(path:string,name:string,size:number)=>void;onReveal:(path:string)=>void;onDelete:(name:string,type:WorkspaceFileEntry['type'])=>void}
const noEntries:WorkspaceFileEntry[]=[]
const entryKey=(entry:WorkspaceFileEntry)=>`${entry.type}:${entry.name}`

export const WorkspaceFileList=memo(function WorkspaceFileList({entries,active,loading,error,path,writable,transferring,opening,deleting,onOpen,onDownload,onReveal,onDelete}:Props){
	const {t}=useTranslation()
	return <VirtualFileList key={path} entries={loading||error?noEntries:entries} active={active} className="workspace-file-list" label={t('workspace.relativePath')} rowHeight={36} entryKey={entryKey} empty={<span className={`workspace-files-state ${error?'error':''}`}>{loading?<><LoaderCircle className="spin" size={13}/>{t('common.loading')}</>:error||t('workspace.emptyDirectory')}</span>} renderEntry={entry=>{
		const fullPath=workspaceChildPath(path,entry.name)
		return <div className="workspace-file-row">
			<button className="workspace-file-open" onClick={()=>onOpen(entry.name,entry.type)} title={entry.type==='file'?t('workspace.previewFile'):t('workspace.openDirectory')}>{opening===fullPath?<LoaderCircle className="spin" size={13}/>:entry.type==='directory'?<FolderOpen size={13}/>:<FileText size={13}/>}<span>{entry.name}</span>{entry.type==='file'&&<small>{formatFileSize(entry.size??0)}</small>}</button>
			{(entry.type==='file'||desktopRuntime||writable)&&<div className="workspace-file-actions">
				{entry.type==='file'&&<button className="workspace-file-download" disabled={transferring} onClick={()=>onDownload(fullPath,entry.name,entry.size??0)} title={t('common.download')}><Download size={12}/></button>}
				{desktopRuntime&&entry.type==='directory'&&<button className="workspace-file-reveal" onClick={()=>onReveal(fullPath)} title={t('workspace.revealDirectory')}><FolderOutput size={12}/></button>}
				{writable&&<button className="workspace-file-delete" onClick={()=>onDelete(entry.name,entry.type)} disabled={deleting===fullPath||transferring} title={t('workspace.deleteEntry',{type:t(`workspace.${entry.type}`)})}><Trash2 size={12}/></button>}
			</div>}
		</div>
	}}/>
})
