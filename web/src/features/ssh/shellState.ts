import type { SSHShell } from '../../types'
import { keepEquivalent } from '../../lib/utils'
import { sshShellActive } from './utils'
import { api } from '../../api/api'

function operatorShell(shell:SSHShell){
	return ['quick','workspace','workspace_operator'].includes(shell.surface)
}

export function sshShellCanReconnect(shell:SSHShell){
	return operatorShell(shell)&&['failed','interrupted'].includes(shell.status)
		&&(!shell.termination_reason||['connection_lost','process_lost','service_stopped'].includes(shell.termination_reason))
}

const reconnectingShells=new Map<string,Promise<SSHShell>>()
export function reconnectOperatorShell(shell:SSHShell){
	const current=reconnectingShells.get(shell.id)
	if(current)return current
	const pending=api.startSSHShell(shell.kind==='workspace'?{workspace_id:shell.workspace_id,cwd:shell.cwd}:{host_id:shell.host_id,surface:shell.surface==='workspace'?'workspace':'quick'})
		.finally(()=>reconnectingShells.delete(shell.id))
	reconnectingShells.set(shell.id,pending)
	return pending
}

// Keep only operator reconnect targets in browser memory, never in Agent history.
export function mergeSSHShellSnapshot(current:SSHShell[],incoming:SSHShell[]){
	const ids=new Set(incoming.map(shell=>shell.id))
	const retained=current.filter(shell=>!ids.has(shell.id)&&operatorShell(shell)&&shell.status!=='stopping')
		.map(shell=>sshShellActive(shell.status)?{...shell,status:'failed' as const,termination_reason:'connection_lost' as const}:shell)
		.filter(sshShellCanReconnect)
	return keepEquivalent(current,[...incoming,...retained])
}
