import type { AgentEvent, Approval, ApprovalExecutionResult, AuditRunDeleteResult, AuthStatus, ChatContextCompressionResult, ChatMessage, ChatMessagePage, ChatQueueMode, ChatSession, ChatState, ConfigurationImportResult, Health, Host, HostInput, LLMToolCatalog, ManagedSkill, MCPActivitySnapshot, MCPOAuthStart, MCPServer, MCPServerInput, MCPTestResult, ModelCatalog, ModelDiscoveryInput, ModelProvider, ModelProviderInput, ModelTestInput, ModelTestJob, ModelTestResult, Proxy, ProxyInput, ProxyTestResult, QueuedChatMessage, Run, RunDetail, RunSearchPage, ServerLogResponse, SFTPFileList, SFTPMutationResult, SSHHostStatus, SSHShell, SSHShellList, SSHShellSnapshot, SSHShellStartInput, SSHTunnel, SSHTunnelList, SSHTunnelStartInput, SSHTunnelUpdateInput, SystemSettings, SystemSettingsInput, ToolCapabilities, WebSearchResponse, WebSearchSettings, WebSearchSettingsInput, WorkspaceCapability, WorkspaceDeleteResult, WorkspaceFileList, WorkspaceFilePreview, WorkspaceInput, WorkspaceUploadResult } from './types'

export type TransferProgress={loaded:number;total:number}
export type TransferOptions={signal?:AbortSignal;onProgress?:(progress:TransferProgress)=>void;totalBytes?:number}
type DownloadWritable={write:(data:Uint8Array)=>Promise<void>;close:()=>Promise<void>;abort?:(reason?:unknown)=>Promise<void>}
type SaveFileHandle={createWritable:()=>Promise<DownloadWritable>}
type SaveFilePicker=(options:{suggestedName:string})=>Promise<SaveFileHandle>

function transferError(status:number,statusText:string,response:unknown,authHeader:string|null){
	if(status===401&&authHeader==='required')window.dispatchEvent(new Event('opsnerva:unauthorized'))
	const body=response&&typeof response==='object'?response as {error?:string}:undefined
	const error=new Error(body?.error||statusText||'Transfer failed') as Error&{status?:number}
	error.status=status
	return error
}

function uploadJSON<T>(method:string,url:string,body:Blob,contentType:string,options:TransferOptions={}):Promise<T>{
	return new Promise((resolve,reject)=>{
		const xhr=new XMLHttpRequest()
		xhr.open(method,url)
		xhr.withCredentials=true
		xhr.responseType='json'
		xhr.setRequestHeader('Content-Type',contentType)
		xhr.upload.onprogress=event=>options.onProgress?.({loaded:event.loaded,total:event.lengthComputable?event.total:options.totalBytes||body.size})
		xhr.onload=()=>xhr.status>=200&&xhr.status<300?resolve(xhr.response as T):reject(transferError(xhr.status,xhr.statusText,xhr.response,xhr.getResponseHeader('X-OpsNerva-Auth')))
		xhr.onerror=()=>reject(new Error(xhr.statusText||'Transfer failed'))
		xhr.onabort=()=>reject(new DOMException('Transfer aborted','AbortError'))
		const abort=()=>xhr.abort()
		options.signal?.addEventListener('abort',abort,{once:true})
		xhr.onloadend=()=>options.signal?.removeEventListener('abort',abort)
		xhr.send(body)
	})
}

