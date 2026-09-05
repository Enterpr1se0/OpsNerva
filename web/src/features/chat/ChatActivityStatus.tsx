import { memo, useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { useDocumentVisible } from '../../lib/hooks'
import type { ConnectionRetryState, ModelRetryState } from './types'

const workStatusKeys=['chat.cooking','chat.pondering','chat.brewing','chat.weaving','chat.polishing','chat.crunching'] as const

export const ChatActivityStatus=memo(function ChatActivityStatus({visible,stopping,connectionRetry,modelRetry}:{visible:boolean;stopping:boolean;connectionRetry:ConnectionRetryState|null;modelRetry:ModelRetryState|null}){
	const {t}=useTranslation()
	const documentVisible=useDocumentVisible()
	const [workStatusIndex,setWorkStatusIndex]=useState(0)
	const [now,setNow]=useState(Date.now)
	const active=visible&&documentVisible
	const rotating=active&&!stopping&&!connectionRetry&&!modelRetry
	useEffect(()=>{
		if(!rotating)return
		const timer=window.setInterval(()=>setWorkStatusIndex(index=>(index+1)%workStatusKeys.length),2600)
		return()=>window.clearInterval(timer)
	},[rotating])
	const readyAt=connectionRetry?.readyAt
	useEffect(()=>{
		if(!active||stopping||readyAt===undefined)return
		let timer:number|undefined
		const tick=()=>{
			const time=Date.now()
			setNow(time)
			if(time<readyAt)timer=window.setTimeout(tick,Math.min(1000,readyAt-time))
		}
		tick()
		return()=>{if(timer!==undefined)window.clearTimeout(timer)}
	},[active,stopping,readyAt])
	const label=stopping?t('chat.stopping')
		:connectionRetry?t('chat.reconnecting',{attempt:connectionRetry.attempt,delay:Math.max(0,Math.ceil((connectionRetry.readyAt-now)/1000))})
		:modelRetry?t('chat.retryingModel',{attempt:modelRetry.attempt,max:modelRetry.max})
		:t(workStatusKeys[workStatusIndex])
	return <div className={`model-activity ${stopping?'stopping':''}`} role="status" aria-live="polite">
		<span className="model-activity-mark" aria-hidden="true">✻</span><b key={stopping||connectionRetry||modelRetry?'priority':workStatusIndex}>{label}</b>
	</div>
})
