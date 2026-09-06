import { useMemo, useState } from 'react'
import { api } from '../../api/api'
import { errorText } from '../../lib/utils'
import type { ChatEntry } from '../chat/types'
import { useChatCardDisclosure, type ChatDisclosurePositionHandler } from '../chat/useChatCardDisclosure'

export function useToolEventDisclosure(sessionID:string,initialEntry:ChatEntry,onDisclosure:ChatDisclosurePositionHandler){
	const disclosure=useChatCardDisclosure(onDisclosure,'tool-summary-chevron')
	const [fullContent,setFullContent]=useState('')
	const [loadingDetail,setLoadingDetail]=useState(false)
	const [detailError,setDetailError]=useState('')
	const [detailSourceID,setDetailSourceID]=useState(initialEntry.sourceMessageId)
	if(detailSourceID!==initialEntry.sourceMessageId){
		setDetailSourceID(initialEntry.sourceMessageId)
		setFullContent('');setLoadingDetail(false);setDetailError('')
	}
	const entry=useMemo(()=>fullContent?{...initialEntry,content:fullContent,contentTruncated:false}:initialEntry,[fullContent,initialEntry])
	const toggleExpanded=(summary:HTMLElement)=>{
		const opening=disclosure.toggle(summary)
		if(!opening||!initialEntry.contentTruncated||!initialEntry.sourceMessageId||loadingDetail||fullContent)return
		setLoadingDetail(true);setDetailError('')
		void api.chatMessage(sessionID,initialEntry.sourceMessageId).then(message=>setFullContent(message.content)).catch(error=>setDetailError(errorText(error))).finally(()=>setLoadingDetail(false))
	}

	return{entry,disclosure,loadingDetail,detailError,toggleExpanded}
}