export function downloadFile(url:string,filename:string,options:TransferOptions={}):Promise<void>{
	const picker=(window as unknown as{showSaveFilePicker?:SaveFilePicker}).showSaveFilePicker?.bind(window)
	if(picker)return downloadFileStream(url,filename,options,picker)
	return new Promise((resolve,reject)=>{
		const xhr=new XMLHttpRequest()
		xhr.open('GET',url)
		xhr.withCredentials=true
		xhr.responseType='blob'
		xhr.onprogress=event=>options.onProgress?.({loaded:event.loaded,total:event.lengthComputable?event.total:options.totalBytes||0})
		xhr.onload=async()=>{
			if(xhr.status<200||xhr.status>=300){
				let body:unknown
				try{body=JSON.parse(await (xhr.response as Blob).text())}catch{/* non-JSON transfer errors use the HTTP status text */}
				reject(transferError(xhr.status,xhr.statusText,body,xhr.getResponseHeader('X-OpsNerva-Auth')));return
			}
			const objectURL=URL.createObjectURL(xhr.response as Blob)
			const anchor=document.createElement('a')
			anchor.href=objectURL;anchor.download=filename
			document.body.appendChild(anchor);anchor.click();anchor.remove()
			window.setTimeout(()=>URL.revokeObjectURL(objectURL),1000)
			resolve()
		}
		xhr.onerror=()=>reject(new Error(xhr.statusText||'Transfer failed'))
		xhr.onabort=()=>reject(new DOMException('Transfer aborted','AbortError'))
		const abort=()=>xhr.abort()
		options.signal?.addEventListener('abort',abort,{once:true})
		xhr.onloadend=()=>options.signal?.removeEventListener('abort',abort)
		xhr.send()
	})
}

async function downloadFileStream(url:string,filename:string,options:TransferOptions,picker:SaveFilePicker){
	const handle=await picker({suggestedName:filename})
	const writable=await handle.createWritable()
	try{
		const response=await fetch(url,{credentials:'same-origin',headers:{Accept:'application/octet-stream'},signal:options.signal})
		if(!response.ok)throw await responseError(response)
		if(!response.body)throw new Error('Download stream is unavailable')
		const responseTotal=Number(response.headers.get('Content-Length'))||0
		const total=responseTotal||options.totalBytes||0
		const reader=response.body.getReader()
		let loaded=0
		options.onProgress?.({loaded,total})
		for(;;){
			const {done,value}=await reader.read()
			if(done)break
			await writable.write(value)
			loaded+=value.byteLength
			options.onProgress?.({loaded,total})
		}
		options.onProgress?.({loaded,total:total||loaded})
		await writable.close()
	}catch(err){
		await writable.abort?.(err).catch(()=>undefined)
		throw err
	}
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
	const multipart=typeof FormData!=='undefined'&&init?.body instanceof FormData
	const headers:Record<string,string> = { ...(multipart?{}:{'Content-Type':'application/json'}), ...(init?.headers as Record<string,string> || {}) }
  const response = await fetch(path, {
    ...init,
	credentials:'same-origin',
	headers,
  })
  if (!response.ok) {
		throw await responseError(response)
  }
  if (response.status === 204) return undefined as T
  return response.json()
}

async function requestList<T>(path: string): Promise<T[]> {
  const value = await request<T[] | null>(path)
  return Array.isArray(value) ? value : []
}

async function waitForModelTest(job:ModelTestJob):Promise<ModelTestResult>{
	let current=job
	while(current.status==='running'){
		await new Promise(resolve=>window.setTimeout(resolve,500))
		current=await request<ModelTestJob>(`/api/v1/model-tests/${encodeURIComponent(current.id)}`)
	}
	if(current.status==='failed')throw new Error(current.error||'Model test failed')
	if(!current.result)throw new Error('Model test returned no result')
	return current.result
}

async function startModelTest(path:string,body:string):Promise<ModelTestResult>{
	const job=await request<ModelTestJob>(path,{method:'POST',body})
	return waitForModelTest(job)
}

