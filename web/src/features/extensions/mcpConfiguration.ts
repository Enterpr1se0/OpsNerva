import i18n from '../../lib/i18n'
import type { MCPServerInput } from '../../types'

function mcpStringMap(value:unknown,serverName:string,field:string){
	if(value===undefined)return undefined
	if(!value||typeof value!=='object'||Array.isArray(value))throw new Error(i18n.t('mcp.invalidField',{name:serverName,field}))
	const result:Record<string,string>={}
	for(const [name,content] of Object.entries(value)){
		if(typeof content!=='string')throw new Error(i18n.t('mcp.invalidField',{name:serverName,field}))
		result[name]=content
	}
	return result
}

export function parseMCPImport(value:string):MCPServerInput[]{
	let parsed:unknown
	try{parsed=JSON.parse(value)}catch{throw new Error(i18n.t('mcp.invalidConfig'))}
	if(!parsed||typeof parsed!=='object'||Array.isArray(parsed))throw new Error(i18n.t('mcp.invalidConfig'))
	const root=(parsed as Record<string,unknown>).mcpServers
	if(!root||typeof root!=='object'||Array.isArray(root)||!Object.keys(root).length)throw new Error(i18n.t('mcp.invalidConfig'))
	return Object.entries(root).map(([rawName,value])=>{
		const name=rawName.trim()
		if(!name||!value||typeof value!=='object'||Array.isArray(value))throw new Error(i18n.t('mcp.invalidEntry',{name:rawName||'?'}))
		const entry=value as Record<string,unknown>
		const url=typeof entry.url==='string'?entry.url.trim():''
		const command=typeof entry.command==='string'?entry.command.trim():''
		if((url?1:0)+(command?1:0)!==1)throw new Error(i18n.t('mcp.invalidEntry',{name}))
		if(entry.args!==undefined&&(!Array.isArray(entry.args)||entry.args.some(item=>typeof item!=='string')))throw new Error(i18n.t('mcp.invalidField',{name,field:'args'}))
		if(entry.cwd!==undefined&&typeof entry.cwd!=='string')throw new Error(i18n.t('mcp.invalidField',{name,field:'cwd'}))
		if(entry.disabled!==undefined&&typeof entry.disabled!=='boolean')throw new Error(i18n.t('mcp.invalidField',{name,field:'disabled'}))
		if(entry.enabled!==undefined&&typeof entry.enabled!=='boolean')throw new Error(i18n.t('mcp.invalidField',{name,field:'enabled'}))
		return {
			name,
			transport:url?'streamable_http':'stdio',
			command,
			args:Array.isArray(entry.args)?entry.args as string[]:[],
			cwd:typeof entry.cwd==='string'?entry.cwd.trim():'',
			url,
			env:mcpStringMap(entry.env,name,'env'),
			headers:mcpStringMap(entry.headers,name,'headers'),
			enabled:typeof entry.disabled==='boolean'?!entry.disabled:typeof entry.enabled==='boolean'?entry.enabled:true,
		}
	})
}

export function parseMCPPairs(value:string,kind:'env'|'header'){
	const result:Record<string,string>={}
	for(const raw of value.split(/\r?\n/)){
		const line=raw.trim();if(!line)continue
		const separator=kind==='env'?line.indexOf('='):line.indexOf(':')
		if(separator<1)throw new Error(i18n.t(kind==='env'?'mcp.invalidEnv':'mcp.invalidHeader',{line}))
		const name=line.slice(0,separator).trim(),content=line.slice(separator+1).trim()
		if(!name)throw new Error(i18n.t('mcp.invalidName',{kind}))
		result[name]=content
	}
	return result
}
