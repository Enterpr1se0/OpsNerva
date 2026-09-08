import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from '../../api/api'
import { useDocumentVisible } from '../../lib/hooks'
import { errorText } from '../../lib/utils'
import type { SFTPFileEntry } from '../../types'
import { remoteParentPath } from './utils'

export function useSFTPDirectory(hostID:string,active:boolean,uploadVersion:number){
	const visible=useDocumentVisible()
	const [path,setPath]=useState('')
	const [pathInput,setPathInput]=useState('')
	const [entries,setEntries]=useState<SFTPFileEntry[]>([])
	const [loading,setLoading]=useState(false)
	const [listError,setListError]=useState('')
	const currentPath=useRef('')
	const request=useRef<AbortController|null>(null)
	const requestedPath=useRef('')
	const enabled=useRef(false)
	const cancelLoad=useCallback(()=>{
		request.current?.abort()
		request.current=null
	},[])
	const load=useCallback(async(target=currentPath.current)=>{
		if(!hostID||!enabled.current)return
		cancelLoad()
		const controller=new AbortController()
		request.current=controller
		requestedPath.current=target
		setLoading(true);setListError('')
		try{
			const result=await api.sftpEntries(hostID,target,controller.signal)
			if(controller.signal.aborted)return
			currentPath.current=result.path
			setPath(result.path);setPathInput(result.path);setEntries(result.entries||[])
		}catch(err){
			if(controller.signal.aborted)return
			setEntries([]);setListError(errorText(err))
		}finally{
			if(request.current===controller){request.current=null;setLoading(false)}
		}
	},[cancelLoad,hostID])
	useEffect(()=>{
		enabled.current=active&&visible
		if(enabled.current){
			void load()
		}
		return()=>{enabled.current=false;cancelLoad()}
	},[active,visible,load,cancelLoad,uploadVersion])
	const reconcileDeletion=useCallback((entryPath:string,completed:boolean)=>{
		if(!enabled.current)return
		const target=request.current?requestedPath.current:currentPath.current
		if(!target){void load();return}
		const parent=remoteParentPath(entryPath)
		if(target===entryPath||target.startsWith(entryPath+'/')){void load(parent);return}
		if(target!==parent)return
		if(!completed||currentPath.current!==parent){void load(target);return}
		// An older directory snapshot must not restore a successfully deleted entry.
		cancelLoad();setLoading(false)
		setEntries(current=>current.filter(entry=>entry.path!==entryPath))
	},[cancelLoad,load])
	return {path,pathInput,setPathInput,entries,loading:active&&visible&&loading,listError,load,reconcileDeletion}
}