export const api = {
	authStatus: () => request<AuthStatus>('/api/v1/auth/status'),
	login: (username:string,password:string) => request<AuthStatus>('/api/v1/auth/login',{method:'POST',body:JSON.stringify({username,password})}),
	logout: () => request<void>('/api/v1/auth/logout',{method:'POST',body:'{}'}),
	exportConfiguration: async(password:string) => {
		const response=await fetch('/api/v1/configuration/export',{method:'POST',credentials:'same-origin',headers:{'Content-Type':'application/json'},body:JSON.stringify({password})})
		if(!response.ok)throw await responseError(response)
		const disposition=response.headers.get('Content-Disposition')||''
		const match=/filename=(?:"([^"]+)"|([^;]+))/i.exec(disposition)
		return{blob:await response.blob(),filename:(match?.[1]||match?.[2]||'opsnerva-configuration.opsnerva-config').trim()}
	},
	importConfiguration: (file:File,password:string) => {const body=new FormData();body.set('file',file,file.name);body.set('password',password);return request<ConfigurationImportResult>('/api/v1/configuration/import',{method:'POST',body})},
  health: () => request<Health>('/api/v1/health'),
	systemSettings: () => request<SystemSettings>('/api/v1/settings'),
	capabilities: () => request<ToolCapabilities>('/api/v1/capabilities'),
	llmTools: () => request<LLMToolCatalog>('/api/v1/agent/tools'),
	setLLMToolEnabled: (name:string,enabled:boolean) => request<LLMToolCatalog>(`/api/v1/agent/tools/${encodeURIComponent(name)}/${enabled?'enable':'disable'}`,{method:'POST',body:'{}'}),
	skills: () => requestList<ManagedSkill>('/api/v1/skills'),
	reloadSkills: () => request<LLMToolCatalog>('/api/v1/skills/reload',{method:'POST',body:'{}'}),
	skill: (name:string) => request<ManagedSkill>(`/api/v1/skills/${encodeURIComponent(name)}`),
	uploadSkill: (name:string,file:File) => {const body=new FormData();body.set('name',name);body.set('file',file);return request<ManagedSkill[]>('/api/v1/skills',{method:'POST',body})},
	saveSkill: (name:string,content:string) => request<ManagedSkill>(`/api/v1/skills/${encodeURIComponent(name)}`,{method:'PUT',body:JSON.stringify({content})}),
	deleteSkill: (name:string) => request<void>(`/api/v1/skills/${encodeURIComponent(name)}`,{method:'DELETE'}),
	setSkillEnabled: (name:string,enabled:boolean) => request<ManagedSkill>(`/api/v1/skills/${encodeURIComponent(name)}/${enabled?'enable':'disable'}`,{method:'POST',body:'{}'}),
	mcpServers: () => requestList<MCPServer>('/api/v1/mcp-servers'),
	saveMCPServer: (server:MCPServerInput) => server.id
		? request<MCPServer>(`/api/v1/mcp-servers/${encodeURIComponent(server.id)}`,{method:'PUT',body:JSON.stringify(server)})
		: request<MCPServer>('/api/v1/mcp-servers',{method:'POST',body:JSON.stringify(server)}),
	deleteMCPServer: (id:string) => request<void>(`/api/v1/mcp-servers/${encodeURIComponent(id)}`,{method:'DELETE'}),
	setMCPServerEnabled: (id:string,enabled:boolean) => request<MCPServer>(`/api/v1/mcp-servers/${encodeURIComponent(id)}/${enabled?'enable':'disable'}`,{method:'POST',body:'{}'}),
	retryMCPServer: (id:string) => request<MCPServer>(`/api/v1/mcp-servers/${encodeURIComponent(id)}/retry`,{method:'POST',body:'{}'}),
	testMCPServer: (id:string) => request<MCPTestResult>(`/api/v1/mcp-servers/${encodeURIComponent(id)}/test`,{method:'POST',body:'{}'}),
	startMCPOAuth: (id:string) => request<MCPOAuthStart>(`/api/v1/mcp-servers/${encodeURIComponent(id)}/oauth`,{method:'POST',body:'{}'}),
	clearMCPOAuth: (id:string) => request<MCPServer>(`/api/v1/mcp-servers/${encodeURIComponent(id)}/oauth`,{method:'DELETE'}),
	mcpActivity: (sessionId='',sessionLimit=100,callLimit=200) => {const query=new URLSearchParams({session_limit:String(sessionLimit),call_limit:String(callLimit)});if(sessionId)query.set('session_id',sessionId);return request<MCPActivitySnapshot>(`/api/v1/mcp/activity?${query}`)},
	createWorkspace: (workspace:WorkspaceInput) => request<WorkspaceCapability>('/api/v1/workspaces',{method:'POST',body:JSON.stringify(workspace)}),
	updateWorkspace: (id:string,workspace:WorkspaceInput) => request<WorkspaceCapability>(`/api/v1/workspaces/${encodeURIComponent(id)}`,{method:'PUT',body:JSON.stringify(workspace)}),
	deleteWorkspace: (id:string) => request<void>(`/api/v1/workspaces/${encodeURIComponent(id)}`,{method:'DELETE'}),
	workspaceFiles: (workspaceId:string,path='.') => request<WorkspaceFileList>(`/api/v1/workspaces/${encodeURIComponent(workspaceId)}/files?path=${encodeURIComponent(path)}`),
	previewWorkspaceFile: (workspaceId:string,path:string) => request<WorkspaceFilePreview>(`/api/v1/workspaces/${encodeURIComponent(workspaceId)}/preview?path=${encodeURIComponent(path)}`),
	saveWorkspaceTextFile: (workspaceId:string,path:string,content:string) => request<WorkspaceUploadResult>(`/api/v1/workspaces/${encodeURIComponent(workspaceId)}/files`,{method:'PUT',body:JSON.stringify({path,content})}),
	uploadWorkspaceFile: (workspaceId:string,file:File,path:string,options:TransferOptions={}) => {const query=new URLSearchParams({path,filename:file.name});return uploadJSON<WorkspaceUploadResult>('POST',`/api/v1/workspaces/${encodeURIComponent(workspaceId)}/files?${query}`,file,file.type||'application/octet-stream',{...options,totalBytes:file.size})},
	deleteWorkspaceEntry: (workspaceId:string,path:string) => request<WorkspaceDeleteResult>(`/api/v1/workspaces/${encodeURIComponent(workspaceId)}/files?path=${encodeURIComponent(path)}`,{method:'DELETE'}),
  saveSystemSettings: (settings: SystemSettingsInput) => request<SystemSettings>('/api/v1/settings', { method: 'PUT', body: JSON.stringify(settings) }),
  webSearchSettings: () => request<WebSearchSettings>('/api/v1/web-search/settings'),
  saveWebSearchSettings: (settings: WebSearchSettingsInput) => request<WebSearchSettings>('/api/v1/web-search/settings', { method: 'PUT', body: JSON.stringify(settings) }),
  testWebSearch: (query='Tavily Search API') => request<WebSearchResponse>('/api/v1/web-search/test', { method: 'POST', body: JSON.stringify({query}) }),
	proxies: () => requestList<Proxy>('/api/v1/proxies'),
	saveProxy: (proxy:ProxyInput) => request<Proxy>('/api/v1/proxies',{method:'POST',body:JSON.stringify(proxy)}),
	deleteProxy: (id:string) => request<void>(`/api/v1/proxies/${encodeURIComponent(id)}`,{method:'DELETE'}),
	testProxy: (id:string) => request<ProxyTestResult>(`/api/v1/proxies/${encodeURIComponent(id)}/test`,{method:'POST',body:'{}'}),
  modelProviders: () => requestList<ModelProvider>('/api/v1/model-providers'),
  discoverModels: (input: ModelDiscoveryInput) => request<ModelCatalog>('/api/v1/model-providers/discover', { method: 'POST', body: JSON.stringify(input) }),
  testModelConfiguration: (input: ModelTestInput) => startModelTest('/api/v1/model-providers/test', JSON.stringify(input)),
  saveModelProvider: (provider: ModelProviderInput) => request<ModelProvider>('/api/v1/model-providers', { method: 'POST', body: JSON.stringify(provider) }),
  activateModelProvider: (id: string) => request<ModelProvider>(`/api/v1/model-providers/${id}/activate`, { method: 'POST', body: '{}' }),
  deleteModelProvider: (id: string) => request<void>(`/api/v1/model-providers/${id}`, { method: 'DELETE' }),
  testModelProvider: (id: string) => startModelTest(`/api/v1/model-providers/${id}/test`, '{}'),
  hosts: () => requestList<Host>('/api/v1/hosts'),
  sshTunnels: () => request<SSHTunnelList>('/api/v1/ssh-tunnels'),
  startSSHTunnel: (input:SSHTunnelStartInput) => request<SSHTunnel>('/api/v1/ssh-tunnels', { method:'POST', body:JSON.stringify(input) }),
  updateSSHTunnel: (id:string,input:SSHTunnelUpdateInput) => request<SSHTunnel>(`/api/v1/ssh-tunnels/${encodeURIComponent(id)}`, { method:'PUT', body:JSON.stringify(input) }),
  stopSSHTunnel: (id:string) => request<SSHTunnel>(`/api/v1/ssh-tunnels/${encodeURIComponent(id)}`, { method:'DELETE' }),
  sshShells: (sessionId='') => request<SSHShellList>(`/api/v1/ssh-shells?session_id=${encodeURIComponent(sessionId)}`),
  startSSHShell: (input:SSHShellStartInput) => request<SSHShell>('/api/v1/ssh-shells', { method:'POST', body:JSON.stringify(input) }),
  sshShell: (id:string,after=0,coalesce=false) => request<SSHShellSnapshot>(`/api/v1/ssh-shells/${encodeURIComponent(id)}?after=${after}&coalesce=${coalesce}`),
  sshShellHostStatus: (id:string) => request<SSHHostStatus>(`/api/v1/ssh-shells/${encodeURIComponent(id)}/host-status`),
  sshShellInput: (id:string,input:string,sensitive=false,submit=false,reason='') => request<void>(`/api/v1/ssh-shells/${encodeURIComponent(id)}/input`, { method:'POST', body:JSON.stringify({input,sensitive,submit,reason}) }),
  resizeSSHShell: (id:string,cols:number,rows:number) => request<SSHShell>(`/api/v1/ssh-shells/${encodeURIComponent(id)}/resize`, { method:'POST', body:JSON.stringify({cols,rows}) }),
  interruptSSHShell: (id:string) => request<SSHShell>(`/api/v1/ssh-shells/${encodeURIComponent(id)}/interrupt`, { method:'POST', body:'{}' }),
  closeSSHShell: (id:string) => request<SSHShell>(`/api/v1/ssh-shells/${encodeURIComponent(id)}`, { method:'DELETE' }),
  sftpEntries: (hostId:string,path='') => request<SFTPFileList>(`/api/v1/hosts/${encodeURIComponent(hostId)}/sftp/entries?path=${encodeURIComponent(path)}`),
  sftpFile: async(hostId:string,path:string) => {
		const response=await fetch(sftpDownloadURL(hostId,path),{credentials:'same-origin',headers:{Accept:'application/octet-stream'}})
		if(!response.ok)throw await responseError(response)
		return response.arrayBuffer()
	},
	  uploadSFTPFile: (hostId:string,path:string,file:File,overwrite=false,options:TransferOptions={}) => uploadJSON<SFTPMutationResult>('PUT',`/api/v1/hosts/${encodeURIComponent(hostId)}/sftp/files?path=${encodeURIComponent(path)}&overwrite=${overwrite}`,file,file.type||'application/octet-stream',{...options,totalBytes:file.size}),
  uploadSFTPTextFile: (hostId:string,path:string,content:string,encoding:'utf-8'|'utf-16le'|'utf-16be'|'gb18030') => request<SFTPMutationResult>(`/api/v1/hosts/${encodeURIComponent(hostId)}/sftp/files?path=${encodeURIComponent(path)}&overwrite=true&encoding=${encodeURIComponent(encoding)}`, { method:'PUT', body:content, headers:{'Content-Type':'text/plain;charset=utf-8'} }),
  createSFTPDirectory: (hostId:string,path:string) => request<SFTPMutationResult>(`/api/v1/hosts/${encodeURIComponent(hostId)}/sftp/directories`, { method:'POST', body:JSON.stringify({path}) }),
  renameSFTPEntry: (hostId:string,sourcePath:string,destinationPath:string) => request<SFTPMutationResult>(`/api/v1/hosts/${encodeURIComponent(hostId)}/sftp/entries`, { method:'PATCH', body:JSON.stringify({source_path:sourcePath,destination_path:destinationPath}) }),
  deleteSFTPEntry: (hostId:string,path:string,recursive=false) => request<SFTPMutationResult>(`/api/v1/hosts/${encodeURIComponent(hostId)}/sftp/entries?path=${encodeURIComponent(path)}&recursive=${recursive}`, { method:'DELETE' }),
  saveHost: (host: HostInput) => request<Host>('/api/v1/hosts', { method: 'POST', body: JSON.stringify(host) }),
  setHostAgentRootAccess: (id:string,enabled:boolean) => request<Host>(`/api/v1/hosts/${encodeURIComponent(id)}/agent-root`, { method:'PUT', body:JSON.stringify({enabled}) }),
  deleteHost: (id: string) => request<void>(`/api/v1/hosts/${id}`, { method: 'DELETE' }),
	  scanKey: (id: string) => request<{ fingerprint: string; algorithm?: string; trusted: boolean }>(`/api/v1/hosts/${id}/scan-key`, { method: 'POST', body: '{}' }),
	  trustKey: (id: string, fingerprint: string) => request<{ fingerprint: string; algorithm?: string; trusted: boolean }>(`/api/v1/hosts/${id}/trust-key`, { method: 'POST', body: JSON.stringify({ fingerprint }) }),
  probe: (id: string) => request<Record<string, string>>(`/api/v1/hosts/${id}/probe`, { method: 'POST', body: '{}' }),
  approvals: () => requestList<Approval>('/api/v1/approvals?status=pending&limit=100'),
  retryApprovalExplanation: (id: string) => request<Approval>(`/api/v1/approvals/${id}/explanation/retry`, { method: 'POST', body: '{}' }),
  approve: (id: string, reason: string) => request<ApprovalExecutionResult>(`/api/v1/approvals/${id}/approve`, { method: 'POST', body: JSON.stringify({ reason }) }),
  reject: (id: string, reason: string) => request(`/api/v1/approvals/${id}/reject`, { method: 'POST', body: JSON.stringify({ reason }) }),
  runs: (query = '') => requestList<Run>(`/api/v1/runs?limit=100&q=${encodeURIComponent(query)}`),
  runSummaries: (input:{query?:string;limit?:number;cursorStartedAt?:string;cursorID?:string}={}) => {
	  const params=new URLSearchParams({limit:String(input.limit||100)})
	  if(input.query)params.set('q',input.query)
	  if(input.cursorStartedAt)params.set('cursor_started_at',input.cursorStartedAt)
	  if(input.cursorID)params.set('cursor_id',input.cursorID)
	  return request<RunSearchPage>(`/api/v1/run-summaries?${params}`)
	},
  deleteAuditRuns: (sessionID?:string|null) => {
	  const suffix=sessionID===undefined?'':`?session_id=${encodeURIComponent(sessionID||'')}`
	  return request<AuditRunDeleteResult>(`/api/v1/audit/runs${suffix}`,{method:'DELETE'})
	},
  runDetail: (id: string) => request<RunDetail>(`/api/v1/runs/${encodeURIComponent(id)}`),
  logs: (filters: {level?:string;component?:string;q?:string;limit?:number} = {}) => {
    const params=new URLSearchParams()
    if(filters.level)params.set('level',filters.level)
    if(filters.component)params.set('component',filters.component)
    if(filters.q)params.set('q',filters.q)
    params.set('limit',String(filters.limit||500))
    return request<ServerLogResponse>(`/api/v1/logs?${params}`)
  },
  chatSessions: () => requestList<ChatSession>('/api/v1/chat/sessions?limit=50'),
	chatState: (id: string) => request<ChatState>(`/api/v1/chat/${encodeURIComponent(id)}/state`),
	chatMessages: (id:string,cursor?:{createdAt:string;id:string},limit=100) => {
		const params=new URLSearchParams({limit:String(limit)})
		if(cursor?.createdAt&&cursor.id){params.set('before_created_at',cursor.createdAt);params.set('before_id',cursor.id)}
		return request<ChatMessagePage>(`/api/v1/chat/${encodeURIComponent(id)}/messages?${params}`)
	},
	chatMessage: (id:string,messageId:string) => request<ChatMessage>(`/api/v1/chat/${encodeURIComponent(id)}/messages/${encodeURIComponent(messageId)}`),
	queueChatMessage: (id:string,message:string,images:File[],mode:ChatQueueMode='followup') => {const body=new FormData();body.set('message',message);for(const image of images)body.append('images',image,image.name);return request<{item:QueuedChatMessage;position:number;steering_requested:boolean}>(`/api/v1/chat/${encodeURIComponent(id)}/queue?mode=${encodeURIComponent(mode)}`,{method:'POST',body})},
	compressChatContext: (id:string) => request<ChatContextCompressionResult>(`/api/v1/chat/${encodeURIComponent(id)}/context/compress`, {method:'POST',body:'{}'}),
	setChatSessionWorkspace: (id:string,workspaceId:string) => request<ChatSession>(`/api/v1/chat/${encodeURIComponent(id)}/workspace`, { method:'PUT', body:JSON.stringify({workspace_id:workspaceId}) }),
	renameChatSession: (id:string,title:string) => request<ChatSession>(`/api/v1/chat/${encodeURIComponent(id)}/title`, { method:'PUT', body:JSON.stringify({title}) }),
	cancelChatSession: (id: string) => request<{cancelled:boolean;cancelled_tools:number;cancelled_queued:number;rejected_approvals:number}>(`/api/v1/chat/${encodeURIComponent(id)}/cancel`, { method: 'POST', body: '{}' }),
  deleteChatSession: (id: string) => request<void>(`/api/v1/chat/${encodeURIComponent(id)}`, { method: 'DELETE' }),
}

