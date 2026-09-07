import type { WorkspaceTransferItem } from '../sftp/types'
import { workspaceChildPath } from './utils'

// Emit each parent before its children and keep empty directories from drops.
function uploadEntries(directory:string){
	const items:WorkspaceTransferItem[]=[]
	const directories=new Set<string>()
	const addDirectory=(relativePath:string)=>{
		const parts=relativePath.split('/')
		let parent=''
		for(const part of parts){
			parent=parent?`${parent}/${part}`:part
			if(directories.has(parent))continue
			directories.add(parent)
			items.push({type:'directory',path:workspaceChildPath(directory,parent)})
		}
	}
	const addFile=(file:File,relativePath:string)=>{
		const separator=relativePath.lastIndexOf('/')
		if(separator>=0)addDirectory(relativePath.slice(0,separator))
		items.push({type:'file',file,path:workspaceChildPath(directory,relativePath)})
	}
	return{items,addDirectory,addFile}
}

export function workspaceUploadFiles(files:File[],directory:string){
	const entries=uploadEntries(directory)
	for(const file of files)entries.addFile(file,file.webkitRelativePath||file.name)
	return entries.items
}

// Capture entries synchronously: the browser protects DataTransfer after drop.
export function workspaceUploadDrop(dataTransfer:DataTransfer,directory:string){
	const roots=Array.from(dataTransfer.items).filter(item=>item.kind==='file').map(item=>item.webkitGetAsEntry()||item.getAsFile())
	return async(signal:AbortSignal)=>{
		const entries=uploadEntries(directory)
		const visit=async(entry:FileSystemEntry,parent:string):Promise<void>=>{
			signal.throwIfAborted()
			const relativePath=parent?`${parent}/${entry.name}`:entry.name
			if(entry.isFile){
				const file=await new Promise<File>((resolve,reject)=>(entry as FileSystemFileEntry).file(resolve,reject))
				entries.addFile(file,relativePath)
				return
			}
			entries.addDirectory(relativePath)
			const reader=(entry as FileSystemDirectoryEntry).createReader()
			// readEntries may return only one batch (e.g. 100 entries in Chromium).
			while(true){
				signal.throwIfAborted()
				const children=await new Promise<FileSystemEntry[]>((resolve,reject)=>reader.readEntries(resolve,reject))
				if(!children.length)break
				for(const child of children)await visit(child,relativePath)
			}
		}
		for(const root of roots){
			signal.throwIfAborted()
			if(root instanceof File)entries.addFile(root,root.name)
			else if(root)await visit(root,'')
		}
		return entries.items
	}
}
