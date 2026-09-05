import type { Host, HostInput } from '../../types'

export function hostInputWithAgentState(host:Host,enabled:boolean):HostInput{
	return {
		id:host.id,name:host.name,address:host.address,port:host.port,user:host.user,agent_enabled:enabled,
		auth_type:host.auth_type,private_key:'',known_hosts_file:host.known_hosts_file||'',proxy_jump_host_id:host.proxy_jump_host_id||'',proxy_id:host.proxy_id||'',
		password:'',sudo_mode:host.sudo_mode,sudo_password:'',
	}
}

export function hostSupportsRoot(host:Host){return host.user.trim().toLowerCase()==='root'||host.sudo_mode==='nopasswd'||host.sudo_mode==='password'&&host.has_sudo_password}