export function chatAttachmentURL(sessionId:string,attachmentId:string){
	return `/api/v1/chat/${encodeURIComponent(sessionId)}/attachments/${encodeURIComponent(attachmentId)}`
}

export function workspaceFileEventsURL(workspaceId:string,path:string){
	return `/api/v1/workspaces/${encodeURIComponent(workspaceId)}/events?path=${encodeURIComponent(path)}`
}

export function workspaceDownloadURL(workspaceId:string,path:string){
	return `/api/v1/workspaces/${encodeURIComponent(workspaceId)}/download?path=${encodeURIComponent(path)}`
}

export function sshShellEventsURL(shellId:string,after=0){
	return `/api/v1/ssh-shells/${encodeURIComponent(shellId)}/events?after=${after}`
}

export function sshShellWebSocketURL(shellId:string,after=0){
	const protocol=window.location.protocol==='https:'?'wss:':'ws:'
	return `${protocol}//${window.location.host}/api/v1/ssh-shells/${encodeURIComponent(shellId)}/ws?after=${Math.max(0,after)}`
}

export function sftpDownloadURL(hostId:string,path:string){
	return `/api/v1/hosts/${encodeURIComponent(hostId)}/sftp/files?path=${encodeURIComponent(path)}`
}

async function responseError(response:Response){
	if(response.status===401&&response.headers.get('X-OpsNerva-Auth')==='required')window.dispatchEvent(new Event('opsnerva:unauthorized'))
	const body=await response.json().catch(()=>({error:response.statusText}))
	const error=new Error(body.error||response.statusText) as Error&{status?:number}
	error.status=response.status
	return error
}

