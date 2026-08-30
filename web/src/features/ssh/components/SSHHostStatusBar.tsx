import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Clock3, Cpu, HardDrive, MemoryStick, Network } from 'lucide-react'
import type { SSHHostStatus, SSHShell } from '../../../types'
import { api } from '../../../api/api'
import { errorStatus } from '../../../lib/utils'
import { formatHostUptime, formatStatusBytes, formatStatusPair, sshShellActive } from '../utils'
import type { SSHHostStatusView } from '../types'

export function SSHHostStatusBar({shell}:{shell:SSHShell}){
	const {t}=useTranslation()
	const [view,setView]=useState<SSHHostStatusView|null>(null)
	const [unavailable,setUnavailable]=useState(false)
	const previousRef=useRef<SSHHostStatus|null>(null)
	useEffect(()=>{
		let disposed=false
		let monitoringEnded=false
		let timer:number|undefined
		previousRef.current=null
		setView(null)
		setUnavailable(false)
		const sample=async()=>{
			let nextDelay=3000
			try{
				const next=await api.sshShellHostStatus(shell.id)
				if(disposed)return
				const previous=previousRef.current
				let cpuPercent:number|null=null
				let receivedPerSecond:number|null=null
				let sentPerSecond:number|null=null
				if(previous){
					const totalDelta=next.cpu_total-previous.cpu_total
					const idleDelta=next.cpu_idle-previous.cpu_idle
					if(totalDelta>0&&idleDelta>=0)cpuPercent=Math.max(0,Math.min(100,(totalDelta-idleDelta)/totalDelta*100))
					const elapsed=(Date.parse(next.sampled_at)-Date.parse(previous.sampled_at))/1000
					if(elapsed>0&&next.network_received_bytes>=previous.network_received_bytes&&next.network_sent_bytes>=previous.network_sent_bytes){
						receivedPerSecond=(next.network_received_bytes-previous.network_received_bytes)/elapsed
						sentPerSecond=(next.network_sent_bytes-previous.network_sent_bytes)/elapsed
					}
				}else nextDelay=1000
				previousRef.current=next
				setView({sample:next,cpuPercent,receivedPerSecond,sentPerSecond})
				setUnavailable(false)
			}catch(err){
				if(errorStatus(err)===404||errorStatus(err)===409)monitoringEnded=true
				if(!disposed)setUnavailable(true)
			}finally{
				if(!disposed&&!monitoringEnded&&sshShellActive(shell.status))timer=window.setTimeout(()=>void sample(),nextDelay)
			}
		}
		if(sshShellActive(shell.status))void sample()
		return()=>{disposed=true;if(timer!==undefined)window.clearTimeout(timer)}
	},[shell.id,shell.status])
	const sample=view?.sample
	const memoryPercent=sample?.memory_total_bytes?sample.memory_used_bytes/sample.memory_total_bytes*100:0
	const diskPercent=sample?.disk_total_bytes?sample.disk_used_bytes/sample.disk_total_bytes*100:0
	const stateLabel=unavailable?t('common.unavailable'):view?t('common.status'):t('common.loading')
	return <div className={`ssh-host-status ${unavailable?'unavailable':''}`} role="status" aria-label={stateLabel}>
		<span aria-label={t('sshWorkspace.cpu')} title={`${t('sshWorkspace.cpu')} · ${view?.cpuPercent==null?'—':`${view.cpuPercent.toFixed(1)}%`}`}><Cpu size={13}/><b>{view?.cpuPercent==null?'—':`${view.cpuPercent.toFixed(1)}%`}</b></span>
		<span aria-label={t('sshWorkspace.memory')} title={`${t('sshWorkspace.memory')} · ${sample?`${memoryPercent.toFixed(1)}%`:'—'}`}><MemoryStick size={13}/><b>{sample?formatStatusPair(sample.memory_used_bytes,sample.memory_total_bytes):'—'}</b></span>
		<span aria-label={t('sshWorkspace.network')} title={`${t('sshWorkspace.network')} · ${view?.receivedPerSecond==null?'—':`↓ ${formatStatusBytes(view.receivedPerSecond)}/s · ↑ ${formatStatusBytes(view.sentPerSecond||0)}/s`}`}><Network size={13}/><b>{view?.receivedPerSecond==null?'—':`↓ ${formatStatusBytes(view.receivedPerSecond)}/s · ↑ ${formatStatusBytes(view.sentPerSecond||0)}/s`}</b></span>
		<span aria-label={t('sshWorkspace.disk')} title={`${t('sshWorkspace.disk')} · ${sample?`${diskPercent.toFixed(1)}%`:'—'}`}><HardDrive size={13}/><b>{sample?formatStatusPair(sample.disk_used_bytes,sample.disk_total_bytes):'—'}</b></span>
		<span aria-label={t('sshWorkspace.uptime')} title={`${t('sshWorkspace.uptime')} · ${sample?formatHostUptime(sample.uptime_seconds):'—'}`}><Clock3 size={13}/><b>{sample?formatHostUptime(sample.uptime_seconds):'—'}</b></span>
	</div>
}
