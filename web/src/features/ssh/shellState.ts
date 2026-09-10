import type { SSHShell } from '../../types'
import { api } from '../../api/api'

function operatorShell(shell:SSHShell){
	return ['quick','workspace','workspace_operator'].includes(shell.surface)
}

export function sshShellCanReconnect(shell:SSHShell){
	return operatorShell(shell)&&shell.status==='failed'
		&&['connection_lost','process_lost'].includes(shell.termination_reason||'')
}

const reconnectingShells=new Map<string,Promise<SSHShell>>()
export function reconnectOperatorShell(shell:SSHShell){
	const current=reconnectingShells.get(shell.id)
	if(current)return current
	const pending=api.reconnectSSHShell(shell.id)
		.finally(()=>reconnectingShells.delete(shell.id))
	reconnectingShells.set(shell.id,pending)
	return pending
}
