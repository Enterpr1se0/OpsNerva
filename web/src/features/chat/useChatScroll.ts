import { useCallback, useEffect, useRef } from 'react'
import type { ChatDisclosurePositionHandler } from './useChatCardDisclosure'

type ChatScrollOptions={visible:boolean;latestConversationEntryID:string;loadingSession:string;sessionId:string}

export function useChatScroll({visible,latestConversationEntryID,loadingSession,sessionId}:ChatScrollOptions){
	const messagesRef=useRef<HTMLDivElement>(null)
  const stickToLatest=useRef(true)
	const lastMessagesScrollTop=useRef(0)
	const disclosureScrollFrame=useRef(0)
	const activeChatDisclosures=useRef(new Set<symbol>())
	const autoScrollFrame=useRef(0)

	useEffect(()=>()=>{window.cancelAnimationFrame(disclosureScrollFrame.current);window.cancelAnimationFrame(autoScrollFrame.current);activeChatDisclosures.current.clear();messagesRef.current?.classList.remove('chat-disclosure-active')},[])
	useEffect(()=>{
		window.cancelAnimationFrame(autoScrollFrame.current)
		if(!visible||!stickToLatest.current)return
		autoScrollFrame.current=window.requestAnimationFrame(()=>{
			const container=messagesRef.current
			if(container&&stickToLatest.current){container.scrollTop=container.scrollHeight;lastMessagesScrollTop.current=container.scrollTop}
		})
		return()=>window.cancelAnimationFrame(autoScrollFrame.current)
	},[latestConversationEntryID,loadingSession,sessionId,visible])

	const trackUserScroll=useCallback(()=>{
		const container=messagesRef.current
		if(!container)return
		const nextScrollTop=container.scrollTop
		const movingUp=nextScrollTop<lastMessagesScrollTop.current-1
		const atLatest=container.scrollHeight-nextScrollTop-container.clientHeight<24
		if(movingUp)stickToLatest.current=false
		else if(atLatest)stickToLatest.current=true
		lastMessagesScrollTop.current=nextScrollTop
	},[])
	const pauseLatestOnWheel=useCallback((event:React.WheelEvent<HTMLDivElement>)=>{if(event.deltaY<0)stickToLatest.current=false},[])
	const pauseLatest=useCallback(()=>{stickToLatest.current=false},[])

	const preserveChatDisclosurePosition=useCallback<ChatDisclosurePositionHandler>((disclosure,summary,holdAnchor)=>{
		const container=messagesRef.current
		if(!container)return
		if(holdAnchor)activeChatDisclosures.current.add(disclosure)
		else activeChatDisclosures.current.delete(disclosure)
		if(!summary||!container.contains(summary)){
			if(activeChatDisclosures.current.size===0)container.classList.remove('chat-disclosure-active')
			return
		}
		stickToLatest.current=false
		const top=summary.getBoundingClientRect().top
		window.cancelAnimationFrame(disclosureScrollFrame.current)
		container.classList.add('chat-disclosure-active')
		disclosureScrollFrame.current=window.requestAnimationFrame(()=>{
			if(container.isConnected&&summary.isConnected){container.scrollTop+=summary.getBoundingClientRect().top-top;lastMessagesScrollTop.current=container.scrollTop}
			if(activeChatDisclosures.current.size===0)container.classList.remove('chat-disclosure-active')
		})
	},[])

	const followLatest=useCallback((resetPosition=false)=>{
		stickToLatest.current=true
		if(resetPosition)lastMessagesScrollTop.current=0
	},[])
	const captureHistoryAnchor=useCallback(()=>{
		const container=messagesRef.current
		const previousHeight=container?.scrollHeight||0
		return()=>{
			if(container){container.scrollTop+=container.scrollHeight-previousHeight;lastMessagesScrollTop.current=container.scrollTop}
		}
	},[])

	return{messagesRef,trackUserScroll,pauseLatestOnWheel,pauseLatest,preserveChatDisclosurePosition,followLatest,captureHistoryAnchor}
}
