import type { SFTPFileEntry } from '../../types'

export type SFTPNameEditor={mode:'create'}|{mode:'rename';entry:SFTPFileEntry}
export type SFTPDeleteCandidate={entry:SFTPFileEntry}
export type SFTPOverwriteCandidate={file:File;path:string;directory:string}
export type SFTPTextEncoding='utf-8'|'utf-16le'|'utf-16be'|'gb18030'
export type SFTPTextFile={entry:SFTPFileEntry;content:string;binary:boolean;encoding:SFTPTextEncoding}
export type ActiveFileTransfer={operation:'upload'|'download';name:string;loaded:number;total:number;index?:number;count?:number}
export type FileTransferRecord={active:ActiveFileTransfer|null;conflict:SFTPOverwriteCandidate|null;uploadVersion:number}
export type WorkspaceTransferItem={file:File;path:string}
export type FileTransferManager={
	record:(key:string)=>FileTransferRecord
	subscribe:(key:string,listener:()=>void)=>(()=>void)
	uploadSFTP:(hostID:string,directory:string,files:File[])=>void
	downloadSFTP:(hostID:string,entry:SFTPFileEntry)=>void
	uploadWorkspace:(workspaceID:string,items:WorkspaceTransferItem[])=>boolean
	downloadWorkspace:(workspaceID:string,path:string,name:string,size:number)=>void
	cancel:(key:string)=>void
}
export type FileBrowserMode='workspace'|'sftp'
