import { useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import type { LiveSSHTaskSnapshot } from '../../../lib/liveTasks'
import type { Host, Run } from '../../../types'
import type { ChatEntry } from '../../chat/types'
import type { ChatDisclosurePositionHandler } from '../../chat/useChatCardDisclosure'
import { parseRecord } from '../payload'
import { buildToolEventView } from '../toolEvent'
import { useToolEventDisclosure } from '../useToolEventDisclosure'
import { ToolEventBody } from './ToolEventBody'
import { ToolEventSummary } from './ToolEventSummary'

type ToolEventCardProps={
	sessionID:string
	entry:ChatEntry
	runs:Run[]
	hosts:Host[]
	liveSSHTaskOwner:boolean
	currentLiveSSHTask?:LiveSSHTaskSnapshot
	onDisclosure:ChatDisclosurePositionHandler
}

export function ToolEventCard({sessionID,entry:initialEntry,runs,hosts,liveSSHTaskOwner,currentLiveSSHTask,onDisclosure}:ToolEventCardProps){
	const {t}=useTranslation()
	const {entry,disclosure,loadingDetail,detailError,toggleExpanded}=useToolEventDisclosure(sessionID,initialEntry,onDisclosure)
	const storedPayload=useMemo(()=>parseRecord(entry.content,t),[entry.content,t])
	const view=useMemo(()=>buildToolEventView({entry,storedPayload,runs,hosts,liveSSHTaskOwner,currentLiveSSHTask,t}),[entry,storedPayload,runs,hosts,liveSSHTaskOwner,currentLiveSSHTask,t])
	const [firstSeenAt]=useState(Date.now)
	return <details className={`tool-event tool-event-rich ${view.sshTaskOperation?'ssh-task-tool ':''}${view.status}`} open={disclosure.expanded} onTransitionEnd={disclosure.finishTransition}>
		<ToolEventSummary view={view} loadingDetail={loadingDetail} startedAt={view.startedAt??firstSeenAt} toggleExpanded={toggleExpanded}/>
		{disclosure.renderBody&&<ToolEventBody view={view} detailError={detailError}/>}
	</details>
}