async function consumeAgentEventStream(response:Response,onEvents:(events:readonly AgentEvent[])=>void){
	if(!response.ok||!response.body)throw await responseError(response)
	const reader=response.body.getReader()
	const decoder=new TextDecoder()
	let buffer=''
	let terminalEventReceived=false
	let flushTimer:number|undefined
	let pending:AgentEvent[]=[]
	const flushInterval=80

	const flushPending=()=>{
		if(flushTimer!==undefined)window.clearTimeout(flushTimer)
		flushTimer=undefined
		const events=pending
		pending=[]
		if(events.length)onEvents(events)
	}
	const isContentDelta=(event:AgentEvent)=>
		!!event.content&&(event.type==='reasoning'||event.type==='tool_output'||(event.type==='message'&&event.role!=='tool'))
	const isProgressUpdate=(event:AgentEvent)=>event.type==='tool_output'&&event.stream==='progress'
	const sameContentStream=(left:AgentEvent,right:AgentEvent)=>
		left.type===right.type&&left.role===right.role&&left.tool_name===right.tool_name&&
		left.message_id===right.message_id&&left.user_message_id===right.user_message_id&&left.tool_call_id===right.tool_call_id&&left.segment_id===right.segment_id&&
		left.session_id===right.session_id&&left.run_id===right.run_id&&left.stream===right.stream&&
		left.status===right.status
	const dispatch=(event:AgentEvent)=>{
		if(event.type==='done'||event.type==='error'||event.type==='model_error'||event.type==='interrupted')terminalEventReceived=true
		if(!isContentDelta(event)&&!isProgressUpdate(event)){
			pending.push(event)
			flushPending()
			return
		}
		const previous=pending.at(-1)
		if(isProgressUpdate(event)&&previous&&isProgressUpdate(previous)&&sameContentStream(previous,event)){
			pending[pending.length-1]={...previous,...event}
		}else if(previous&&isContentDelta(previous)&&sameContentStream(previous,event)){
			previous.content=(previous.content||'')+event.content
			previous.event_id=event.event_id||previous.event_id
		}else pending.push({...event})
		if(flushTimer===undefined)flushTimer=window.setTimeout(flushPending,flushInterval)
	}
	const processFrame=(frame:string)=>{
		const data=frame.split('\n').filter(line=>line.startsWith('data:')).map(line=>line.slice(5).replace(/^ /,'')).join('\n')
		if(!data)return
		dispatch(JSON.parse(data) as AgentEvent)
	}

	try{
		while(true){
			const{value,done}=await reader.read()
			if(done)break
			buffer+=decoder.decode(value,{stream:true})
			buffer=buffer.replace(/\r\n/g,'\n')
			let boundary=buffer.indexOf('\n\n')
			while(boundary>=0){processFrame(buffer.slice(0,boundary));buffer=buffer.slice(boundary+2);boundary=buffer.indexOf('\n\n')}
		}
		buffer+=decoder.decode()
	}finally{flushPending()}
	if(buffer.trim())throw new Error('SSE stream ended with an incomplete event')
	if(!terminalEventReceived)throw new Error('SSE stream ended before the Agent sent a terminal event')
}

export async function streamChat(sessionId: string, workspaceId:string, message: string, images:File[], onEvents: (events:readonly AgentEvent[]) => void, signal?: AbortSignal) {
	const body=new FormData()
	body.set('session_id',sessionId)
	body.set('workspace_id',workspaceId)
	body.set('message',message)
	for(const image of images)body.append('images',image,image.name)
  const response = await fetch('/api/v1/chat', {
    method: 'POST',
	credentials:'same-origin',
	body,
    signal,
  })
	return consumeAgentEventStream(response,onEvents)
}

export async function reconnectChatStream(sessionId:string,after:number,onEvents:(events:readonly AgentEvent[])=>void,signal?:AbortSignal){
	const response=await fetch(`/api/v1/chat/${encodeURIComponent(sessionId)}/events?after=${Math.max(0,after)}`,{
		credentials:'same-origin',headers:{Accept:'text/event-stream'},signal,
	})
	return consumeAgentEventStream(response,onEvents)
}
