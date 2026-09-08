import { useCallback, useEffect, useEffectEvent, useRef, useState } from 'react'
import { api, workspaceFileEventsURL } from '../../api/api'
import { errorText } from '../../lib/utils'
import type { WorkspaceFileEntry } from '../../types'

export function useWorkspaceDirectory(workspaceID:string,path:string,active:boolean,uploadVersion:number,onChange:()=>void){
	const [entries,setEntries]=useState<WorkspaceFileEntry[]>([])
	const [loading,setLoading]=useState(false)
	const [error,setError]=useState('')
	const enabled=useRef(false)
	const request=useRef<AbortController|null>(null)
	const invalidated=useRef(false)
	const changed=useEffectEvent(onChange)
	const load=useCallback(async(showLoading=true)=>{
		if(!enabled.current||!workspaceID)return
		// Coalesce watch events during a read into one follow-up, without starving
		// directory results by repeatedly cancelling them during a folder upload.
		if(!showLoading&&request.current){invalidated.current=true;return}
		request.current?.abort()
		const controller=new AbortController()
		request.current=controller
		invalidated.current=false
		if(showLoading)setLoading(true)
		try{
			do {
				invalidated.current=false
				const result=await api.workspaceFiles(workspaceID,path,controller.signal)
				if(controller.signal.aborted)return
				setEntries(result.entries||[]);setError('');setLoading(false)
			}while(invalidated.current&&enabled.current)
		}catch(err){
			if(!controller.signal.aborted){setEntries([]);setError(errorText(err))}
		}finally{if(request.current===controller){request.current=null;setLoading(false)}}
	},[workspaceID,path])
	useEffect(()=>{
		enabled.current=active
		if(!active||!workspaceID)return
		const source=new EventSource(workspaceFileEventsURL(workspaceID,path))
		const sync=()=>{void load(false);changed()}
		source.addEventListener('workspace-change',sync)
		source.onopen=sync
		// eslint-disable-next-line react-hooks/set-state-in-effect -- Start loading with the directory request; results do not restart the subscription.
		void load()
		return()=>{
			enabled.current=false;invalidated.current=false
			request.current?.abort();request.current=null
			source.removeEventListener('workspace-change',sync);source.close()
		}
	},[active,workspaceID,path,load])
	const observedUpload=useRef(uploadVersion)
	useEffect(()=>{
		if(observedUpload.current===uploadVersion)return
		observedUpload.current=uploadVersion
		void load(false)
	},[uploadVersion,load])
	return {entries,loading:active&&loading,error,load}
}
