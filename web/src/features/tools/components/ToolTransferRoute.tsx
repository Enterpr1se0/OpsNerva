import { FolderOpen, Server } from 'lucide-react'
import type { WorkspaceTransferEndpoint, WorkspaceTransferRouteValue } from '../toolEvent'

export function ToolTransferRoute({sourceHost,sourcePath,destinationHost,destinationPath}:{sourceHost:string;sourcePath:string;destinationHost:string;destinationPath:string}){
	const route=`${sourceHost}:${sourcePath} → ${destinationHost}:${destinationPath}`
	return <span className="tool-transfer-route" title={route}><span><b>{sourceHost}</b><code>:{sourcePath}</code></span><i>→</i><span><b>{destinationHost}</b><code>:{destinationPath}</code></span></span>
}
function WorkspaceTransferEndpointLabel({endpoint}:{endpoint:WorkspaceTransferEndpoint}){
	return <span data-endpoint-kind={endpoint.kind}><b>{endpoint.kind==='workspace'?<FolderOpen size={11}/>:<Server size={11}/>} {endpoint.name}</b><code>:{endpoint.path}</code></span>
}
export function WorkspaceTransferRoute({source,destination}:WorkspaceTransferRouteValue){
	const route=`${source.name}:${source.path} → ${destination.name}:${destination.path}`
	return <span className="tool-transfer-route tool-workspace-transfer-route" title={route}><WorkspaceTransferEndpointLabel endpoint={source}/><i>→</i><WorkspaceTransferEndpointLabel endpoint={destination}/></span>
}
