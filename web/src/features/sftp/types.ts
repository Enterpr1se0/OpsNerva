import type { SFTPFileEntry } from '../../types'
import type { SFTPDeletionManager } from './useSFTPDeletion'
import type { FileTransferStore } from './transferStore'

export type SFTPNameEditor={mode:'create'}|{mode:'rename';entry:SFTPFileEntry}
export type SFTPDeleteCandidate={entry:SFTPFileEntry}
export type SFTPOverwriteCandidate={file:File;path:string;directory:string}
export type SFTPTextEncoding='utf-8'|'utf-16le'|'utf-16be'|'gb18030'
export type SFTPTextFile={entry:SFTPFileEntry;content:string;binary:boolean;encoding:SFTPTextEncoding}
export type ActiveFileTransfer={operation:'upload'|'download';name:string;loaded:number;total:number;index?:number;count?:number}
export type FileTransferRecord={active:ActiveFileTransfer|null;conflict:SFTPOverwriteCandidate|null;uploadVersion:number}
export type FileTransferState={operation:ActiveFileTransfer['operation']|null;uploadVersion:number}
export type WorkspaceTransferItem={type:'file';file:File;path:string}|{type:'directory';path:string}
export type WorkspaceTransferSource=WorkspaceTransferItem[]|((signal:AbortSignal)=>Promise<WorkspaceTransferItem[]>)
export type FileTransferManager={
	deletions:SFTPDeletionManager
	store:FileTransferStore
	uploadSFTP:(hostID:string,directory:string,files:File[])=>void
	downloadSFTP:(hostID:string,entry:SFTPFileEntry)=>void
	uploadWorkspace:(workspaceID:string,source:WorkspaceTransferSource)=>boolean
	downloadWorkspace:(workspaceID:string,path:string,name:string,size:number)=>void
	cancel:(key:string)=>void
}
export type FileBrowserMode='workspace'|'sftp'
